variable "region" {
  description = "AWS region."
  type        = string
}

variable "name" {
  description = "Resource prefix, e.g. quantos-dev."
  type        = string
}

variable "env" {
  description = "Environment name. Appears in tags, log group paths and the Prometheus external labels."
  type        = string

  validation {
    condition     = contains(["dev", "prod"], var.env)
    error_message = "env must be dev or prod: those are the two overlays in infra/kubernetes/overlays."
  }
}

variable "vpc_cidr" {
  description = "VPC CIDR."
  type        = string
}

variable "azs" {
  description = "Availability zones."
  type        = list(string)
}

variable "single_nat_gateway" {
  description = "One NAT gateway for the whole VPC instead of one per AZ."
  type        = bool
}

variable "kubernetes_version" {
  description = "EKS control-plane version."
  type        = string
}

variable "cluster_public_access_cidrs" {
  description = "CIDRs allowed to reach the public API server endpoint. Empty means private-only."
  type        = list(string)
}

variable "node_groups" {
  description = "Managed node groups. See modules/eks/variables.tf for the shape."

  type = map(object({
    instance_types = list(string)
    capacity_type  = string
    min_size       = number
    max_size       = number
    desired_size   = number
    disk_size_gb   = number
    labels         = map(string)
    taints = list(object({
      key    = string
      value  = string
      effect = string
    }))
  }))
}

variable "postgres_instance_class" {
  type        = string
  description = "RDS instance class."
}

variable "postgres_multi_az" {
  type        = bool
  description = "Synchronous standby in a second AZ."
}

variable "postgres_allocated_storage_gb" {
  type        = number
  description = "Initial gp3 volume size."
}

variable "postgres_max_allocated_storage_gb" {
  type        = number
  description = "Storage autoscaling ceiling."
}

variable "postgres_backup_retention_days" {
  type        = number
  description = "Automated backup retention."
}

variable "postgres_deletion_protection" {
  type        = bool
  description = "Refuse terraform destroy on the database."
}

variable "redis_node_type" {
  type        = string
  description = "ElastiCache node type."
}

variable "redis_replicas_per_node_group" {
  type        = number
  description = "Read replicas per shard."
}

variable "msk_serverless" {
  type        = bool
  description = "MSK Serverless instead of provisioned brokers."
}

variable "msk_broker_instance_type" {
  type        = string
  description = "Provisioned broker instance type. Ignored when msk_serverless is true."
  default     = "kafka.m7g.large"
}

variable "msk_broker_count" {
  type        = number
  description = "Provisioned broker count. Must be a multiple of the AZ count."
  default     = 3
}

variable "msk_broker_storage_gb" {
  type        = number
  description = "EBS per provisioned broker."
  default     = 200
}

variable "log_retention_days" {
  type        = number
  description = "Application log retention."
}

variable "s3_force_destroy" {
  type        = bool
  description = "Allow terraform destroy to empty non-empty buckets."
}

variable "namespace" {
  type        = string
  description = "Kubernetes namespace the workloads run in. Must match infra/kubernetes/base/namespace.yaml."
  default     = "quantos"
}
