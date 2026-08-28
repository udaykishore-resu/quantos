# Kafka for `cluster` mode.
#
# Two shapes behind one variable: MSK Serverless for dev, provisioned MSK for
# prod. Both expose IAM SASL only. Plaintext and SCRAM are not offered, because
# IAM authentication is what lets modules/iam scope a service to the exact
# topics it produces to and consumes from - which is the only way "least
# privilege" means anything on a shared bus.
#
# Topics themselves are NOT created here. Kafka has no CloudFormation-style
# resource model in Terraform without a third-party provider that needs broker
# reachability at plan time, and the partition counts are already authoritative
# in internal/bus.TopicConfigs. The topic Job in infra/kubernetes/base runs
# inside the VPC and creates them from that same specification.

locals {
  tags = merge(var.tags, {
    "quantos.io/environment" = var.env
    "quantos.io/module"      = "messaging"
  })
}

# ------------------------------------------------------------------ kms ------

# Cost: $1/month plus request charges.
resource "aws_kms_key" "msk" {
  description             = "${var.name} MSK data at rest"
  enable_key_rotation     = true
  deletion_window_in_days = 30

  tags = merge(local.tags, { Name = "${var.name}-msk" })
}

resource "aws_kms_alias" "msk" {
  name          = "alias/${var.name}-msk"
  target_key_id = aws_kms_key.msk.key_id
}

# -------------------------------------------------------- security group -----

resource "aws_security_group" "msk" {
  name        = "${var.name}-msk"
  description = "MSK brokers: in-cluster clients only"
  vpc_id      = var.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-msk" })
}

resource "aws_vpc_security_group_ingress_rule" "msk_iam_tls" {
  for_each = toset(var.client_security_group_ids)

  security_group_id            = aws_security_group.msk.id
  description                  = "Kafka over TLS with IAM SASL"
  ip_protocol                  = "tcp"
  from_port                    = 9098
  to_port                      = 9098
  referenced_security_group_id = each.value
}

# Brokers replicate to each other, so unlike the data stores this group does
# need to talk to itself. It is scoped to itself rather than to the VPC.
resource "aws_vpc_security_group_ingress_rule" "msk_interbroker" {
  security_group_id            = aws_security_group.msk.id
  description                  = "Inter-broker replication"
  ip_protocol                  = "tcp"
  from_port                    = 9090
  to_port                      = 9098
  referenced_security_group_id = aws_security_group.msk.id
}

resource "aws_vpc_security_group_egress_rule" "msk_interbroker" {
  security_group_id            = aws_security_group.msk.id
  description                  = "Inter-broker replication"
  ip_protocol                  = "tcp"
  from_port                    = 9090
  to_port                      = 9098
  referenced_security_group_id = aws_security_group.msk.id
}

# ------------------------------------------------------------------ logs -----

# The key comes from modules/observability rather than being the MSK key above:
# CloudWatch Logs refuses a customer key whose policy does not name the regional
# logs service principal, and that grant belongs on one key rather than on every
# key in the account.
resource "aws_cloudwatch_log_group" "broker" {
  name              = "/aws/msk/${var.name}/broker"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = local.tags
}

# --------------------------------------------------------- serverless --------

# Cost: MSK Serverless bills ~$0.75/hr per cluster (~$548/mo) plus ~$0.0015 per
# partition-hour and ~$0.10/GB in and out. The fixed hourly charge dominates at
# dev volumes and is the reason dev is not left running overnight.
resource "aws_msk_serverless_cluster" "this" {
  count = var.serverless ? 1 : 0

  cluster_name = "${var.name}-kafka"

  vpc_config {
    subnet_ids         = slice(var.private_subnet_ids, 0, min(3, length(var.private_subnet_ids)))
    security_group_ids = [aws_security_group.msk.id]
  }

  client_authentication {
    sasl {
      iam {
        enabled = true
      }
    }
  }

  tags = local.tags
}

# -------------------------------------------------------- provisioned --------

resource "aws_msk_configuration" "this" {
  count = var.serverless ? 0 : 1

  name           = "${var.name}-kafka"
  kafka_versions = [var.kafka_version]

  # Auto-creation is off for the same reason it is off in the compose stack: a
  # topic created implicitly gets the broker default partition count, and a
  # market.quotes with one partition destroys the per-symbol ordering guarantee
  # in ADR-002 without producing a single error.
  server_properties = <<-PROPERTIES
    auto.create.topics.enable=false
    default.replication.factor=3
    min.insync.replicas=2
    num.partitions=12
    unclean.leader.election.enable=false
    log.retention.hours=168
  PROPERTIES
}

# Cost: kafka.m7g.large is ~$0.21/hr per broker (~$153/mo); three brokers is
# ~$460/mo before storage at ~$0.10/GB-month (200 GB x 3 = ~$60/mo) and before
# cross-AZ data transfer on replication.
resource "aws_msk_cluster" "this" {
  count = var.serverless ? 0 : 1

  cluster_name           = "${var.name}-kafka"
  kafka_version          = var.kafka_version
  number_of_broker_nodes = var.broker_count

  broker_node_group_info {
    instance_type   = var.broker_instance_type
    client_subnets  = var.private_subnet_ids
    security_groups = [aws_security_group.msk.id]

    storage_info {
      ebs_storage_info {
        volume_size = var.broker_storage_gb

        provisioned_throughput {
          enabled = false
        }
      }
    }
  }

  configuration_info {
    arn      = aws_msk_configuration.this[0].arn
    revision = aws_msk_configuration.this[0].latest_revision
  }

  client_authentication {
    sasl {
      iam = true
    }

    # No unauthenticated listener. Every client presents an IAM identity, which
    # is what the per-service topic scoping in modules/iam relies on.
    unauthenticated = false
  }

  encryption_info {
    encryption_at_rest_kms_key_arn = aws_kms_key.msk.arn

    encryption_in_transit {
      client_broker = "TLS"
      in_cluster    = true
    }
  }

  open_monitoring {
    prometheus {
      jmx_exporter {
        enabled_in_broker = true
      }

      node_exporter {
        enabled_in_broker = true
      }
    }
  }

  logging_info {
    broker_logs {
      cloudwatch_logs {
        enabled   = true
        log_group = aws_cloudwatch_log_group.broker.name
      }
    }
  }

  tags = local.tags
}

# ---------------------------------------------------------------- secret -----

# The bootstrap string is not secret in the cryptographic sense - MSK
# authenticates with IAM, not with this - but it is environment-specific and
# changes when the cluster is replaced. Storing it here means the manifests in
# infra/kubernetes carry a reference rather than a value, and a cluster replace
# does not require a commit.
resource "aws_secretsmanager_secret" "bootstrap" {
  name                    = "${var.name}/kafka"
  description             = "MSK IAM-SASL bootstrap brokers (QUANTOS_KAFKA_BROKERS)"
  kms_key_id              = aws_kms_key.msk.arn
  recovery_window_in_days = 7

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "bootstrap" {
  secret_id = aws_secretsmanager_secret.bootstrap.id

  secret_string = jsonencode({
    bootstrap_brokers = var.serverless ? aws_msk_serverless_cluster.this[0].bootstrap_brokers_sasl_iam : aws_msk_cluster.this[0].bootstrap_brokers_sasl_iam
  })
}
