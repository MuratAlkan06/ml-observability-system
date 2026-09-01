# Import-in-place: adopt the hand-built host into state without recreating it
# (docs/PLAN.md D10). These blocks are config, not a one-shot CLI invocation —
# they are reviewable, they run inside the normal plan/apply, and they make the
# adoption reproducible for anyone reading the PR.
#
# The ids below are the real objects in account 601548053958 / us-west-2.
# Resource ids are accepted exposure for this repo; the SSH CIDR is not, and
# is nowhere in this directory.
#
# Import blocks are idempotent once the objects are in state: after the
# bootstrap apply, later plans see them as already-managed and no-op. They stay
# committed as the documented provenance of every resource in compute.tf.

import {
  to = aws_instance.app
  id = "i-0ed558a5144e76f4d"
}

import {
  to = aws_security_group.app
  id = "sg-0406bb41c9a1c76fb"
}

# One import per security-group rule. Rule ids are per-rule, not per-group, so
# the four below cover the group completely: tcp/8000, tcp/3000, tcp/22 and the
# default allow-all egress.

import {
  to = aws_vpc_security_group_ingress_rule.api
  id = "sgr-01aa4f16b5235e4cb"
}

import {
  to = aws_vpc_security_group_ingress_rule.grafana
  id = "sgr-0c1d9faea74e8404f"
}

import {
  to = aws_vpc_security_group_ingress_rule.ssh
  id = "sgr-08774e87def4125c0"
}

import {
  to = aws_vpc_security_group_egress_rule.all
  id = "sgr-056aa6d4b3a3cf875"
}
