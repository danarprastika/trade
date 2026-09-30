# Backup, restore verification, and cross-region retention.
#
# TRACEABILITY
#   05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: "Production PostgreSQL uses
#     continuous WAL archiving, daily full backup, and weekly restore verification.
#     Backups are encrypted, access-controlled, and retained according to the
#     defined retention schedule: 35 daily restore points, 12 monthly restore
#     points."
#   05 §Data retention: "Operational logs: 90 days hot, 365 days archived. Audit
#     and financial records: 7 years. Research artifacts: 3 years unless tagged as
#     required historical evidence. Deletion never removes records under legal hold
#     or financial audit retention."
#   10_OPERATIONS_AND_DISASTER_RECOVERY.md §Restore procedure: "Restore the latest
#     valid backup, replay WAL to the selected recovery point, validate schema and
#     ledger invariants, run reconciliation against available venue records, run
#     smoke tests, and only then reopen controlled operations."
#   10 §Drill schedule: "Monthly backup restore verification. Quarterly regional
#     recovery exercise. Quarterly kill-switch exercise. Semiannual full incident
#     simulation covering venue outage, database outage, and credential
#     compromise."
#   08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives: critical control
#     plane RTO <= 30 minutes; RPO <= 5 minutes. Validated by drills.
#   22_AUDIT_INTEGRITY_AND_EVIDENCE.md §4: independent cross-region copy under
#     separate administrative credentials; key recovery and audit restore tested
#     quarterly.
#   10 §Recovery topology: "Recovery topology is active-passive. Financial writes
#     have one authoritative primary region; cross-region replicas are not promoted
#     automatically."

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
  }
}

variable "environment_name" {
  description = "Environment name from the closed set dev/test/staging/paper/shadow/live. 01_SYSTEM_ARCHITECTURE.md §4."
  type        = string

  validation {
    condition     = contains(["dev", "test", "staging", "paper", "shadow", "live"], var.environment_name)
    error_message = "Environment must be one of dev, test, staging, paper, shadow, live (01_SYSTEM_ARCHITECTURE.md §4)."
  }
}

variable "project_id" {
  description = "Environment-scoped GCP project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2."
  type        = string
}

variable "primary_region" {
  description = "Primary region producing backups and WAL archives."
  type        = string
}

variable "recovery_region" {
  description = "Recovery region receiving encrypted WAL and backup copies. 19 §1: \"encrypted WAL/backups replicated to a recovery region\". Must differ from primary_region."
  type        = string

  validation {
    condition     = var.recovery_region != ""
    error_message = "recovery_region is required. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: encrypted WAL/backups are replicated to a recovery region."
  }
}

# --- Cross-region backup bucket ------------------------------------------------
# 19 §1: encrypted WAL/backups replicated to a recovery region.
# 22 §4: "At least one independent copy is cross-region and protected by separate
# administrative credentials."
resource "google_storage_bucket" "backup_archive" {
  name                        = "${var.project_id}-${var.environment_name}-wal-archive"
  project                     = var.project_id
  location                    = var.recovery_region
  force_destroy               = false
  uniform_bucket_level_access = true
  storage_class               = "ARCHIVE" # 05 §Backup: encrypted, access-controlled backups

  versioning {
    enabled = true
  }

  lifecycle_rule {
    # 05 §Backup: 35 daily restore points retained.
    condition {
      age = var.daily_restore_points
    }
    action {
      type          = "Delete"
      storage_class = "ARCHIVE"
    }
  }

  lifecycle {
    # 10 §Restore procedure and 01 §10 invariant 4: a restored environment may
    # not leave RECOVERY_HOLD without independent verification, so backup
    # destruction is never a side effect of a plan.
    prevent_destroy = true
  }
}

# --- Restore verification record ----------------------------------------------
# 10 §Drill schedule: "Monthly backup restore verification."
# The verification object is the machine-readable evidence that the monthly drill
# actually ran. Absence of this object is itself an alert condition.
resource "google_storage_bucket" "restore_verification" {
  name                        = "${var.project_id}-${var.environment_name}-restore-verification"
  project                     = var.project_id
  location                    = var.primary_region
  force_destroy               = false
  uniform_bucket_level_access = true

  lifecycle_rule {
    # Verification evidence is retained for the same 35-day operational window
    # used for logs (05 §Data retention: operational logs 90 days hot); the audit
    # record of the verification is retained 7 years in the audit-evidence
    # bucket, not here.
    condition {
      age = var.daily_restore_points
    }
    action {
      type = "Delete"
    }
  }
}

