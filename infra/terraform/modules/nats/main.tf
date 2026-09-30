# OPTIONAL NATS JetStream event backbone.
#
# THIS RESOURCE IS OFF BY DEFAULT AND IS GATED.
#
# TRACEABILITY
#   23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: "Eventing | PostgreSQL
#     transactional outbox first | Add NATS JetStream when sustained outbox lag
#     exceeds 5 seconds for 10 minutes, or independent consumer fan-out exceeds 5
#     critical consumer groups. Migration requires replay/idempotency proof."
#   23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6: "NATS introduction threshold |
#     Outbox lag >5 seconds for 10 minutes or >5 critical consumer groups"
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "If NATS JetStream is enabled,
#     deploy three nodes across three zones with replicated durable streams; it is
#     not required for the initial transactional-outbox implementation."
#   12_DECISION_REGISTER.md ADR-020: "PostgreSQL transactional outbox is the
#     initial event backbone; NATS JetStream is introduced only at the defined
#     fan-out/lag threshold | No exactly-once assumption; consumers remain
#     idempotent."
#   05_PERSISTENCE_EVENTING_RECONCILIATION.md §Outbox: "Financial correctness
#     never depends on exactly-once transport."
#
# The gate below is a real precondition evaluated at plan/apply time. Setting
# `enabled = true` without measured threshold evidence is a DENY, not a warning.

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
  }
}

locals {
  # Gate is satisfied only when enabled AND the measurement evidence crosses the
  # 23 §6 threshold. `null` evidence while enabled => deny.
  gate_evidence_present = var.gate_evidence != null
  gate_crossed_by_lag = (
    var.gate_evidence != null &&
    var.gate_evidence.observed_value > var.gate_evidence.threshold_value
  )
  gate_crossed_by_consumers = (
    var.gate_evidence != null &&
    var.gate_evidence.observed_value > var.critical_consumer_group_threshold &&
    var.gate_evidence.metric_name == "critical_consumer_groups"
  )
  gate_satisfied = var.enabled && var.gate_evidence_present && (gate_crossed_by_lag || gate_crossed_by_consumers)
}

# The gate is expressed as an explicit, auditable Terraform object rather than an
# always-on resource. `node_count` is only ever 0 or 3.
resource "terraform_data" "introduction_gate" {
  count = var.enabled ? 1 : 0

  input = {
    enabled                     = var.enabled
    outbox_lag_threshold_s      = var.outbox_lag_threshold_seconds
    outbox_lag_sustain_minutes  = var.outbox_lag_sustain_minutes
    consumer_group_threshold    = var.critical_consumer_group_threshold
    evidence_uri                = try(var.gate_evidence.evidence_uri, "")
  }

  lifecycle {
    precondition {
      condition = local.gate_evidence_present
      error_message = <<-EOT
        NATS JetStream requested but no gate evidence supplied. 23_ARCHITECTURE_AND_
        COMPLIANCE_DECISIONS.md §2: NATS is added "when sustained outbox lag exceeds 5
        seconds for 10 minutes, or independent consumer fan-out exceeds 5 critical consumer
        groups". 12_DECISION_REGISTER.md ADR-020 requires the threshold to be met. Deny.
      EOT
    }
    precondition {
      condition = local.gate_crossed_by_lag || local.gate_crossed_by_consumers
      error_message = <<-EOT
        NATS JetStream requested but the supplied evidence does not cross the
        23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 threshold
        (outbox lag > 5s for 10 minutes, or > 5 critical consumer groups).
        19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "it is not required for the initial
        transactional-outbox implementation". Deny.
      EOT
    }
    precondition {
      condition     = local.gate_crossed_by_lag ? var.gate_evidence.sustain_minutes >= var.outbox_lag_sustain_minutes : true
      error_message = "Outbox lag evidence must have been sustained for at least 10 minutes. 23 §6: \"outbox lag >5 seconds for 10 minutes\". Deny."
    }
    precondition {
      condition     = var.outbox_lag_threshold_seconds == 5
      error_message = "outbox_lag_threshold_seconds must be 5. 23 §6 binding default. A relaxed value requires a reviewed ADR (23 §6)."
    }
    precondition {
      condition     = var.outbox_lag_sustain_minutes == 10
      error_message = "outbox_lag_sustain_minutes must be 10. 23 §6 binding default. A relaxed value requires a reviewed ADR (23 §6)."
    }
    precondition {
      condition     = var.critical_consumer_group_threshold == 5
      error_message = "critical_consumer_group_threshold must be 5. 23 §6 binding default."
    }
  }
}

# --- Three nodes across three zones, only when gated on ----------------------
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "If NATS JetStream is enabled, deploy
# three nodes across three zones with replicated durable streams."
resource "google_compute_instance" "nats_node" {
  count = local.gate_satisfied ? length(var.zones) : 0

  name         = "${var.environment_name}-nats-${count.index + 1}"
  project      = var.project_id
  zone         = var.zones[count.index]
  machine_type = var.node_machine_type
  tags         = ["${var.environment_name}-nats"]

  boot_disk {
    initialize_params {
      image = var.node_image
      size  = var.node_disk_size_gb
      type  = "pd-ssd"
    }
  }

  network_interface {
    subnetwork = var.data_subnetwork_id
    # Private only. 06_SECURITY_AND_ACCESS_CONTROL.md §1: event transport remains
    # private.
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    nats_role        = "jetstream-node"
    environment      = var.environment_name
    zone_index       = tostring(count.index)
    financial_authority = "none"
  }

  # JetStream delivers at-least-once. 05 §Outbox: "Duplicate publication is
  # acceptable; consumers must be idempotent. Financial correctness never depends
  # on exactly-once transport."
  lifecycle {
    precondition {
      condition     = length(var.zones) == 3
      error_message = "Exactly three zones required. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"deploy three nodes across three zones with replicated durable streams\"."
    }
  }
}

# Replicated durable streams: JetStream R3 across the three nodes, giving
# replication factor 3 in line with 19 §3.
resource "terraform_data" "jetstream_topology" {
  count = local.gate_satisfied ? 1 : 0

  input = {
    nodes                = length(var.zones)
    zones                = join(",", var.zones)
    stream_replicas      = 3
    delivery             = "at-least-once"
    consumer_idempotency = "required"
  }
}
