# NATS JetStream module variables.

variable "environment_name" {
  description = "Environment name from the closed set dev/test/staging/paper/shadow/live. 01_SYSTEM_ARCHITECTURE.md §4."
  type        = string
}

variable "project_id" {
  description = "Environment-scoped GCP project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2."
  type        = string
}

variable "enabled" {
  description = "Gate for the OPTIONAL NATS JetStream backbone. Default false. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: NATS \"is not required for the initial transactional-outbox implementation\". Enabling still requires gate evidence."
  type        = bool
  default     = false
}

variable "gate_evidence" {
  description = "Measured evidence that the 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 introduction threshold was crossed. Required when enabled; the module precondition denies otherwise. 23 §2: \"Add NATS JetStream when sustained outbox lag exceeds 5 seconds for 10 minutes, or independent consumer fan-out exceeds 5 critical consumer groups. Migration requires replay/idempotency proof.\""
  type = object({
    metric_name      = string
    observed_value   = number
    threshold_value  = number
    sustain_minutes  = number
    window_start_utc = string
    window_end_utc   = string
    evidence_uri     = string
    reviewed_by      = string
  })
  default = null
}

variable "outbox_lag_threshold_seconds" {
  description = "Outbox lag trigger in seconds. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6: \"NATS introduction threshold | Outbox lag >5 seconds for 10 minutes or >5 critical consumer groups\". A relaxed value requires a reviewed ADR (23 §6)."
  type        = number
  default     = 5

  validation {
    condition     = var.outbox_lag_threshold_seconds == 5
    error_message = "outbox_lag_threshold_seconds must be exactly 5 per 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6. Relaxation requires a reviewed ADR, risk assessment, and updated tests."
  }
}

variable "outbox_lag_sustain_minutes" {
  description = "Sustained minutes the lag must exceed the threshold. 23 §6: \"outbox lag >5 seconds for 10 minutes\". A relaxed value requires a reviewed ADR."
  type        = number
  default     = 10

  validation {
    condition     = var.outbox_lag_sustain_minutes == 10
    error_message = "outbox_lag_sustain_minutes must be exactly 10 per 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6."
  }
}

variable "critical_consumer_group_threshold" {
  description = "Critical consumer group count that independently triggers introduction. 23 §6: \"or >5 critical consumer groups\"."
  type        = number
  default     = 5

  validation {
    condition     = var.critical_consumer_group_threshold == 5
    error_message = "critical_consumer_group_threshold must be exactly 5 per 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6."
  }
}

variable "zones" {
  description = "Exactly three zones for the JetStream nodes. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"deploy three nodes across three zones with replicated durable streams\"."
  type        = list(string)

  validation {
    condition     = length(var.zones) == 3
    error_message = "Exactly three zones required. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  }
}

variable "data_subnetwork_id" {
  description = "Data zone subnetwork. 06_SECURITY_AND_ACCESS_CONTROL.md §1: event transport remains private."
  type        = string
}

variable "node_machine_type" {
  description = "JetStream node machine type. 19 §3 specifies node count, zone spread, and replicated durable streams, but not a per-node SKU; this is an engineering default that must be benchmarked under the capacity envelope before live activation."
  type        = string
  default     = "n2-standard-4"
}

variable "node_disk_size_gb" {
  description = "JetStream node disk size in GiB. Not specified per-node in 19 §3; must be sized from measured WAL rate and queue recovery behaviour under the capacity envelope (19 §6)."
  type        = number
  default     = 200
}

variable "node_image" {
  description = "Pinned OS image for JetStream nodes. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §7 prohibits end-of-life runtimes; the image must be a supported release and pinned per environment."
  type        = string
  default     = "projects/debian-cloud/global/images/family/debian-12"
}
