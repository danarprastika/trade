# Root module variables. Every variable declares a description and a type.

variable "project_id_prefix" {
  description = "Prefix for per-environment GCP project ids. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2 requires separate accounts/projects per environment; the suffix is the environment name."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id_prefix))
    error_message = "project_id_prefix must be a valid GCP project id prefix (lowercase alphanumeric and hyphens, 6-30 chars)."
  }
}

variable "admin_project_id" {
  description = "Administration project that holds shared bootstrap state only. It holds no financial data and no environment secrets. 24_ENTERPRISE_RELEASE_STANDARD.md §16 prohibits production credentials in non-production environments."
  type        = string
}

variable "primary_region" {
  description = "Primary region for edge, application, data, and management zones. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: initial production sizing starts with a three-zone primary region and a separately secured recovery region."
  type        = string
}

variable "primary_region_zones" {
  description = "Exactly three zones in the primary region. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"Initial production sizing starts with a three-zone primary region\"; NATS, when enabled, requires three nodes across three zones."
  type        = list(string)

  validation {
    condition     = length(var.primary_region_zones) == 3
    error_message = "19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 requires a three-zone primary region; exactly 3 zones must be supplied."
  }
}

variable "recovery_region" {
  description = "Separately secured recovery region for encrypted WAL and backup replication. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: PostgreSQL WAL/backups are replicated to a recovery region. Must differ from primary_region."
  type        = string
}

variable "owner_identity" {
  description = "Named technical owner accountable for the environment. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §7: \"An unowned service is not eligible for production promotion.\""
  type        = string
}

variable "oncall_rotation" {
  description = "Primary and secondary on-call coverage identifiers. 25 §7 requires primary and secondary on-call coverage for every production service."
  type = object({
    primary   = string
    secondary = string
  })
}

# --- Optional subsystem gate variables -------------------------------------
# 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: "Add NATS JetStream when
# sustained outbox lag exceeds 5 seconds for 10 minutes, or independent consumer
# fan-out exceeds 5 critical consumer groups."
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "If NATS JetStream is enabled,
# deploy three nodes across three zones with replicated durable streams; it is not
# required for the initial transactional-outbox implementation."
#
# NATS is OFF by default and is a real documented gate, not an always-on resource.

variable "nats_enabled" {
  description = "Gate for the OPTIONAL NATS JetStream event backbone. OFF by default. Enabling requires 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 threshold evidence: outbox lag >5s sustained 10 minutes, OR >5 critical consumer groups. A boolean alone is not sufficient authorization; `nats_gate_evidence` must be supplied."
  type        = bool
  default     = false
}

variable "nats_outbox_lag_threshold_seconds" {
  description = "Outbox oldest-record age, in seconds, that constitutes the lag trigger for the NATS introduction gate. 23 §6: \"NATS introduction threshold | Outbox lag >5 seconds for 10 minutes or >5 critical consumer groups\"."
  type        = number
  default     = 5
}

variable "nats_outbox_lag_sustain_minutes" {
  description = "Sustained minutes the outbox lag must exceed the threshold before NATS is introduced. 23 §6: \"outbox lag >5 seconds for 10 minutes\"."
  type        = number
  default     = 10
}

variable "nats_critical_consumer_group_threshold" {
  description = "Number of independent critical consumer groups that triggers NATS introduction. 23 §6: \"or >5 critical consumer groups\"."
  type        = number
  default     = 5
}

variable "nats_gate_evidence" {
  description = "Evidence object proving the 23 §6 threshold was measured and crossed. Required when nats_enabled is true; the module precondition denies otherwise. This is a real gate, not a documentation stub."
  type = object({
    metric_name          = string
    observed_value       = number
    threshold_value      = number
    sustain_minutes      = number
    window_start_utc     = string
    window_end_utc       = string
    evidence_uri         = string
    reviewed_by          = string
  })
  default = null
}

# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Redis is optional cache-only
# infrastructure and must not be a dependency for risk authorization or order
# correctness." 12_DECISION_REGISTER.md ADR-007: "Redis is cache only".
variable "redis_enabled" {
  description = "Gate for OPTIONAL Redis cache. OFF by default and false for live. Redis is cache-only: it MUST NOT be a dependency of any risk or order-correctness path. Enabling this does not relax that constraint; infra/tests/test_terraform_invariants.py asserts the constraint comments are present in modules/redis."
  type        = bool
  default     = false
}

variable "region_drift_detection_enabled" {
  description = "Enable scheduled terraform plan-based drift detection (infra/ci/terraform-plan-gate.yaml). 23 §2: \"Drift is detected; production changes require reviewed plan and release evidence.\" 24_ENTERPRISE_RELEASE_STANDARD.md §5: \"Drift detection compares desired and observed state continuously; unexplained drift creates an operational incident and blocks further privileged changes until resolved.\""
  type        = bool
  default     = true
}
