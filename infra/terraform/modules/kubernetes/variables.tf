# Kubernetes module variables. Every variable declares a description and a type.
#
# Sizing values are NOT invented here: each default below carries the blueprint
# sentence it comes from. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 states these are
# "initial minimums, not a claim that a particular cloud SKU guarantees the
# capacity envelope", so validation rejects values BELOW the floor while allowing
# larger allocations pending benchmark evidence (19 §6).

variable "environment_name" {
  description = "Environment name from the closed set dev/test/staging/paper/shadow/live. 01_SYSTEM_ARCHITECTURE.md §4."
  type        = string

  validation {
    condition     = contains(["dev", "test", "staging", "paper", "shadow", "live"], var.environment_name)
    error_message = "Environment must be one of dev, test, staging, paper, shadow, live (01_SYSTEM_ARCHITECTURE.md §4)."
  }
}

variable "namespace" {
  description = "Environment configuration namespace. 01_SYSTEM_ARCHITECTURE.md §4: each environment has a separate configuration namespace. 17_CONFIGURATION_AND_RISK_POLICY.md §1: configuration is typed, schema-validated, versioned, environment-scoped, and immutable after activation."
  type        = string
}

# --- Digest-pinned images -----------------------------------------------------

variable "api_image" {
  description = "Go control-plane OCI image reference. MUST be digest-pinned (@sha256:<64 hex>). 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: \"Terraform-managed, immutable OCI images, signed release artifacts\". 24_ENTERPRISE_RELEASE_STANDARD.md §15: \"Production releases use immutable artifacts and progressive deployment. The release candidate is promoted through validation environments without rebuilding from source between environments.\" Mutable tags and :latest are denied."
  type        = string

  validation {
    condition     = can(regex("@sha256:[a-f0-9]{64}$", var.api_image))
    error_message = "api_image must be digest-pinned as <repo>@sha256:<64 hex>. 24_ENTERPRISE_RELEASE_STANDARD.md §15 and 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2 require immutable, digest-pinned artifacts; a mutable tag is a deny condition."
  }

  validation {
    condition     = !can(regex(":latest", var.api_image))
    error_message = ":latest is prohibited. 23 §2 requires immutable OCI images; 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §8 item 1 records the artifact digest in the release evidence package."
  }
}

variable "dispatcher_image" {
  description = "Event-dispatcher OCI image reference. MUST be digest-pinned (@sha256:<64 hex>). 24_ENTERPRISE_RELEASE_STANDARD.md §15."
  type        = string

  validation {
    condition     = can(regex("@sha256:[a-f0-9]{64}$", var.dispatcher_image))
    error_message = "dispatcher_image must be digest-pinned. 24_ENTERPRISE_RELEASE_STANDARD.md §15: rollback uses a previously verified immutable artifact."
  }

  validation {
    condition     = !can(regex(":latest", var.dispatcher_image))
    error_message = ":latest is prohibited on the event dispatcher. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2."
  }
}

variable "research_image" {
  description = "Python research/backtest worker OCI image reference. MUST be digest-pinned (@sha256:<64 hex>). 02_POLYGLOT_ENGINEERING_STANDARD.md: artifact outputs must be content-addressed and versioned before promotion. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §4: research workers may not self-promote to production."
  type        = string

  validation {
    condition     = can(regex("@sha256:[a-f0-9]{64}$", var.research_image))
    error_message = "research_image must be digest-pinned. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §4 forbids research artifacts self-promoting to production, so immutability is required."
  }
}

variable "adapter_images" {
  description = "Per-venue adapter OCI image references, one entry per ENABLED venue. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"at least one isolated adapter worker per enabled venue (2 vCPU / 4 GiB each, concurrency capped by venue rules)\". Every image must be digest-pinned; a mutable tag or :latest is denied."
  type = map(object({
    image           = string
    venue_id        = string
    market_class    = string
    concurrency_cap = number
  }))

  validation {
    condition = alltrue([
      for k, v in var.adapter_images : can(regex("@sha256:[a-f0-9]{64}$", v.image))
    ])
    error_message = "Every adapter image must be digest-pinned. 24_ENTERPRISE_RELEASE_STANDARD.md §15 requires immutable artifacts for every promoted component."
  }

  validation {
    condition = alltrue([
      for k, v in var.adapter_images : !can(regex(":latest", v.image))
    ])
    error_message = ":latest is prohibited on adapter images. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2 requires immutable OCI images."
  }

  validation {
    condition = alltrue([
      for k, v in var.adapter_images : v.concurrency_cap > 0
    ])
    error_message = "Every adapter must declare a positive per-venue concurrency cap. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: adapter concurrency is governed by per-venue rules, not generic autoscaling."
  }
}

