variable "name" {
  description = "Resource prefix."
  type        = string
}

variable "env" {
  description = "Environment name (dev|prod)."
  type        = string
}

variable "services" {
  description = <<-EOT
    The nine deployable binaries. This list must match services/ exactly: a log
    group that no service writes to is dead weight, and a service with no log
    group writes to a group created implicitly with infinite retention.
  EOT
  type        = list(string)

  default = [
    "api-gateway",
    "market-service",
    "signal-service",
    "risk-service",
    "portfolio-service",
    "news-service",
    "evaluation-service",
    "alert-service",
    "backtest-service",
  ]
}

variable "log_retention_days" {
  description = <<-EOT
    Application log retention.

    Thirty days covers the rolling 28-day SLO window in docs/operations/slo.md,
    which is the longest lookback any operational question actually needs.
    Anything older belongs in the archive bucket, not in CloudWatch at
    $0.03/GB-month.
  EOT
  type        = number
  default     = 30
}

variable "audit_log_retention_days" {
  description = <<-EOT
    Retention for the audit log group. Longer than application logs because the
    audit trail answers questions asked months later, which is the same reason
    signal.generated is retained for a year in ADR-002.
  EOT
  type        = number
  default     = 365
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags merged onto every resource."
}
