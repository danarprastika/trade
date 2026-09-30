# live environment root module variables.
#
# TWO CLASSES OF VARIABLE:
#
#   1. PLATFORM wiring (project, region, images) — required to plan at all.
#   2. G11 LIVE-ELIGIBILITY EVIDENCE — every one of these DEFAULTS TO NULL, and
#      every null FAILS the corresponding precondition in modules/live_gate.
#
# 11_EXECUTION_GATES.md G11: "Failure of any condition leaves live mode disabled.
# This gate is not satisfied by documentation alone."
# 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §3 invariant 7:
# "Live capability is disabled by default and cannot be activated without
# exact-scope legal eligibility, verified adult account-holder authority, dual
# approval, and passing release gates. No control may be bypassed or inferred from
# device location, IP, or a successful API connection."

# --- Platform wiring ---------------------------------------------------------

variable "project_id" {
  description = "The live project's id. MUST differ from every lower environment's project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: \"Dev/test/staging/paper/shadow/live have separate accounts/projects, network policies, databases, keys, service identities, and venue credentials.\""
  type        = string
}

variable "config_namespace" {
  description = "The live configuration namespace. 01_SYSTEM_ARCHITECTURE.md §4: separate configuration namespace per environment. 17_CONFIGURATION_AND_RISK_POLICY.md §1: \"Live configuration cannot be copied automatically from lower environments.\""
  type        = string
  default     = "trading-live"
}

variable "kms_key_name" {
  description = "live-scoped KMS key, non-exportable where supported. 01_SYSTEM_ARCHITECTURE.md §4: separate encryption keys per environment. 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"Use separate key hierarchies per environment and purpose; production keys are non-exportable where supported.\""
  type        = string
}

variable "provider_credentials_path" {
  description = "Path to the live-scoped, just-in-time service-account credential file used by the provider. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: \"Production administration uses a dedicated identity and audited just-in-time access.\" 06_SECURITY_AND_ACCESS_CONTROL.md §5: \"Infrastructure privilege is JIT and expires automatically.\" The value is a PATH, never key material: 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2 prohibits secrets in source, prompts, logs, or artifacts."
  type        = string
  sensitive   = true
}

variable "k8s_endpoint" {
  description = "live cluster API endpoint. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate service identities per environment."
  type        = string
}

variable "k8s_token" {
  description = "live-scoped short-lived cluster access token. 06_SECURITY_AND_ACCESS_CONTROL.md §4: \"Use short-lived workload credentials and mutual TLS for private service-to-service calls... No static shared service secrets.\" 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §3: service workload credential TTL target is 15 minutes, 1 hour maximum token lifetime."
  type        = string
  sensitive   = true
}

variable "k8s_cluster_ca" {
  description = "Base64-encoded live cluster CA certificate used to verify the API server. 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"Encrypt transport with TLS 1.2 minimum (TLS 1.3 preferred)\"."
  type        = string
  sensitive   = true
}

variable "primary_region" {
  description = "Primary region for live. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"Initial production sizing starts with a three-zone primary region and a separately secured recovery region.\""
  type        = string
}

variable "primary_region_zones" {
  description = "Exactly three zones in the live primary region. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  type        = list(string)

  validation {
    condition     = length(var.primary_region_zones) == 3
    error_message = "Exactly three zones required. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: a three-zone primary region."
  }
}

variable "recovery_region" {
  description = "Separately secured live recovery region. 19 §1: \"PostgreSQL is a managed highly available primary with synchronous protection within the primary region and encrypted WAL/backups replicated to a recovery region.\" 23 §2: recovery is active-passive and enters RECOVERY_HOLD."
  type        = string
}

variable "network_cidr_base" {
  description = "Base CIDR for the live five-zone network. MUST NOT overlap any lower environment's base. 19 §2 requires separate networks per environment."
  type        = string
}

variable "otlp_endpoint" {
  description = "live OpenTelemetry collector endpoint. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: \"Telemetry is exported through OpenTelemetry collectors to metrics, logs, and traces backends.\" 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Observability."
  type        = string
}

variable "api_image" {
  description = "Digest-pinned Go control-plane image. The SAME digest promoted from staging through paper and shadow: 24_ENTERPRISE_RELEASE_STANDARD.md §15: \"The release candidate is promoted through validation environments without rebuilding from source between environments.\" :latest and mutable tags are denied."
  type        = string
}

