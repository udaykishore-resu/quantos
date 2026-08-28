# Remote state.
#
# The bucket and the lock table are NOT created by this configuration: a
# configuration cannot create the store it keeps its own state in without a
# bootstrap step that is itself unmanaged. See the "Not automated" section of
# infra/terraform/README.md for the two commands that create them.
#
# Values are supplied at init time rather than hard-coded, so the same tree
# works against a different account without an edit:
#
#   terraform init \
#     -backend-config=bucket=quantos-tfstate-<account-id> \
#     -backend-config=key=prod/terraform.tfstate \
#     -backend-config=region=us-east-1 \
#     -backend-config=dynamodb_table=quantos-tfstate-lock \
#     -backend-config=encrypt=true
#
# CI runs `terraform init -backend=false` for validation, which needs none of
# this and reaches no network.
terraform {
  backend "s3" {}
}
