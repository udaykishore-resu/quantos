# One IAM role per service, scoped to the topics, buckets, secrets and log group
# that service actually touches.
#
# The grant table below is derived from the code, not from a guess:
#
#   producers   internal/app/run.go  (a.Bus.Publish call sites)
#   consumers   internal/app/run.go  registerEngineConsumers / registerStreamConsumers
#               internal/app/roles.go RunAlertDelivery
#   groups      Cfg.Bus.GroupPrefix + ".engine" | ".stream" | ".alert-delivery"
#   topics      internal/bus.TopicConfigs
#
# There is no wildcard action anywhere in this file and no wildcard resource
# outside a bucket-object prefix. The reason is concrete rather than
# ceremonial: on a shared bus a service that can write every topic can forge a
# risk.updated, and domain.NewSignal's structural guarantee is only as strong as
# the identity that produced the assessment it read.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  tags = merge(var.tags, {
    "quantos.io/environment" = var.env
    "quantos.io/module"      = "iam"
  })

  # MSK resource ARNs share the cluster ARN's shape with the resource type
  # swapped. Deriving them keeps the policies correct across a cluster replace,
  # where the trailing UUID changes.
  topic_arn_prefix = replace(var.msk_cluster_arn, ":cluster/", ":topic/")
  group_arn_prefix = replace(var.msk_cluster_arn, ":cluster/", ":group/")

  # Every service reads the DSN and the cache endpoint because every binary
  # builds the same App (internal/app.New) with store.driver=sql, and every
  # binary serves the API surface, which validates tokens with the HMAC key
  # named by auth.hmac_secret_env.
  common_secrets = ["postgres", "redis", "jwt", "kafka"]

  services = {
    "api-gateway" = {
      # RoleAPI. Holds no decision logic; consumes the read model for SSE.
      produce_topics  = []
      consume_topics  = ["market.quotes", "regime.updated", "signal.generated", "signal.invalidated", "alert.generated", "prediction.generated"]
      consumer_groups = ["quantos.stream"]
      s3_read         = ["backtests"]
      s3_write        = []
      extra_secrets   = []
      writes_audit    = true
    }

    "market-service" = {
      # RoleIngest. Sole producer of the market.* namespace (ADR-002), and a
      # consumer of nothing: ingestion has no upstream inside the platform.
      produce_topics  = ["market.quotes", "market.trades", "market.bars", "market.events", "market.stale", "market.rejected"]
      consume_topics  = []
      consumer_groups = []
      s3_read         = []
      s3_write        = []
      extra_secrets   = []
      writes_audit    = false
    }

    "signal-service" = {
      # RoleEngine. The decision path. Reads bars and quotes, writes everything
      # downstream of them including the alert, which is generated here and
      # delivered elsewhere.
      produce_topics  = ["features.updated", "regime.updated", "prediction.generated", "risk.updated", "signal.generated", "signal.invalidated", "alert.generated"]
      consume_topics  = ["market.quotes", "market.bars"]
      consumer_groups = ["quantos.engine"]
      s3_read         = ["models"]
      s3_write        = []
      # Explanations are generated here (cli.Spec.Explain), strictly after the
      # decision is final and with no path back into it (ADR-005).
      extra_secrets = ["llm_api_key"]
      writes_audit  = false
    }

    "risk-service" = {
      # RoleSweeper. Re-evaluates invalidation conditions on a timer, so its
      # only bus output is the invalidation itself.
      produce_topics  = ["signal.invalidated"]
      consume_topics  = []
      consumer_groups = []
      s3_read         = []
      s3_write        = []
      extra_secrets   = []
      writes_audit    = false
    }

    "portfolio-service" = {
      # Marks the paper book and persists snapshots. Postgres only; it produces
      # and consumes nothing on the bus.
      produce_topics  = []
      consume_topics  = []
      consumer_groups = []
      s3_read         = []
      s3_write        = []
      extra_secrets   = []
      writes_audit    = false
    }

    "news-service" = {
      # RoleNews. The only other service that may call the language model, and
      # then only to refine a deterministic classification into the same typed
      # schema (ADR-005).
      produce_topics  = ["market.news"]
      consume_topics  = []
      consumer_groups = []
      s3_read         = []
      s3_write        = []
      extra_secrets   = ["llm_api_key"]
      writes_audit    = false
    }

    "evaluation-service" = {
      # RoleEvaluation. Scores elapsed predictions and reports drift.
      #
      # It consumes nothing today because internal/evaluation tracks predictions
      # in-process (App.Evaluator.Track, called from the engine). If that ever
      # moves to a real consumer, "prediction.generated" belongs in
      # consume_topics with its own group - not in a widened wildcard.
      produce_topics  = ["prediction.evaluated", "model.drift.detected"]
      consume_topics  = []
      consumer_groups = []
      s3_read         = ["models"]
      s3_write        = []
      extra_secrets   = []
      writes_audit    = false
    }

    "alert-service" = {
      # Delivery only. Generation happens in the decision path, so this role
      # cannot write alert.generated - which is what stops a delivery bug from
      # manufacturing an alert.
      produce_topics  = []
      consume_topics  = ["alert.generated"]
      consumer_groups = ["quantos.alert-delivery"]
      s3_read         = []
      s3_write        = []
      extra_secrets   = []
      writes_audit    = false
    }

    "backtest-service" = {
      # Reads model artifacts, writes reports. No bus traffic: a backtest is a
      # closed computation over stored history (ADR-007).
      produce_topics  = []
      consume_topics  = []
      consumer_groups = []
      s3_read         = ["models"]
      s3_write        = ["backtests"]
      extra_secrets   = []
      writes_audit    = false
    }
  }
}

