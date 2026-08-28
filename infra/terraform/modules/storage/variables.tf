variable "name" {
  description = "Resource prefix. Bucket names are derived from it and the account id."
  type        = string
}

variable "env" {
  description = "Environment name (dev|prod)."
  type        = string
}

variable "model_artifact_retention_days" {
  description = <<-EOT
    Noncurrent-version retention for model artifacts.

    Long on purpose: ml/artifacts holds the model a prediction was served by, and
    a prediction from four months ago cannot be re-derived if its artifact was
    expired. This is the same reason signal.generated is retained for a year in
    ADR-002.
  EOT
  type        = number
  default     = 365
}

variable "backtest_retention_days" {
  description = "Days before a backtest result transitions to infrequent access."
  type        = number
  default     = 90
}

variable "log_retention_days" {
  description = "Days before archived logs expire outright."
  type        = number
  default     = 180
}

variable "force_destroy" {
  description = <<-EOT
    Allow `terraform destroy` to empty a non-empty bucket. True in dev only:
    in prod the refusal is the point.
  EOT
  type        = bool
  default     = false
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags merged onto every resource."
}
