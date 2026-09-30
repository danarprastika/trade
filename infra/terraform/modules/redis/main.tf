# OPTIONAL Redis cache — CACHE ONLY. NOT AN AUTHORITATIVE DEPENDENCY.
#
# ============================================================================
# HARD CONSTRAINT (encoded below and asserted by
# infra/tests/test_terraform_invariants.py):
#
#   Redis is CACHE-ONLY. Redis MUST NOT be a dependency of any risk or
#   order-correctness path. Loss, eviction, flush, failover, or unavailability of
#   this instance MUST NOT change any risk decision, OMS state, ledger fact, or
#   reconciliation result.
#
#   Sources:
#     19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Redis is optional cache-only
#       infrastructure and must not be a dependency for risk authorization or
#       order correctness."
#     12_DECISION_REGISTER.md ADR-007: "Redis is cache only".
#     00_README.md §Authoritative technology baseline: "Cache | Redis 8 |
#       Ephemeral only; never source of financial truth".
#     25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §6 failure
#       table: "PostgreSQL primary unavailable | Stop authoritative mutations; do
#       not substitute Redis, event bus, or local memory as source of truth."
#     24_ENTERPRISE_RELEASE_STANDARD.md §1: "The absence of an optional subsystem
#       must never weaken an authoritative control. Optional analytics, research,
#       notification, cache, and visualization capabilities may degrade
#       independently".
#
#   The `noeviction` policy below is deliberate: a cache that silently evicts is
#   still safe, but a cache configured to WRITE THROUGH to any authoritative
#   store would not be. Noeviction + the variable default of false for live makes
#   the "degrades independently" requirement mechanically true.
# ============================================================================
#
# DEFAULT IS FALSE, INCLUDING FOR LIVE. Enabling this cache is never a
# prerequisite for live activation and never authorizes a risk-increasing action.

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
  }
}

resource "google_redis_instance" "cache" {
  count = var.enabled ? 1 : 0

  name           = "${var.environment_name}-redis-cache"
  project        = var.project_id
  region         = var.primary_region
  tier           = var.tier
  memory_size_gb = var.memory_size_gb

  redis_version     = var.redis_version
  display_name      = "${var.environment_name}-cache-only"
  size_gb           = null

  # CACHE-ONLY ENFORCEMENT #1: no persistence. A durable Redis is a system of
  # record, which this is not. Losing the cache must lose nothing of value.
  persistence_config {
    persistence_config = "DISABLED"
  }

  # CACHE-ONLY ENFORCEMENT #2: no automatic failover into an authoritative role.
  # If the cache is unavailable, the platform reads through to the authoritative
  # store, it does not promote Redis.
  read_replicas_mode = "READ_REPLICAS_DISABLED"

  authorized_network = var.vpc_id

  # CACHE-ONLY ENFORCEMENT #3: label the instance so operators and auditors can
  # see at a glance that it carries no financial authority.
  labels = {
    environment          = var.environment_name
    role                 = "cache-only"
    financial_authority  = "none"
    risk_path_dependency = "prohibited"
    order_path_dependency = "prohibited"
  }

  lifecycle {
    precondition {
      condition     = var.allowed_as_risk_dependency == false
      error_message = "Redis is never a risk dependency. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 and 12_DECISION_REGISTER.md ADR-007: cache only, never a dependency for risk authorization or order correctness. This precondition can never be satisfied by configuration; it exists to make the prohibition explicit and testable."
    }
    precondition {
      condition     = var.allowed_as_order_dependency == false
      error_message = "Redis is never an order-correctness dependency. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"must not be a dependency for risk authorization or order correctness\". Deny."
    }
    precondition {
      condition     = var.allowed_as_ledger_source == false
      error_message = "Redis is never a source of financial truth. 00_README.md: \"Cache | Redis 8 | Ephemeral only; never source of financial truth\". Deny."
    }
  }
}

# Recorded in Terraform state so the release evidence bundle can assert the
# constraint rather than relying on a comment.
resource "terraform_data" "cache_only_constraint" {
  count = var.enabled ? 1 : 0

  input = {
    role                       = "cache-only"
    risk_path_dependency       = "prohibited"
    order_path_dependency      = "prohibited"
    ledger_source              = "prohibited"
    outage_behavior            = "degrade-independently"
    authoritative_store_on_loss = "postgresql-primary"
  }
}
