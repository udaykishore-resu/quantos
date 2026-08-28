variable "name" {
  description = "Resource prefix."
  type        = string
}

variable "env" {
  description = "Environment name (dev|prod)."
  type        = string
}

variable "vpc_id" {
  type        = string
  description = "VPC to place the stores in."
}

variable "private_subnet_ids" {
  description = "Private subnets. Nothing here is ever placed in a public subnet."
  type        = list(string)
}

variable "client_security_group_ids" {
  description = <<-EOT
    Security groups allowed to reach Postgres and Redis. In practice this is the
    EKS node security group and nothing else: reachability is granted by group
    membership rather than by CIDR, so a new subnet does not silently widen
    access.
  EOT
  type        = list(string)
}

# ------------------------------------------------------------- postgres ------

variable "postgres_version" {
  description = "PostgreSQL major.minor. ADR-003 fixes the major at 16."
  type        = string
  default     = "16.4"
}

variable "postgres_instance_class" {
  description = "RDS instance class."
  type        = string
  default     = "db.t4g.medium"
}

variable "postgres_allocated_storage_gb" {
  description = "Initial gp3 volume size."
  type        = number
  default     = 50
}

variable "postgres_max_allocated_storage_gb" {
  description = <<-EOT
    Storage autoscaling ceiling. Postgres holds the provenance chain, and
    App.CanEmitSignals suspends signal emission the moment it becomes
    unwritable - running out of disk is therefore an availability incident for
    the whole platform, not just for the database.
  EOT
  type        = number
  default     = 200
}

variable "postgres_multi_az" {
  description = "Synchronous standby in a second AZ. False in dev, true in prod."
  type        = bool
  default     = false
}

variable "postgres_backup_retention_days" {
  description = "Automated backup retention. Zero disables backups and is never acceptable in prod."
  type        = number
  default     = 7

  validation {
    condition     = var.postgres_backup_retention_days >= 1
    error_message = "Backups may not be disabled: the provenance store has no other recovery path."
  }
}

variable "postgres_deletion_protection" {
  description = "Refuse `terraform destroy` on the database."
  type        = bool
  default     = true
}

variable "postgres_db_name" {
  description = "Initial database name. Must match the DSN the services are given."
  type        = string
  default     = "quantos"
}

variable "postgres_username" {
  description = "Master username. The password is generated, never supplied."
  type        = string
  default     = "quantos"
}

# --------------------------------------------------------------- redis -------

variable "redis_version" {
  description = "ElastiCache engine version. ADR-003 fixes the major at 7."
  type        = string
  default     = "7.1"
}

variable "redis_node_type" {
  description = "ElastiCache node type."
  type        = string
  default     = "cache.t4g.micro"
}

variable "redis_replicas_per_node_group" {
  description = <<-EOT
    Read replicas per shard. Redis is a cache and a coordination primitive, never
    a source of truth (ADR-003), so losing it degrades latency and the dedup set
    rather than losing data - one replica in prod, none in dev.
  EOT
  type        = number
  default     = 0
}

variable "redis_snapshot_retention_days" {
  description = "Snapshot retention. Zero is defensible for a pure cache."
  type        = number
  default     = 0
}

# ------------------------------------------------------------- secrets -------

variable "secret_recovery_window_days" {
  description = <<-EOT
    Secrets Manager recovery window. Zero would allow immediate deletion, which
    turns a fat-fingered destroy into an unrecoverable one.
  EOT
  type        = number
  default     = 7
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags merged onto every resource."
}
