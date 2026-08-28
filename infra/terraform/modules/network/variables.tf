variable "name" {
  description = "Name prefix for every resource in this module."
  type        = string
}

variable "env" {
  description = "Environment name (dev|prod). Used in tags and in log group names."
  type        = string
}

variable "cidr_block" {
  description = "VPC CIDR. /16 leaves room for the /20 subnets below plus future ones."
  type        = string
  default     = "10.40.0.0/16"
}

variable "azs" {
  description = <<-EOT
    Availability zones to spread subnets across. Three is the minimum that lets
    MSK and RDS multi-AZ place a quorum without a single-AZ failure taking the
    cluster with it.
  EOT
  type        = list(string)

  validation {
    condition     = length(var.azs) >= 2
    error_message = "At least two AZs are required: RDS multi-AZ and MSK both need a second placement."
  }
}

variable "single_nat_gateway" {
  description = <<-EOT
    Route every private subnet through one NAT gateway instead of one per AZ.

    True in dev, where a NAT outage is an inconvenience. False in prod, where an
    AZ losing its NAT would otherwise take the whole private tier's egress with
    it. The cost difference is the reason this is a knob at all — see the NAT
    resource below.
  EOT
  type        = bool
  default     = true
}

variable "flow_log_retention_days" {
  description = "CloudWatch retention for VPC flow logs."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Tags merged onto every resource."
  type        = map(string)
  default     = {}
}
