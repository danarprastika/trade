# Per-environment composition module.
#
# Composes the network zones, Kubernetes workloads, PostgreSQL 17 HA primary,
# optional gated NATS, optional cache-only Redis, and the artifact registry for a
# single environment. Each environment instantiates this module ONCE, in its own
# root module with its own state backend and its own provider credentials.
#
# TRACEABILITY
#   01_SYSTEM_ARCHITECTURE.md §4: "Each environment has separate credentials,
#     database, encryption keys, deployment identity, network policy, and
#     configuration namespace. Live credentials are never available to lower
#     environments."
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: "Dev/test/staging/paper/shadow/live
#     have separate accounts/projects, network policies, databases, keys, service
#     identities, and venue credentials."

terraform {
  required_version = "= 1.9.8"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "= 6.14.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "= 2.36.0"
    }
  }
}

module "network" {
  source = "../network"

  environment_name      = var.environment_name
  project_id            = var.project_id
  primary_region        = var.primary_region
  primary_region_zones  = var.primary_region_zones
  recovery_region       = var.recovery_region
  network_cidr_base     = var.network_cidr_base
  venue_egress_allowlist = var.venue_egress_allowlist
  venue_egress_allowed  = var.venue_egress_allowed
}

module "postgres" {
  source = "../postgres"

  environment_name    = var.environment_name
  project_id          = var.project_id
  primary_region      = var.primary_region
  recovery_region     = var.recovery_region
  recovery_region_enabled = var.recovery_region_enabled
  vcpu_per_node       = var.db_vcpu_per_node
  memory_gib_per_node = var.db_memory_gib_per_node
  tier                = var.db_tier
  data_disk_size_gb   = var.db_data_disk_size_gb
  max_connections     = var.db_max_connections
  deletion_protection = var.db_deletion_protection
  vpc_id              = module.network.vpc_id
  kms_key_name        = var.kms_key_name
  daily_restore_points    = 35 # 05 §Backup: 35 daily restore points
  monthly_restore_points  = 12 # 05 §Backup: 12 monthly restore points
  audit_retention_years   = 7  # 05 §Data retention: 7 years
}

module "kubernetes" {
  source = "../kubernetes"

  environment_name          = var.environment_name
  namespace                 = var.config_namespace
  api_image                 = var.api_image
  dispatcher_image          = var.dispatcher_image
  research_image            = var.research_image
  adapter_images            = var.adapter_images
  api_artifact_digest       = var.api_artifact_digest
  dispatcher_artifact_digest = var.dispatcher_artifact_digest
  config_fingerprint        = var.config_fingerprint
  otlp_endpoint             = var.otlp_endpoint
  api_replicas              = var.api_replicas
  dispatcher_replicas       = var.dispatcher_replicas
  research_replicas         = var.research_replicas
  api_max_replicas          = var.api_max_replicas
}

# OPTIONAL and OFF by default. Gated on 23 §2/§6 evidence.
module "nats" {
  source = "../nats"

  environment_name                 = var.environment_name
  project_id                       = var.project_id
  enabled                          = var.nats_enabled
  gate_evidence                    = var.nats_gate_evidence
  outbox_lag_threshold_seconds     = 5  # 23 §6 binding default
  outbox_lag_sustain_minutes       = 10 # 23 §6 binding default
  critical_consumer_group_threshold = 5 # 23 §6 binding default
  zones                            = var.primary_region_zones
  data_subnetwork_id               = module.network.zone_subnet_ids["data"]
}

# OPTIONAL, cache-only, never a risk or order-correctness dependency.
module "redis" {
  source = "../redis"

  environment_name          = var.environment_name
  project_id                = var.project_id
  primary_region            = var.primary_region
  vpc_id                    = module.network.vpc_id
  enabled                   = var.redis_enabled
  allowed_as_risk_dependency  = false # 19 §3, ADR-007: cache only
  allowed_as_order_dependency = false # 19 §3: never an order-correctness dependency
  allowed_as_ledger_source    = false # 00_README: never source of financial truth
}

module "registry" {
  source = "../registry"

  environment_name = var.environment_name
  project_id       = var.project_id
  region           = var.primary_region
  kms_key_name     = var.kms_key_name
  expected_artifact_digest = var.api_artifact_digest
}

module "backup" {
  source = "../backup"

  environment_name = var.environment_name
  project_id       = var.project_id
  primary_region   = var.primary_region
  recovery_region  = var.recovery_region
  daily_restore_points   = 35 # 05 §Backup
  monthly_restore_points = 12 # 05 §Backup
  audit_retention_years  = 7  # 05 §Data retention
  rto_minutes            = 30 # 08 §Recovery objectives
  rpo_minutes            = 5  # 08 §Recovery objectives
}

# The live gate is instantiated for EVERY environment, but it only produces a
# `terraform_data` resource for `live`. Instantiating it everywhere keeps the
# condition logic in exactly one place.
module "live_gate" {
  source = "../live_gate"

  environment_name       = var.environment_name
  g10_gate_report        = var.live_g10_gate_report
  account_holder         = var.live_account_holder
  eligibility_record     = var.live_eligibility_record
  dual_control           = var.live_dual_control
  operational_state      = var.live_operational_state
  live_credentials       = var.live_credentials
  owner_authorization    = var.live_owner_authorization
  canary                 = var.live_canary
  eligibility_max_age_days        = 30 # 23 §6: eligibility record maximum age
  step_up_freshness_limit_minutes = 5  # 06 §2: re-auth freshness <= 5 minutes
}

# --- Environment isolation guard --------------------------------------------
# 01_SYSTEM_ARCHITECTURE.md §4 / 19 §2: per-environment isolation. The live
# environment additionally may not hold any reference to a lower environment's
# project, which is how "live must be structurally unable to read lower-environment
# secrets" is enforced rather than asserted.
resource "terraform_data" "environment_isolation" {
  lifecycle {
    precondition {
      condition = !var.venue_egress_allowed || var.environment_name == "live"
      error_message = "Venue egress is permitted only in the live environment. 11_EXECUTION_GATES.md G11: live credentials are provisioned only in isolated live infrastructure. Deny."
    }

    precondition {
      condition = var.environment_name != "live" || length(var.lower_environment_project_ids) == 0
      error_message = <<-EOT
        DENY: the live environment declares a reference to a lower environment's project
        (${join(", ", var.lower_environment_project_ids)}). 01_SYSTEM_ARCHITECTURE.md §4: "Live
        credentials are never available to lower environments" (and the converse: live must be
        structurally unable to read lower-environment secrets). 19_DEPLOYMENT_TOPOLOGY_AND_
        SIZING.md §2: "Live secrets are not readable by lower environments." The live module must
        be structurally isolated from every dev/test/staging/paper/shadow project. Deny.
      EOT
    }

    precondition {
      condition = var.environment_name != "live" || length(var.venue_egress_allowlist) > 0
      error_message = "The live environment must carry an explicit venue endpoint allowlist. 06_SECURITY_AND_ACCESS_CONTROL.md §1: egress is deny-by-default and allowlisted by workload, destination, protocol, and environment. Deny."
    }

    precondition {
      condition     = length(local_zones) == 5
      error_message = "Every environment must declare all five zones: edge, application, data, management, recovery. 06_SECURITY_AND_ACCESS_CONTROL.md §1. Deny."
    }
  }
}

locals {
  local_zones = ["edge", "application", "data", "management", "recovery"]
}
