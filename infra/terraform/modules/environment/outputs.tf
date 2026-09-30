# Per-environment composition module outputs.

output "environment_name" {
  description = "Environment this stack represents. 01_SYSTEM_ARCHITECTURE.md §4."
  value       = var.environment_name
}

output "project_id" {
  description = "Environment-scoped project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate accounts/projects per environment."
  value       = var.project_id
}

output "config_namespace" {
  description = "Environment-scoped configuration namespace. 17_CONFIGURATION_AND_RISK_POLICY.md §1."
  value       = var.config_namespace
}

output "zone_subnet_ids" {
  description = "Subnet ids for edge, application, data, management, and recovery. 06_SECURITY_AND_ACCESS_CONTROL.md §1 zone taxonomy."
  value       = module.network.zone_subnet_ids
}

output "declared_zones" {
  description = "The five zones declared by this environment. Asserted by infra/tests/test_terraform_invariants.py to be identical across all six environments."
  value       = module.network.zone_names
}

output "venue_egress_rules" {
  description = "Venue egress firewall rules. Empty for every non-live environment. 06_SECURITY_AND_ACCESS_CONTROL.md §1: egress is allowlisted by workload, destination, protocol, and environment; live additionally allowlists venue endpoints only."
  value       = module.network.venue_egress_rule_names
}

output "postgres_primary_instance" {
  description = "Authoritative PostgreSQL 17 HA primary. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: single writer for authoritative financial state."
  value       = module.postgres.primary_instance_name
}

output "postgres_sizing_floor" {
  description = "Applied database resource floor for release evidence. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  value       = module.postgres.sizing_floor
}

output "workload_sizing" {
  description = "Applied workload replica counts and resource floor. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  value = {
    api_replicas         = module.kubernetes.api_replica_count
    dispatcher_replicas  = module.kubernetes.dispatcher_replica_count
    adapter_venues       = module.kubernetes.adapter_venue_ids
    per_replica_vcpu     = 2 # 19 §3: 2 vCPU / 4 GiB per API, dispatcher, and adapter replica
    per_replica_memory   = "4Gi" # 19 §3
    research_pool_quota  = module.kubernetes.research_pool_quota
  }
}

output "workload_hardening" {
  description = "Pod/container hardening applied to every workload. 19 §1: non-root OCI containers; 06_SECURITY_AND_ACCESS_CONTROL.md §1/§4."
  value       = module.kubernetes.workload_hardening
}

output "image_digests" {
  description = "Digest-pinned image digests deployed here. 24_ENTERPRISE_RELEASE_STANDARD.md §15; 23 §2: immutable OCI images, no :latest."
  value       = module.kubernetes.image_digests
}

output "jetstream_enabled" {
  description = "Whether the gated NATS JetStream backbone exists. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 threshold; 12_DECISION_REGISTER.md ADR-020."
  value       = module.nats.jetstream_enabled
}

output "jetstream_node_count" {
  description = "JetStream node count. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: three nodes across three zones when enabled."
  value       = module.nats.node_count
}

output "cache_enabled" {
  description = "Whether the OPTIONAL cache-only Redis exists. Default false, including for live. 19 §3: cache only, never a risk or order-correctness dependency."
  value       = module.redis.cache_enabled
}

output "authoritative_store_on_cache_loss" {
  description = "What is read when the cache is unavailable. Always the PostgreSQL primary. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §6: \"do not substitute Redis, event bus, or local memory as source of truth.\""
  value       = module.redis.authoritative_store_on_cache_loss
}

output "retention_schedule" {
  description = "Backup and audit retention applied. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup and §Data retention."
  value       = module.backup.retention_schedule
}

output "recovery_objectives" {
  description = "Bound RTO/RPO with claim status. 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives; 12_DECISION_REGISTER.md ADR-025: objectives must not be claimed until drills demonstrate them."
  value       = module.backup.recovery_objectives
}

output "live_capability_state" {
  description = "G11 evaluation result. `DISABLED` unless every live precondition passes. 11_EXECUTION_GATES.md G11: \"Failure of any condition leaves live mode disabled.\" For non-live environments this is `DISABLED` by construction."
  value       = module.live_gate.live_capability_state
}

output "live_gate_evaluation" {
  description = "Per-precondition G11 evaluation for release evidence. 11_EXECUTION_GATES.md: binary PASS/FAIL, no partial completion."
  value       = module.live_gate.gate_evaluation
}
