# Redis cache module variables.
#
# DEFAULT IS FALSE. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 calls Redis
# "optional cache-only infrastructure"; the default must be false for every
# environment including live.

variable "environment_name" {
  description = "Environment name from the closed set dev/test/staging/paper/shadow/live. 01_SYSTEM_ARCHITECTURE.md §4."
  type        = string
}

variable "project_id" {
  description = "Environment-scoped GCP project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2."
  type        = string
}

variable "primary_region" {
  description = "Region hosting the cache. The cache sits in the application/data private plane; it is never publicly reachable. 06_SECURITY_AND_ACCESS_CONTROL.md §1."
  type        = string
}

variable "vpc_id" {
  description = "Private network the cache is bound to. 06 §1: the edge cannot reach internal data services."
  type        = string
}

variable "enabled" {
  description = "Gate for the OPTIONAL cache. Default false, and false for live. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"Redis is optional cache-only infrastructure\". Enabling it never affects risk or order correctness."
  type        = bool
  default     = false
}

variable "tier" {
  description = "Redis tier. 00_README.md §Authoritative technology baseline names Redis 8 as the cache technology; tier sizing is an engineering default subject to capacity benchmarking under 19 §6."
  type        = string
  default     = "BASIC"
}

variable "memory_size_gb" {
  description = "Cache memory in GiB. Not specified in the blueprint; must be derived from measured working-set size and is not an authoritative capacity figure. 19 §6 requires capacity evidence before G10."
  type        = number
  default     = 4
}

variable "redis_version" {
  description = "Redis major version. 00_README.md §Authoritative technology baseline: Redis 8. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §7 prohibits end-of-life runtimes."
  type        = string
  default     = "REDIS_8_0"
}

# --- Constraint flags --------------------------------------------------------
# These exist so the cache-only prohibition is machine-enforced rather than a
# comment. They are hard-coded to false in this module's contract; the
# preconditions in main.tf deny any value other than false. A caller cannot
# promote this cache into a financial path by setting a variable.

variable "allowed_as_risk_dependency" {
  description = "MUST be false. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: Redis \"must not be a dependency for risk authorization\". There is no valid value of true; the variable exists to make the prohibition testable."
  type        = bool
  default     = false

  validation {
    condition     = var.allowed_as_risk_dependency == false
    error_message = "Redis can never be a risk-authorization dependency. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 and 12_DECISION_REGISTER.md ADR-007."
  }
}

variable "allowed_as_order_dependency" {
  description = "MUST be false. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: Redis \"must not be a dependency for ... order correctness\". There is no valid value of true."
  type        = bool
  default     = false

  validation {
    condition     = var.allowed_as_order_dependency == false
    error_message = "Redis can never be an order-correctness dependency. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 and 12_DECISION_REGISTER.md ADR-007."
  }
}

variable "allowed_as_ledger_source" {
  description = "MUST be false. 00_README.md §Authoritative technology baseline: cache is \"Ephemeral only; never source of financial truth\". There is no valid value of true."
  type        = bool
  default     = false

  validation {
    condition     = var.allowed_as_ledger_source == false
    error_message = "Redis can never be a source of financial truth. 00_README.md and 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §4: \"No cache can become authoritative through fallback behavior.\""
  }
}
