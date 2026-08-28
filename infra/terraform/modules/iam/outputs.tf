output "service_role_arns" {
  description = <<-EOT
    Service name to IAM role ARN. Each becomes the
    eks.amazonaws.com/role-arn annotation on the matching ServiceAccount in
    infra/kubernetes/base/serviceaccounts.yaml.
  EOT
  value = { for k, r in aws_iam_role.service : k => r.arn }
}

output "external_secrets_role_arn" {
  description = "IAM role for the External Secrets operator's ServiceAccount."
  value       = aws_iam_role.external_secrets.arn
}

output "permissions_boundary_arn" {
  description = "Boundary policy capping what any workload role can hold."
  value       = aws_iam_policy.boundary.arn
}

output "topics_job_role_arn" {
  description = "IRSA role for the topic-provisioning Job's ServiceAccount (quantos-topics)."
  value       = aws_iam_role.topics.arn
}
