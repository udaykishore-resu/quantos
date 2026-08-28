# Dev environment.
#
# Sized to be usable, not to be resilient. Every knob that costs money is turned
# down and every knob that protects data is left on, because losing a dev
# database is annoying and losing the habit of protecting it is expensive.
#
#   terraform apply -var-file=dev.tfvars
#
# Rough monthly floor at these settings, us-east-1, running continuously:
#   EKS control plane          ~$73
#   2 x m6i.large on-demand    ~$140
#   NAT gateway (single)        ~$33
#   RDS db.t4g.medium single-AZ ~$47 + storage
#   ElastiCache t4g.micro       ~$12
#   MSK Serverless              ~$548   <- the dominant line item
#   KMS keys (5)                  ~$5
#   ------------------------------------
#   ~$860/month if left running.
#
# MSK Serverless bills a flat cluster-hour whether or not anything is producing.
# A dev environment that is used for an hour a day should be destroyed at the
# end of the day, or should point bus.driver at `wal` and skip Kafka entirely -
# the compose profile does exactly that (config/quantos.compose.yaml).

region = "us-east-1"
name   = "quantos-dev"
env    = "dev"

vpc_cidr           = "10.40.0.0/16"
azs                = ["us-east-1a", "us-east-1b", "us-east-1c"]
single_nat_gateway = true

kubernetes_version = "1.30"

# Replace with the operator's egress ranges before applying. Left as the two
# RFC 5737 documentation prefixes so an accidental apply cannot open the API
# server to anything real.
cluster_public_access_cidrs = ["192.0.2.0/24", "198.51.100.0/24"]

node_groups = {
  platform = {
    instance_types = ["m6i.large"]
    capacity_type  = "ON_DEMAND"
    min_size       = 2
    max_size       = 4
    desired_size   = 2
    disk_size_gb   = 50
    labels         = { "quantos.io/pool" = "platform" }
    taints         = []
  }

  batch = {
    instance_types = ["c6i.large"]
    capacity_type  = "SPOT"
    min_size       = 0
    max_size       = 3
    desired_size   = 0
    disk_size_gb   = 100
    labels         = { "quantos.io/pool" = "batch" }

    # Only backtest-service and evaluation-service tolerate this. A parameter
    # sweep is minutes of single-threaded work and the live path has a
    # hundred-millisecond budget; they must not share cores.
    taints = [{
      key    = "quantos.io/pool"
      value  = "batch"
      effect = "NO_SCHEDULE"
    }]
  }
}

postgres_instance_class           = "db.t4g.medium"
postgres_multi_az                 = false
postgres_allocated_storage_gb     = 20
postgres_max_allocated_storage_gb = 100
postgres_backup_retention_days    = 3
# Off in dev so the environment can actually be torn down. This is the one
# safety knob that differs from prod on purpose.
postgres_deletion_protection = false

redis_node_type               = "cache.t4g.micro"
redis_replicas_per_node_group = 0

msk_serverless = true

log_retention_days = 7
s3_force_destroy   = true