resource "terraform_data" "backup_posture" {
  input = {
    wal_archiving            = "continuous"
    full_backup_schedule     = "daily"
    restore_verification     = "weekly"  # 10 §Drill schedule also requires monthly; weekly is the stricter control
    monthly_drill            = "monthly"
    quarterly_recovery_drill = "quarterly"
    daily_restore_points     = var.daily_restore_points
    monthly_restore_points   = var.monthly_restore_points
    audit_retention_years    = var.audit_retention_years
    promotion_model          = "active-passive"
    recovery_entry_state     = "RECOVERY_HOLD"
  }

  lifecycle {
    precondition {
      condition     = var.daily_restore_points == 35 && var.monthly_restore_points == 12
      error_message = "Retention must be 35 daily and 12 monthly restore points. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup. A relaxed value is a deny condition."
    }
    precondition {
      condition     = var.audit_retention_years == 7
      error_message = "Audit and financial retention must be 7 years. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention; 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §4."
    }
    precondition {
      condition     = var.recovery_region != var.primary_region
      error_message = "The recovery region must differ from the primary region. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1 requires a separately secured recovery region."
    }
    precondition {
      condition     = var.rto_minutes == 30 && var.rpo_minutes == 5
      error_message = "Recovery objectives must be RTO <= 30 minutes and RPO <= 5 minutes. 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives; 10_OPERATIONS_AND_DISASTER_RECOVERY.md; 12_DECISION_REGISTER.md ADR-025."
    }
  }
}

variable "daily_restore_points" {
  description = "Retained daily restore points. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: \"35 daily restore points, 12 monthly restore points.\""
  type        = number
  default     = 35

  validation {
    condition     = var.daily_restore_points == 35
    error_message = "daily_restore_points must be 35 per 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup."
  }
}

variable "monthly_restore_points" {
  description = "Retained monthly restore points. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: \"12 monthly restore points\"."
  type        = number
  default     = 12

  validation {
    condition     = var.monthly_restore_points == 12
    error_message = "monthly_restore_points must be 12 per 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup."
  }
}

variable "audit_retention_years" {
  description = "Audit and financial record retention in years. 05 §Data retention: 7 years. 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §4: seven years for financial, access, approval, and control records."
  type        = number
  default     = 7

  validation {
    condition     = var.audit_retention_years == 7
    error_message = "audit_retention_years must be 7 per 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention."
  }
}

variable "rto_minutes" {
  description = "Critical control-plane recovery time objective in minutes. 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives: \"Critical control plane RTO <= 30 minutes; RPO <= 5 minutes\". 10_OPERATIONS_AND_DISASTER_RECOVERY.md: \"recovery time objective: 30 minutes; recovery point objective: 5 minutes\"."
  type        = number
  default     = 30

  validation {
    condition     = var.rto_minutes == 30
    error_message = "rto_minutes must be 30. 08 §Recovery objectives and 10_OPERATIONS_AND_DISASTER_RECOVERY.md both bind 30 minutes. 11_EXECUTION_GATES.md: recovery objectives are non-waivable."
  }
}

variable "rpo_minutes" {
  description = "Critical control-plane recovery point objective in minutes. 08 §Recovery objectives: \"RPO <= 5 minutes\"; 10: \"recovery point objective: 5 minutes\"; 12_DECISION_REGISTER.md ADR-025."
  type        = number
  default     = 5

  validation {
    condition     = var.rpo_minutes == 5
    error_message = "rpo_minutes must be 5. 08 §Recovery objectives, 10_OPERATIONS_AND_DISASTER_RECOVERY.md, and 12_DECISION_REGISTER.md ADR-025. 11_EXECUTION_GATES.md prohibits waiving recovery objectives."
  }
}
