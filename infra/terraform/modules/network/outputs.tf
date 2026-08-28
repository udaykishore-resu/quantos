output "vpc_id" {
  description = "VPC identifier."
  value       = aws_vpc.this.id
}

output "vpc_cidr_block" {
  description = "VPC CIDR, used by downstream security groups that allow in-VPC traffic only."
  value       = aws_vpc.this.cidr_block
}

output "public_subnet_ids" {
  description = "Public subnets. Load balancers and NAT gateways only."
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "Private subnets. Every node group, database, cache and broker."
  value       = aws_subnet.private[*].id
}

output "availability_zones" {
  description = "AZs the subnets were placed in, in the same order as the subnet lists."
  value       = var.azs
}

output "flow_log_group_name" {
  description = "CloudWatch log group carrying VPC flow logs."
  value       = aws_cloudwatch_log_group.flow.name
}