# ------------------------------------------------------------ trust policy ---

# IRSA. The condition pins both the audience and the exact
# system:serviceaccount:<namespace>:<name> subject, so the role is assumable by
# one ServiceAccount in one namespace and by nothing else. Omitting the :sub
# condition would make every pod in the cluster a valid assumer, which is the
# classic IRSA misconfiguration.
data "aws_iam_policy_document" "assume" {
  for_each = local.services

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [var.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:sub"
      values   = ["system:serviceaccount:${var.namespace}:quantos-${each.key}"]
    }
  }
}

resource "aws_iam_role" "service" {
  for_each = local.services

  name               = "${var.name}-${each.key}"
  description        = "QuantOS ${each.key} (IRSA, namespace ${var.namespace})"
  assume_role_policy = data.aws_iam_policy_document.assume[each.key].json

  # A ceiling on what any attached policy can grant. Even if a future policy is
  # written carelessly, the effective permission set cannot exceed this.
  permissions_boundary = aws_iam_policy.boundary.arn

  tags = merge(local.tags, { "quantos.io/service" = each.key })
}

# ------------------------------------------------------ permissions boundary --

# The boundary names the service categories QuantOS uses and nothing else. Note
# what is absent: no iam:*, no ec2:*, no kms:CreateKey, and nothing resembling a
# payments, brokerage or order-routing API. This platform is paper-trading and
# research (internal/paper enforces the same thing at runtime); the boundary is
# the infrastructure-side statement of that.
data "aws_iam_policy_document" "boundary" {
  # resources = ["*"] is correct here and only here. A permissions boundary is a
  # ceiling, not a grant: it gives nothing on its own, and the effective
  # permission set is the intersection of it with the role's own policies - each
  # of which names an exact ARN. Narrowing the boundary's resources would only
  # duplicate that scoping in a second place where it could drift.
  statement {
    sid    = "PlatformServicesOnly"
    effect = "Allow"

    actions = [
      "kafka-cluster:Connect",
      "kafka-cluster:DescribeCluster",
      "kafka-cluster:DescribeTopic",
      "kafka-cluster:ReadData",
      "kafka-cluster:WriteData",
      "kafka-cluster:DescribeGroup",
      "kafka-cluster:AlterGroup",
      "s3:GetObject",
      "s3:GetObjectVersion",
      "s3:PutObject",
      "s3:ListBucket",
      "s3:GetBucketLocation",
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
      "kms:Decrypt",
      "kms:DescribeKey",
      "kms:GenerateDataKey",
      "logs:CreateLogStream",
      "logs:PutLogEvents",
      "logs:DescribeLogStreams",
    ]

    resources = ["*"]
  }

  statement {
    sid       = "NeverEscalatePrivileges"
    effect    = "Deny"
    resources = ["*"]

    actions = [
      "iam:CreateUser",
      "iam:CreateAccessKey",
      "iam:AttachRolePolicy",
      "iam:PutRolePolicy",
      "iam:DeleteRolePermissionsBoundary",
      "iam:UpdateAssumeRolePolicy",
      "sts:AssumeRole",
    ]
  }
}

resource "aws_iam_policy" "boundary" {
  name        = "${var.name}-workload-boundary"
  description = "Maximum permission set any QuantOS workload role may hold"
  policy      = data.aws_iam_policy_document.boundary.json

  tags = local.tags
}

# ------------------------------------------------------------ msk access -----

