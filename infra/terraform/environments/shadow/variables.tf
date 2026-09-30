# shadow environment root module variables.

variable "project_id" {
  description = "The shadow project's id. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2 requires a separate account/project per environment; shadow must never be the live project."
  type        = string
}

variable "config_namespace" {
  description = "Environment-scoped configuration namespace for shadow. 01_SYSTEM_ARCHITECTURE.md §4; 17_CONFIGURATION_AND_RISK_POLICY.md §1."
  type        = string
  default     = "trading-shadow"
}

variable "kms_key_name" {
  description = "shadow-scoped KMS key. 01_SYSTEM_ARCHITECTURE.md §4: separate encryption keys per environment. 06_SECURITY_AND_ACCESS_CONTROL.md §6: separate key hierarchies per environment and purpose."
  type        = string
}

variable "provider_credentials_path" {
  description = "Path to the shadow-scoped service-account key file used by the provider. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate credentials per environment. 06_SECURITY_AND_ACCESS_CONTROL.md §4: unique workload identity bound to deployment and environment. The value is a PATH, never key material; 23 §2 prohibits secrets in source or artifacts."
  type        = string
  sensitive   = true
}

variable "k8s_endpoint" {
  description = "shadow cluster API endpoint. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate service identities and networks per environment."
  type        = string
}

variable "k8s_token" {
  description = "shadow-scoped short-lived cluster access token. 06_SECURITY_AND_ACCESS_CONTROL.md §4: \"Use short-lived workload credentials\"; no static shared service secrets."
  type        = string
  sensitive   = true
}

variable "k8s_cluster_ca" {
  description = "Base64-encoded shadow cluster CA certificate used to verify the API server (06_SECURITY_AND_ACCESS_CONTROL.md §6: TLS 1.2 minimum)."
  type        = string
  sensitive   = true
}

variable "primary_region" {
  description = "Primary region for the shadow environment's edge, application, data, and management zones. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: three-zone primary region."
  type        = string
}

variable "primary_region_zones" {
  description = "Exactly three zones. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: three-zone primary region."
  type        = list(string)
}

variable "recovery_region" {
  description = "Dev recovery region. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: encrypted WAL/backups replicated to a separately secured recovery region."
  type        = string
}

variable "network_cidr_base" {
  description = "Base CIDR for the shadow five-zone network. Must not overlap any other environment's base. 19 §2 requires separate networks per environment."
  type        = string
}

variable "otlp_endpoint" {
  description = "shadow OpenTelemetry collector endpoint. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: telemetry exported through OpenTelemetry collectors."
  type        = string
}

variable "api_image" {
  description = "Digest-pinned Go control-plane image for shadow. 24_ENTERPRISE_RELEASE_STANDARD.md §15: promoted through validation environments without rebuilding; :latest prohibited."
  type        = string
}

variable "dispatcher_image" {
  description = "Digest-pinned event-dispatcher image for shadow. 24_ENTERPRISE_RELEASE_STANDARD.md §15."
  type        = string
}

variable "research_image" {
  description = "Digest-pinned Python research image for shadow. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §4: no live credentials, no self-promotion to production."
  type        = string
}

variable "adapter_images" {
  description = "Per-venue adapter images enabled in shadow. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: at least one isolated adapter worker per enabled venue."
  type = map(object({
    image           = string
    venue_id        = string
    market_class    = string
    concurrency_cap = number
  }))
  default = {}
}

variable "api_artifact_digest" {
  description = "SHA-256 digest of the promoted Go API artifact. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package."
  type        = string
}

variable "dispatcher_artifact_digest" {
  description = "SHA-256 digest of the promoted event-dispatcher artifact. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package."
  type        = string
}

variable "config_fingerprint" {
  description = "Immutable shadow configuration snapshot fingerprint. 17_CONFIGURATION_AND_RISK_POLICY.md §1; 24_ENTERPRISE_RELEASE_STANDARD.md §5."
  type        = string
}
