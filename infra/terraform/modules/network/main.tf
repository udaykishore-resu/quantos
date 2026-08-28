# Network foundation for the `cluster` runtime mode.
#
# The shape is conventional on purpose: public subnets carry only the NAT
# gateways and the load balancers, and everything that holds state or runs a
# QuantOS binary lives in a private subnet with no route from the internet.
# RDS, ElastiCache and MSK are all reachable only from inside this VPC, which is
# the property the rest of the tree assumes.

locals {
  # /20 per subnet: 4094 usable addresses. EKS assigns a pod one VPC address
  # under the VPC CNI, so subnet sizing is really pod sizing, and a /24 runs out
  # at a few hundred pods on one node group.
  public_cidrs   = [for i, _ in var.azs : cidrsubnet(var.cidr_block, 4, i)]
  private_cidrs  = [for i, _ in var.azs : cidrsubnet(var.cidr_block, 4, i + 8)]
  nat_gateway_ct = var.single_nat_gateway ? 1 : length(var.azs)

  tags = merge(var.tags, {
    "quantos.io/environment" = var.env
    "quantos.io/module"      = "network"
  })
}

resource "aws_vpc" "this" {
  cidr_block           = var.cidr_block
  enable_dns_support   = true
  enable_dns_hostnames = true # EKS and RDS endpoints are resolved by name.

  tags = merge(local.tags, { Name = var.name })
}

# --------------------------------------------------------------- subnets ----

resource "aws_subnet" "public" {
  count = length(var.azs)

  vpc_id            = aws_vpc.this.id
  availability_zone = var.azs[count.index]
  cidr_block        = local.public_cidrs[count.index]

  # Public IPs are assigned to the NAT gateways and the load balancers only;
  # no QuantOS workload is scheduled here, so this is not a pod-exposure risk.
  map_public_ip_on_launch = true

  tags = merge(local.tags, {
    Name                     = "${var.name}-public-${var.azs[count.index]}"
    "kubernetes.io/role/elb" = "1"
  })
}

resource "aws_subnet" "private" {
  count = length(var.azs)

  vpc_id                  = aws_vpc.this.id
  availability_zone       = var.azs[count.index]
  cidr_block              = local.private_cidrs[count.index]
  map_public_ip_on_launch = false

  tags = merge(local.tags, {
    Name                              = "${var.name}-private-${var.azs[count.index]}"
    "kubernetes.io/role/internal-elb" = "1"
  })
}

# --------------------------------------------------------------- routing ----

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = var.name })
}

# Cost: an Elastic IP attached to a running NAT gateway is free; the NAT gateway
# itself is ~$0.045/hr (~$33/mo) per gateway plus ~$0.045 per GB processed.
# One gateway in dev, one per AZ in prod: ~$33/mo vs ~$99/mo before data.
resource "aws_eip" "nat" {
  count = local.nat_gateway_ct

  domain = "vpc"
  tags   = merge(local.tags, { Name = "${var.name}-nat-${count.index}" })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count = local.nat_gateway_ct

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id
  tags          = merge(local.tags, { Name = "${var.name}-nat-${count.index}" })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-public" })
}

resource "aws_route" "public_default" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