data "aws_iam_policy_document" "msk" {
  # Only for services that actually speak to the bus. A service with no topics
  # gets no MSK policy at all rather than an empty one.
  for_each = {
    for k, v in local.services : k => v
    if length(v.produce_topics) > 0 || length(v.consume_topics) > 0
  }

  statement {
    sid       = "ConnectToCluster"
    effect    = "Allow"
    actions   = ["kafka-cluster:Connect", "kafka-cluster:DescribeCluster"]
    resources = [var.msk_cluster_arn]
  }

  dynamic "statement" {
    for_each = length(each.value.produce_topics) > 0 ? [1] : []

    content {
      sid     = "Produce"
      effect  = "Allow"
      actions = ["kafka-cluster:WriteData", "kafka-cluster:DescribeTopic"]

      # Each topic plus its dead-letter partner. deploy/kafka/create-topics.sh
      # creates a <topic>.dlq for every topic, and a producer that cannot reach
      # it retries a poison message forever.
      resources = flatten([
        for t in each.value.produce_topics : [
          "${local.topic_arn_prefix}/${t}",
          "${local.topic_arn_prefix}/${t}.dlq",
        ]
      ])
    }
  }

  dynamic "statement" {
    for_each = length(each.value.consume_topics) > 0 ? [1] : []

    content {
      sid     = "Consume"
      effect  = "Allow"
      actions = ["kafka-cluster:ReadData", "kafka-cluster:DescribeTopic"]

      resources = [
        for t in each.value.consume_topics : "${local.topic_arn_prefix}/${t}"
      ]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.consume_topics) > 0 ? [1] : []

    content {
      sid     = "ConsumeDeadLetter"
      effect  = "Allow"
      actions = ["kafka-cluster:WriteData", "kafka-cluster:DescribeTopic"]

      # A consumer needs write access to the dead-letter topic of what it reads,
      # and to nothing else: that is the whole point of having one.
      resources = [
        for t in each.value.consume_topics : "${local.topic_arn_prefix}/${t}.dlq"
      ]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.consumer_groups) > 0 ? [1] : []

    content {
      sid     = "CommitOffsets"
      effect  = "Allow"
      actions = ["kafka-cluster:AlterGroup", "kafka-cluster:DescribeGroup"]

      resources = [
        for g in each.value.consumer_groups : "${local.group_arn_prefix}/${g}"
      ]
    }
  }
}

resource "aws_iam_role_policy" "msk" {
  for_each = data.aws_iam_policy_document.msk

  name   = "msk"
  role   = aws_iam_role.service[each.key].id
  policy = each.value.json
}

# ------------------------------------------------------------- s3 access -----

