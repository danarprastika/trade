# NATS module outputs.

output "jetstream_enabled" {
  description = "Whether the gated JetStream backbone was created. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: NATS \"is not required for the initial transactional-outbox implementation\"; 12_DECISION_REGISTER.md ADR-020 gates it on the measured threshold."
  value       = local.gate_satisfied
}

output "node_names" {
  description = "JetStream node names. Empty when the gate is not satisfied."
  value       = [for n in google_compute_instance.nats_node : n.name]
}

output "node_count" {
  description = "Node count. Must be exactly 3 when enabled, per 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  value       = length(google_compute_instance.nats_node)
}

output "stream_replication_factor" {
  description = "Replicated durable stream factor. 19 §3: \"replicated durable streams\"."
  value       = local.gate_satisfied ? 3 : 0
}

output "delivery_semantics" {
  description = "At-least-once delivery. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Outbox: \"Financial correctness never depends on exactly-once transport\"; consumers must be idempotent."
  value       = "at-least-once"
}
