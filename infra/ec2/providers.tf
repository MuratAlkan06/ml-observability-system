# AWS provider for the single region this root owns (docs/PLAN.md D14).
#
# default_tags stamp every taggable resource in the root. The imported
# instance, security group, root volume and per-rule resources carry only a
# `Name` tag today, so the first apply adds these two tags in place — an
# additive tag update, never a replacement. Nothing as-found conflicts with
# them, so reality is transcribed as-is and the defaults ride on top; see
# ../README.md for the expected first-apply diff.

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      project    = "mlobs"
      managed-by = "terraform"
    }
  }
}
