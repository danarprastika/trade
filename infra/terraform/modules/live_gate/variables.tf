# Live gate module — variables only (all variables are declared in main.tf
# alongside the preconditions they feed, so each precondition can be read together
# with the evidence field it requires).
#
# Every variable in this module has a `description` and a `type`; the invariant
# test in infra/tests/test_terraform_invariants.py enforces that rule across the
# whole of infra/terraform.
