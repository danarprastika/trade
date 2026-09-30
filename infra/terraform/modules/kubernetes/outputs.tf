# Kubernetes module outputs.

output "api_deployment_name" {
  description = "Go control-plane API deployment name. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: minimum three replicas."
  value       = kubernetes_deployment.api.metadata[0].name
}

output "api_replica_count" {
  description = "Go API replica count applied. Floor of three per 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  value       = kubernetes_deployment.api.spec[0].replicas
}

output "dispatcher_replica_count" {
  description = "Event-dispatcher replica count applied. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: two replicas."
  value       = kubernetes_deployment.event_dispatcher.spec[0].replicas
}

output "adapter_deployment_names" {
  description = "One isolated adapter deployment per ENABLED venue. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"at least one isolated adapter worker per enabled venue\"."
  value       = { for k, v in kubernetes_deployment.adapter : v.metadata[0].name }
}

output "adapter_venue_ids" {
  description = "Venue identifiers with an enabled adapter. 19 §3 sizing is per enabled venue; 16_MARKET_DATA_AND_VENUE_ADAPTERS.md §4 requires least-privilege venue-specific credentials."
  value       = [for k, v in kubernetes_deployment.adapter : v.metadata[0].labels["venue"]]
}

output "research_pool_quota" {
  description = "Applied quota for the isolated research pool. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: research workers \"cannot consume reserved API, risk, OMS, or adapter capacity\"."
  value = {
    cpu    = var.research_quota_cpu
    memory = var.research_quota_memory
  }
}

output "research_live_identity" {
  description = "Prohibition record: the research pool mounts no service account token and therefore holds no live environment identity. 06_SECURITY_AND_ACCESS_CONTROL.md §4: \"Python research workers have no live environment identity.\""
  value       = "prohibited"
}

output "workload_hardening" {
  description = "Security controls applied to every pod template, for release evidence. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1 (non-root OCI containers) and 06_SECURITY_AND_ACCESS_CONTROL.md §1/§4."
  value = {
    run_as_non_root             = true
    run_as_user                = 65532
    read_only_root_filesystem   = true
    allow_privilege_escalation  = false
    privileged                 = false
    capabilities_dropped       = "ALL"
    seccomp_profile            = "RuntimeDefault"
    automount_token_research   = false
  }
}

output "image_digests" {
  description = "Digest-pinned image digests deployed in this environment. 24_ENTERPRISE_RELEASE_STANDARD.md §15 and 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: immutable artifacts, no :latest, no rebuild between environments."
  value = {
    api         = var.api_artifact_digest
    dispatcher  = var.dispatcher_artifact_digest
    research    = element(split("@", var.research_image), 1)
    adapters    = { for k, v in var.adapter_images : k => element(split("@", v.image), 1) }
  }
}
