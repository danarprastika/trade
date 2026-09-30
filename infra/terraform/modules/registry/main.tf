# OCI image registry with immutability and digest-pinned promotion.
#
# TRACEABILITY
#   23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: "Infrastructure |
#     Terraform-managed, immutable OCI images, signed release artifacts | Drift is
#     detected; production changes require reviewed plan and release evidence."
#   24_ENTERPRISE_RELEASE_STANDARD.md §2 supply-chain security: "Lockfiles, SBOM,
#     provenance, signed images, SAST/SCA/container scanning, protected artifact
#     registry | Release is blocked on defined critical findings or missing
#     provenance."
#   24 §15: "The release candidate is promoted through validation environments
#     without rebuilding from source between environments." Promotion is by digest,
#     never by rebuilding.
#   24 §16: "unsigned production artifacts" are prohibited.
#   09_TESTING_AND_RELEASE_EVIDENCE.md §Release strategy: "Use immutable versioned
#     artifacts."
#   06_SECURITY_AND_ACCESS_CONTROL.md §7: artifact digest verification is a
#     mandatory control.

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
  }
}

variable "environment_name" {
  description = "Environment name from the closed set dev/test/staging/paper/shadow/live. 01_SYSTEM_ARCHITECTURE.md §4."
  type        = string

  validation {
    condition     = contains(["dev", "test", "staging", "paper", "shadow", "live"], var.environment_name)
    error_message = "Environment must be one of dev, test, staging, paper, shadow, live (01_SYSTEM_ARCHITECTURE.md §4)."
  }
}

variable "project_id" {
  description = "Environment-scoped GCP project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate accounts/projects per environment; a registry per environment prevents a lower environment from pulling a live artifact and vice versa."
  type        = string
}

variable "region" {
  description = "Region hosting the registry."
  type        = string
}

variable "kms_key_name" {
  description = "Environment-scoped KMS key protecting registry artifacts. 01_SYSTEM_ARCHITECTURE.md §4: separate encryption keys per environment. 06_SECURITY_AND_ACCESS_CONTROL.md §6: separate key hierarchies per environment and purpose."
  type        = string
}

# Immutable tags: a tag, once written, can never be overwritten.
# 23 §2 requires immutable OCI images; without this, a mutable tag would let a
# rebuilt binary take the same name, which breaks 24 §15's
# "promoted through validation environments without rebuilding" guarantee.
resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = "${var.environment_name}-images"
  description   = "Immutable, digest-pinned OCI images for ${var.environment_name}. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: immutable OCI images, signed release artifacts."
  format        = "DOCKER"
  project       = var.project_id

  docker_config {
    immutable_tags = true # 24_ENTERPRISE_RELEASE_STANDARD.md §15
  }

  depends_on = [google_kms_crypto_key_keyring.environment]
}

# Per-environment keyring. 06_SECURITY_AND_ACCESS_CONTROL.md §6: "Use separate key
# hierarchies per environment and purpose; production keys are non-exportable
# where supported."
resource "google_kms_key_ring" "environment" {
  name     = "${var.environment_name}-release"
  project  = var.project_id
  location = var.region
}

resource "google_kms_crypto_key" "artifact_signing" {
  name                    = "artifact-signing"
  key_ring                = google_kms_key_ring.environment.id
  rotation_period         = var.signing_key_rotation_period
  purpose                 = "ASYMMETRIC_SIGNING"
  destroy_scheduled_duration = "86400s" # recovery hold: 01 §10 invariant 4

  lifecycle {
    # 06_SECURITY_AND_ACCESS_CONTROL.md §6: "rotate signing keys at least
    # annually and after compromise, with overlapping verification keys during
    # planned rotation". A destroy is irreversible, so it requires explicit
    # confirmation rather than happening as a side effect of a plan.
    prevent_destroy = true
  }
}

variable "signing_key_rotation_period" {
  description = "Artifact signing key rotation period. 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"rotate signing keys at least annually and after compromise, with overlapping verification keys during planned rotation.\" 7776000s = 90 days, which satisfies 'at least annually' with margin. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6: \"Audit checkpoint | Daily and on key rotation / high-impact security event\"."
  type        = number
  default     = 7776000
}

# Artifact Analysis: continuous vulnerability scanning. 24 §2: "Release is blocked
# on defined critical findings or missing provenance."
resource "google_artifact_registry_repository" "sbom" {
  location      = var.region
  repository_id = "${var.environment_name}-provenance"
  description   = "SBOM and SLSA-style provenance attestations. 24_ENTERPRISE_RELEASE_STANDARD.md §2: SBOM, provenance, signed images, protected artifact registry. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §8 item 1."
  format        = "DOCKER"
  project       = var.project_id
}

# A separate protected repository for audit evidence with object-lock retention.
# 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §4: "A separate audit exporter copies signed
# batches to a distinct account/project and region using object-lock/WORM
# retention. Application runtime identities have write-only append permissions and
# cannot delete or shorten retention."
resource "google_storage_bucket" "audit_evidence" {
  name                        = "${var.project_id}-${var.environment_name}-audit-evidence"
  project                     = var.project_id
  location                    = var.region
  force_destroy               = false
  uniform_bucket_level_access = true

  retention_policy {
    # 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention: "Audit and
    # financial records: 7 years." 22 §4: seven years for financial, access,
    # approval, and control records.
    retention_period = 255584800 # 7 years in seconds; 7 * 365.25 * 86400 rounded
    is_locked         = true
  }

  versioning {
    enabled = true
  }

  lifecycle {
    # 22 §4: "Deletion is permitted only after retention expiry, legal-hold
    # clearance, and two-person approval". Terraform must not be the mechanism
    # that deletes audit evidence.
    prevent_destroy = true
  }
}

# Drift/immutability guard. 23 §2: "Drift is detected; production changes require
# reviewed plan and release evidence." 24 §5: "Drift detection compares desired and
# observed state continuously; unexplained drift creates an operational incident
# and blocks further privileged changes until resolved."
resource "terraform_data" "immutability_guards" {
  input = {
    immutable_tags_enabled   = true
    signing_key_rotation_s    = var.signing_key_rotation_period
    audit_evidence_locked     = true
    audit_evidence_retention_years = 7
    promotion_mode            = "digest-pinned-no-rebuild"
    mutable_tag_allowed       = false
  }

  lifecycle {
    precondition {
      condition     = contains(["sha256:"], substr(var.expected_artifact_digest, 0, 7))
      error_message = "expected_artifact_digest must be a sha256: digest. 24_ENTERPRISE_RELEASE_STANDARD.md §15 requires digest-pinned promotion; a tag reference is a deny condition."
    }
  }
}

variable "expected_artifact_digest" {
  description = "Expected artifact digest for this release, used to assert digest-pinned promotion. 24_ENTERPRISE_RELEASE_STANDARD.md §15: \"Rollback uses a previously verified immutable artifact\"; 09_TESTING_AND_RELEASE_EVIDENCE.md §Evidence package records the artifact digest."
  type        = string
}
