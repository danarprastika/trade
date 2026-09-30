# Per-environment composition module variables.
#
# Sizing values are never invented here: the defaults below are the floors from
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3, and the live-eligibility values are the
# non-waivable preconditions of 11_EXECUTION_GATES.md G11.

# --- Identity and isolation ---------------------------------------------------

variable "environment_name" {
  description = "Environment name from the closed set dev/test/staging/paper/shadow/live. 01_SYSTEM_ARCHITECTURE.md §4. Promotion order is dev -> test -> staging -> paper -> shadow -> live (09_TESTING_AND_RELEASE_EVIDENCE.md §Release strategy)."
  type        = string

  validation {
    condition     = contains(["dev", "test", "staging", "paper", "shadow", "live"], var.environment_name)
    error_message = "Environment must be one of dev, test, staging, paper, shadow, live (01_SYSTEM_ARCHITECTURE.md §4)."
  }
}

variable "project_id" {
  description = "Environment-scoped GCP project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: \"Dev/test/staging/paper/shadow/live have separate accounts/projects, network policies, databases, keys, service identities, and venue credentials.\""
  type        = string
}

variable "lower_environment_project_ids" {
  description = "Projects of LOWER environments. MUST be empty for the live environment: live must be structurally unable to read lower-environment secrets. 01_SYSTEM_ARCHITECTURE.md §4: \"Live credentials are never available to lower environments.\" 19 §2: \"Live secrets are not readable by lower environments.\" The live precondition in main.tf denies any non-empty value here."
  type        = list(string)
  default     = []
}

variable "config_namespace" {
  description = "Environment-scoped configuration namespace. 01_SYSTEM_ARCHITECTURE.md §4: separate configuration namespace per environment. 17_CONFIGURATION_AND_RISK_POLICY.md §1: configuration is environment-scoped and immutable after activation; \"Live configuration cannot be copied automatically from lower environments.\""
  type        = string
}

variable "kms_key_name" {
  description = "Environment-scoped KMS key. 01_SYSTEM_ARCHITECTURE.md §4: separate encryption keys per environment. 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"Use separate key hierarchies per environment and purpose; production keys are non-exportable where supported.\""
  type        = string
}

variable "primary_region" {
  description = "Primary region for the edge, application, data, and management zones. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"Initial production sizing starts with a three-zone primary region and a separately secured recovery region.\""
  type        = string
}

