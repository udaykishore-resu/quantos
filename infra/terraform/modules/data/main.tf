# Stateful stores for `cluster` mode: RDS PostgreSQL, ElastiCache Redis, and the
# Secrets Manager entries that hold the credentials for them.
#
# ADR-003 splits the persistence layer three ways. Two of the three are here.
# ClickHouse is deliberately not provisioned by this module - see the "not
# automated" section of infra/terraform/README.md for why and for what to run
# instead.
#
# The Secrets Manager entries live in this module because a secret and the thing
# it grants access to should have one lifecycle: destroying the database without
# destroying its credential leaves a live secret pointing at nothing.

locals {
  tags = merge(var.tags, {
    "quantos.io/environment" = var.env
    "quantos.io/module"      = "data"
  })
}

# ------------------------------------------------------------------ kms ------

# One key for stateful data at rest. Cost: $1/month plus ~$0.03 per 10k API
# calls. Rotation is annual and transparent to the encrypted resources.
resource "aws_kms_key" "data" {
  description             = "${var.name} data-at-rest (RDS, ElastiCache, Secrets Manager)"
  enable_key_rotation     = true
  deletion_window_in_days = 30

  tags = merge(local.tags, { Name = "${var.name}-data" })
}

resource "aws_kms_alias" "data" {
  name          = "alias/${var.name}-data"
  target_key_id = aws_kms_key.data.key_id
}

# -------------------------------------------------------- security groups ----

resource "aws_security_group" "postgres" {
  name        = "${var.name}-postgres"
  description = "RDS PostgreSQL: in-cluster clients only"
  vpc_id      = var.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-postgres" })
}

resource "aws_vpc_security_group_ingress_rule" "postgres" {
  for_each = toset(var.client_security_group_ids)

  security_group_id            = aws_security_group.postgres.id
  description                  = "PostgreSQL from an approved client security group"
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = each.value
}

resource "aws_security_group" "redis" {
  name        = "${var.name}-redis"
  description = "ElastiCache Redis: in-cluster clients only"
  vpc_id      = var.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-redis" })
}

resource "aws_vpc_security_group_ingress_rule" "redis" {
  for_each = toset(var.client_security_group_ids)

  security_group_id            = aws_security_group.redis.id
  description                  = "Redis from an approved client security group"
  ip_protocol                  = "tcp"
  from_port                    = 6379
  to_port                      = 6379
  referenced_security_group_id = each.value
}

# No egress rules are declared on either group. Neither a database nor a cache
# has any reason to originate a connection, and an empty egress set is the
# default-deny that says so.

# ------------------------------------------------------------- postgres ------

resource "aws_db_subnet_group" "postgres" {
  name       = "${var.name}-postgres"
  subnet_ids = var.private_subnet_ids
  tags       = local.tags
}

resource "aws_db_parameter_group" "postgres" {
  name   = "${var.name}-postgres16"
  family = "postgres16"

  parameter {
    # Reject unencrypted connections at the server. The DSN the services build
    # sets sslmode=require, but the server is the place that can enforce it.
    name         = "rds.force_ssl"
    value        = "1"
    apply_method = "pending-reboot"
  }

  parameter {
    # Statements slower than a second are logged. The API read SLO is a 300 ms
    # P99 and its tail is Postgres joins on provenance, so a one-second query is
    # already an SLO event worth having a record of.
    name  = "log_min_duration_statement"
    value = "1000"
  }

  tags = local.tags
}

resource "random_password" "postgres" {
  length = 40
  # RDS rejects these in a master password.
  override_special = "!#$%&*()-_=+[]{}<>:?"
}

# Cost: db.t4g.medium is ~$0.065/hr single-AZ (~$47/mo) and roughly double
# multi-AZ (~$95/mo), plus gp3 storage at ~$0.115/GB-month and backup storage
# beyond the allocated size at ~$0.095/GB-month.
resource "aws_db_instance" "postgres" {
  identifier     = "${var.name}-postgres"
  engine         = "postgres"
  engine_version = var.postgres_version
  instance_class = var.postgres_instance_class

  db_name  = var.postgres_db_name
  username = var.postgres_username
  password = random_password.postgres.result

  allocated_storage     = var.postgres_allocated_storage_gb
  max_allocated_storage = var.postgres_max_allocated_storage_gb
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = aws_kms_key.data.arn

  db_subnet_group_name   = aws_db_subnet_group.postgres.name
  parameter_group_name   = aws_db_parameter_group.postgres.name
  vpc_security_group_ids = [aws_security_group.postgres.id]

  # Non-negotiable. A publicly addressable provenance store is a data breach
  # waiting for a weak password, and there is no access pattern that needs it.
  publicly_accessible = false

  multi_az                = var.postgres_multi_az
  backup_retention_period = var.postgres_backup_retention_days
  backup_window           = "07:00-08:00" # UTC, before the US cash open.
  maintenance_window      = "sun:08:30-sun:09:30"
  copy_tags_to_snapshot   = true

  auto_minor_version_upgrade = true
  deletion_protection        = var.postgres_deletion_protection
  skip_final_snapshot        = false
  final_snapshot_identifier  = "${var.name}-postgres-final"

  performance_insights_enabled    = true
  performance_insights_kms_key_id = aws_kms_key.data.arn
  # Cost: seven days of Performance Insights retention is free; longer is not.
  performance_insights_retention_period = 7

  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]

  tags = merge(local.tags, { Name = "${var.name}-postgres" })
}

