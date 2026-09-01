# GitHub OIDC federation and the read-only role CI assumes to run
# `terraform plan` (docs/PLAN.md D13). No long-lived AWS access keys exist for
# CI: Actions exchanges its short-lived OIDC token for an equally short-lived
# STS session, and there is no secret to leak or rotate.

# AWS validates GitHub's OIDC tokens against its own library of trusted root
# CAs, so thumbprint_list is intentionally omitted — pinning a leaf thumbprint
# here would only create a future outage when GitHub rotates its certificate.
resource "aws_iam_openid_connect_provider" "github" {
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
}

# Trust policy. Two conditions, both mandatory:
#
#   aud — pins the audience to sts.amazonaws.com, the value
#         aws-actions/configure-aws-credentials requests.
#   sub — enumerated, never wildcarded. A `repo:owner/name:*` subject would let
#         any workflow in the repository — including one added by a fork's
#         pull_request_target or a pushed tag — assume this role. The two
#         subjects below are exactly the contexts the CI plan job runs in.
data "aws_iam_policy_document" "plan_assume_role" {
  statement {
    sid     = "GitHubActionsWebIdentity"
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values = [
        "repo:MuratAlkan06/ml-observability-system:pull_request",
        "repo:MuratAlkan06/ml-observability-system:ref:refs/heads/main",
      ]
    }
  }
}

resource "aws_iam_role" "tf_plan" {
  name               = "mlobs-tf-plan"
  description        = "Read-only role assumed by GitHub Actions to run terraform plan for infra/ec2."
  assume_role_policy = data.aws_iam_policy_document.plan_assume_role.json
}

# Permissions are a hand-written customer policy, not the AWS-managed
# ReadOnlyAccess: ReadOnlyAccess grants read across every service in the
# account — Secrets Manager metadata, S3 object listings, DynamoDB scans — none
# of which a plan of this root needs. This policy is the exact read surface of
# the resources declared here, and nothing else.
data "aws_iam_policy_document" "tf_plan" {
  # data.aws_vpc, data.aws_subnet, the instance, the security group, its rules
  # and the root volume. EC2 Describe* is not resource-scopable, so the
  # resource is "*" — bounded instead by the action list being read-only.
  statement {
    sid       = "DescribeEc2"
    effect    = "Allow"
    actions   = ["ec2:Describe*"]
    resources = ["*"]
  }

  # Remote state, read side only. The plan job runs with -lock=false, so no
  # PutObject/DeleteObject on <key>.tflock is granted: this role structurally
  # cannot write state, acquire a lock, or leave a stale one behind.
  statement {
    sid       = "ListStateBucket"
    effect    = "Allow"
    actions   = ["s3:ListBucket"]
    resources = ["arn:aws:s3:::${local.state_bucket}"]
  }

  statement {
    sid       = "ReadStateObject"
    effect    = "Allow"
    actions   = ["s3:GetObject"]
    resources = ["arn:aws:s3:::${local.state_bucket}/${local.state_key}"]
  }

  # Refreshing the IAM resources this root manages — the role and its inline
  # policy — scoped to the role's own ARN. GetPolicy/GetPolicyVersion are inert
  # while the permissions live in an inline policy; if a customer managed
  # policy is ever attached to this role, extend `resources` with that policy's
  # ARN rather than widening the scope.
  statement {
    sid    = "ReadOwnRole"
    effect = "Allow"
    actions = [
      "iam:GetRole",
      "iam:GetRolePolicy",
      "iam:ListRolePolicies",
      "iam:ListAttachedRolePolicies",
      "iam:GetPolicy",
      "iam:GetPolicyVersion",
    ]
    resources = [aws_iam_role.tf_plan.arn]
  }

  statement {
    sid       = "ReadOidcProvider"
    effect    = "Allow"
    actions   = ["iam:GetOpenIDConnectProvider"]
    resources = [aws_iam_openid_connect_provider.github.arn]
  }
}

resource "aws_iam_role_policy" "tf_plan" {
  name   = "mlobs-tf-plan-read"
  role   = aws_iam_role.tf_plan.id
  policy = data.aws_iam_policy_document.tf_plan.json
}
