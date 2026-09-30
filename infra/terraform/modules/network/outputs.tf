# Network module outputs.

output "vpc_id" {
  description = "Environment VPC id for the zone set edge/application/data/management/recovery (06_SECURITY_AND_ACCESS_CONTROL.md §1)."
  value       = google_compute_network.vpc.id
}

output "zone_subnet_ids" {
  description = "Subnet ids keyed by zone name. 06_SECURITY_AND_ACCESS_CONTROL.md §1 zone taxonomy."
  value = {
    edge        = google_compute_subnetwork.zone_edge.id
    application = google_compute_subnetwork.zone_application.id
    data        = google_compute_subnetwork.zone_data.id
    management  = google_compute_subnetwork.zone_management.id
    recovery    = google_compute_subnetwork.zone_recovery.id
  }
}

output "zone_names" {
  description = "The five declared zones, used by the infra invariant test to assert every environment declares complete zone separation."
  value       = ["edge", "application", "data", "management", "recovery"]
}

output "venue_egress_rule_names" {
  description = "Venue egress rules actually created. Empty for every non-live environment; only `live` adds venue endpoints. 06_SECURITY_AND_ACCESS_CONTROL.md §1."
  value       = [for r in google_compute_firewall.venue_egress : r.name]
}

output "flow_log_bucket" {
  description = "Bucket holding firewall and VPC flow logs used as network-denial evidence. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §5."
  value       = google_storage_bucket.flow_logs.id
}
