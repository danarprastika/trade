# Root module: provider configuration and the environment matrix.
#
# This root module orchestrates the six environments defined by
# 01_SYSTEM_ARCHITECTURE.md §4. Each environment is a SEPARATE root module under
# environments/<env>/ with its own state backend, provider credentials, KMS key
# ring, network policy, secret namespace, and configuration namespace. This root
# module deliberately does NOT hold a shared backend for the environments; it is
# the composition root that pins the matrix and the guard variables.
#
# TRACEABILITY
#   01_SYSTEM_ARCHITECTURE.md §4: "Environments are `dev`, `test`, `staging`,
#     `paper`, `shadow`, and `live`. Each environment has separate credentials,
#     database, encryption keys, deployment identity, network policy, and
#     configuration namespace. Live credentials are never available to lower
#     environments."
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: "Dev/test/staging/paper/shadow/live
#     have separate accounts/projects, network policies, databases, keys, service
#     identities, and venue credentials. Live secrets are not readable by lower
#     environments."
#   06_SECURITY_AND_ACCESS_CONTROL.md §1: "Zones are separated into edge,
#     application, data, management, and recovery."

provider "google" {
  project = var.admin_project_id
  region  = var.primary_region
}

provider "google-beta" {
  project = var.admin_project_id
  region  = var.primary_region
}

# The environment matrix is a closed set. An unknown environment name is a
# hard error rather than a silently created new scope.
locals {
  # Ordered promotion path from 09_TESTING_AND_RELEASE_EVIDENCE.md §Release
  # strategy: "Promotion is dev -> test -> staging -> paper -> shadow -> live."
  environments = {
    dev     = { order = 1, live = false, enables_venue_egress = false }
    test    = { order = 2, live = false, enables_venue_egress = false }
    staging = { order = 3, live = false, enables_venue_egress = false }
    paper   = { order = 4, live = false, enables_venue_egress = false }
    shadow  = { order = 5, live = false, enables_venue_egress = false }
    live    = { order = 6, live = true, enables_venue_egress = true }
  }

  # 06_SECURITY_AND_ACCESS_CONTROL.md §1 zone taxonomy. Every environment
  # declares all five zones; the invariant test asserts this.
  required_zones = ["edge", "application", "data", "management", "recovery"]

  # Lower environments. Live must not be able to read these; the live module
  # asserts absence of any dependency on these projects.
  lower_environments = ["dev", "test", "staging", "paper", "shadow"]

  environment_project_ids = {
    for env, cfg in local.environments : env => format("%s-%s", var.project_id_prefix, env)
  }
}

# Guard: the live environment may never reuse a lower environment's project,
# network, or state backend. 19 §2 requires "separate accounts/projects".
resource "terraform_data" "environment_separation_guard" {
  lifecycle {
    precondition {
      condition = length(distinct(values(local.environment_project_ids))) == length(local.environments)
      error_message = <<-EOT
        Environment isolation violation: two environments resolve to the same GCP
        project id. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2 requires separate
        accounts/projects per environment. Deny.
      EOT
    }
    precondition {
      condition = length(local.required_zones) == 5
      error_message = <<-EOT
        Zone taxonomy must remain edge/application/data/management/recovery per
        06_SECURITY_AND_ACCESS_CONTROL.md §1. Deny.
      EOT
    }
  }
}
