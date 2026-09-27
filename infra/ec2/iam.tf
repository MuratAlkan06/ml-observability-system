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
# of which a plan of this root needs. What this policy narrows is the set of
# services reachable at all: EC2, one state object, and this root's own IAM
# objects — plus, since P2c, the one SSM document and the configuration of the
# one S3 bucket that deploy.tf manages. Within EC2 it is not a per-resource
# grant, and the statement below says so plainly rather than implying a
# tighter boundary than IAM can express.
data "aws_iam_policy_document" "tf_plan" {
  # What the plan actually reads is data.aws_vpc, data.aws_subnet, the
  # instance, the security group, its rules and the root volume. `ec2:Describe*`
  # cannot say that: EC2 Describe actions do not support resource-level
  # permissions, so IAM ignores the resource element and "*" is the only form
  # that works. What this grants is therefore read-only, account-wide EC2
  # metadata — roughly 200 actions, among them DescribeInstanceAttribute, which
  # returns userData for any instance in account 601548053958, not just ours.
  #
  # Accepted because the account currently holds exactly one workload: this
  # demo. Revisit when a second one lands — the fix then is an enumerated
  # action list plus a condition key, not a resource ARN, which Describe would
  # go on ignoring.
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

  # --- P2c: read-back of everything deploy.tf creates (docs/PLAN.md D28–D30) --
  #
  # Granted in the same change that creates those objects, and that ordering is
  # the point. The plan on the P2c pull request only has to *create* them, which
  # reads nothing; every plan after the owner's apply has to *refresh* them, and
  # would fail on an AccessDenied the moment they exist if these grants arrived
  # a change later. Each list covers what the locked provider's (6.62.0) read
  # path calls for that resource type; the few read-only extras beyond that are
  # named where they appear.

  # The three P2c roles, read the same way ReadOwnRole reads this one. The
  # extra is ListInstanceProfilesForRole, which the provider calls only while
  # deleting a role — an apply, never a plan. It is read-only, inert on
  # refresh, and granted because the P2c design enumerates it.
  statement {
    sid    = "ReadDeployPipelineRoles"
    effect = "Allow"
    actions = [
      "iam:GetRole",
      "iam:GetRolePolicy",
      "iam:ListRolePolicies",
      "iam:ListAttachedRolePolicies",
      "iam:ListInstanceProfilesForRole",
    ]
    resources = [
      aws_iam_role.deploy.arn,
      aws_iam_role.artifact_publish.arn,
      aws_iam_role.host.arn,
    ]
  }

  statement {
    sid       = "ReadHostInstanceProfile"
    effect    = "Allow"
    actions   = ["iam:GetInstanceProfile"]
    resources = [aws_iam_instance_profile.host.arn]
  }

  # DescribeDocumentPermission is not optional: the provider reads the
  # document's sharing permissions on every refresh and fails the read if it
  # is denied. Tags come back on DescribeDocument today; ListTagsForResource is
  # granted so a provider change to that path cannot turn a no-op plan red.
  statement {
    sid    = "ReadDeployDocument"
    effect = "Allow"
    actions = [
      "ssm:DescribeDocument",
      "ssm:GetDocument",
      "ssm:DescribeDocumentPermission",
      "ssm:ListTagsForResource",
    ]
    resources = [aws_ssm_document.mlobs_deploy.arn]
  }

  # Bucket configuration only, on the bucket ARN only. aws_s3_bucket's read
  # still queries every legacy sub-resource — ACL, CORS, website, accelerate,
  # request payment, logging, replication, object lock — and fails on a denied
  # one, so the list is longer than what deploy.tf configures. s3:ListBucket is
  # what HeadBucket, the provider's existence and region check, is authorised
  # as; it lists keys, which are commit SHAs. GetBucketLocation is the extra —
  # the provider resolves the region through HeadBucket. There is no object
  # grant: this role cannot read a tarball.
  statement {
    sid    = "ReadArtifactsBucketConfig"
    effect = "Allow"
    actions = [
      "s3:ListBucket",
      "s3:GetBucketLocation",
      "s3:GetBucketPolicy",
      "s3:GetBucketAcl",
      "s3:GetBucketCORS",
      "s3:GetBucketWebsite",
      "s3:GetBucketVersioning",
      "s3:GetAccelerateConfiguration",
      "s3:GetBucketRequestPayment",
      "s3:GetBucketLogging",
      "s3:GetLifecycleConfiguration",
      "s3:GetReplicationConfiguration",
      "s3:GetEncryptionConfiguration",
      "s3:GetBucketObjectLockConfiguration",
      "s3:GetBucketPublicAccessBlock",
      "s3:GetBucketTagging",
      "s3:ListTagsForResource",
    ]
    resources = [aws_s3_bucket.artifacts.arn]
  }
}

resource "aws_iam_role_policy" "tf_plan" {
  name   = "mlobs-tf-plan-read"
  role   = aws_iam_role.tf_plan.id
  policy = data.aws_iam_policy_document.tf_plan.json
}
