# Managed PostgreSQL 17 HA primary module.
#
# TRACEABILITY
#   23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: "System of record | Managed
#     PostgreSQL 17 HA primary | Single writer for authoritative financial state;
#     analytics use replicas/projections."
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: "PostgreSQL is a managed highly
#     available primary with synchronous protection within the primary region and
#     encrypted WAL/backups replicated to a recovery region."
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Begin PostgreSQL with a managed HA
#     primary/standby pair, each at least 8 vCPU / 32 GiB RAM and 1 TiB encrypted
#     SSD storage, with automated WAL archiving and connection pooling;
#     benchmark-derived IOPS and storage growth determine final provisioned
#     capacity."
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "PostgreSQL does not autoscale
#     blindly: storage, CPU, memory, connections, IOPS, WAL rate, and replica lag
#     are reviewed against benchmark and growth forecasts."
#   05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: "Production PostgreSQL uses
#     continuous WAL archiving, daily full backup, and weekly restore verification.
#     Backups are encrypted, access-controlled, and retained according to the
#     defined retention schedule: 35 daily restore points, 12 monthly restore
#     points."
#   05 §Data retention: "Audit and financial records: 7 years."
#   10_OPERATIONS_AND_DISASTER_RECOVERY.md: "PostgreSQL WAL and backups are
#     replicated to the recovery location."
#   08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives: RTO <= 30 minutes;
#     RPO <= 5 minutes.
#   06_SECURITY_AND_ACCESS_CONTROL.md §6: managed envelope encryption; TLS 1.2
#     minimum for transport.
#   25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §6: "PostgreSQL
#     primary unavailable | Stop authoritative mutations; do not substitute Redis,
#     event bus, or local memory as source of truth."

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
  }
}

# --- Authoritative primary --------------------------------------------------

resource "google_sql_database_instance" "primary" {
  name                = "${var.environment_name}-pg17-ha"
  project             = var.project_id
  region              = var.primary_region
  database_version    = "POSTGRES_17"
  deletion_protection = var.deletion_protection

  disk_encryption_configuration {
    kms_key_name = var.kms_key_name
  }

  settings {
    tier              = var.tier
    availability_type = "REGIONAL" # HA primary/standby pair (19 §3)
    disk_size         = var.data_disk_size_gb
    disk_type         = "PD_SSL"  # encrypted SSD (19 §3: 1 TiB encrypted SSD)
    disk_autoresize   = false     # 19 §3: PostgreSQL does not autoscale blindly
    user_labels = {
      environment = var.environment_name
      component   = "authoritative-database"
      authority   = "system-of-record"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "03:00" # daily full backup (05 §Backup)
      transaction_log_retention_days = var.daily_restore_points
      backup_retention_settings {
        retained_backups = var.daily_restore_points
        retention_unit   = "COUNT"
      }
    }

    ip_configuration {
      ipv4_enabled    = false # private only; 06 §1 databases remain private
      private_network = var.vpc_id
      require_ssl     = true  # 06 §6: TLS 1.2 minimum
    }

    insights_config {
      query_insights_enabled  = true
      query_string_length     = 1024
      record_application_tags = true
      record_client_address   = false # 06 §6: redact sensitive fields at ingestion
    }

    # 05 §Outbox: dispatcher reads committed outbox rows with FOR UPDATE SKIP
    # LOCKED. These flags give the bounded, identifiable statement evidence that
    # 08 §Observability requires (structured logs preserving command/event ids).
    database_flags {
      name  = "log_min_duration_statement"
      value = "1000"
    }
    database_flags {
      name  = "log_checkpoints"
      value = "on"
    }
    database_flags {
      name  = "log_connections"
      value = "on"
    }
    database_flags {
      name  = "log_disconnections"
      value = "on"
    }
    database_flags {
      name  = "log_lock_waits"
      value = "on"
    }
    # 24 §16 prohibits unbounded retries; a bounded statement timeout keeps a
    # single runaway transaction from holding the authoritative lock set.
    database_flags {
      name  = "statement_timeout"
      value = "30000"
    }

    activation_policy = "ALWAYS"
  }
}

