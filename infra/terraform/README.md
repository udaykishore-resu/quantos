# QuantOS — Terraform

AWS infrastructure for the `cluster` runtime mode (`config/quantos.yaml`,
`mode: cluster`). The other two modes need none of this: `embedded` runs the
whole platform in one process with in-memory stores, and `compose` runs it on a
laptop from `deploy/docker-compose.yml`. See `docs/operations/deployment.md`.

```
modules/
  network        VPC, public/private subnets, NAT, S3 gateway endpoint, flow logs
  observability  CloudWatch log groups + the KMS key CloudWatch will actually accept
  eks            control plane, managed node groups, IRSA/OIDC provider, addons
  storage        S3 (models, backtests, logs) + KMS
  data           RDS PostgreSQL, ElastiCache Redis, Secrets Manager + KMS
  messaging      MSK (serverless in dev, provisioned in prod) + KMS
  iam            one IRSA role per service, scoped to what that service touches
environments/
  dev/           dev.tfvars   — cheap, single-AZ, destroyable
  prod/          prod.tfvars  — multi-AZ, deletion-protected, private API server
```

Provider versions are pinned in every module (`aws ~> 5.60`, `random ~> 3.6`,
`tls ~> 4.0`) and `required_version >= 1.6.0`.

## Apply order

The module graph already encodes the dependencies, so a single `terraform apply`
gets the order right. It matters when you are applying a subset with `-target`,
or reading a plan and wondering why something is waiting:

1. **network** — everything else needs subnets.
2. **observability** — owns the CloudWatch KMS key. `messaging` takes that key
   as an input, because CloudWatch Logs rejects a customer key whose policy does
   not name the regional `logs.` service principal, and that grant belongs on
   one key rather than on every key in the account.
3. **eks** — creates the OIDC provider and the node security group. Both data
   modules allow ingress from that group and from nothing else.
4. **storage**, **data**, **messaging** — independent of each other; they run in
   parallel. `data` and `messaging` both need `eks`'s node security group.
5. **iam** — last, because every policy in it is scoped to an ARN produced by
   one of the modules above.

```sh
cd environments/dev

terraform init \
  -backend-config=bucket=quantos-tfstate-<account-id> \
  -backend-config=key=dev/terraform.tfstate \
  -backend-config=region=us-east-1 \
  -backend-config=dynamodb_table=quantos-tfstate-lock \
  -backend-config=encrypt=true

terraform plan  -var-file=dev.tfvars -out=dev.plan
terraform apply dev.plan
```

A full apply from empty takes roughly 25 minutes in dev and 40 in prod; the
long poles are the EKS control plane (~10 min), RDS multi-AZ (~15 min) and MSK
provisioned brokers (~25 min).

Then hand the outputs to the manifests:

```sh
aws eks update-kubeconfig --name "$(terraform output -raw cluster_name)"
terraform output -json service_role_arns   # -> ServiceAccount annotations
terraform output -raw kafka_bootstrap_brokers
```

## What is deliberately NOT automated

Each of these is a decision, not an omission.

**The state backend itself.** `environments/*/backend.tf` declares an empty
`backend "s3" {}` and takes its configuration at `init` time. A configuration
cannot create the bucket it stores its own state in. Bootstrap once, by hand:

```sh
aws s3api create-bucket --bucket quantos-tfstate-<account-id> --region us-east-1
aws s3api put-bucket-versioning --bucket quantos-tfstate-<account-id> \
  --versioning-configuration Status=Enabled
aws s3api put-public-access-block --bucket quantos-tfstate-<account-id> \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws dynamodb create-table --table-name quantos-tfstate-lock \
  --attribute-definitions AttributeName=LockID,AttributeType=S \
  --key-schema AttributeName=LockID,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST
```

**The LLM API key value.** `modules/data` creates the Secrets Manager *container*
`<name>/llm-api-key` and never writes to it. A key in a variable default is a
key in git; a key in a `.tfvars` is a key in git; a key in a plan output is a
key in every CI log that renders a plan. Populate it out of band:

