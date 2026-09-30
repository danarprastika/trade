# live environment root module outputs.

output "environment_name" {
  description = "Environment identity. 01_SYSTEM_ARCHITECTURE.md §4."
  value       = "live"
}

output "project_id" {
  description = "live project id, distinct from every lower environment. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate accounts/projects per environment."
  value       = module.environment.project_id
}

output "state_backend" {
  description = "This environment's state backend is separate from every other environment. 19 §2. A shared backend would permit a lower-environment apply to read or destroy live state."
  value       = "gcs://${var.project_id}-tfstate/environments/live"
}

output "declared_zones" {
  description = "The five zones declared by live. 06_SECURITY_AND_ACCESS_CONTROL.md §1: edge, application, data, management, recovery."
  value       = module.environment.declared_zones
}

output "zone_subnet_ids" {
  description = "live zone subnet ids. 06_SECURITY_AND_ACCESS_CONTROL.md §1."
  value       = module.environment.zone_subnet_ids
}

output "config_namespace" {
  description = "live configuration namespace. 17_CONFIGURATION_AND_RISK_POLICY.md §1: live configuration cannot be copied automatically from lower environments."
  value       = module.environment.config_namespace
}

output "venue_egress_rules" {
  description = "Venue egress firewall rules. These exist ONLY in live: modules/network creates them with count = venue_egress_allowed ? length(allowlist) : 0, and 06_SECURITY_AND_ACCESS_CONTROL.md §1 requires egress to be allowlisted by workload, destination, protocol, and environment. Each destination is an explicit FQDN (no wildcard) and withdrawal/transfer is disabled (06 §6)."
  value       = module.environment.venue_egress_rules
}

output "live_capability_state" {
  description = "G11 evaluation result: ACTIVATABLE only when every precondition passes, otherwise DISABLED. 11_EXECUTION_GATES.md G11: \"Failure of any condition leaves live mode disabled. This gate is not satisfied by documentation alone.\" 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §3 invariant 7."
  value       = module.environment.live_capability_state
}

output "live_gate_evaluation" {
  description = "Per-precondition G11 evaluation, recorded as release evidence. 11_EXECUTION_GATES.md: binary PASS/FAIL only; \"partial completion is FAIL, not a percentage.\" 25 §8 item 9 requires evidence references, reviewer identities, UTC timestamps, commit and artifact digests."
  value       = module.environment.live_gate_evaluation
}

output "cache_enabled" {
  description = "Always false in live. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: Redis is \"optional cache-only infrastructure and must not be a dependency for risk authorization or order correctness\". 12_DECISION_REGISTER.md ADR-007."
  value       = module.environment.cache_enabled
}

output "authoritative_store_on_cache_loss" {
  description = "What is read when a cache is unavailable: the PostgreSQL primary, never Redis. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §6: \"do not substitute Redis, event bus, or local memory as source of truth.\""
  value       = module.environment.authoritative_store_on_cache_loss
}

output "jetstream_enabled" {
  description = "Gated NATS JetStream state. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 threshold; 19 §3 requires three nodes across three zones when enabled; 12_DECISION_REGISTER.md ADR-020 requires replay/idempotency proof."
  value       = module.environment.jetstream_enabled
}

output "workload_sizing" {
  description = "Applied live workload replica counts and per-replica floor. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: 3 API replicas, 2 dispatcher replicas, >=1 adapter per enabled venue, 2 vCPU / 4 GiB each."
  value       = module.environment.workload_sizing
}

output "postgres_sizing_floor" {
  description = "Applied live database floor. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: HA primary/standby pair, each at least 8 vCPU / 32 GiB RAM / 1 TiB encrypted SSD."
  value       = module.environment.postgres_sizing_floor
}

output "retention_schedule" {
  description = "Applied live backup and audit retention. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup (35 daily + 12 monthly restore points) and §Data retention (audit/financial 7 years)."
  value       = module.environment.retention_schedule
}

output "recovery_objectives" {
  description = "Bound RTO/RPO with claim status. 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives; 12_DECISION_REGISTER.md ADR-025: the platform must not claim these objectives until restore/failover drills demonstrate them."
  value       = module.environment.recovery_objectives
}
