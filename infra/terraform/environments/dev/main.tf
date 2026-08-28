# Composition root.
#
# Module order is the dependency order and also the apply order documented in
# infra/terraform/README.md: network, then observability (it owns the log key
# every other module's log groups use), then the compute and data planes, then
# IAM last because it needs every ARN it scopes to.

module "network" {
  source = "../../modules/network"

  name               = var.name
  env                = var.env
  cidr_block         = var.vpc_cidr
  azs                = var.azs
  single_nat_gateway = var.single_nat_gateway
}

module "observability" {
  source = "../../modules/observability"

  name               = var.name
  env                = var.env
  log_retention_days = var.log_retention_days
}

module "eks" {
  source = "../../modules/eks"

  name                = var.name
  env                 = var.env
  kubernetes_version  = var.kubernetes_version
  vpc_id              = module.network.vpc_id
  private_subnet_ids  = module.network.private_subnet_ids
  public_access_cidrs = var.cluster_public_access_cidrs
  node_groups         = var.node_groups
  log_retention_days  = var.log_retention_days
}

module "storage" {
  source = "../../modules/storage"

  name          = var.name
  env           = var.env
  force_destroy = var.s3_force_destroy
}

module "data" {
  source = "../../modules/data"

  name               = var.name
  env                = var.env
  vpc_id             = module.network.vpc_id
  private_subnet_ids = module.network.private_subnet_ids

  # Reachability is granted by security-group membership, never by CIDR: adding
  # a subnet must not silently widen who can reach the provenance store.
  client_security_group_ids = [module.eks.node_security_group_id]

  postgres_instance_class           = var.postgres_instance_class
  postgres_multi_az                 = var.postgres_multi_az
  postgres_allocated_storage_gb     = var.postgres_allocated_storage_gb
  postgres_max_allocated_storage_gb = var.postgres_max_allocated_storage_gb
  postgres_backup_retention_days    = var.postgres_backup_retention_days
  postgres_deletion_protection      = var.postgres_deletion_protection

  redis_node_type               = var.redis_node_type
  redis_replicas_per_node_group = var.redis_replicas_per_node_group
}

module "messaging" {
  source = "../../modules/messaging"

  name               = var.name
  env                = var.env
  vpc_id             = module.network.vpc_id
  private_subnet_ids = module.network.private_subnet_ids

  client_security_group_ids = [module.eks.node_security_group_id]

  serverless           = var.msk_serverless
  broker_instance_type = var.msk_broker_instance_type
  broker_count         = var.msk_broker_count
  broker_storage_gb    = var.msk_broker_storage_gb
  log_kms_key_arn      = module.observability.logs_kms_key_arn
}

module "iam" {
  source = "../../modules/iam"

  name              = var.name
  env               = var.env
  namespace         = var.namespace
  oidc_provider_arn = module.eks.oidc_provider_arn
  oidc_provider_url = module.eks.oidc_provider_url

  msk_cluster_arn = module.messaging.cluster_arn

  s3_bucket_arns = module.storage.bucket_arns
  s3_kms_key_arn = module.storage.kms_key_arn

  data_kms_key_arn = module.data.kms_key_arn

  secret_arns = {
    postgres    = module.data.postgres_secret_arn
    redis       = module.data.redis_secret_arn
    jwt         = module.data.jwt_secret_arn
    kafka       = module.messaging.bootstrap_secret_arn
    llm_api_key = module.data.llm_api_key_secret_arn
  }

  log_group_arns      = module.observability.log_group_arns
  audit_log_group_arn = module.observability.audit_log_group_arn
}
