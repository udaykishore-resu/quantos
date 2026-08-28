# Production environment.
#
# The differences from dev are all in one direction: survive the loss of an AZ,
# refuse to be destroyed by accident, and keep enough history to answer a
# question asked next quarter.
#
#   terraform apply -var-file=prod.tfvars
#
# Rough monthly floor at these settings, us-east-1, running continuously:
#   EKS control plane                ~$73
#   3 x m6i.xlarge on-demand         ~$420
#   NAT gateways (3, one per AZ)     ~$99  + per-GB processing
#   RDS db.r6g.large multi-AZ        ~$370 + 200 GB gp3 (~$23)
#   ElastiCache t4g.small x2         ~$50
#   MSK 3 x kafka.m7g.large          ~$460 + 600 GB storage (~$60)
#   KMS keys (5)                       ~$5
#   CloudWatch logs (30d, ~20 GB)     ~$15
#   ---------------------------------------
#   ~$1,575/month before data transfer.
#
# Cross-AZ transfer is the line that surprises people: MSK replication and RDS
# multi-AZ both cross zones continuously. Budget for it rather than discovering
# it.

region = "us-east-1"
name   = "quantos-prod"
env    = "prod"

vpc_cidr = "10.50.0.0/16"
azs      = ["us-east-1a", "us-east-1b", "us-east-1c"]

# One NAT per AZ. A single gateway means one AZ's failure removes egress for
# every private subnet, including the pods in the two healthy zones.
single_nat_gateway = false

kubernetes_version = "1.30"

# Private-only API server. kubectl goes through the bastion or the VPN; there is
# no public endpoint to find.
cluster_public_access_cidrs = []

node_groups = {
  platform = {
    instance_types = ["m6i.xlarge"]
    capacity_type  = "ON_DEMAND"
    min_size       = 3
    max_size       = 12
    desired_size   = 3
    disk_size_gb   = 100
    labels         = { "quantos.io/pool" = "platform" }
    taints         = []
  }

  batch = {
    instance_types = ["c6i.2xlarge", "c6a.2xlarge"]
    # Spot is correct here and nowhere else: a preempted backtest is a retried
    # backtest, while a preempted signal-service instance is a gap in coverage
    # that no retry recovers.
    capacity_type = "SPOT"
    min_size      = 0
    max_size      = 8
    desired_size  = 1
    disk_size_gb  = 200
    labels        = { "quantos.io/pool" = "batch" }

    taints = [{
      key    = "quantos.io/pool"
      value  = "batch"
      effect = "NO_SCHEDULE"
    }]
  }
}

postgres_instance_class = "db.r6g.large"
# Non-negotiable. Postgres holds the provenance chain, and App.CanEmitSignals
# suspends signal emission the moment it is unwritable: a single-AZ provenance
# store makes one AZ's failure a platform-wide outage rather than a degradation.
postgres_multi_az                 = true
postgres_allocated_storage_gb     = 200
postgres_max_allocated_storage_gb = 1000
postgres_backup_retention_days    = 30
postgres_deletion_protection      = true

redis_node_type = "cache.t4g.small"
# One replica so a node loss is a failover rather than a cold cache. Redis is
# never a source of truth (ADR-003), so this buys latency, not durability.
redis_replicas_per_node_group = 1

# Provisioned, not serverless. The partition counts in ADR-002 are deliberate,
# and sustained throughput is cheaper on reserved brokers than on
# per-partition-hour pricing.
msk_serverless           = false
msk_broker_instance_type = "kafka.m7g.large"
msk_broker_count         = 3
msk_broker_storage_gb    = 200

# Covers the rolling 28-day SLO window in docs/operations/slo.md.
log_retention_days = 30

# The refusal is the point.
s3_force_destroy = false
