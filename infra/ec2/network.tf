# The host lives in the account's default VPC and the default subnet of its AZ.
# Both are read, never managed (docs/PLAN.md D10): they predate this root, they
# are shared with anything else in the account, and a `terraform destroy` here
# must not be able to take the account's default networking with it.

data "aws_vpc" "default" {
  default = true
}

data "aws_subnet" "default" {
  vpc_id            = data.aws_vpc.default.id
  availability_zone = var.availability_zone
  default_for_az    = true
}
