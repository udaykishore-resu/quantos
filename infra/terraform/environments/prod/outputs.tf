# Outputs are the handover to infra/kubernetes and infra/helm. None of them is
# a secret: the DSN and the API key live in Secrets Manager and are named here
# by ARN only, so `terraform output` is safe to paste into a ticket.

output "cluster_name" {
  description = "EKS cluster name. `aws eks update-kubeconfig --name <this>`."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API server endpoint."
  value       = module.eks.cluster_endpoint
}

output "kafka_bootstrap_brokers" {
  description = "Value for QUANTOS_KAFKA_BROKERS / helm values kafka.brokers."
  value       = module.messaging.bootstrap_brokers
}

output "postgres_address" {
  description = "RDS hostname. The full DSN is in Secrets Manager, not here."
  value       = module.data.postgres_address
}

output "redis_addr" {
  description = "Value for QUANTOS_REDIS_ADDR."
  value       = "${module.data.redis_primary_endpoint}:6379"
}

output "s3_buckets" {
  description = "Logical name to bucket name."
  value       = module.storage.bucket_names
}

output "secret_arns" {
  description = "Secrets Manager ARNs referenced by the ExternalSecret manifests."
  value = {
    postgres    = module.data.postgres_secret_arn
    redis       = module.data.redis_secret_arn
    jwt         = module.data.jwt_secret_arn
    kafka       = module.messaging.bootstrap_secret_arn
    llm_api_key = module.data.llm_api_key_secret_arn
  }
}

output "service_role_arns" {
  description = <<-EOT
    Service name to IRSA role ARN. Feed these into the
    eks.amazonaws.com/role-arn annotations in the kustomize overlay or into
    serviceAccounts.<service>.roleArn in the Helm values.
  EOT
  value       = module.iam.service_role_arns
}

output "external_secrets_role_arn" {
  description = "IRSA role for the External Secrets operator."
  value       = module.iam.external_secrets_role_arn
}

output "log_group_names" {
  description = "Service name to CloudWatch log group."
  value       = module.observability.log_group_names
}

output "topics_job_role_arn" {
  description = "IRSA role for the topic-provisioning Job."
  value       = module.iam.topics_job_role_arn
}