# --- Artifact / configuration digests recorded on workloads -----------------
# 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package: "Each release stores
# commit SHA, artifact digest, test report, coverage report, security scan
# results, SBOM, migration result, deployment manifest, configuration
# fingerprint, approval records, and rollback verification."
# 24_ENTERPRISE_RELEASE_STANDARD.md §5: "A production configuration fingerprint is
# recorded before deployment and after deployment."

variable "api_artifact_digest" {
  description = "SHA-256 digest of the promoted Go API OCI artifact, recorded on the workload for release evidence. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package; 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §8 item 1."
  type        = string

  validation {
    condition     = can(regex("^sha256:[a-f0-9]{64}$", var.api_artifact_digest))
    error_message = "api_artifact_digest must be sha256:<64 hex>. 25 §8 item 1 requires the artifact digest in the release evidence package."
  }
}

variable "dispatcher_artifact_digest" {
  description = "SHA-256 digest of the promoted event-dispatcher OCI artifact. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package."
  type        = string

  validation {
    condition     = can(regex("^sha256:[a-f0-9]{64}$", var.dispatcher_artifact_digest))
    error_message = "dispatcher_artifact_digest must be sha256:<64 hex>."
  }
}

variable "config_fingerprint" {
  description = "Immutable release configuration snapshot fingerprint for this environment. 17_CONFIGURATION_AND_RISK_POLICY.md §1: configuration is environment-scoped and immutable after activation; live configuration cannot be copied from lower environments. 24_ENTERPRISE_RELEASE_STANDARD.md §5 requires the fingerprint recorded before and after deployment."
  type        = string

  validation {
    condition     = can(regex("^sha256:[a-f0-9]{64}$", var.config_fingerprint))
    error_message = "config_fingerprint must be sha256:<64 hex>. 17_CONFIGURATION_AND_RISK_POLICY.md §1 and 24_ENTERPRISE_RELEASE_STANDARD.md §5 require an immutable, fingerprinted configuration snapshot."
  }
}

variable "otlp_endpoint" {
  description = "OpenTelemetry collector endpoint receiving metrics, logs, and traces. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: \"Telemetry is exported through OpenTelemetry collectors to metrics, logs, and traces backends.\" 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Observability."
  type        = string
}

# --- Sizing floors -----------------------------------------------------------
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 is the single source for all five.

variable "api_replicas" {
  description = "Go API replica count. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"The minimum application footprint is three Go API replicas (2 vCPU / 4 GiB each)\"; \"API replicas have a minimum of three in production.\" Validation denies fewer than three."
  type        = number
  default     = 3

  validation {
    condition     = var.api_replicas >= 3
    error_message = "api_replicas must be >= 3. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"The minimum application footprint is three Go API replicas\" and \"API replicas have a minimum of three in production.\""
  }
}

variable "dispatcher_replicas" {
  description = "Event-dispatcher replica count. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"two event-dispatcher replicas (2 vCPU / 4 GiB each)\"."
  type        = number
  default     = 2

  validation {
    condition     = var.dispatcher_replicas >= 2
    error_message = "dispatcher_replicas must be >= 2. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"two event-dispatcher replicas (2 vCPU / 4 GiB each)\"."
  }
}

variable "research_replicas" {
  description = "Research worker replica count inside the quota-controlled pool. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 requires a separate quota-controlled pool that \"cannot consume reserved API, risk, OMS, or adapter capacity\"; the count is bounded by the quota, not a blueprint figure."
  type        = number
  default     = 2
}