data "aws_iam_policy_document" "s3" {
  for_each = {
    for k, v in local.services : k => v
    if length(v.s3_read) > 0 || length(v.s3_write) > 0
  }

  dynamic "statement" {
    for_each = length(each.value.s3_read) > 0 ? [1] : []

    content {
      sid     = "ReadObjects"
      effect  = "Allow"
      actions = ["s3:GetObject", "s3:GetObjectVersion"]

      resources = [
        for b in each.value.s3_read : "${var.s3_bucket_arns[b]}/*"
      ]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.s3_write) > 0 ? [1] : []

    content {
      sid    = "WriteObjects"
      effect = "Allow"
      # No s3:DeleteObject. Buckets are versioned, and a backtest result that
      # can be deleted by the process that wrote it is a result nobody can audit.
      actions = ["s3:PutObject"]

      resources = [
        for b in each.value.s3_write : "${var.s3_bucket_arns[b]}/*"
      ]
    }
  }

  statement {
    sid     = "ListOwnBuckets"
    effect  = "Allow"
    actions = ["s3:ListBucket", "s3:GetBucketLocation"]

    resources = distinct([
      for b in concat(each.value.s3_read, each.value.s3_write) : var.s3_bucket_arns[b]
    ])
  }

  statement {
    sid    = "UseBucketKey"
    effect = "Allow"

    # GenerateDataKey is needed only by a writer, but splitting the statement
    # buys nothing here: both actions are already pinned to the one key that
    # encrypts these buckets, via a condition on the calling service.
    actions   = ["kms:Decrypt", "kms:DescribeKey", "kms:GenerateDataKey"]
    resources = [var.s3_kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["s3.${data.aws_region.current.name}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "s3" {
  for_each = data.aws_iam_policy_document.s3

  name   = "s3"
  role   = aws_iam_role.service[each.key].id
  policy = each.value.json
}

# ---------------------------------------------------------- secrets access ---

data "aws_iam_policy_document" "secrets" {
  for_each = local.services

  statement {
    sid     = "ReadOwnSecrets"
    effect  = "Allow"
    actions = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]

    # No PutSecretValue and no UpdateSecret anywhere: a workload that can
    # rewrite the DSN it reads can point the platform at a database of its
    # choosing.
    resources = [
      for s in concat(local.common_secrets, each.value.extra_secrets) : var.secret_arns[s]
    ]
  }

  statement {
    sid       = "DecryptSecrets"
    effect    = "Allow"
    actions   = ["kms:Decrypt", "kms:DescribeKey"]
    resources = [var.data_kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "secrets" {
  for_each = local.services

  name   = "secrets"
  role   = aws_iam_role.service[each.key].id
  policy = data.aws_iam_policy_document.secrets[each.key].json
}

# -------------------------------------------------------------- logging ------

data "aws_iam_policy_document" "logs" {
  for_each = local.services

  statement {
    sid     = "WriteOwnLogGroup"
    effect  = "Allow"
    actions = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]

    # Its own group and no other. Cross-service log writes would let one
    # compromised service forge another's record.
    resources = concat(
      ["${var.log_group_arns[each.key]}:*"],
      each.value.writes_audit ? ["${var.audit_log_group_arn}:*"] : [],
    )
  }
}

resource "aws_iam_role_policy" "logs" {
  for_each = local.services

  name   = "logs"
  role   = aws_iam_role.service[each.key].id
  policy = data.aws_iam_policy_document.logs[each.key].json
}

# ------------------------------------------------------------ topics job -----

# The topic-creation Job gets its own role rather than borrowing a service's.
#
# It needs CreateTopic and AlterTopicDynamicConfiguration, which no running
# service should ever hold: a producer that can reshape a topic can change its
# partition count, and a changed partition count silently re-keys every symbol
# and breaks the per-symbol ordering guarantee mid-flight (ADR-002). Conversely
# this role holds no ReadData or WriteData at all - it provisions topics and
# then has nothing further to do with them.
locals {
  # internal/bus.TopicConfigs, plus the dead-letter partner each one gets.
  all_topics = [
    "market.quotes", "market.trades", "market.bars", "market.news",
    "market.events", "market.stale", "market.rejected", "features.updated",
    "regime.updated", "prediction.generated", "signal.generated",
    "signal.invalidated", "risk.updated", "alert.generated",
    "prediction.evaluated", "model.drift.detected",
  ]
}

data "aws_iam_policy_document" "topics_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [var.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:sub"
      values   = ["system:serviceaccount:${var.namespace}:quantos-topics"]
    }
  }
}

resource "aws_iam_role" "topics" {
  name               = "${var.name}-topics"
  description        = "QuantOS topic-provisioning Job: creates the ADR-002 topic set, nothing else"
  assume_role_policy = data.aws_iam_policy_document.topics_assume.json

  tags = merge(local.tags, { "quantos.io/service" = "topics-job" })
}

data "aws_iam_policy_document" "topics" {
  statement {
    sid       = "ConnectToCluster"
    effect    = "Allow"
    actions   = ["kafka-cluster:Connect", "kafka-cluster:DescribeCluster"]
    resources = [var.msk_cluster_arn]
  }

  statement {
    sid    = "ProvisionTopics"
    effect = "Allow"

    actions = [
      "kafka-cluster:CreateTopic",
      "kafka-cluster:DescribeTopic",
      "kafka-cluster:AlterTopicDynamicConfiguration",
      "kafka-cluster:DescribeTopicDynamicConfiguration",
    ]

    # Named topics only. Not a prefix wildcard: a topic outside this list is one
    # the platform does not know about, and creating it here would hide that.
    resources = flatten([
      for t in local.all_topics : [
        "${local.topic_arn_prefix}/${t}",
        "${local.topic_arn_prefix}/${t}.dlq",
      ]
    ])
  }
}

resource "aws_iam_role_policy" "topics" {
  name   = "provision-topics"
  role   = aws_iam_role.topics.id
  policy = data.aws_iam_policy_document.topics.json
}

# ------------------------------------------------ external-secrets operator ---

# The External Secrets operator is the only workload that reads every secret,
# because its job is to project them into Kubernetes Secrets for the pods. It is
# still bounded to the four QuantOS secrets rather than to the account.
data "aws_iam_policy_document" "external_secrets_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [var.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:sub"
      values   = ["system:serviceaccount:external-secrets:external-secrets"]
    }
  }
}

resource "aws_iam_role" "external_secrets" {
  name               = "${var.name}-external-secrets"
  description        = "External Secrets operator: projects QuantOS secrets into the cluster"
  assume_role_policy = data.aws_iam_policy_document.external_secrets_assume.json

  tags = local.tags
}

data "aws_iam_policy_document" "external_secrets" {
  statement {
    sid       = "ReadQuantOSSecrets"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
    resources = values(var.secret_arns)
  }

  statement {
    sid       = "DecryptSecrets"
    effect    = "Allow"
    actions   = ["kms:Decrypt", "kms:DescribeKey"]
    resources = [var.data_kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "external_secrets" {
  name   = "read-quantos-secrets"
  role   = aws_iam_role.external_secrets.id
  policy = data.aws_iam_policy_document.external_secrets.json
}
