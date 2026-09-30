# Live activation gate.
#
# THIS MODULE STRUCTURALLY BLOCKS THE `live` ENVIRONMENT unless live eligibility
# evidence is present. It is not documentation; it is an evaluated precondition
# that fails `terraform plan`/`apply`.
#
# TRACEABILITY
#   11_EXECUTION_GATES.md G11: "Pass criteria: G10 is passed; the account holder
#     is legally eligible and identity/account authority is verified; the exact
#     jurisdiction/account/venue/product/instrument/activity tuple has current
#     approved eligibility evidence; live credentials are provisioned only in
#     isolated live infrastructure with withdrawal/transfer permissions disabled;
#     all account-, venue-, instrument-, and strategy-specific risk limits are
#     configured and independently reviewed; no material reconciliation break,
#     stale market data, unresolved UNKNOWN order, or active halt exists; required
#     dual-control approvals come from two distinct authorized identities; owner
#     authorization is recorded against the exact configuration/artifact digests;
#     a canary activation runs in the narrowest permitted scope; abort thresholds
#     and the responsible operator are confirmed; and the activation evidence is
#     immutable. Failure of any condition leaves live mode disabled."
#   11_EXECUTION_GATES.md: "Waivers are prohibited for financial invariants,
#     authorization, live credential isolation, reconciliation, halt controls, and
#     recovery objectives."
#   18_GOVERNANCE_DATA_AND_COMPLIANCE.md §5: "Unknown, expired, contradictory, or
#     revoked eligibility records fail closed... The platform blocks live
#     activation when the account holder is not legally eligible... No live
#     credentials are provisioned before eligibility approval."
#   23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §5: "Eligibility records expire
#     after at most 30 days unless a stricter interval is required." (§6 table:
#     "Eligibility record maximum age | 30 days")
#   25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §3 invariant 7:
#     "Live capability is disabled by default and cannot be activated without
#     exact-scope legal eligibility, verified adult account-holder authority, dual
#     approval, and passing release gates."
#   04_TRADING_DOMAIN_AND_RISK.md §Live safety defaults: "Live is disabled by
#     default. Credentials are unavailable until activation."
#   11_EXECUTION_GATES.md: "Each gate is PASS only when every listed criterion is
#     satisfied; partial completion is FAIL, not a percentage."

terraform {
  required_version = "= 1.9.8"
}

variable "environment_name" {
  description = "Environment under evaluation. The gate applies only to `live`. 01_SYSTEM_ARCHITECTURE.md §4."
  type        = string
}

variable "g10_gate_report" {
  description = "Evidence that G10 (Production Readiness) passed. 11_EXECUTION_GATES.md G11: \"G10 is passed\". 11: \"A gate is PASS only when every listed criterion is satisfied; partial completion is FAIL, not a percentage.\""
  type = object({
    gate_id     = string
    result      = string
    evidence_uri = string
    reviewed_by = string
  })
  default = null
}

variable "account_holder" {
  description = "Legal eligibility and adult status of the account holder. 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §5.1: the platform \"must refuse live activation for a person who is not legally eligible to hold and operate the relevant account, including applicable minimum-age requirements.\" 23 §5: \"Live capability is restricted to a legally eligible adult account holder with verified identity and account authority.\""
  type = object({
    legal_capacity_confirmed = bool
    adult_confirmed          = bool
    identity_verified        = bool
    account_authority_verified = bool
    evidence_uri             = string
  })
  default = null
}

variable "eligibility_record" {
  description = "Exact-scope eligibility evidence for the jurisdiction/account/venue/product/instrument/activity tuple. 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §5 requires a signed eligibility record; 23 §5 requires evaluation over the tuple declared_residence + account_holder + legal_entity + venue + account_type + market_class + instrument + product + activity + api_permission + effective_date."
  type = object({
    record_id         = string
    decision          = string
    effective_date    = string
    expiry            = string
    next_review       = string
    reviewer          = string
    source_authority  = string
    rule_citation     = string
    evidence_source   = string
    restrictions      = list(string)
    record_age_days   = number
  })
  default = null
}

