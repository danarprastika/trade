# test environment root module outputs.

output "environment_name" {
  description = "Environment identity. 01_SYSTEM_ARCHITECTURE.md §4."
  value       = "test"
}

output "project_id" {
  description = "test project id. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate accounts/projects per environment."
  value       = module.environment.project_id
}

output "state_backend" {
  description = "This environment's state backend is separate from every other environment. 19 §2: environments have separate accounts/projects, and a shared backend would permit cross-environment state access."
  value       = "gcs://${var.project_id}-tfstate/environments/test"
}

output "declared_zones" {
  description = "The five zones declared by test. 06_SECURITY_AND_ACCESS_CONTROL.md §1: edge, application, data, management, recovery."
  value       = module.environment.declared_zones
}

output "zone_subnet_ids" {
  description = "test zone subnet ids. 06_SECURITY_AND_ACCESS_CONTROL.md §1."
  value       = module.environment.zone_subnet_ids
}

output "config_namespace" {
  description = "test configuration namespace. 17_CONFIGURATION_AND_RISK_POLICY.md §1: environment-scoped and immutable after activation."
  value       = module.environment.config_namespace
}

output "venue_egress_rules" {
  description = "Always empty in test. 11_EXECUTION_GATES.md G11: live credentials are provisioned only in isolated live infrastructure; venue egress exists only in live."
  value       = module.environment.venue_egress_rules
}

output "live_capability_state" {
  description = "Always DISABLED outside live. 11_EXECUTION_GATES.md G11: \"Failure of any condition leaves live mode disabled.\" 09_TESTING_AND_RELEASE_EVIDENCE.md §Release strategy promotes test -> test -> staging -> paper -> shadow -> live."
  value       = module.environment.live_capability_state
}

output "cache_enabled" {
  description = "Cache-only Redis state for test. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: never a risk or order-correctness dependency."
  value       = module.environment.cache_enabled
}

output "jetstream_enabled" {
  description = "Gated NATS JetStream state for test. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 threshold; 12_DECISION_REGISTER.md ADR-020."
  value       = module.environment.jetstream_enabled
}
