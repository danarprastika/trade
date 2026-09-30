# Terraform CLI and provider version pinning.
#
# TRACEABILITY
#   23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §7: "Language/runtime versions are
#     selected at repository bootstrap from stable, security-supported releases,
#     pinned exactly, and recorded in release evidence. End-of-life runtimes are
#     prohibited. ... CI rejects floating dependencies and toolchain drift."
#   02_POLYGLOT_ENGINEERING_STANDARD.md §Dependency policy: "CI rejects floating
#     dependency versions, uncommitted lockfile changes, and toolchain drift."
#   00_README.md §Toolchain lifecycle: "CI must fail on unpinned dependencies or
#     toolchain drift."
#
# CONVENTION: every provider constraint uses the `=` exact-match operator, never
# `~>`, `>=`, or a bare major. infra/tests/test_terraform_invariants.py asserts
# this so a floating version cannot be introduced by a later edit.

terraform {
  # Pinned CLI version; enforced by .terraform-version (tfenv/asdf/tenv) and
  # asserted by the CI drift gate in infra/ci/terraform-plan-gate.yaml.
  required_version = "= 1.9.8"

  required_providers {
    # Infrastructure provider (GKE, Cloud SQL, KMS, Artifact Registry, networking).
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "= 6.14.0"
    }
    # Kubernetes workload definitions (non-root, read-only rootfs, seccomp).
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "= 2.36.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "= 3.7.2"
    }
    # Used only for non-authoritative identifiers (KMS key name suffixes, WAL
    # archive object prefixes). Never for financial state.
    time = {
      source  = "hashicorp/time"
      version = "= 0.13.1"
    }
  }
}
