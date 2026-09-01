# Inputs for the EC2 shadow-test root. Every default transcribes the host as
# it was found on 2026-09-01 by `aws ec2 describe-*`, so a plan against
# untouched infrastructure is a no-op (docs/PLAN.md D10, D14).

variable "region" {
  description = "AWS region holding the shadow-test host."
  type        = string
  default     = "us-west-2"
}

variable "availability_zone" {
  description = "AZ of the default subnet the host sits in. Selects the subnet data source; changing it would move the host."
  type        = string
  default     = "us-west-2b"
}

variable "instance_type" {
  description = "Instance size the EC2 load-test numbers in README.md were certified on. Changing it invalidates those numbers."
  type        = string
  default     = "t3.medium"
}

variable "ami_id" {
  description = <<-EOT
    AMI backing the host, as found: Canonical
    ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20260714.

    Deliberately a literal, not a `data "aws_ami"` most-recent lookup: a lookup
    re-resolves on every plan, and because `ami` forces replacement on
    aws_instance, the next Canonical publish would silently turn a read-only
    CI plan into a queued destroy/create of the certified host. Moving to a new
    image is an explicit edit of this default plus a re-run of the load test.
  EOT
  type        = string
  default     = "ami-0ac74609c6396bed3"
}

variable "key_pair_name" {
  description = "Name of the pre-existing EC2 key pair authorised for SSH. The key pair itself is not managed here — Terraform never sees private key material."
  type        = string
  default     = "mlobs-demo"
}

variable "ssh_ingress_cidr" {
  description = <<-EOT
    Single /32 allowed to reach port 22, i.e. the operator's home address.
    Personally identifying, so it has NO default and is never committed:
    supply it locally through the gitignored terraform.tfvars, and in CI
    through the TF_VAR_ssh_ingress_cidr Actions secret (../README.md).

    `sensitive = true` keeps it out of plan output and CI logs. It still lands
    in remote state in the clear — which is why the state bucket is private,
    encrypted and versioned, and why the bootstrap runbook ends with a state
    secret sweep.
  EOT
  type        = string
  sensitive   = true
}