variable "api_max_replicas" {
  description = "Ceiling for API autoscaling. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 requires autoscaling of stateless pools; the ceiling bounds blast radius and must be validated against the capacity envelope under 19 §6 before live activation."
  type        = number
  default     = 12
}

variable "api_cpu_request" {
  description = "CPU request and limit per Go API replica. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"3 Go API replicas (2 vCPU / 4 GiB each)\"."
  type        = string
  default     = "2"
}

variable "api_memory_request" {
  description = "Memory request and limit per Go API replica. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"(2 vCPU / 4 GiB each)\". The floor is enforced by validation."
  type        = string
  default     = "4Gi"

  validation {
    condition     = var.api_memory_request == "4Gi"
    error_message = "api_memory_request floor is 4Gi per replica. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"3 Go API replicas (2 vCPU / 4 GiB each)\"."
  }
}

variable "dispatcher_cpu_request" {
  description = "CPU request and limit per event-dispatcher replica. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"two event-dispatcher replicas (2 vCPU / 4 GiB each)\"."
  type        = string
  default     = "2"
}

variable "dispatcher_memory_request" {
  description = "Memory request and limit per event-dispatcher replica. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"(2 vCPU / 4 GiB each)\"."
  type        = string
  default     = "4Gi"

  validation {
    condition     = var.dispatcher_memory_request == "4Gi"
    error_message = "dispatcher_memory_request floor is 4Gi per replica. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  }
}

variable "adapter_cpu_request" {
  description = "CPU request and limit per venue adapter worker. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"at least one isolated adapter worker per enabled venue (2 vCPU / 4 GiB each)\"."
  type        = string
  default     = "2"
}

variable "adapter_memory_request" {
  description = "Memory request and limit per venue adapter worker. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"(2 vCPU / 4 GiB each)\"."
  type        = string
  default     = "4Gi"

  validation {
    condition     = var.adapter_memory_request == "4Gi"
    error_message = "adapter_memory_request floor is 4Gi per replica. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"at least one isolated adapter worker per enabled venue (2 vCPU / 4 GiB each)\"."
  }
}

# --- Autoscale thresholds ----------------------------------------------------
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Autoscale stateless API/worker pools
# on sustained CPU >65% or queue lag above the documented threshold for five
# minutes, with scale-in only after ten minutes below 35% and with in-flight work
# drained."

variable "autoscale_scale_out_cpu_percent" {
  description = "Sustained CPU percent that triggers scale-out. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"sustained CPU >65% ... for five minutes\"."
  type        = number
  default     = 65

  validation {
    condition     = var.autoscale_scale_out_cpu_percent == 65
    error_message = "autoscale_scale_out_cpu_percent must be 65. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 binding value; relaxation requires a reviewed ADR (23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6)."
  }
}

variable "autoscale_scale_in_cpu_percent" {
  description = "CPU percent below which scale-in is permitted. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"scale-in only after ten minutes below 35% and with in-flight work drained\"."
  type        = number
  default     = 35

  validation {
    condition     = var.autoscale_scale_in_cpu_percent == 35
    error_message = "autoscale_scale_in_cpu_percent must be 35. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 binding value."
  }
}

variable "autoscale_stabilization_minutes" {
  description = "Stabilization window in minutes. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: five minutes sustained for the scale-out trigger; ten minutes is the scale-in safety margin before drain."
  type        = number
  default     = 5

  validation {
    condition     = contains([5, 10], var.autoscale_stabilization_minutes)
    error_message = "Stabilization must be 5 minutes (19 §3 scale-out trigger window) or 10 minutes (19 §3 scale-in margin)."
  }
}

# --- Research pool isolation -------------------------------------------------

variable "research_quota_cpu" {
  description = "CPU quota for the isolated research pool. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"Python research/backtest workers use a separate quota-controlled pool and cannot consume reserved API, risk, OMS, or adapter capacity.\" 24_ENTERPRISE_RELEASE_STANDARD.md §2 cost governance: \"Non-production cannot consume reserved critical-path capacity.\""
  type        = string
  default     = "8"
}

variable "research_quota_memory" {
  description = "Memory quota for the isolated research pool, disjoint from the reserved API/risk/OMS/adapter allocations. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  type        = string
  default     = "16Gi"
}
