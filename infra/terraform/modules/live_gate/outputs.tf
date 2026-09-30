# Live gate module outputs.

output "live_capability_state" {
  description = "Result of the evaluated G11 preconditions. `ACTIVATABLE` only when every precondition passes; otherwise `DISABLED`. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §3 invariant 7: \"Live capability is disabled by default and cannot be activated without exact-scope legal eligibility, verified adult account-holder authority, dual approval, and passing release gates.\""
  value       = local.all_preconditions_satisfied ? "ACTIVATABLE" : "DISABLED"
}

output "gate_evaluation" {
  description = "Per-precondition evaluation recorded as release evidence. 11_EXECUTION_GATES.md: \"A gate is PASS only when every listed criterion is satisfied; partial completion is FAIL, not a percentage.\" 25 §8 item 9 requires binary PASS/FAIL with evidence references and reviewer identities."
  value = {
    g10_passed              = local.g10_passed
    adult_and_eligible      = local.adult_and_eligible
    eligibility_current     = local.eligibility_present_and_current
    dual_control_distinct   = local.dual_control_distinct
    operational_state_clean = local.operational_clean
    credentials_isolated    = local.credentials_isolated
    owner_authorized        = local.owner_authorized
    canary_defined          = local.canary_defined
    overall                 = local.all_preconditions_satisfied ? "PASS" : "FAIL"
  }
}

output "non_waivable_conditions" {
  description = "G11 conditions that 11_EXECUTION_GATES.md prohibits waiving: financial invariants, authorization, live credential isolation, reconciliation, halt controls, recovery objectives."
  value = [
    "adult_and_eligible",
    "eligibility_current",
    "dual_control_distinct",
    "operational_state_clean",
    "credentials_isolated",
  ]
}