variable "eligibility_max_age_days" {
  description = "Maximum age of an eligibility record. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6: \"Eligibility record maximum age | 30 days\". 18 §5.1: \"An eligibility record is revalidated at least every 30 days\"."
  type        = number
  default     = 30

  validation {
    condition     = var.eligibility_max_age_days == 30
    error_message = "eligibility_max_age_days must be 30. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6 binding default; 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §5.1 requires revalidation at least every 30 days. A longer interval requires a recorded risk approval (18 §5.1)."
  }
}

variable "dual_control" {
  description = "Dual-control approval record. 11_EXECUTION_GATES.md G11: \"required dual-control approvals come from two distinct authorized identities\". 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §6: \"Same identity cannot satisfy both approvals; any diff invalidates approval.\" 06_SECURITY_AND_ACCESS_CONTROL.md §3: \"The initiating actor and approver must be distinct identities.\""
  type = object({
    requester_identity   = string
    approver_identity    = string
    approval_valid_hours = number
    exact_diff_reviewed  = bool
    step_up_freshness_minutes = number
  })
  default = null
}

variable "operational_state" {
  description = "Live-blocking operational conditions. 11_EXECUTION_GATES.md G11: \"no material reconciliation break, stale market data, unresolved UNKNOWN order, or active halt exists\". 04_TRADING_DOMAIN_AND_RISK.md §Order lifecycle: \"UNKNOWN is mandatory when a submission timeout prevents determination of venue outcome.\" 18 §5: material unresolved break blocks affected risk-increasing actions."
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
  description = "Live credential isolation posture. 11_EXECUTION_GATES.md G11: \"live credentials are provisioned only in isolated live infrastructure with withdrawal/transfer permissions disabled\". 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"withdrawal/transfer permissions are prohibited unless separately justified and approved, and are disabled by default.\" 11: live credential isolation is non-waivable."
  type = object({
    isolated_infrastructure     = bool
    withdrawal_permission       = bool
    transfer_permission         = bool
    credentials_in_lower_envs   = bool
  })
  default = null
}

variable "owner_authorization" {
  description = "Owner authorization bound to exact digests. 11_EXECUTION_GATES.md G11: \"owner authorization is recorded against the exact configuration/artifact digests\". 25 §8 item 9: \"Gate report with binary PASS/FAIL, evidence references, reviewer identities, UTC timestamps, commit and artifact digests.\""
  type = object({
    owner_identity        = string
    config_digest         = string
    artifact_digest       = string
    authorization_record_uri = string
    immutable             = bool
  })
  default = null
}

variable "canary" {
  description = "Canary scope and abort criteria. 11_EXECUTION_GATES.md G11: \"a canary activation runs in the narrowest permitted scope; abort thresholds and the responsible operator are confirmed\". 23 §7: G11 \"requires ... a narrow canary with explicit abort criteria\"."
  type = object({
    scope             = string
    abort_thresholds  = list(string)
    responsible_operator = string
  })
  default = null
}

# --- Evaluated gate ----------------------------------------------------------
# Every condition below is a `precondition`, so a failure fails the plan. There is
# no "warning" path and no partial-pass path.
#
# NOTE ON 23 §5: the blueprint states account-holder and eligibility requirements
# and the 30-day maximum record age. It does NOT supply a numeric minimum age
# (18 §5.1 refers to "applicable minimum-age requirements" without a figure, and
# 20_FINAL_AUDIT_CLOSURE_AND_ACCEPTANCE.md §3 lists under-age declarations as a
# deny condition). This module therefore encodes ADULT (age of majority) as
# verified by a qualified reviewer, not as an invented numeric age. The
# accompanying Python guard treats any declared age below 18 as ineligible.