resource "aws_route_table_association" "public" {
  count = length(aws_subnet.public)

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# One route table per private subnet even when there is a single NAT gateway, so
# that flipping single_nat_gateway to false is an in-place change to the routes
# rather than a subnet re-association.
resource "aws_route_table" "private" {
  count = length(var.azs)

  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-private-${var.azs[count.index]}" })
}

resource "aws_route" "private_default" {
  count = length(var.azs)

  route_table_id         = aws_route_table.private[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
}

resource "aws_route_table_association" "private" {
  count = length(aws_subnet.private)

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# ---------------------------------------------------------- vpc endpoints ---

# S3 through a gateway endpoint rather than the NAT. Model artifacts and
# backtest results are the largest flows in the system and NAT charges per GB;
# a gateway endpoint costs nothing and keeps the traffic off the public path.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.name}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = aws_route_table.private[*].id

  tags = merge(local.tags, { Name = "${var.name}-s3" })
}

data "aws_region" "current" {}

# Interface endpoints for the AWS APIs the workloads actually call. They exist
# so the NetworkPolicies in infra/kubernetes/base/networkpolicy.yaml can allow
# egress to the VPC CIDR and nothing else: without them, STS and Secrets Manager
# are public endpoints reached over the NAT, and "allow 443 to the internet" is
# not a policy, it is the absence of one.
#
# Cost: ~$0.01/hr per endpoint per AZ (~$7.30/month), plus ~$0.01/GB processed.
# Five endpoints across three AZs is ~$110/month - the price of not having a
# blanket internet egress rule.
resource "aws_security_group" "endpoints" {
  name        = "${var.name}-vpc-endpoints"
  description = "Interface VPC endpoints: HTTPS from inside the VPC"
  vpc_id      = aws_vpc.this.id

  tags = merge(local.tags, { Name = "${var.name}-vpc-endpoints" })
}

resource "aws_vpc_security_group_ingress_rule" "endpoints" {
  security_group_id = aws_security_group.endpoints.id
  description       = "HTTPS from inside the VPC"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = aws_vpc.this.cidr_block
}

resource "aws_vpc_endpoint" "interface" {
  for_each = toset([
    "sts",            # IRSA credential exchange, on every pod's start-up path.
    "secretsmanager", # External Secrets operator.
    "logs",           # CloudWatch log delivery.
    "kafka",          # MSK control-plane calls made by the IAM SASL client.
    "ecr.dkr",        # Image pulls; ecr.api is served by the S3 gateway endpoint.
  ])

  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${data.aws_region.current.name}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private[*].id
  security_group_ids  = [aws_security_group.endpoints.id]
  private_dns_enabled = true

  tags = merge(local.tags, { Name = "${var.name}-${each.value}" })
}

# ------------------------------------------------------------- flow logs ----

# Flow logs are the only record of "who talked to the database at 03:00" once a
# pod is gone. They are cheap relative to the incident they shorten.
resource "aws_cloudwatch_log_group" "flow" {
  name              = "/aws/vpc/${var.name}/flow-logs"
  retention_in_days = var.flow_log_retention_days
  # Cost: ~$0.50/GB ingested plus ~$0.03/GB-month stored. A busy VPC produces a
  # few GB/month at REJECT+ACCEPT; retention is the lever if that grows.

  tags = local.tags
}

data "aws_iam_policy_document" "flow_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["vpc-flow-logs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "flow" {
  name               = "${var.name}-vpc-flow-logs"
  assume_role_policy = data.aws_iam_policy_document.flow_assume.json
  tags               = local.tags
}

data "aws_iam_policy_document" "flow" {
  statement {
    effect = "Allow"

    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
      "logs:DescribeLogStreams",
    ]

    # Scoped to this VPC's log group only. The delivery role has no reason to
    # see any other log group, and CloudWatch is where the audit trail lives.
    resources = ["${aws_cloudwatch_log_group.flow.arn}:*"]
  }
}

resource "aws_iam_role_policy" "flow" {
  name   = "publish-flow-logs"
  role   = aws_iam_role.flow.id
  policy = data.aws_iam_policy_document.flow.json
}

resource "aws_flow_log" "this" {
  vpc_id                   = aws_vpc.this.id
  traffic_type             = "ALL"
  log_destination_type     = "cloud-watch-logs"
  log_destination          = aws_cloudwatch_log_group.flow.arn
  iam_role_arn             = aws_iam_role.flow.arn
  max_aggregation_interval = 60

  tags = merge(local.tags, { Name = var.name })
}

# ------------------------------------------------------- default sg lockdown -

# The default security group is created by AWS with an allow-all egress rule and
# a self-referencing ingress rule. Nothing should ever use it, so it is emptied
# rather than left as a way to accidentally get a permissive attachment.
resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-default-do-not-use" })
}
