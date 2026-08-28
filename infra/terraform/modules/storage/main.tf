# S3 for the three durable artefact classes named in ADR-003: model artifacts,
# backtest results and archived logs.
#
# Every bucket gets the same four properties, and none of them is optional:
# versioning (an overwritten model artifact is an unreproducible prediction),
# KMS encryption, a full public-access block, and TLS-only access enforced in
# the bucket policy rather than assumed from the client.

data "aws_caller_identity" "current" {}

locals {
  # Bucket names are globally unique, so the account id is part of the name.
  # This also means two environments in one account do not collide.
  suffix = "${var.name}-${data.aws_caller_identity.current.account_id}"

  tags = merge(var.tags, {
    "quantos.io/environment" = var.env
    "quantos.io/module"      = "storage"
  })

  buckets = {
    models = {
      bucket  = "${local.suffix}-models"
      purpose = "Versioned model artifacts served by internal/mlinfer."
    }
    backtests = {
      bucket  = "${local.suffix}-backtests"
      purpose = "Backtest reports and equity curves written by backtest-service."
    }
    logs = {
      bucket  = "${local.suffix}-logs"
      purpose = "Archived application logs and ClickHouse TTL offload."
    }
  }
}

# ------------------------------------------------------------------ kms ------

# Cost: $1/month for the key, plus ~$0.03 per 10k requests. With bucket keys
# enabled below, S3 makes one KMS call per object prefix per hour rather than
# one per object, which is the difference between negligible and noticeable on
# a bucket holding millions of feature snapshots.
resource "aws_kms_key" "s3" {
  description             = "${var.name} S3 object encryption"
  enable_key_rotation     = true
  deletion_window_in_days = 30

  tags = merge(local.tags, { Name = "${var.name}-s3" })
}

resource "aws_kms_alias" "s3" {
  name          = "alias/${var.name}-s3"
  target_key_id = aws_kms_key.s3.key_id
}

# --------------------------------------------------------------- buckets -----

# Cost: S3 Standard is ~$0.023/GB-month plus request charges. The lifecycle
# rules below move cold objects to cheaper classes rather than deleting them,
# because a deleted backtest is a result nobody can check.
resource "aws_s3_bucket" "this" {
  for_each = local.buckets

  bucket        = each.value.bucket
  force_destroy = var.force_destroy

  tags = merge(local.tags, {
    Name                 = each.value.bucket
    "quantos.io/purpose" = each.value.purpose
  })
}

resource "aws_s3_bucket_versioning" "this" {
  for_each = aws_s3_bucket.this

  bucket = each.value.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "this" {
  for_each = aws_s3_bucket.this

  bucket = each.value.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.s3.arn
    }

    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "this" {
  for_each = aws_s3_bucket.this

  bucket = each.value.id

  # All four, always. Three of the four is how a bucket ends up public.
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "this" {
  for_each = aws_s3_bucket.this

  bucket = each.value.id

  rule {
    # ACLs disabled entirely. Object ownership is the bucket owner's, which
    # removes a whole class of "the writer granted itself something" bugs.
    object_ownership = "BucketOwnerEnforced"
  }
}

# Deny anything that arrives over plain HTTP. S3 is reachable from the VPC
# gateway endpoint, and an endpoint does not imply encryption in transit.
data "aws_iam_policy_document" "tls_only" {
  for_each = aws_s3_bucket.this

  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [each.value.arn, "${each.value.arn}/*"]

    # A Deny that applies to everyone, which is what a transport requirement
    # means. A principal-scoped Deny would leave every unlisted caller allowed.
    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "tls_only" {
  for_each = aws_s3_bucket.this

  bucket = each.value.id
  policy = data.aws_iam_policy_document.tls_only[each.key].json

  # A bucket policy applied before the public-access block can be rejected as
  # "public" by the block's evaluation; ordering it after removes the race.
  depends_on = [aws_s3_bucket_public_access_block.this]
}

# ------------------------------------------------------------- lifecycle -----

resource "aws_s3_bucket_lifecycle_configuration" "models" {
  bucket = aws_s3_bucket.this["models"].id

  rule {
    id     = "expire-noncurrent-artifacts"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.model_artifact_retention_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.this]
}

resource "aws_s3_bucket_lifecycle_configuration" "backtests" {
  bucket = aws_s3_bucket.this["backtests"].id

  rule {
    id     = "cool-old-results"
    status = "Enabled"

    filter {}

    # Backtest reports are read intensively for a week and then almost never.
    # They are never deleted: a result nobody can re-open is a result nobody
    # can check.
    transition {
      days          = var.backtest_retention_days
      storage_class = "STANDARD_IA"
    }

    noncurrent_version_expiration {
      noncurrent_days = 90
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.this]
}

resource "aws_s3_bucket_lifecycle_configuration" "logs" {
  bucket = aws_s3_bucket.this["logs"].id

  rule {
    id     = "tier-and-expire"
    status = "Enabled"

    filter {}

    transition {
      days          = 30
      storage_class = "STANDARD_IA"
    }

    transition {
      days          = 90
      storage_class = "GLACIER_IR"
    }

    expiration {
      days = var.log_retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = 30
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.this]
}
