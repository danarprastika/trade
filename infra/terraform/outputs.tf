# Root module outputs. Outputs are non-sensitive identifiers only.
#
# 24_ENTERPRISE_RELEASE_STANDARD.md §16 prohibits unsigned production artifacts
# and unreviewed production configuration changes; 09_TESTING_AND_RELEASE_EVIDENCE.md
# §Evidence package requires a configuration fingerprint. Nothing here exposes a
# secret value: only resource names, ids, and digests.

output "environment_project_ids" {
  description = "Per-environment GCP project ids. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate accounts/projects per environment."
  value       = local.environment_project_ids
}

output "required_zones" {
  description = "The five mandatory network zones. 06_SECURITY_AND_ACCESS_CONTROL.md §1: edge, application, data, management, recovery."
  value       = local.required_zones
}

output "lower_environments" {
  description = "Environments whose secrets must never be readable from live. 01_SYSTEM_ARCHITECTURE.md §4: \"Live credentials are never available to lower environments.\""
  value       = local.lower_environments
}

output "root_backend_strategy" {
  description = "Declares that each environment owns a separate remote state backend. 19 §2 requires per-environment isolation; a single shared root state would violate it."
  value       = "per-environment-separate-backend"
}
