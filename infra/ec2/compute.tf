# The shadow-test host and its security group, transcribed from the running
# account rather than redesigned (docs/PLAN.md D10). Every literal below was
# read back with `aws ec2 describe-instances` / `describe-security-group-rules`
# / `describe-volumes` on 2026-09-01; imports.tf binds each resource to the
# object it describes.

resource "aws_security_group" "app" {
  name        = "mlobs-demo-sg"
  description = "mlobs demo: api 8000 + grafana 3000 public, ssh from admin IP"
  vpc_id      = data.aws_vpc.default.id

  tags = {
    Name = "mlobs-demo"
  }
}

# Rules are modelled one resource per rule (aws_vpc_security_group_*_rule)
# instead of inline ingress/egress blocks, so each carries its own sgr- id and
# can be imported and diffed independently. Every rule that exists on the group
# is represented here, including the default allow-all egress — an unmodelled
# rule would be silently deleted on the first apply.

resource "aws_vpc_security_group_ingress_rule" "api" {
  security_group_id = aws_security_group.app.id
  description       = "api public"
  ip_protocol       = "tcp"
  from_port         = 8000
  to_port           = 8000
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_ingress_rule" "grafana" {
  security_group_id = aws_security_group.app.id
  description       = "grafana public"
  ip_protocol       = "tcp"
  from_port         = 3000
  to_port           = 3000
  cidr_ipv4         = "0.0.0.0/0"
}

# No description as-found; left absent so the plan stays a no-op. The CIDR is
# the operator's home address and arrives from an untracked tfvars file or the
# TF_VAR_ssh_ingress_cidr secret — see variables.tf.
resource "aws_vpc_security_group_ingress_rule" "ssh" {
  security_group_id = aws_security_group.app.id
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
  cidr_ipv4         = var.ssh_ingress_cidr
}

# ip_protocol "-1" means every protocol and port; from_port/to_port must stay
# unset for that to round-trip cleanly.
resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.app.id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_instance" "app" {
  ami                    = var.ami_id
  instance_type          = var.instance_type
  key_name               = var.key_pair_name
  subnet_id              = data.aws_subnet.default.id
  vpc_security_group_ids = [aws_security_group.app.id]

  # IMDSv2 only — the README's deploy claim depends on this staying "required".
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    http_protocol_ipv6          = "disabled"
    instance_metadata_tags      = "disabled"
  }

  # 30 GiB gp3 at the volume-type defaults (3000 IOPS, 125 MiB/s), unencrypted
  # as found. Encryption at rest would force a replacement of the root volume,
  # so it is left as-is here and tracked as a deliberate gap, not fixed by a
  # drive-by change to an imported disk.
  root_block_device {
    volume_type           = "gp3"
    volume_size           = 30
    iops                  = 3000
    throughput            = 125
    encrypted             = false
    delete_on_termination = true

    tags = {
      Name = "mlobs-demo"
    }
  }

  # No user_data: the host was configured by hand over SSH and its userData
  # attribute is empty. Codifying provisioning is out of scope for this slice
  # (see ../README.md, "What this root does not do").

  tags = {
    Name = "mlobs-demo"
  }
}
