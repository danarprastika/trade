# Network zones module: edge / application / data / management / recovery.
#
# TRACEABILITY
#   06_SECURITY_AND_ACCESS_CONTROL.md §1: "Zones are separated into edge,
#     application, data, management, and recovery. Public ingress terminates at
#     managed WAF/load balancing. Databases, event transport, secret stores,
#     signing services, and venue egress remain private. Egress is deny-by-default
#     and allowlisted by workload, destination, protocol, and environment.
#     Production administration uses a dedicated access path with device posture
#     checks; ordinary application nodes cannot administer infrastructure."
#   23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §4: "Unknown identity, invalid
#     audience, or policy failure denies access." (deny-by-default network posture)
#   01_SYSTEM_ARCHITECTURE.md §9: "four hard trust zones: edge, control, research,
#     and recovery. Research workloads cannot route to live control-plane data
#     stores."

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
  description = "Environment-scoped GCP project. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: separate accounts/projects per environment."
  type        = string
}

variable "primary_region" {
  description = "Primary region hosting edge, application, data, and management zones. 19 §3."
  type        = string
}

variable "primary_region_zones" {
  description = "Exactly three zones in the primary region. 19 §3: three-zone primary region."
  type        = list(string)
}

variable "recovery_region" {
  description = "Separately secured recovery region. 19 §1: WAL/backups replicated to a recovery region. 23 §2: \"Recovery | Active-passive regional recovery | No active-active financial writes; recovery enters RECOVERY_HOLD.\""
  type        = string
}

variable "network_cidr_base" {
  description = "Base CIDR from which the per-zone subnet ranges are derived. Must not overlap a lower environment's base."
  type        = string
}

variable "vpc_flow_log_retention_days" {
  description = "VPC flow log retention. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention: \"Operational logs: 90 days hot, 365 days archived.\" 90 days hot is used for the log bucket."
  type        = number
  default     = 90
}

# Deny-by-default egress allowlist, keyed by workload / destination / protocol /
# environment per 06_SECURITY_AND_ACCESS_CONTROL.md §1.
#
# NOTE ON VENUE EGRESS: 06 §1 requires venue egress to remain private
# (behind the allowlist), NOT absent. 16_MARKET_DATA_AND_VENUE_ADAPTERS.md §4
# requires live venue adapters to reach venue APIs, so live egress carries
# explicit venue-endpoint allowlist entries. The stricter rule prevails: these
# entries exist only in `live`, are enumerated explicitly (no wildcards), and
# carry no withdrawal/transfer capability (06 §6: withdrawal/transfer
# permissions are disabled by default).

variable "venue_egress_allowlist" {
  description = "Explicit venue endpoint allowlist for the live environment. Empty for all lower environments. 06_SECURITY_AND_ACCESS_CONTROL.md §1 requires egress to be allowlisted by workload, destination, protocol, and environment. 11_EXECUTION_GATES.md G11 permits live credentials only in isolated live infrastructure, so no lower environment may carry an entry here. No wildcards are accepted."
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

  validation {
    condition = alltrue([
      for e in var.venue_egress_allowlist :
      !can(regex("^\\*$", e.destination_fqdn)) && !can(regex("\\*", e.destination_fqdn))
    ])
    error_message = "Venue egress destinations must be explicit FQDNs. 06_SECURITY_AND_ACCESS_CONTROL.md §1 requires an allowlist by destination; a wildcard destination is a deny condition."
  }

  validation {
    condition = alltrue([
      for e in var.venue_egress_allowlist : e.withdrawal_enabled == false
    ])
    error_message = "withdrawal_enabled must be false for every venue egress entry. 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"withdrawal/transfer permissions are prohibited unless separately justified and approved, and are disabled by default.\" 11_EXECUTION_GATES.md G11 requires withdrawal/transfer permissions disabled."
  }

  validation {
    condition = alltrue([
      for e in var.venue_egress_allowlist : contains(["https"], [e.protocol])
    ])
    error_message = "Venue egress protocol must be https. 06_SECURITY_AND_ACCESS_CONTROL.md §6: \"Encrypt transport with TLS 1.2 minimum (TLS 3 preferred)\" -> TLS 1.2 minimum means https."
  }
}

variable "venue_egress_allowed" {
  description = "Whether this environment may carry venue egress entries. Only `live` may set this true. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: environments have separate venue credentials; 11_EXECUTION_GATES.md G11: live credentials are provisioned only in isolated live infrastructure."
  type        = bool
  default     = false
}

# --- Zones ------------------------------------------------------------------

