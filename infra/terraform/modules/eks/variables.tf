variable "name" {
  description = "Cluster name and resource prefix."
  type        = string
}

variable "env" {
  description = "Environment name (dev|prod)."
  type        = string
}

variable "kubernetes_version" {
  description = <<-EOT
    EKS control-plane version. Pinned rather than floating: an unplanned minor
    upgrade rewrites admission behaviour and deprecates API versions the
    manifests in infra/kubernetes still use.
  EOT
  type        = string
  default     = "1.30"
}

variable "vpc_id" {
  type        = string
  description = "VPC to place the cluster in."
}

variable "private_subnet_ids" {
  description = "Subnets for the control-plane ENIs and every node group."
  type        = list(string)
}

variable "public_access_cidrs" {
  description = <<-EOT
    CIDRs allowed to reach the public API server endpoint.

    Left empty the endpoint is private-only, which is the prod posture: kubectl
    then requires a bastion or VPN. Dev opens it to the operator's ranges so the
    cluster is usable without standing up a jump host.
  EOT
  type        = list(string)
  default     = []
}

variable "node_groups" {
  description = <<-EOT
    Managed node groups, keyed by name.

    The default split matches the workload shape rather than being uniform:
    `platform` carries the latency-sensitive decision path, `batch` carries
    backtests and model evaluation which are CPU-hungry, interruptible and must
    never contend with the live path for cores (see services/backtest-service).
  EOT

  type = map(object({
    instance_types = list(string)
    capacity_type  = string # ON_DEMAND | SPOT
    min_size       = number
    max_size       = number
    desired_size   = number
    disk_size_gb   = number
    labels         = map(string)
    taints = list(object({
      key    = string
      value  = string
      effect = string # NO_SCHEDULE | PREFER_NO_SCHEDULE | NO_EXECUTE
    }))
  }))
}

variable "log_retention_days" {
  description = "Retention for the control-plane log group."
  type        = number
  default     = 30
}

variable "enabled_cluster_log_types" {
  description = <<-EOT
    Control-plane logs to ship. `audit` and `authenticator` are the two that
    answer "who did this to the cluster"; without them a compromise
    investigation has nothing to read.
  EOT
  type        = list(string)
  default     = ["api", "audit", "authenticator"]
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags merged onto every resource."
}
