output "cluster_arn" {
  description = "MSK cluster ARN. IAM policies in modules/iam derive topic and group ARNs from it."
  value       = var.serverless ? aws_msk_serverless_cluster.this[0].arn : aws_msk_cluster.this[0].arn
}

output "cluster_name" {
  description = "MSK cluster name."
  value       = var.serverless ? aws_msk_serverless_cluster.this[0].cluster_name : aws_msk_cluster.this[0].cluster_name
}

output "bootstrap_brokers" {
  description = <<-EOT
    IAM-SASL bootstrap string. Becomes QUANTOS_KAFKA_BROKERS, which
    internal/config splits on commas into bus.brokers.
  EOT
  value       = var.serverless ? aws_msk_serverless_cluster.this[0].bootstrap_brokers_sasl_iam : aws_msk_cluster.this[0].bootstrap_brokers_sasl_iam
}

output "security_group_id" {
  description = "Security group protecting the brokers."
  value       = aws_security_group.msk.id
}

output "kms_key_arn" {
  description = "KMS key encrypting broker storage."
  value       = aws_kms_key.msk.arn
}

output "bootstrap_secret_arn" {
  description = "Secrets Manager entry holding the bootstrap broker string."
  value       = aws_secretsmanager_secret.bootstrap.arn
}