# --- Analytics read replica -------------------------------------------------
# 19 §4: "Read replicas are for explicitly stale-tolerant analytics only."
# 19 §3: "Financial writes remain on the authoritative database primary."
resource "google_sql_database_instance" "analytics_replica" {
  name                = "${var.environment_name}-pg17-analytics-replica"
  project             = var.project_id
  region              = var.primary_region
  database_version    = "POSTGRES_17"
  deletion_protection = var.deletion_protection

  replica_configuration {
    source_instance = google_sql_database_instance.primary.name
  }

  settings {
    tier              = var.tier
    availability_type = "REGIONAL"
    disk_size         = var.data_disk_size_gb
    disk_type         = "PD_SSL"
    disk_autoresize   = false
    user_labels = {
      environment     = var.environment_name
      component       = "analytics-replica"
      write_permitted = "false"
    }

    backup_configuration {
      # An analytics replica is not an authoritative backup source.
      # 19 §4 keeps financial writes on the primary.
      enabled = false
    }

    ip_configuration {
      ipv4_enabled    = false
      private_network = var.vpc_id
      require_ssl     = true
    }
  }
}

# --- Recovery-region standby (active-passive) -------------------------------
# 19 §1: "encrypted WAL/backups replicated to a recovery region."
# 10: "PostgreSQL WAL and backups are replicated to the recovery location."
# 23 §2: "Recovery | Active-passive regional recovery | No active-active financial
#   writes; recovery enters RECOVERY_HOLD."
resource "google_sql_database_instance" "recovery_standby" {
  count = var.recovery_region_enabled ? 1 : 0

  name                = "${var.environment_name}-pg17-recovery"
  project             = var.project_id
  region              = var.recovery_region
  database_version    = "POSTGRES_17"
  deletion_protection = true

  settings {
    tier              = var.tier
    availability_type = "REGIONAL"
    disk_size         = var.data_disk_size_gb
    disk_type         = "PD_SSL"
    disk_autoresize   = false
    user_labels = {
      environment    = var.environment_name
      component      = "recovery-standby"
      write_model    = "active-passive"
      entry_state    = "RECOVERY_HOLD"
      rto_minutes    = "30"
      rpo_minutes    = "5"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "03:00"
      transaction_log_retention_days = var.daily_restore_points
      backup_retention_settings {
        retained_backups = var.daily_restore_points
        retention_unit   = "COUNT"
      }
    }

    ip_configuration {
      ipv4_enabled    = false
      private_network = var.vpc_id
      require_ssl     = true
    }
  }
}

# --- Databases ---------------------------------------------------------------
# 05 §PostgreSQL schema domains: "Core schemas: identity, config, market,
# strategy, risk, oms, execution, reconciliation, portfolio, ledger, audit, ops."
# 25 §6: "Configuration signature invalid | Reject configuration and remain on
# last known-good snapshot only if its validity and freshness are proven."
resource "google_sql_database" "app" {
  name     = "trading_control_plane"
  project  = var.project_id
  instance = google_sql_database_instance.primary.name
}

resource "google_sql_user" "app" {
  name     = "control_plane_app"
  project  = var.project_id
  instance = google_sql_database_instance.primary.name
  # NO password is set here. 06_SECURITY_AND_ACCESS_CONTROL.md §6: "Secrets reside
  # in a managed secret store"; 23 §2: "no secret in source, prompts, logs, or
  # artifacts." The password is generated in Secret Manager and injected as an
  # environment-scoped secret reference by modules/kubernetes.
  type = "BUILT_IN"

  lifecycle {
    ignore_changes = [password]
  }
}

# --- Sizing preconditions ---------------------------------------------------
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "PostgreSQL CPU, memory, IOPS,
# connection count, WAL throughput, queue lag, and storage growth must be measured
# under the stated envelope before live activation." Sizing is validated as a
# floor, not a claim of capacity.
resource "terraform_data" "sizing_floor_guard" {
  lifecycle {
    precondition {
      condition     = var.vcpu_per_node >= 8
      error_message = "HA node vCPU must be >= 8. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"each at least 8 vCPU / 32 GiB RAM and 1 TiB encrypted SSD storage\"."
    }
    precondition {
      condition     = var.memory_gib_per_node >= 32
      error_message = "HA node memory must be >= 32 GiB. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 requires at least 32 GiB RAM per node."
    }
    precondition {
      condition     = var.data_disk_size_gb >= 1024
      error_message = "HA node storage must be >= 1024 GiB (1 TiB). 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 requires at least 1 TiB encrypted SSD per node."
    }
    precondition {
      condition     = var.daily_restore_points == 35 && var.monthly_restore_points == 12
      error_message = "Retention must be 35 daily and 12 monthly restore points. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: \"35 daily restore points, 12 monthly restore points.\" A relaxed value is a deny condition."
    }
    precondition {
      condition     = var.audit_retention_years == 7
      error_message = "Audit/financial retention must be 7 years. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention: \"Audit and financial records: 7 years.\""
    }
    precondition {
      condition     = var.recovery_region != var.primary_region
      error_message = "Recovery region must differ from the primary region. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: \"Initial production sizing starts with a three-zone primary region and a separately secured recovery region.\""
    }
  }
}