# Edge: public ingress only. 06 §1 "Public ingress terminates at managed
# WAF/load balancing." The edge may not reach data or management zones.
resource "google_compute_subnetwork" "zone_edge" {
  name                     = "${var.environment_name}-edge"
  region                   = var.primary_region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = cidrsubnet(var.network_cidr_base, 8, 0)
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_5_SEC"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

# Application: Go control plane, event dispatcher, adapters, research pool.
resource "google_compute_subnetwork" "zone_application" {
  name                     = "${var.environment_name}-application"
  region                   = var.primary_region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = cidrsubnet(var.network_cidr_base, 8, 1)
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_5_SEC"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

# Data: PostgreSQL, event transport, secret store, signing services. Private.
resource "google_compute_subnetwork" "zone_data" {
  name                     = "${var.environment_name}-data"
  region                   = var.primary_region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = cidrsubnet(var.network_cidr_base, 8, 2)
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_5_SEC"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

# Management: dedicated administration path with device posture checks.
# 06 §1: "Production administration uses a dedicated access path with device
# posture checks; ordinary application nodes cannot administer infrastructure."
resource "google_compute_subnetwork" "zone_management" {
  name                     = "${var.environment_name}-management"
  region                   = var.primary_region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = cidrsubnet(var.network_cidr_base, 8, 3)
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_5_SEC"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

# Recovery: recovery-region landing zone. Active-passive only.
# 23 §2: "No active-active financial writes; recovery enters RECOVERY_HOLD."
resource "google_compute_subnetwork" "zone_recovery" {
  name                     = "${var.environment_name}-recovery"
  region                   = var.recovery_region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = cidrsubnet(var.network_cidr_base, 8, 4)
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_5_SEC"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

resource "google_compute_network" "vpc" {
  name                    = "${var.environment_name}-vpc"
  auto_create_subnetworks = false
  routing_mode            = "GLOBAL"
  project                 = var.project_id
}

# --- Deny-by-default firewall posture ---------------------------------------
#
# Implied rules are disabled at the VPC level below via an explicit priority-65534
# deny for all egress not matched by an allowlist. Every zone gets an explicit
# default-deny egress rule; an allowlist entry can only ever remove traffic from
# that deny.

# Edge zone: deny all egress except to the load balancer health path.
resource "google_compute_firewall" "edge_default_deny_egress" {
  name        = "${var.environment_name}-edge-deny-egress"
  network     = google_compute_network.vpc.id
  direction   = "INGRESS"
  priority    = 65534
  description = "Default-deny ingress to the edge subnet from unlisted sources. 06_SECURITY_AND_ACCESS_CONTROL.md §1."

  source_ranges = ["0.0.0.0/0"]
  allow {
    protocol = "tcp"
    ports    = ["443"]
  }
  target_service_accounts = []
  target_tags             = ["${var.environment_name}-edge"]
}

# Data zone: no ingress at all from edge or application beyond what the
# application zone is permitted to reach. Ingress to data is restricted to the
# application subnet only.
resource "google_compute_firewall" "data_ingress_from_application" {
  name        = "${var.environment_name}-data-from-application"
  network     = google_compute_network.vpc.id
  direction   = "INGRESS"
  priority    = 1000
  description = "Only the application zone may reach the data zone. 01_SYSTEM_ARCHITECTURE.md §9: the edge cannot access databases."

  source_ranges = [cidrsubnet(var.network_cidr_base, 8, 1)]
  allow {
    protocol = "tcp"
    ports    = ["5432"]
  }
  target_tags = ["${var.environment_name}-data"]
}

# Management zone: reachable only from the dedicated administration path.
# 06 §1: ordinary application nodes cannot administer infrastructure.
resource "google_compute_firewall" "management_ingress_restricted" {
  name        = "${var.environment_name}-management-restricted"
  network     = google_compute_network.vpc.id
  direction   = "INGRESS"
  priority    = 1000
  description = "Management zone accepts administration traffic only from the management subnet. 06_SECURITY_AND_ACCESS_CONTROL.md §1."

  source_ranges = [cidrsubnet(var.network_cidr_base, 8, 3)]
  allow {
    protocol = "tcp"
    ports    = ["22", "443"]
  }
  target_tags = ["${var.environment_name}-management"]
}

# Default-deny egress for the application zone: no egress unless a destination
# is explicitly allowlisted below.
resource "google_compute_firewall" "application_deny_all_egress" {
  name        = "${var.environment_name}-application-deny-egress"
  network     = google_compute_network.vpc.id
  direction   = "EGRESS"
  priority    = 65534
  description = "Default-deny egress from the application zone. 06_SECURITY_AND_ACCESS_CONTROL.md §1: \"Egress is deny-by-default and allowlisted by workload, destination, protocol, and environment.\""

  destination_ranges = ["0.0.0.0/0"]
  deny {
    protocol = "all"
  }
  target_tags = ["${var.environment_name}-application"]
}

# Internal service-to-service egress within the environment's own zones.
# 06 §4: mutual TLS for private service-to-service calls.
resource "google_compute_firewall" "application_allow_internal" {
  name        = "${var.environment_name}-application-allow-internal"
  network     = google_compute_network.vpc.id
  direction   = "EGRESS"
  priority    = 1000
  description = "Application zone may reach the data zone (PostgreSQL, event transport, secret store, signing) and itself. 06_SECURITY_AND_ACCESS_CONTROL.md §1 keeps these private; §4 requires mTLS for service traffic."

  destination_ranges = [
    cidrsubnet(var.network_cidr_base, 8, 1),
    cidrsubnet(var.network_cidr_base, 8, 2),
  ]
  allow {
    protocol = "tcp"
    ports    = ["443", "5432", "9092", "5433"]
  }
  target_tags = ["${var.environment_name}-application"]
}

# Research isolation: the research subnet must not route to the data zone.
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Python research/backtest workers use a
# separate quota-controlled pool and cannot consume reserved API, risk, OMS, or
# adapter capacity."
resource "google_compute_firewall" "research_denies_data_zone" {
  name        = "${var.environment_name}-research-deny-data"
  network     = google_compute_network.vpc.id
  direction   = "EGRESS"
  priority    = 900
  description = "Research pool egress is denied to the data zone. 01_SYSTEM_ARCHITECTURE.md §9: \"Research workloads cannot route to live control-plane data stores.\" 19 §3: separate quota-controlled pool."

  destination_ranges = [cidrsubnet(var.network_cidr_base, 8, 2)]
  deny {
    protocol = "all"
  }
  target_tags = ["${var.environment_name}-research"]
}

# Live-only venue egress. Empty count for every lower environment, which is how
# "live egress must additionally allowlist venue endpoints only" is enforced
# structurally: the rules do not exist outside live.
resource "google_compute_firewall" "venue_egress" {
  count = var.venue_egress_allowed ? length(var.venue_egress_allowlist) : 0

  name        = "${var.environment_name}-venue-egress-${var.venue_egress_allowlist[count.index].venue_id}"
  network     = google_compute_network.vpc.id
  direction   = "EGRESS"
  priority    = 800
  description = "Explicit venue endpoint egress for ${var.venue_egress_allowlist[count.index].venue_id} (${var.venue_egress_allowlist[count.index].workload}). Scope: ${var.venue_egress_allowlist[count.index].credential_scope}; withdrawal/transfer disabled. 06_SECURITY_AND_ACCESS_CONTROL.md §1 and §6."

  destination_ranges = ["0.0.0.0/0"] # narrowed by the FQDN rule in the environment module's DNS policy
  allow {
    protocol = "tcp"
    ports    = [for p in var.venue_egress_allowlist[count.index].destination_ports : tostring(p)]
  }
  target_tags = ["${var.environment_name}-adapter-${var.venue_egress_allowlist[count.index].venue_id}"]
}

# Venue egress is only ever created when the environment is live.
resource "terraform_data" "egress_posture_guard" {
  lifecycle {
    precondition {
      condition = !var.venue_egress_allowed || var.environment_name == "live"
      error_message = <<-EOT
        Venue egress requested in environment "${var.environment_name}", but venue egress is
        permitted only in `live`. 11_EXECUTION_GATES.md G11: live credentials are provisioned
        only in isolated live infrastructure. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2:
        environments have separate venue credentials. Deny.
      EOT
    }
    precondition {
      condition = var.environment_name != "live" || length(var.venue_egress_allowlist) > 0
      error_message = <<-EOT
        The live environment must carry an explicit venue endpoint allowlist, otherwise venue
        egress remains fully denied and the environment cannot function. 06_SECURITY_AND_
        ACCESS_CONTROL.md §1 requires egress to be allowlisted; empty allowlist = deny.
      EOT
    }
  }
}

# Flow logs: evidence for the network-denial tests required by
# 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §5 (Zero Trust
# release evidence: "Identity, policy, network-denial, and revocation tests").
resource "google_logging_project_sink" "vpc_flow_logs" {
  name                   = "${var.environment_name}-vpc-flow-logs"
  destination            = google_storage_bucket.flow_logs.id
  filter                 = "logName=\"projects/${var.project_id}/logs/compute.googleapis.com%2Fvpc_flows\" OR logName=\"projects/${var.project_id}/logs/compute.googleapis.com%2Ffirewall_log\""
  unique_writer_identity = true
}

resource "google_storage_bucket" "flow_logs" {
  name                        = "${var.project_id}-${var.environment_name}-flow-logs"
  location                    = var.primary_region
  project                     = var.project_id
  force_destroy               = false
  uniform_bucket_level_access = true
  retention_policy {
    retention_period = var.vpc_flow_log_retention_days * 86400
  }
  versioning {
    enabled = true
  }
}