```sh
aws secretsmanager put-secret-value \
  --secret-id quantos-prod/llm-api-key \
  --secret-string '{"api_key":"<paste>"}'
```

The key is granted to `signal-service` and `news-service` only. `api-gateway`
is the process that renders responses to a browser and must not hold it.

**ClickHouse.** ADR-003 requires it and AWS has no managed equivalent. There is
no honest way to express "run ClickHouse" as an AWS resource, so this tree does
not pretend to. Two supported paths:

- ClickHouse Cloud, provisioned through their own console or provider, peered
  into this VPC. Set `QUANTOS_CLICKHOUSE_URL` from the resulting endpoint.
- Self-managed on EKS via the Altinity operator, on the `platform` node pool
  with its own storage class.

Either way `store.driver` stays `sql` and only `clickhouse_url` changes. The
platform degrades correctly without ClickHouse — `SQL.Degraded()` reports it and
the API says so in every response envelope — whereas it stops emitting signals
without Postgres (`App.CanEmitSignals`). That asymmetry is why Postgres is
managed here and ClickHouse is not.

**Kafka topics.** Terraform has no first-class Kafka resource, and the
partition counts are already authoritative in `internal/bus.TopicConfigs`. The
`Job` in `infra/kubernetes/base/topics-job.yaml` runs inside the VPC and creates
them from that same specification, which is also what
`deploy/kafka/create-topics.sh` does locally. Two renderings of one source of
truth, rather than three.

**Database schema.** `deploy/sql/postgres/001_schema.sql` and its ClickHouse
counterpart are applied by `scripts/db-migrate.sh`, not by Terraform. A schema
change and an infrastructure change have different review requirements and
different rollback stories, and coupling them means a bad migration needs a
`terraform apply` to undo.

**Kubernetes objects.** Nothing in this tree installs a Deployment, a Helm
release or an ArgoCD Application. Terraform provisions the cluster; ArgoCD
(`infra/argocd/`) reconciles what runs on it. Mixing the two produces a state
file that thinks it owns objects a controller is actively changing.

**The External Secrets and AWS Load Balancer Controller charts.** Their IRSA
roles are created here (`modules/iam`); the charts themselves are cluster
add-ons and are installed by the same GitOps path as the application.

**DNS and certificates.** `infra/kubernetes/base/ingress.yaml` references an
ACM certificate ARN and a hostname. Both are environment-specific and usually
live in a different account or a different team's zone.

## Cost

Every resource that bills is commented at its definition with an approximate
`us-east-1` rate. The per-environment totals are at the top of `dev.tfvars` and
`prod.tfvars`: roughly **$860/month** for dev and **$1,575/month** for prod if
left running, before data transfer.

Two things dominate and are worth knowing before an apply:

- **MSK.** Serverless bills ~$0.75/hr per cluster whether or not anything is
  producing — about $548/month for an idle dev cluster. A dev environment used
  for an hour a day should be destroyed at the end of the day, or should skip
  Kafka entirely and run the WAL bus driver as the compose profile does.
- **Cross-AZ data transfer.** MSK replication and RDS multi-AZ both cross zones
  continuously and are billed per GB. It is not in the figures above because it
  depends on throughput, and it is the line that surprises people.

## Security posture

- No resource is publicly addressable. RDS, ElastiCache and MSK live in private
  subnets with `publicly_accessible = false` and security groups that reference
  the EKS node group rather than a CIDR.
- Every S3 bucket has all four public-access blocks, versioning, KMS encryption
  and a bucket policy denying non-TLS requests.
- Every KMS key has rotation enabled and a 30-day deletion window.
- Every workload role carries a permissions boundary (`modules/iam`) that names
  the service categories QuantOS uses and denies privilege escalation. There is
  no payments, brokerage or order-routing action anywhere in it: this is a
  paper-trading and research platform, and `internal/paper` refuses to start if
  `QUANTOS_ALLOW_REAL_MONEY` is set.
- There is no wildcard IAM action in this tree and no wildcard resource outside
  a bucket-object prefix.
- No credential, key or token appears in any file here. Passwords are generated
  by `random_password` and land in Secrets Manager; the one secret Terraform
  cannot generate is the one it does not write.
