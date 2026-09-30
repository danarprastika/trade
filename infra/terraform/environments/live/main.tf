# live environment - separate deployable root module.
#
# ############################################################################
# # THIS ENVIRONMENT IS STRUCTURALLY GATED.                                    #
# #                                                                          #
# # `terraform plan` for live FAILS unless every G11 precondition in           #
# # modules/live_gate is satisfied by supplied evidence. 11_EXECUTION_GATES.md #
# # G11: "Failure of any condition leaves live mode disabled."                #
# # 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §3         #
# # invariant 7: "Live capability is disabled by default and cannot be        #
# # activated without exact-scope legal eligibility, verified adult           #
# # account-holder authority, dual approval, and passing release gates."      #
# #                                                                          #
# # Every *_evidence variable below DEFAULTS TO NULL. A null value fails the  #
# # corresponding precondition. There is no partial-pass path:               #
# # 11_EXECUTION_GATES.md: "A gate is PASS only when every listed criterion   #
# # is satisfied; partial completion is FAIL, not a percentage."              #
# ############################################################################
#
# TRACEABILITY
#   01_SYSTEM_ARCHITECTURE.md §4: "Each environment has separate credentials,
#     database, encryption keys, deployment identity, network policy, and
#     configuration namespace. Live credentials are never available to lower
#     environments."
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: "Dev/test/staging/paper/shadow/live
#     have separate accounts/projects, network policies, databases, keys, service
#     identities, and venue credentials. Live secrets are not readable by lower
#     environments. Production administration uses a dedicated identity and
#     audited just-in-time access. Live deployment requires protected approval and
#     signed immutable artifacts."
#   09_TESTING_AND_RELEASE_EVIDENCE.md §Release strategy: "Promotion is dev ->
#     test -> staging -> paper -> shadow -> live." Live is step 6 of 6.
#   11_EXECUTION_GATES.md G11: full precondition list (G10 passed; legally
#     eligible adult account holder; current exact-scope eligibility; isolated
#     live credentials with withdrawal/transfer disabled; reviewed risk limits;
#     no material reconciliation break / stale market data / UNKNOWN order /
#     active halt; two distinct approvers; owner authorization against exact
#     config/artifact digests; narrow canary; confirmed abort thresholds and
#     operator; immutable evidence).

terraform {
  required_version = "= 1.9.8"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 6.14.0"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "= 6.14.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "= 2.36.0"
    }
  }

  # PARTIAL CONFIGURATION. The bucket and prefix are supplied at init time:
  #   terraform init \
  #     -backend-config="bucket=<live-state-bucket>" \
  #     -backend-config="prefix=environments/live"
  #
  # 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: a separate state backend per
  # environment. Live state is additionally expected to live in an account with
  # stricter IAM than the lower environments.
  backend "gcs" {}
}

# Live provider credentials. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: "Production
# administration uses a dedicated identity and audited just-in-time access."
# 06_SECURITY_AND_ACCESS_CONTROL.md §4: unique workload identity bound to
# deployment and environment. The value is a PATH to a short-lived JIT credential
# file, never key material in source: 23 §2 prohibits secrets in source, prompts,
# logs, or artifacts.
provider "google" {
  project     = var.project_id
  region      = var.primary_region
  credentials = var.provider_credentials_path
}

provider "google-beta" {
  project     = var.project_id
  region      = var.primary_region
  credentials = var.provider_credentials_path
}

provider "kubernetes" {
  host                   = var.k8s_endpoint
  token                  = var.k8s_token
  cluster_ca_certificate = base64decode(var.k8s_cluster_ca)
}

module "environment" {
  source = "../../modules/environment"

  environment_name = "live"
  project_id       = var.project_id
  config_namespace = var.config_namespace
  kms_key_name     = var.kms_key_name

  # LIVE IS STRUCTURALLY ISOLATED FROM LOWER ENVIRONMENTS.
  # This MUST remain empty. modules/environment/main.tf carries a precondition
  # that FAILS the plan if it is not. 01_SYSTEM_ARCHITECTURE.md §4: "Live
  # credentials are never available to lower environments." 19 §2: "Live secrets
  # are not readable by lower environments." The converse also holds: live must be
  # structurally unable to read lower-environment secrets.
  lower_environment_project_ids = []

  primary_region       = var.primary_region
  primary_region_zones = var.primary_region_zones
  recovery_region      = var.recovery_region
  network_cidr_base    = var.network_cidr_base
  otlp_endpoint        = var.otlp_endpoint

  # Full production sizing floor. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3.
  db_vcpu_per_node       = 8  # 19 §3: each at least 8 vCPU
  db_memory_gib_per_node = 32 # 19 §3: each at least 32 GiB RAM
  db_data_disk_size_gb   = 1024 # 19 §3: 1 TiB encrypted SSD
  db_tier                = "db-custom-8-32768"
  db_max_connections     = 500
  db_deletion_protection = true

  recovery_region_enabled = true # 19 §1: encrypted WAL/backups to a recovery region

  api_replicas        = 3 # 19 §3: "API replicas have a minimum of three in production"
  dispatcher_replicas = 2 # 19 §3: "two event-dispatcher replicas (2 vCPU / 4 GiB each)"
  research_replicas   = 2 # 19 §3: separate quota-controlled pool

  api_image                 = var.api_image
  dispatcher_image          = var.dispatcher_image
  research_image            = var.research_image
  adapter_images            = var.adapter_images # one per ENABLED venue (19 §3)
  api_artifact_digest       = var.api_artifact_digest
  dispatcher_artifact_digest = var.dispatcher_artifact_digest
  config_fingerprint        = var.config_fingerprint

  # 17_CONFIGURATION_AND_RISK_POLICY.md §1: "Live configuration cannot be copied
  # automatically from lower environments." The config_fingerprint supplied here
  # is the live release snapshot, not a promotion of a lower-environment file.
  nats_enabled       = var.nats_enabled       # gated; default false
  nats_gate_evidence = var.nats_gate_evidence # null unless 23 §6 evidence exists

  # CACHE-ONLY, and false by default even in live.
  # 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Redis is optional cache-only
  # infrastructure and must not be a dependency for risk authorization or order
  # correctness." 12_DECISION_REGISTER.md ADR-007: "Redis is cache only."
  redis_enabled = false

  # VENUE EGRESS: live is the ONLY environment permitted to carry venue endpoint
  # allowlist entries. 06_SECURITY_AND_ACCESS_CONTROL.md §1: "Egress is
  # deny-by-default and allowlisted by workload, destination, protocol, and
  # environment." 11_EXECUTION_GATES.md G11: live credentials are provisioned only
  # in isolated live infrastructure.
  #
  # The allowlist is explicit: no wildcard destinations (validation in
  # modules/network rejects any `*`), protocol must be https, and
  # withdrawal_enabled must be false (06 §6: "withdrawal/transfer permissions are
  # prohibited ... and are disabled by default").
  venue_egress_allowed   = true
  venue_egress_allowlist = var.venue_egress_allowlist

  # --- G11 live-eligibility evidence. ALL DEFAULT TO NULL. ---
  # Each null fails its precondition in modules/live_gate.
  live_g10_gate_report     = var.g10_gate_report
  live_account_holder      = var.account_holder
  live_eligibility_record  = var.eligibility_record
  live_dual_control        = var.dual_control
  live_operational_state   = var.operational_state
  live_credentials         = var.live_credentials
  live_owner_authorization = var.owner_authorization
  live_canary              = var.canary
}
