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
  description = "VPC to place the cluster in."
}

variable "private_subnet_ids" {
  description = <<-EOT
    Private subnets for the broker ENIs. MSK Serverless accepts up to three and
    provisioned MSK requires the broker count to be a multiple of the subnet
    count, so this list is what determines placement in both modes.
  EOT
  type        = list(string)
}

variable "client_security_group_ids" {
  description = "Security groups allowed to reach the brokers. The EKS node group, in practice."
  type        = list(string)
}

variable "serverless" {
  description = <<-EOT
    Use MSK Serverless instead of provisioned brokers.

    Serverless is the dev default: it has no idle broker cost and it sizes
    partitions itself, which suits an environment that is busy for an hour a day.
    Prod runs provisioned, because the partition counts in ADR-002 are chosen
    deliberately and because sustained throughput is cheaper on reserved brokers
    than on per-partition-hour pricing.
  EOT
  type        = bool
  default     = true
}

variable "kafka_version" {
  description = "Broker version for the provisioned cluster. Matches the compose stack's 3.7 line."
  type        = string
  default     = "3.7.x"
}

variable "broker_instance_type" {
  description = "Provisioned broker instance type."
  type        = string
  default     = "kafka.m7g.large"
}

variable "broker_count" {
  description = <<-EOT
    Total brokers. Must be a multiple of the subnet count. Three across three
    AZs is the minimum that supports replication factor 3 with
    min.insync.replicas 2, which is what makes an acknowledged write survive an
    AZ loss.
  EOT
  type        = number
  default     = 3
}

variable "broker_storage_gb" {
  description = "EBS per broker. Sized from the ADR-002 retention table, not guessed."
  type        = number
  default     = 200
}

variable "log_retention_days" {
  description = "CloudWatch retention for broker logs."
  type        = number
  default     = 14
}

variable "log_kms_key_arn" {
  description = <<-EOT
    KMS key for the broker log group. Use the one from modules/observability:
    its key policy already grants the regional CloudWatch Logs principal, which
    is a requirement CloudWatch enforces at CreateLogGroup.
  EOT
  type        = string
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags merged onto every resource."
}
