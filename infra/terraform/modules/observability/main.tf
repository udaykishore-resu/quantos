# CloudWatch log groups, created explicitly so retention is set.
#
# A log group that CloudWatch creates implicitly on first write has retention
# "Never expire". That is not a policy decision anybody made; it is a bill
# nobody noticed. Declaring the groups here is the only way retention is a
# reviewed number.
#
# Metrics and traces do not come through here. Prometheus scrapes /metrics
# in-cluster and the OTel collector forwards traces, exactly as in the compose
# stack (deploy/prometheus/prometheus.yml, deploy/otel/config.yaml). CloudWatch
# holds logs and the control-plane trail, nothing else.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  tags = merge(var.tags, {
    "quantos.io/environment" = var.env
    "quantos.io/module"      = "observability"
  })
}

# ------------------------------------------------------------------ kms ------

# CloudWatch Logs will not accept a customer key unless the key policy names the
# regional logs service principal and constrains it to this account's log
# groups. A key created elsewhere with a default policy fails at
# CreateLogGroup with an opaque InvalidParameterException, which is why this
# module owns its own key rather than taking one as an input.
data "aws_iam_policy_document" "logs_key" {
  # The default KMS root statement. Without it the key becomes unmanageable -
  # not even the account owner can schedule its deletion or change its policy -
  # and `resources` in a key policy always means "this key", never the account.
  statement {
    sid       = "AccountRoot"
    effect    = "Allow"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["arn:aws:iam::${data.aws_caller_identity.current.account_id}:root"]
    }
  }

  statement {
    sid       = "CloudWatchLogs"
    effect    = "Allow"
    resources = ["*"]

    actions = [
      "kms:Encrypt*",
      "kms:Decrypt*",
      "kms:ReEncrypt*",
      "kms:GenerateDataKey*",
      "kms:Describe*",
    ]

    principals {
      type        = "Service"
      identifiers = ["logs.${data.aws_region.current.name}.amazonaws.com"]
    }

    condition {
      test     = "ArnLike"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:aws:logs:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:log-group:*"]
    }
  }
}

# Cost: $1/month plus request charges.
resource "aws_kms_key" "logs" {
  description             = "${var.name} CloudWatch Logs encryption"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.logs_key.json

  tags = merge(local.tags, { Name = "${var.name}-logs" })
}

resource "aws_kms_alias" "logs" {
  name          = "alias/${var.name}-logs"
  target_key_id = aws_kms_key.logs.key_id
}

# ------------------------------------------------------------- log groups ----

resource "aws_cloudwatch_log_group" "service" {
  for_each = toset(var.services)

  name              = "/quantos/${var.env}/${each.value}"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.logs.arn
  # Cost: ~$0.50/GB ingested and ~$0.03/GB-month stored. JSON logs at info level
  # across nine services run to a few GB/month; the lever is log.level, which is
  # QUANTOS_LOG_LEVEL and can be turned down without a redeploy.

  tags = merge(local.tags, { "quantos.io/service" = each.value })
}

# The audit log group is separate from api-gateway's application group so its
# retention can be longer and its access can be narrower. The GET /api/v1/audit
# endpoint reads from Postgres; this group is the shipped copy that survives the
# database.
resource "aws_cloudwatch_log_group" "audit" {
  name              = "/quantos/${var.env}/audit"
  retention_in_days = var.audit_log_retention_days
  kms_key_id        = aws_kms_key.logs.arn

  tags = merge(local.tags, { "quantos.io/service" = "audit" })
}

# --------------------------------------------------------- metric filters ----

# Two log-derived metrics for conditions that are visible in logs before they
# are visible anywhere else.

# App.CanEmitSignals returning false is the platform choosing silence over an
# unexplainable signal (ADR-003). It is correct behaviour and it is also an
# incident, so it needs to be countable. Runbook:
# docs/runbooks/postgres-unavailable.md.
resource "aws_cloudwatch_log_metric_filter" "provenance_suppression" {
  name           = "${var.name}-provenance-suppression"
  log_group_name = aws_cloudwatch_log_group.service["signal-service"].name
  pattern        = "{ $.msg = \"*provenance store is unavailable*\" }"

  metric_transformation {
    name      = "ProvenanceSuppression"
    namespace = "QuantOS/${var.env}"
    value     = "1"
    unit      = "Count"
  }
}

# A binary that cannot load its configuration exits non-zero before it serves
# anything, so this fires on a bad ConfigMap rollout rather than on a runtime
# fault. Catching it here is faster than reading a CrashLoopBackOff.
resource "aws_cloudwatch_log_metric_filter" "config_invalid" {
  for_each = aws_cloudwatch_log_group.service

  name           = "${var.name}-config-invalid-${each.key}"
  log_group_name = each.value.name
  pattern        = "\"config invalid\""

  metric_transformation {
    name      = "ConfigInvalid"
    namespace = "QuantOS/${var.env}"
    value     = "1"
    unit      = "Count"
  }
}
