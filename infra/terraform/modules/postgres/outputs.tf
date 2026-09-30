# PostgreSQL module outputs. No secret values are exported.
# 06_SECURITY_AND_ACCESS_CONTROL.md §6: "Secrets, authentication factors, private
# keys, and raw access tokens never enter source, logs, prompts, analytics, test
# fixtures, or client bundles."

output "primary_instance_name" {
  description = "Name of the authoritative PostgreSQL 17 HA primary. 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2: single writer for authoritative financial state."
  value       = google_sql_database_instance.primary.name
}

output "primary_connection_name" {
  description = "Cloud SQL connection name. The application connects over IAM + TLS; no password is present in Terraform state (06 §6)."
  value       = google_sql_database_instance.primary.connection_name
}

output "primary_private_ip" {
  description = "Private IP of the authoritative primary in the data zone. 06 §1: the edge cannot access databases."
  value       = google_sql_database_instance.primary.private_ip_address
}

output "analytics_replica_name" {
  description = "Read replica for explicitly stale-tolerant analytics only. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §4: financial writes remain on the primary."
  value       = google_sql_database_instance.analytics_replica.name
}

output "recovery_standby_names" {
  description = "Recovery-region standbys. Entering these requires Incident Commander and Risk Owner approval and results in RECOVERY_HOLD. 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Recovery topology and failover authority."
  value       = [for r in google_sql_database_instance.recovery_standby : r.name]
}

output "database_name" {
  description = "Authoritative database name."
  value       = google_sql_database.app.name
}

output "app_user_name" {
  description = "Application database user name. The password lives only in the environment-scoped secret store (06_SECURITY_AND_ACCESS_CONTROL.md §6)."
  value       = google_sql_user.app.name
}

output "sizing_floor" {
  description = "Applied resource floor, echoed for release evidence. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  value = {
    vcpu_per_node       = var.vcpu_per_node
    memory_gib_per_node = var.memory_gib_per_node
    data_disk_size_gb   = var.data_disk_size_gb
    ha_topology         = "REGIONAL primary/standby pair"
  }
}
