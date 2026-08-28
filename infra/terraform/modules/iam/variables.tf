variable "name" {
  description = "Resource prefix."
  type        = string
}

variable "env" {
  description = "Environment name (dev|prod)."
  type        = string
}

variable "namespace" {
  description = <<-EOT
    Kubernetes namespace the workloads run in. It is half of the IRSA subject
    condition, so a pod in another namespace cannot assume these roles even if
    it uses the same ServiceAccount name.
  EOT
  type        = string
  default     = "quantos"
}

variable "oidc_provider_arn" {
  description = "EKS IAM OIDC provider ARN, from modules/eks."
  type        = string
}

variable "oidc_provider_url" {
  description = "EKS OIDC issuer host and path, without the https:// scheme."
  type        = string
}

variable "msk_cluster_arn" {
  description = "MSK cluster ARN. Topic and group ARNs are derived from it."
  type        = string
}

variable "s3_bucket_arns" {
  description = "Logical bucket name (models|backtests|logs) to ARN, from modules/storage."
  type        = map(string)
}

variable "s3_kms_key_arn" {
  description = "KMS key encrypting the S3 objects. s3:GetObject without kms:Decrypt returns an access-denied nobody expects."
  type        = string
}

variable "data_kms_key_arn" {
  description = "KMS key encrypting the Secrets Manager entries in modules/data."
  type        = string
}

variable "secret_arns" {
  description = <<-EOT
    Logical secret name to ARN. Expected keys: postgres, redis, jwt, kafka, llm_api_key.

    llm_api_key is granted to exactly two services. api-gateway is not one of
    them: it is the process that renders responses to a browser, and a provider
    key must not be one bug away from a response body.
  EOT
  type        = map(string)
}

variable "log_group_arns" {
  description = "Service name to CloudWatch log group ARN, from modules/observability."
  type        = map(string)
}

variable "audit_log_group_arn" {
  description = "Audit log group ARN. Only api-gateway writes to it."
  type        = string
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags merged onto every resource."
}