variable "dispatcher_image" {
  description = "Digest-pinned event-dispatcher image. 24_ENTERPRISE_RELEASE_STANDARD.md §15."
  type        = string
}

variable "research_image" {
  description = "Digest-pinned Python research image. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §4: research workers have \"Live credentials, direct authoritative writes, self-promotion to production\" explicitly prohibited. The live research pool mounts no service account token."
  type        = string
}

variable "adapter_images" {
  description = "Per-venue adapter images, one entry per ENABLED venue, all digest-pinned. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"at least one isolated adapter worker per enabled venue (2 vCPU / 4 GiB each, concurrency capped by venue rules)\". 16_MARKET_DATA_AND_VENUE_ADAPTERS.md §6: live eligibility additionally requires environment isolation, operational runbooks, venue-specific risk policy, owner approval, and successful paper/shadow evidence."
  type = map(object({
    image           = string
    venue_id        = string
    market_class    = string
    concurrency_cap = number
  }))
  default = {}
}

variable "api_artifact_digest" {
  description = "SHA-256 digest of the promoted Go API artifact. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package; 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §8 item 1: \"Source commit, protected-branch review, build identity, toolchain versions, dependency lockfiles, SBOM, provenance, and artifact digest.\""
  type        = string
}

variable "dispatcher_artifact_digest" {
  description = "SHA-256 digest of the promoted event-dispatcher artifact. 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package."
  type        = string
}

variable "config_fingerprint" {
  description = "The immutable live configuration snapshot fingerprint. 17_CONFIGURATION_AND_RISK_POLICY.md §1: configuration is \"immutable after activation\"; a change creates a new revision with actor, reason, timestamp, approval, diff, and effective scope. 24_ENTERPRISE_RELEASE_STANDARD.md §5: \"A production configuration fingerprint is recorded before deployment and after deployment.\" 11_EXECUTION_GATES.md G11 requires owner authorization recorded against this exact digest."
  type        = string
}

variable "nats_enabled" {
  description = "Gate for the OPTIONAL NATS JetStream backbone in live. Default false. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: NATS \"is not required for the initial transactional-outbox implementation\". 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: add only when outbox lag exceeds 5 seconds for 10 minutes or consumer fan-out exceeds 5 critical consumer groups; \"Migration requires replay/idempotency proof.\""
  type        = bool
  default     = false
}