locals {
  adult_and_eligible = (
    var.account_holder != null &&
    var.account_holder.legal_capacity_confirmed &&
    var.account_holder.adult_confirmed &&
    var.account_holder.identity_verified &&
    var.account_holder.account_authority_verified
  )

  eligibility_present_and_current = (
    var.eligibility_record != null &&
    var.eligibility_record.decision == "APPROVED" &&
    var.eligibility_record.record_age_days >= 0 &&
    var.eligibility_record.record_age_days <= var.eligibility_max_age_days
  )

  dual_control_distinct = (
    var.dual_control != null &&
    length(var.dual_control.requester_identity) > 0 &&
    length(var.dual_control.approver_identity) > 0 &&
    var.dual_control.requester_identity != var.dual_control.approver_identity &&
    var.dual_control.exact_diff_reviewed &&
    var.dual_control.step_up_freshness_minutes <= var.step_up_freshness_limit_minutes
  )

  operational_clean = (
    var.operational_state != null &&
    var.operational_state.unresolved_material_reconciliation_breaks == 0 &&
    var.operational_state.unresolved_unknown_orders == 0 &&
    var.operational_state.active_halts == 0 &&
    !var.operational_state.market_data_stale &&
    var.operational_state.risk_limits_reviewed
  )

  credentials_isolated = (
    var.live_credentials != null &&
    var.live_credentials.isolated_infrastructure &&
    !var.live_credentials.withdrawal_permission &&
    !var.live_credentials.transfer_permission &&
    !var.live_credentials.credentials_in_lower_envs
  )

  owner_authorized = (
    var.owner_authorization != null &&
    length(var.owner_authorization.owner_identity) > 0 &&
    can(regex("^sha256:[a-f0-9]{64}$", var.owner_authorization.config_digest)) &&
    can(regex("^sha256:[a-f0-9]{64}$", var.owner_authorization.artifact_digest)) &&
    var.owner_authorization.immutable
  )

  canary_defined = (
    var.canary != null &&
    length(var.canary.scope) > 0 &&
    length(var.canary.abort_thresholds) > 0 &&
    length(var.canary.responsible_operator) > 0
  )

  g10_passed = (
    var.g10_gate_report != null &&
    var.g10_gate_report.gate_id == "G10" &&
    var.g10_gate_report.result == "PASS"
  )

  all_preconditions_satisfied = (
    g10_passed &&
    adult_and_eligible &&
    eligibility_present_and_current &&
    dual_control_distinct &&
    operational_clean &&
    credentials_isolated &&
    owner_authorized &&
    canary_defined
  )
}

variable "step_up_freshness_limit_minutes" {
  description = "Maximum step-up authentication freshness for live activation. 06_SECURITY_AND_ACCESS_CONTROL.md §2: \"Re-authentication freshness is at most 5 minutes for live activation and other high-impact actions.\" 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §3: \"5 minutes for high-impact actions\"."
  type        = number
  default     = 5

  validation {
    condition     = var.step_up_freshness_limit_minutes == 5
    error_message = "step_up_freshness_limit_minutes must be 5. 06_SECURITY_AND_ACCESS_CONTROL.md §2: \"Re-authentication freshness is at most 5 minutes for live activation\". 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §3."
  }
}

