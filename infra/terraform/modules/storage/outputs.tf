output "bucket_names" {
  description = "Logical name to bucket name."
  value       = { for k, b in aws_s3_bucket.this : k => b.id }
}

output "bucket_arns" {
  description = "Logical name to bucket ARN. modules/iam scopes each service to exactly the ones it needs."
  value       = { for k, b in aws_s3_bucket.this : k => b.arn }
}

output "kms_key_arn" {
  description = "KMS key encrypting every object in these buckets. A reader needs kms:Decrypt on it as well as s3:GetObject."
  value       = aws_kms_key.s3.arn
}