variable "primary_region_zones" {
  description = "Exactly three zones in the primary region. 19 §3: three-zone primary region; NATS, when enabled, requires three nodes across three zones."
  type        = list(string)

  validation {
    condition     = length(var.primary_region_zones) == 3
    error_message = "Exactly three zones required. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  }
}

variable "recovery_region" {
  description = "Separately secured recovery region. 19 §1: \"PostgreSQL is a managed highly available primary with synchronous protection within the primary region and encrypted WAL/backups replicated to a recovery region.\" 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: recovery is active-passive and enters RECOVERY_HOLD."
  type        = string
}

variable "network_cidr_base" {
  description = "Base CIDR for the five zone subnets. Must not overlap any other environment's base; 19 §2 requires separate networks per environment."
  type        = string
}

variable "otlp_endpoint" {
  description = "OpenTelemetry collector endpoint. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: \"Telemetry is exported through OpenTelemetry collectors to metrics, logs, and traces backends.\""
  type        = string
}

# --- Database sizing floor ----------------------------------------------------
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Begin PostgreSQL with a managed HA
# primary/standby pair, each at least 8 vCPU / 32 GiB RAM and 1 TiB encrypted SSD
# storage".

variable "db_vcpu_per_node" {
  description = "vCPU per HA primary/standby node. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 floor: 8 vCPU."
  type        = number
  default     = 8

  validation {
    condition     = var.db_vcpu_per_node >= 8
    error_message = "db_vcpu_per_node must be >= 8. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"each at least 8 vCPU / 32 GiB RAM\"."
  }
}

variable "db_memory_gib_per_node" {
  description = "Memory (GiB) per HA primary/standby node. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 floor: 32 GiB."
  type        = number
  default     = 32

  validation {
    condition     = var.db_memory_gib_per_node >= 32
    error_message = "db_memory_gib_per_node must be >= 32. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  }
}

variable "db_tier" {
  description = "Cloud SQL machine tier matching the vCPU/memory floor."
  type        = string
  default     = "db-custom-8-32768"
}

variable "db_data_disk_size_gb" {
  description = "Per-node encrypted SSD size in GiB. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 floor: 1 TiB = 1024 GiB."
  type        = number
  default     = 1024

  validation {
    condition     = var.db_data_disk_size_gb >= 1024
    error_message = "db_data_disk_size_gb must be >= 1024. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"1 TiB encrypted SSD storage\"."
  }
}

variable "db_max_connections" {
  description = "Maximum client connections with pooling. 19 §3 requires connection pooling; the value must be benchmarked under the capacity envelope (19 §6) before live activation."
  type        = number
  default     = 500
}

variable "db_deletion_protection" {
  description = "Deletion protection on the authoritative database. 19 §2 requires live databases be separate and protected; 01 §10 invariant 4 forbids a restored environment leaving RECOVERY_HOLD without independent verification."
  type        = bool
  default     = true
}

variable "recovery_region_enabled" {
  description = "Whether to provision the recovery-region standby. 19 §1: encrypted WAL/backups replicated to a recovery region. 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives: cross-region RTO <= 30 minutes, RPO <= 5 minutes."
  type        = bool
  default     = true
}

# --- Release artifacts --------------------------------------------------------

variable "api_image" {
  description = "Go control-plane OCI image, digest-pinned. 24_ENTERPRISE_RELEASE_STANDARD.md §15: immutable artifacts, no rebuild between environments. :latest is prohibited."
  type        = string
}

variable "dispatcher_image" {
  description = "Event-dispatcher OCI image, digest-pinned. 24_ENTERPRISE_RELEASE_STANDARD.md §15."
  type        = string
}

variable "research_image" {
  description = "Python research/backtest OCI image, digest-pinned. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §4: research workers have no live credentials and cannot self-promote to production."
  type        = string
}

variable "adapter_images" {
  description = "Per-venue adapter images, one entry per enabled venue, all digest-pinned. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"at least one isolated adapter worker per enabled venue (2 vCPU / 4 GiB each, concurrency capped by venue rules)\"."
  type = map(object({
    image           = string
    venue_id        = string
    market_class    = string
    concurrency_cap = number
  }))
}

variable "api_artifact_digest" {
  description = "SHA-256 digest of the promoted Go API artifact. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package; 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §8 item 1."
  type        = string
}

variable "dispatcher_artifact_digest" {
  description = "SHA-256 digest of the promoted event-dispatcher artifact. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package."
  type        = string
}

variable "config_fingerprint" {
  description = "Immutable configuration snapshot fingerprint. 17_CONFIGURATION_AND_RISK_POLICY.md §1; 24_ENTERPRISE_RELEASE_STANDARD.md §5 requires the fingerprint recorded before and after deployment."
  type        = string
}

# --- Workload sizing ----------------------------------------------------------

variable "api_replicas" {
  description = "Go API replicas. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 floor: three."
  type        = number
  default     = 3

  validation {
    condition     = var.api_replicas >= 3
    error_message = "api_replicas must be >= 3. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"API replicas have a minimum of three in production.\""
  }
}

variable "dispatcher_replicas" {
  description = "Event-dispatcher replicas. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: two."
  type        = number
  default     = 2

  validation {
    condition     = var.dispatcher_replicas >= 2
    error_message = "dispatcher_replicas must be >= 2. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  }
}

variable "research_replicas" {
  description = "Research workers inside the quota-controlled pool. 19 §3 requires the pool be separate and quota-controlled."
  type        = number
  default     = 2
}

variable "api_max_replicas" {
  description = "API autoscaling ceiling. 19 §3 requires autoscaling; the ceiling must be validated against the capacity envelope under 19 §6."
  type        = number
  default     = 12
}

# --- Optional subsystem gates -------------------------------------------------

variable "nats_enabled" {
  description = "Gate for the OPTIONAL NATS JetStream backbone. Default false. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: NATS \"is not required for the initial transactional-outbox implementation\". 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: add only when outbox lag exceeds 5 seconds for 10 minutes or consumer fan-out exceeds 5 critical consumer groups."
  type        = bool
  default     = false
}

variable "nats_gate_evidence" {
  description = "Measured evidence that the 23 §6 introduction threshold was crossed. Required when nats_enabled is true; the modules/nats precondition denies otherwise. 23 §2: \"Migration requires replay/idempotency proof.\""
  type = object({
    metric_name      = string
    observed_value   = number
    threshold_value  = number
    sustain_minutes  = number
    window_start_utc = string
    window_end_utc   = string
    evidence_uri     = string
    reviewed_by      = string
  })
  default = null
}

variable "redis_enabled" {
  description = "Gate for the OPTIONAL cache-only Redis. Default false, including for live. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"Redis is optional cache-only infrastructure and must not be a dependency for risk authorization or order correctness.\""
  type        = bool
  default     = false
}

# --- Venue egress -------------------------------------------------------------

variable "venue_egress_allowed" {
  description = "Whether this environment may carry venue egress. TRUE only for `live`; the isolation precondition in main.tf denies any other value. 11_EXECUTION_GATES.md G11: live credentials are provisioned only in isolated live infrastructure."
  type        = bool
  default     = false
}

variable "venue_egress_allowlist" {
  description = "Explicit per-venue egress allowlist, keyed by workload, destination, protocol, and environment. 06_SECURITY_AND_ACCESS_CONTROL.md §1: \"Egress is deny-by-default and allowlisted by workload, destination, protocol, and environment.\" Entries exist only in `live`; no wildcard destinations; withdrawal/transfer always disabled (06 §6)."
  type = list(object({
    venue_id           = string
    workload           = string
    destination_fqdn   = string
    destination_ports  = list(number)
    protocol           = string
    credential_scope   = string
    withdrawal_enabled = bool
  }))
  default = []
}

# --- Live eligibility evidence (G11) ------------------------------------------
#
# ALL of these default to null. For the `live` environment, any null causes the
# live_gate module to fail the plan. 11_EXECUTION_GATES.md G11: "Failure of any
# condition leaves live mode disabled." 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_
# RELEASE_PROFILE.md §3 invariant 7: "Live capability is disabled by default and
# cannot be activated without exact-scope legal eligibility, verified adult
# account-holder authority, dual approval, and passing release gates."

variable "live_g10_gate_report" {
  description = "G10 Production Readiness gate report. 11_EXECUTION_GATES.md G11: \"G10 is passed\". 11: a gate is PASS only when every criterion is satisfied; partial completion is FAIL."
  type = object({
    gate_id      = string
    result       = string
    evidence_uri = string
    reviewed_by  = string
  })
  default = null
}

variable "live_account_holder" {
  description = "Legal eligibility and adult status of the account holder. 11_EXECUTION_GATES.md G11: \"the account holder is legally eligible and identity/account authority is verified\". 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §5.1: the platform must refuse live activation for a person who is not legally eligible, \"including applicable minimum-age requirements\"."
  type = object({
    legal_capacity_confirmed     = bool
    adult_confirmed              = bool
    identity_verified            = bool
    account_authority_verified   = bool
    evidence_uri                 = string
  })
  default = null
}

variable "live_eligibility_record" {
  description = "Exact-scope eligibility record for the declared_residence + account_holder + legal_entity + venue + account_type + market_class + instrument + product + activity + api_permission + effective_date tuple. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §5 defines the tuple; §6 sets a 30-day maximum record age. 18 §5: \"Unknown, expired, contradictory, or revoked eligibility records fail closed.\""
  type = object({
    record_id        = string
    decision         = string
    effective_date   = string
    expiry           = string
    next_review      = string
    reviewer         = string
    source_authority = string
    rule_citation    = string
    evidence_source  = string
    restrictions     = list(string)
    record_age_days  = number
  })
  default = null
}

variable "live_dual_control" {
  description = "Dual-control record with two DISTINCT identities. 11_EXECUTION_GATES.md G11: \"required dual-control approvals come from two distinct authorized identities\". 06_SECURITY_AND_ACCESS_CONTROL.md §3: \"The initiating actor and approver must be distinct identities.\" 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §6: any change to the diff invalidates approval; approval expires after 24 hours."
  type = object({
    requester_identity         = string
    approver_identity          = string
    approval_valid_hours       = number
    exact_diff_reviewed        = bool
    step_up_freshness_minutes  = number
  })
  default = null
}

variable "live_operational_state" {
  description = "Live-blocking operational conditions. 11_EXECUTION_GATES.md G11: \"no material reconciliation break, stale market data, unresolved UNKNOWN order, or active halt exists\". 04_TRADING_DOMAIN_AND_RISK.md: UNKNOWN orders \"are mandatory when a submission timeout prevents determination of venue outcome\". 05 §Reconciliation: \"A material unresolved break blocks affected risk-increasing actions.\""
  type = object({
    unresolved_material_reconciliation_breaks = number
    unresolved_unknown_orders                = number
    active_halts                             = number
    market_data_stale                        = bool
    risk_limits_reviewed                     = bool
  })
  default = null
}

variable "live_credentials" {
  description = "Live credential isolation posture. 11_EXECUTION_GATES.md G11: \"live credentials are provisioned only in isolated live infrastructure with withdrawal/transfer permissions disabled\". 06_SECURITY_AND_ACCESS_CONTROL.md §6: withdrawal/transfer \"are disabled by default\". 11: live credential isolation is NOT waivable."
  type = object({
    isolated_infrastructure   = bool
    withdrawal_permission     = bool
    transfer_permission       = bool
    credentials_in_lower_envs = bool
  })
  default = null
}

variable "live_owner_authorization" {
  description = "Owner authorization bound to exact configuration and artifact digests. 11_EXECUTION_GATES.md G11: \"owner authorization is recorded against the exact configuration/artifact digests\". 25 §8 item 9: gate reports carry \"commit and artifact digests\"."
  type = object({
    owner_identity           = string
    config_digest            = string
    artifact_digest          = string
    authorization_record_uri = string
    immutable                = bool
  })
  default = null
}

variable "live_canary" {
  description = "Canary scope, abort thresholds, and responsible operator. 11_EXECUTION_GATES.md G11: \"a canary activation runs in the narrowest permitted scope; abort thresholds and the responsible operator are confirmed\". 23 §7: G11 \"requires ... a narrow canary with explicit abort criteria\"."
  type = object({
    scope                = string
    abort_thresholds     = list(string)
    responsible_operator = string
  })
  default = null
}
