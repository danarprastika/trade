# Backup module outputs.

output "wal_archive_bucket" {
  description = "Cross-region encrypted WAL and backup archive. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1; 10_OPERATIONS_AND_DISASTER_RECOVERY.md."
  value       = google_storage_bucket.backup_archive.id
}

output "wal_archive_location" {
  description = "Region holding the archive. Differs from the primary region (validated)."
  value       = google_storage_bucket.backup_archive.location
}

output "restore_verification_bucket" {
  description = "Bucket holding restore-verification evidence. 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Drill schedule: monthly backup restore verification."
  value       = google_storage_bucket.restore_verification.id
}

output "retention_schedule" {
  description = "Applied retention schedule for release evidence. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup and §Data retention."
  value = {
    daily_restore_points   = var.daily_restore_points
    monthly_restore_points = var.monthly_restore_points
    audit_retention_years  = var.audit_retention_years
  }
}

output "recovery_objectives" {
  description = "Bound recovery objectives, evidence-required. 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives; 12_DECISION_REGISTER.md ADR-025: the platform must not claim these objectives until drills demonstrate them."
  value = {
    rto_minutes = var.rto_minutes
    rpo_minutes = var.rpo_minutes
    claim_status = "unproven-until-quarterly-drill-evidence"
  }
}
