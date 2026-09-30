# test environment — separate deployable root module.
#
# TRACEABILITY
#   01_SYSTEM_ARCHITECTURE.md §4: "Each environment has separate credentials,
#     database, encryption keys, deployment identity, network policy, and
#     configuration namespace."
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: "Dev/test/staging/paper/shadow/live
#     have separate accounts/projects, network policies, databases, keys, service
#     identities, and venue credentials. Live secrets are not readable by lower
#     environments."
#   24_ENTERPRISE_RELEASE_STANDARD.md §16: "production credentials in research or
#     development environments" are prohibited.
#
# SEPARATE STATE BACKEND: every environment is initialized with its own
# `-backend-config` pointing at a distinct bucket and key. A shared backend would
# let a lower-environment apply read or destroy live state, which violates 19 §2.
#
# SEPARATE CREDENTIALS: the provider below is authenticated with this
# environment's own service-account key. 06_SECURITY_AND_ACCESS_CONTROL.md §4:
# "Every service and worker has a unique workload identity bound to its deployment
# and environment." 19 §2: "Production administration uses a dedicated identity
# and audited just-in-time access."

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

  # PARTIAL CONFIGURATION. The bucket and prefix are supplied per environment at
  # init time:
  #   terraform init \
  #     -backend-config="bucket=<test-state-bucket>" \
  #     -backend-config="prefix=environments/test"
  # 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2 requires a separate state backend per
  # environment.
  backend "gcs" {}
}

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

  environment_name  = "test"
  project_id        = var.project_id
  config_namespace  = var.config_namespace
  kms_key_name      = var.kms_key_name

  primary_region       = var.primary_region
  primary_region_zones = var.primary_region_zones
  recovery_region      = var.recovery_region
  network_cidr_base    = var.network_cidr_base
  otlp_endpoint        = var.otlp_endpoint

  # Sizing floor. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 states these are the
  # *minimum* production footprint; test intentionally runs at the reduced floor
  # permitted for non-production, but the *validation floors declared in the
  # modules* are NOT relaxed for test — see infra/README.md §Sizing floors and the
  # note that test is not a capacity claim.
  db_vcpu_per_node       = 8
  db_memory_gib_per_node = 32
  db_data_disk_size_gb   = 1024
  db_tier                = "db-custom-8-32768"
  db_max_connections     = 200

  api_replicas        = 3
  dispatcher_replicas = 2
  research_replicas   = 1

  api_image                 = var.api_image
  dispatcher_image          = var.dispatcher_image
  research_image            = var.research_image
  adapter_images            = var.adapter_images
  api_artifact_digest       = var.api_artifact_digest
  dispatcher_artifact_digest = var.dispatcher_artifact_digest
  config_fingerprint        = var.config_fingerprint

  # Optional subsystems stay OFF in test.
  nats_enabled      = false # 19 §3: not required for the initial outbox implementation
  nats_gate_evidence = null
  redis_enabled     = false # 19 §3: cache-only, never a risk or order-correctness dependency

  # No venue egress outside live. 11_EXECUTION_GATES.md G11: live credentials are
  # provisioned only in isolated live infrastructure.
  venue_egress_allowed   = false
  venue_egress_allowlist = []
}
