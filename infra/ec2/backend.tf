# Remote state on S3 with native object-lock coordination (docs/PLAN.md D9).
#
# `use_lockfile = true` puts the lock in S3 itself (`<key>.tflock`). There is
# deliberately NO DynamoDB table: DynamoDB-based locking is deprecated upstream,
# and a single-writer demo root does not justify a second billed resource.
#
# Backend blocks cannot interpolate — no variables, no locals, no data sources —
# so the bucket name carries the literal account id. The account id is
# accepted exposure for this repo; the SSH ingress CIDR is not (see
# variables.tf) and appears nowhere in version control.
#
# The bucket is created once, by hand, before the first `terraform init`. See
# ../README.md "One-time bootstrap" for the exact commands and their
# verification steps.

terraform {
  backend "s3" {
    bucket       = "mlobs-tfstate-601548053958"
    key          = "ec2/terraform.tfstate"
    region       = "us-west-2"
    use_lockfile = true
  }
}

# The same two values, restated as locals so iam.tf can scope the plan role's
# S3 grants to exactly this bucket and key. Keep the three in sync by hand:
# the backend block above is the source of truth, and a mismatch shows up as
# an AccessDenied on the CI `init`, not as a silent widening of permissions.
locals {
  state_bucket = "mlobs-tfstate-601548053958"
  state_key    = "ec2/terraform.tfstate"
}
