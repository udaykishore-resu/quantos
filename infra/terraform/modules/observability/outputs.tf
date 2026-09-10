output "log_group_names" {
  description = "Service name to CloudWatch log group name."
  value       = { for k, g in aws_cloudwatch_log_group.service : k => g.name }
}

output "log_group_arns" {
  description = "Service name to log group ARN. modules/iam scopes each service to its own group only."
  value       = { for k, g in aws_cloudwatch_log_group.service : k => g.arn }
}

output "audit_log_group_name" {
  description = "Audit log group name."
  value       = aws_cloudwatch_log_group.audit.name
}

output "audit_log_group_arn" {
  description = "Audit log group ARN. Only api-gateway writes to it."
  value       = aws_cloudwatch_log_group.audit.arn
}

output "logs_kms_key_arn" {
  description = <<-EOT
    KMS key for CloudWatch log groups. Its key policy already names the regional
    logs service principal, so other modules that create log groups should reuse
    it rather than minting a key that CloudWatch will reject.
  EOT
  value       = aws_kms_key.logs.arn
}
