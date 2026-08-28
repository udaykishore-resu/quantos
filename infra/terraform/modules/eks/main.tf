# EKS control plane, managed node groups and the OIDC provider that IRSA needs.
#
# The IRSA provider is the whole point of this module for the rest of the tree:
# every service role in modules/iam trusts *this* provider and a specific
# namespace/serviceaccount pair, which is what makes "one role per service"
# enforceable rather than aspirational.

locals {
  tags = merge(var.tags, {
    "quantos.io/environment" = var.env
    "quantos.io/module"      = "eks"
  })
}

# ------------------------------------------------------------------- kms -----

# Envelope encryption for Kubernetes Secrets at rest in etcd. The manifests
# never carry literal secret material, but ExternalSecrets materialises real
# values into Secret objects at runtime, and those land in etcd.
resource "aws_kms_key" "secrets" {
  description             = "${var.name} EKS secret envelope encryption"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  # Cost: $1/month per key, plus $0.03 per 10k requests. Negligible.

  tags = merge(local.tags, { Name = "${var.name}-eks-secrets" })
}

resource "aws_kms_alias" "secrets" {
  name          = "alias/${var.name}-eks-secrets"
  target_key_id = aws_kms_key.secrets.key_id
}

# ------------------------------------------------------- cluster iam role ----

data "aws_iam_policy_document" "cluster_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "cluster" {
  name               = "${var.name}-eks-cluster"
  assume_role_policy = data.aws_iam_policy_document.cluster_assume.json
  tags               = local.tags
}

# AWS-managed policies are used only where the control plane itself is the
# principal. Workload roles in modules/iam are hand-written and scoped.
resource "aws_iam_role_policy_attachment" "cluster" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
    "arn:aws:iam::aws:policy/AmazonEKSVPCResourceController",
  ])

  role       = aws_iam_role.cluster.name
  policy_arn = each.value
}

# --------------------------------------------------------- security group ----

resource "aws_security_group" "cluster" {
  name        = "${var.name}-eks-cluster"
  description = "EKS control-plane ENIs"
  vpc_id      = var.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-eks-cluster" })
}

# Egress is required for the control plane to reach node kubelets. It is bounded
# to the VPC rather than 0.0.0.0/0 because there is nothing outside the VPC the
# control-plane ENIs need to originate a connection to.
resource "aws_vpc_security_group_egress_rule" "cluster_to_vpc" {
  security_group_id = aws_security_group.cluster.id
  description       = "Control plane to node kubelets"
  ip_protocol       = "tcp"
  from_port         = 1025
  to_port           = 65535
  cidr_ipv4         = data.aws_vpc.this.cidr_block
}

data "aws_vpc" "this" {
  id = var.vpc_id
}

# ----------------------------------------------------------------- logs ------

resource "aws_cloudwatch_log_group" "cluster" {
  # EKS writes to this exact name; creating it here rather than letting EKS do
  # it is the only way to set retention and stop logs accruing forever.
  name              = "/aws/eks/${var.name}/cluster"
  retention_in_days = var.log_retention_days
  tags              = local.tags
}

# --------------------------------------------------------------- cluster -----

# Cost: the EKS control plane is $0.10/hr (~$73/month) per cluster, regardless
# of size. Node capacity is separate and is where the real spend lives.
resource "aws_eks_cluster" "this" {
  name     = var.name
  version  = var.kubernetes_version
  role_arn = aws_iam_role.cluster.arn

  enabled_cluster_log_types = var.enabled_cluster_log_types

  vpc_config {
    subnet_ids              = var.private_subnet_ids
    security_group_ids      = [aws_security_group.cluster.id]
    endpoint_private_access = true
    # An empty public_access_cidrs list means the endpoint is private-only.
    endpoint_public_access = length(var.public_access_cidrs) > 0
    public_access_cidrs    = length(var.public_access_cidrs) > 0 ? var.public_access_cidrs : null
  }

  encryption_config {
    provider {
      key_arn = aws_kms_key.secrets.arn
    }
    resources = ["secrets"]
  }

  access_config {
    # API mode drops the aws-auth ConfigMap, so cluster access is granted by
    # IAM access entries and is therefore auditable in CloudTrail.
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }

  tags = local.tags

  depends_on = [
    aws_iam_role_policy_attachment.cluster,
    aws_cloudwatch_log_group.cluster,
  ]
}

# ------------------------------------------------------------ irsa / oidc ----

data "tls_certificate" "oidc" {
  url = aws_eks_cluster.this.identity[0].oidc[0].issuer
}

resource "aws_iam_openid_connect_provider" "this" {
  url             = aws_eks_cluster.this.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]

  tags = local.tags
}

# ------------------------------------------------------------ node groups ----

data "aws_iam_policy_document" "node_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = "${var.name}-eks-node"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json
  tags               = local.tags
}

# The node role is deliberately thin: pull images, join the cluster, wire the
# CNI. It holds no application permissions at all. Anything a QuantOS service
# needs comes through IRSA (modules/iam), so a compromised pod inherits the
# service's role and not the node's.
resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
  ])

  role       = aws_iam_role.node.name
  policy_arn = each.value
}

# Cost: node groups are the largest recurring line item. As a rough guide in
# us-east-1 on-demand: m6i.large ~$0.096/hr (~$70/mo), m6i.xlarge ~$0.192/hr
# (~$140/mo), c6i.2xlarge ~$0.34/hr (~$248/mo). SPOT is typically 60-70% less
# and is why the batch group defaults to it — a preempted backtest is a retried
# backtest, whereas a preempted signal-service instance is a gap in coverage.
resource "aws_eks_node_group" "this" {
  for_each = var.node_groups

  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "${var.name}-${each.key}"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = var.private_subnet_ids

  instance_types = each.value.instance_types
  capacity_type  = each.value.capacity_type
  disk_size      = each.value.disk_size_gb
  labels         = each.value.labels

  scaling_config {
    min_size     = each.value.min_size
    max_size     = each.value.max_size
    desired_size = each.value.desired_size
  }

  update_config {
    # One node at a time. A rolling node replacement drains pods, and draining
    # two nodes at once can violate the PodDisruptionBudgets in
    # infra/kubernetes/base/pdb.yaml for the single-writer roles.
    max_unavailable = 1
  }

  dynamic "taint" {
    for_each = each.value.taints

    content {
      key    = taint.value.key
      value  = taint.value.value
      effect = taint.value.effect
    }
  }

  lifecycle {
    # The desired count is owned by the cluster autoscaler once it is running;
    # Terraform re-asserting it would fight the autoscaler on every apply.
    ignore_changes = [scaling_config[0].desired_size]
  }

  tags = merge(local.tags, { Name = "${var.name}-${each.key}" })

  depends_on = [aws_iam_role_policy_attachment.node]
}

# ------------------------------------------------------------- addons --------

# Addons are managed rather than left to the default install so their versions
# move on a schedule we choose. `most_recent` is acceptable here because these
# three are the ones AWS tests against the pinned control-plane version.
resource "aws_eks_addon" "this" {
  for_each = toset(["vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent"])

  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = each.value
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  tags = local.tags

  depends_on = [aws_eks_node_group.this]
}