resource "terraform_data" "live_activation_gate" {
  count = var.environment_name == "live" ? 1 : 0

  input = {
    environment                  = var.environment_name
    g10_passed                   = local.g10_passed
    adult_and_eligible           = local.adult_and_eligible
    eligibility_current          = local.eligibility_present_and_current
    dual_control_distinct        = local.dual_control_distinct
    operational_state_clean      = local.operational_clean
    credentials_isolated         = local.credentials_isolated
    owner_authorized             = local.owner_authorized
    canary_defined               = local.canary_defined
    live_capability_state        = local.all_preconditions_satisfied ? "ACTIVATABLE" : "DISABLED"
  }

  lifecycle {
    # G11: G10 passed.
    precondition {
      condition = local.g10_passed
      error_message = <<-EOT
        DENY: G10 (Production Readiness) has not passed. 11_EXECUTION_GATES.md G11 requires
        "G10 is passed" as the first live-activation precondition. 11: "A gate is PASS only
        when every listed criterion is satisfied; partial completion is FAIL, not a
        percentage." 13_IMPLEMENTATION_HANDOFF.md: "G11 is not implied by passing G0-G10."
      EOT
    }

    # G11: legally eligible adult account holder with verified identity/authority.
    precondition {
      condition = local.adult_and_eligible
      error_message = <<-EOT
        DENY: account holder is not confirmed legally eligible, adult, identity-verified, and
        account-authority-verified. 11_EXECUTION_GATES.md G11: "the account holder is legally
        eligible and identity/account authority is verified". 18_GOVERNANCE_DATA_AND_
        COMPLIANCE.md §5.1: the platform "must refuse live activation for a person who is not
        legally eligible to hold and operate the relevant account, including applicable
        minimum-age requirements. It must not support account sharing, false declarations, or
        bypass of identity/age checks." 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_
        PROFILE.md §3 invariant 7. This is NOT waivable.
      EOT
    }

    # G11: current exact-scope eligibility evidence.
    precondition {
      condition = local.eligibility_present_and_current
      error_message = <<-EOT
        DENY: exact-scope eligibility evidence is missing, unapproved, or stale. 11_
        EXECUTION_GATES.md G11: "the exact jurisdiction/account/venue/product/instrument/
        activity tuple has current approved eligibility evidence". 18_GOVERNANCE_DATA_AND_
        COMPLIANCE.md §5: "Unknown, expired, contradictory, or revoked eligibility records fail
        closed." 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6: "Eligibility record maximum
        age | 30 days" -- the record is ${var.eligibility_max_age_days}-day bounded and the
        supplied record is missing or exceeds that age. This is NOT waivable.
      EOT
    }

    # G11: two distinct approver identities with exact-diff review.
    precondition {
      condition = local.dual_control_distinct
      error_message = <<-EOT
        DENY: dual control is not satisfied by two DISTINCT authorized identities reviewing the
        exact diff. 11_EXECUTION_GATES.md G11: "required dual-control approvals come from two
        distinct authorized identities". 06_SECURITY_AND_ACCESS_CONTROL.md §3: "The initiating
        actor and approver must be distinct identities." 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md
        §6: "Same identity cannot satisfy both approvals; any diff invalidates approval."
        Step-up freshness must also be at most 5 minutes
        (06_SECURITY_AND_ACCESS_CONTROL.md §2). This is NOT waivable.
      EOT
    }

    # G11: no material reconciliation break, no UNKNOWN order, no active halt.
    precondition {
      condition = local.operational_clean
      error_message = <<-EOT
        DENY: live-blocking operational state detected. 11_EXECUTION_GATES.md G11 requires "no
        material reconciliation break, stale market data, unresolved UNKNOWN order, or active
        halt exists". 04_TRADING_DOMAIN_AND_RISK.md §Order lifecycle: UNKNOWN "is mandatory when
        a submission timeout prevents determination of venue outcome". 18_GOVERNANCE_DATA_AND_
        COMPLIANCE.md §5: "A material unresolved break blocks affected risk-increasing actions."
        10_OPERATIONS_AND_DISASTER_RECOVERY.md: "If venue state cannot be established, orders
        with uncertain outcome remain UNKNOWN; the system must not retry an exposure-increasing
        command." This is NOT waivable.
      EOT
    }

    # G11: live credentials isolated, withdrawal/transfer disabled.
    precondition {
      condition = local.credentials_isolated
      error_message = <<-EOT
        DENY: live credentials are not isolated or still carry prohibited permissions. 11_
        EXECUTION_GATES.md G11: "live credentials are provisioned only in isolated live
        infrastructure with withdrawal/transfer permissions disabled". 06_SECURITY_AND_ACCESS_
        CONTROL.md §6: "withdrawal/transfer permissions are prohibited unless separately
        justified and approved, and are disabled by default." 01_SYSTEM_ARCHITECTURE.md §4:
        "Live credentials are never available to lower environments." 11: live credential
        isolation is NOT waivable.
      EOT
    }

    # G11: owner authorization against exact configuration/artifact digests.
    precondition {
      condition = local.owner_authorized
      error_message = <<-EOT
        DENY: owner authorization is not recorded against valid, immutable configuration and
        artifact digests. 11_EXECUTION_GATES.md G11: "owner authorization is recorded against
        the exact configuration/artifact digests". 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_
        RELEASE_PROFILE.md §8 item 9 requires "commit and artifact digests" in the gate report.
        Both digests must be well-formed sha256 values and the evidence must be immutable.
      EOT
    }

    # G11: canary scope, abort thresholds, responsible operator.
    precondition {
      condition = local.canary_defined
      error_message = <<-EOT
        DENY: canary scope, abort thresholds, or the responsible operator are not confirmed. 11_
        EXECUTION_GATES.md G11: "a canary activation runs in the narrowest permitted scope;
        abort thresholds and the responsible operator are confirmed". 23_ARCHITECTURE_AND_
        COMPLIANCE_DECISIONS.md §7: G11 "requires ... a narrow canary with explicit abort
        criteria."
      EOT
    }
  }
}
