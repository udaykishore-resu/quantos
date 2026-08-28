output "postgres_endpoint" {
  description = "RDS endpoint, host:port."
  value       = aws_db_instance.postgres.endpoint
}

output "postgres_address" {
  description = "RDS hostname."
  value       = aws_db_instance.postgres.address
}

output "postgres_security_group_id" {
  description = "Security group protecting RDS."
  value       = aws_security_group.postgres.id
}

output "redis_primary_endpoint" {
  description = "ElastiCache primary endpoint. Becomes QUANTOS_REDIS_ADDR with :6379 appended."
  value       = aws_elasticache_replication_group.redis.primary_endpoint_address
}

output "redis_security_group_id" {
  description = "Security group protecting ElastiCache."
  value       = aws_security_group.redis.id
}

output "kms_key_arn" {
  description = "KMS key encrypting RDS, ElastiCache and every secret in this module."
  value       = aws_kms_key.data.arn
}

output "postgres_secret_arn" {
  description = "Secrets Manager entry holding the PostgreSQL DSN."
  value       = aws_secretsmanager_secret.postgres.arn
}

output "redis_secret_arn" {
  description = "Secrets Manager entry holding the Redis endpoint and AUTH token."
  value       = aws_secretsmanager_secret.redis.arn
}

output "jwt_secret_arn" {
  description = "Secrets Manager entry holding the JWT HMAC signing key."
  value       = aws_secretsmanager_secret.jwt.arn
}

output "llm_api_key_secret_arn" {
  description = <<-EOT
    Secrets Manager entry reserved for the LLM provider key. Terraform creates
    the container only; the value is written out of band.
  EOT
  value       = aws_secretsmanager_secret.llm_api_key.arn
}