# ---------------------------------------------------------------- redis ------

resource "aws_elasticache_subnet_group" "redis" {
  name       = "${var.name}-redis"
  subnet_ids = var.private_subnet_ids
  tags       = local.tags
}

resource "aws_elasticache_parameter_group" "redis" {
  name   = "${var.name}-redis7"
  family = "redis7"

  parameter {
    # Matches the compose stack (deploy/docker-compose.yml). Redis is a cache;
    # evicting the coldest key is correct behaviour under pressure, and refusing
    # writes is not.
    name  = "maxmemory-policy"
    value = "allkeys-lru"
  }

  tags = local.tags
}

resource "random_password" "redis_auth" {
  length  = 64
  special = false # ElastiCache AUTH tokens are restricted to alphanumerics.
}

# Cost: cache.t4g.micro is ~$0.016/hr (~$12/mo) per node; a replica doubles it.
# Data transfer between AZs applies to replication.
resource "aws_elasticache_replication_group" "redis" {
  replication_group_id = "${var.name}-redis"
  description          = "QuantOS cache, rate-limit buckets and dedup set"

  engine         = "redis"
  engine_version = var.redis_version
  node_type      = var.redis_node_type
  port           = 6379

  num_node_groups            = 1
  replicas_per_node_group    = var.redis_replicas_per_node_group
  automatic_failover_enabled = var.redis_replicas_per_node_group > 0
  multi_az_enabled           = var.redis_replicas_per_node_group > 0

  subnet_group_name  = aws_elasticache_subnet_group.redis.name
  security_group_ids = [aws_security_group.redis.id]
  parameter_group_name = aws_elasticache_parameter_group.redis.name

  at_rest_encryption_enabled = true
  kms_key_id                 = aws_kms_key.data.arn
  transit_encryption_enabled = true
  auth_token                 = random_password.redis_auth.result

  snapshot_retention_limit = var.redis_snapshot_retention_days
  maintenance_window       = "sun:09:30-sun:10:30"
  apply_immediately        = false

  auto_minor_version_upgrade = true

  tags = merge(local.tags, { Name = "${var.name}-redis" })
}

# --------------------------------------------------------------- secrets -----

# The DSN is assembled here and stored whole, so no service has to compose one
# from parts and get sslmode wrong. QUANTOS_POSTGRES_DSN is read verbatim by
# internal/config.applyEnv.
resource "aws_secretsmanager_secret" "postgres" {
  name                    = "${var.name}/postgres"
  description             = "QuantOS PostgreSQL DSN (QUANTOS_POSTGRES_DSN)"
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "postgres" {
  secret_id = aws_secretsmanager_secret.postgres.id

  secret_string = jsonencode({
    username = aws_db_instance.postgres.username
    password = random_password.postgres.result
    host     = aws_db_instance.postgres.address
    port     = aws_db_instance.postgres.port
    dbname   = aws_db_instance.postgres.db_name
    dsn = format(
      "postgres://%s:%s@%s:%d/%s?sslmode=require",
      aws_db_instance.postgres.username,
      urlencode(random_password.postgres.result),
      aws_db_instance.postgres.address,
      aws_db_instance.postgres.port,
      aws_db_instance.postgres.db_name,
    )
  })
}

resource "aws_secretsmanager_secret" "redis" {
  name                    = "${var.name}/redis"
  description             = "QuantOS Redis endpoint and AUTH token"
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "redis" {
  secret_id = aws_secretsmanager_secret.redis.id

  secret_string = jsonencode({
    addr       = "${aws_elasticache_replication_group.redis.primary_endpoint_address}:6379"
    auth_token = random_password.redis_auth.result
  })
}

# JWT signing key for the HMAC path (config auth.hmac_secret_env =
# QUANTOS_JWT_SECRET). Generated, never typed: a human-chosen signing key is the
# single most common way a token forgery becomes possible.
resource "random_password" "jwt" {
  length  = 64
  special = false
}

resource "aws_secretsmanager_secret" "jwt" {
  name                    = "${var.name}/jwt-signing-key"
  description             = "QuantOS JWT HMAC signing key (QUANTOS_JWT_SECRET)"
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "jwt" {
  secret_id     = aws_secretsmanager_secret.jwt.id
  secret_string = jsonencode({ secret = random_password.jwt.result })
}

# The LLM API key. Terraform creates the *container* and never the value: a key
# in a variable default, a tfvars file or a plan output is a key in version
# control and in every CI log that renders a plan.
#
# Populate it out of band, once:
#   aws secretsmanager put-secret-value \
#     --secret-id <name>/llm-api-key \
#     --secret-string "$(read -rs k; echo "{\"api_key\":\"$k\"}")"
#
# The key is read only by news-service and signal-service (modules/iam). It is
# never mounted into api-gateway, because api-gateway serves the browser and a
# provider key must not sit one bug away from a response body.
resource "aws_secretsmanager_secret" "llm_api_key" {
  name                    = "${var.name}/llm-api-key"
  description             = "Anthropic API key (config llm.api_key_env = ANTHROPIC_API_KEY). Value set out of band."
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}
