# Redis module outputs.

output "cache_enabled" {
  description = "Whether the OPTIONAL cache was created. Default false, including for live. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  value       = var.enabled
}

output "cache_role" {
  description = "Role of this instance when present. Always cache-only. 12_DECISION_REGISTER.md ADR-007."
  value       = "cache-only"
}

output "authoritative_store_on_cache_loss" {
  description = "What the platform reads from when the cache is unavailable. Always the PostgreSQL primary, never Redis. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §6: \"PostgreSQL primary unavailable | ... do not substitute Redis, event bus, or local memory as source of truth.\""
  value       = "postgresql-primary"
}

output "risk_path_dependency" {
  description = "Prohibition record: Redis is not a dependency of any risk or order-correctness path. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  value       = "prohibited"
}
