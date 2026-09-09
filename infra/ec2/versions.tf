# Toolchain and provider constraints for the EC2 shadow-test root (docs/PLAN.md D9).
#
# required_version is a patch-band on 1.14: the S3 backend's `use_lockfile`
# behaviour and the `import` block semantics this root depends on are 1.14-era,
# and a silent jump to 1.15 would be an unreviewed change to both.
#
# The AWS provider is held to a MINOR band (~> 6.33 = >= 6.33.0, < 7.0.0) so
# Dependabot can offer 6.x bumps as reviewable PRs while a 7.x major — which
# would be a breaking-schema event for the imported resources — cannot land
# without a deliberate edit here.

terraform {
  required_version = "~> 1.14.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.33"
    }
  }
}
