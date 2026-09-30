# PostgreSQL module variables. Every variable has a description and a type.

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
  description = "Primary region. Synchronous protection stays within this region. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: \"synchronous protection within the primary region\"."
  type        = string
}

variable "recovery_region" {
  description = "Separately secured recovery region for encrypted WAL and backup replication. 19 §1."
  type        = string
}

variable "recovery_region_enabled" {
  description = "Whether to provision the recovery-region standby. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §1: encrypted WAL/backups replicated to a recovery region. 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives: cross-region RTO <= 30 minutes, RPO <= 5 minutes."
  type        = bool
  default     = true
}

variable "vcpu_per_node" {
  description = "vCPU per node in the HA primary/standby pair. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 floor: 8 vCPU. Validation denies anything below the floor."
  type        = number
  default     = 8

  validation {
    condition     = var.vcpu_per_node >= 8
    error_message = "vcpu_per_node must be >= 8. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"each at least 8 vCPU / 32 GiB RAM\"."
  }
}

variable "memory_gib_per_node" {
  description = "Memory (GiB) per node in the HA primary/standby pair. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 floor: 32 GiB. Validation denies anything below the floor."
  type        = number
  default     = 32

  validation {
    condition     = var.memory_gib_per_node >= 32
    error_message = "memory_gib_per_node must be >= 32. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"each at least 8 vCPU / 32 GiB RAM\"."
  }
}

variable "tier" {
  description = "Cloud SQL machine tier derived from the vCPU/memory floor. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3."
  type        = string
  default     = "db-custom-8-32768"

  validation {
    condition     = can(regex("^db-custom-([0-9]+)-([0-9]+)$", var.tier))
    error_message = "tier must be db-custom-<vcpu>-<memory_mib>."
  }
}

variable "data_disk_size_gb" {
  description = "Per-node encrypted SSD size in GiB. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"1 TiB encrypted SSD storage\" per node, so the floor is 1024 GiB."
  type        = number
  default     = 1024

  validation {
    condition     = var.data_disk_size_gb >= 1024
    error_message = "data_disk_size_gb must be >= 1024 (1 TiB). 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: \"at least ... 1 TiB encrypted SSD storage\"."
  }
}

variable "max_connections" {
  description = "Maximum client connections. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3 requires connection pooling; this reviewed value must be benchmarked under the capacity envelope before live activation."
  type        = number
  default     = 500
}

variable "deletion_protection" {
  description = "Deletion protection. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2 requires live databases be separate and protected; 01_SYSTEM_ARCHITECTURE.md §10 invariant 4 forbids a restored environment leaving RECOVERY_HOLD without independent verification."
  type        = bool
  default     = true
}

variable "vpc_id" {
  description = "VPC id hosting the data zone subnet. 06_SECURITY_AND_ACCESS_CONTROL.md §1: databases, event transport, secret stores, and signing services remain private."
  type        = string
}

variable "kms_key_name" {
  description = "Environment-scoped KMS key for storage encryption. 01_SYSTEM_ARCHITECTURE.md §4: separate encryption keys per environment. 19 §3: 1 TiB encrypted SSD. 06 §6: managed envelope encryption."
  type        = string
}

variable "daily_restore_points" {
  description = "Number of retained daily restore points. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: \"35 daily restore points, 12 monthly restore points.\" A value other than 35 is a deny condition."
  type        = number
  default     = 35

  validation {
    condition     = var.daily_restore_points == 35
    error_message = "daily_restore_points must be 35. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: \"35 daily restore points\"."
  }
}

variable "monthly_restore_points" {
  description = "Number of retained monthly restore points. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: \"12 monthly restore points\"."
  type        = number
  default     = 12

  validation {
    condition     = var.monthly_restore_points == 12
    error_message = "monthly_restore_points must be 12. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup: \"12 monthly restore points\"."
  }
}

variable "audit_retention_years" {
  description = "Retention in years for audit and financial records. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention: \"Audit and financial records: 7 years.\" 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §4: seven years for financial, access, approval, and control records."
  type        = number
  default     = 7

  validation {
    condition     = var.audit_retention_years == 7
    error_message = "audit_retention_years must be 7. 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention: \"Audit and financial records: 7 years.\""
  }
}