variable "nats_gate_evidence" {
  description = "Measured evidence that the 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 introduction threshold was crossed. Null unless the threshold was actually measured and exceeded; modules/nats denies otherwise. 12_DECISION_REGISTER.md ADR-020."
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

variable "venue_egress_allowlist" {
  description = "Explicit venue endpoint allowlist for live. 06_SECURITY_AND_ACCESS_CONTROL.md §1: \"Egress is deny-by-default and allowlisted by workload, destination, protocol, and environment.\" Live is the only environment that may carry entries, and each entry is validated in modules/network: destination must be an explicit FQDN (no wildcard), protocol must be https, and withdrawal_enabled must be false (06 §6: withdrawal/transfer permissions are disabled by default; 11_EXECUTION_GATES.md G11 requires them disabled). 11_EXECUTION_GATES.md G11: \"API connectivity, provider marketing, or another user's approval is not evidence of eligibility\" is unrelated to connectivity itself, which remains required for adapters."
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

# --- G11 LIVE-ELIGIBILITY EVIDENCE ------------------------------------------
# Every variable below defaults to null. A null value FAILS its precondition in
# modules/live_gate, which fails `terraform plan` for live.

variable "g10_gate_report" {
  description = "G10 Production Readiness gate report. 11_EXECUTION_GATES.md G11: \"G10 is passed\". 11: \"A gate is PASS only when every listed criterion is satisfied; partial completion is FAIL, not a percentage.\" 13_IMPLEMENTATION_HANDOFF.md: \"G11 is not implied by passing G0-G10.\""
  type = object({
    gate_id      = string
    result       = string
    evidence_uri = string
    reviewed_by  = string
  })
  default = null
}

variable "account_holder" {
  description = "Legal eligibility and adult status of the account holder. 11_EXECUTION_GATES.md G11: \"the account holder is legally eligible and identity/account authority is verified\". 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §5.1: the platform \"must refuse live activation for a person who is not legally eligible to hold and operate the relevant account, including applicable minimum-age requirements. It must not support account sharing, false declarations, or bypass of identity/age checks.\" 23 §5: \"The system does not support shared credentials, false age/residence declarations, or bypasses.\" NON-WAIVABLE."
  type = object({
    legal_capacity_confirmed   = bool
    adult_confirmed            = bool
    identity_verified          = bool
    account_authority_verified = bool
    evidence_uri               = string
  })
  default = null
}

variable "eligibility_record" {
  description = "Exact-scope eligibility record over the 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §5 tuple: declared_residence + account_holder + legal_entity + venue + account_type + market_class + instrument + product + activity + api_permission + effective_date. §6 sets \"Eligibility record maximum age | 30 days\". 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §5: \"Unknown, expired, contradictory, or revoked eligibility records fail closed. Scope expansion requires a new approval.\" §5.1: revalidated at least every 30 days; if a source cannot be refreshed within 72 hours the affected scope is blocked. NON-WAIVABLE."
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

variable "dual_control" {
  description = "Dual-control record with two DISTINCT identities. 11_EXECUTION_GATES.md G11: \"required dual-control approvals come from two distinct authorized identities\". 06_SECURITY_AND_ACCESS_CONTROL.md §3: \"The initiating actor and approver must be distinct identities.\" 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §6: \"A distinct approver reviews the exact diff and current risk/eligibility status. Any change to the diff invalidates approval. Approval expires after 24 hours.\" Step-up freshness must be at most 5 minutes (06 §2). NON-WAIVABLE."
  type = object({
    requester_identity        = string
    approver_identity         = string
    approval_valid_hours      = number
    exact_diff_reviewed       = bool
    step_up_freshness_minutes = number
  })
  default = null
}

variable "operational_state" {
  description = "Live-blocking operational state. 11_EXECUTION_GATES.md G11: \"no material reconciliation break, stale market data, unresolved UNKNOWN order, or active halt exists\". 04_TRADING_DOMAIN_AND_RISK.md §Order lifecycle: \"UNKNOWN is mandatory when a submission timeout prevents determination of venue outcome. The system reconciles before any retry that could duplicate exposure.\" 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Reconciliation: \"A material unresolved break blocks affected risk-increasing actions.\" 16 §2: risk-increasing commands are rejected when required inputs are stale or degraded. 17_CONFIGURATION_AND_RISK_POLICY.md §2: an active halt means reject. NON-WAIVABLE."
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
  description = "Live credential isolation posture. 11_EXECUTION_GATES.md G11: \"live credentials are provisioned only in isolated live infrastructure with withdrawal/transfer permissions disabled\". 11: \"Waivers are prohibited for financial invariants, authorization, live credential isolation, reconciliation, halt controls, and recovery objectives.\" 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"Live venue credentials are scoped to required API functions; withdrawal/transfer permissions are prohibited unless separately justified and approved, and are disabled by default.\" 01_SYSTEM_ARCHITECTURE.md §4: \"Live credentials are never available to lower environments.\" HIGHEST-SEVERITY NON-WAIVABLE CONTROL."
  type = object({
    isolated_infrastructure   = bool
    withdrawal_permission     = bool
    transfer_permission       = bool
    credentials_in_lower_envs = bool
  })
  default = null
}

variable "owner_authorization" {
  description = "Owner authorization recorded against the EXACT configuration and artifact digests. 11_EXECUTION_GATES.md G11: \"owner authorization is recorded against the exact configuration/artifact digests\". 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §8 item 9: \"Gate report with binary PASS/FAIL, evidence references, reviewer identities, UTC timestamps, commit and artifact digests.\" 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §1: the owner is accountable for eligible account holder status, jurisdiction scope, market enablement, risk mandate, and live activation."
  type = object({
    owner_identity           = string
    config_digest            = string
    artifact_digest          = string
    authorization_record_uri = string
    immutable                = bool
  })
  default = null
}

variable "canary" {
  description = "Canary scope, abort thresholds, and responsible operator. 11_EXECUTION_GATES.md G11: \"a canary activation runs in the narrowest permitted scope; abort thresholds and the responsible operator are confirmed\". 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §7: G11 \"requires exact-scope eligibility, verified adult/account authority, distinct dual approvers, no unresolved material reconciliation break or UNKNOWN order, reviewed risk configuration, immutable evidence, and a narrow canary with explicit abort criteria.\" 25 §8 item 10: post-deployment verification plan, canary boundaries, abort criteria, rollback decision owner, and explicit completion record."
  type = object({
    scope                = string
    abort_thresholds     = list(string)
    responsible_operator = string
  })
  default = null
}
