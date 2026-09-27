# The deploy pipeline (docs/PLAN.md D24, D28–D30): the SSM document the deploy
# workflow runs, the roles around it, and the private bucket the shadow-scorer
# image travels through. One concern, one file (D12): iam.tf keeps the OIDC
# provider and the read-only plan role, and the plan role's read-back of
# everything below is granted there, alongside the rest of what it may read.
#
# Every GitHub-facing role here follows iam.tf's trust rules: `aud` pinned, and
# exactly one `sub`, spelled out in full rather than wildcarded, so the subject
# a role trusts can be found by grepping for it.

# -----------------------------------------------------------------------------
# Artifacts bucket
# -----------------------------------------------------------------------------
#
# The shadow-scorer image is the one image not on GHCR: the model it bakes is
# published on Hugging Face with no upstream licence stated, and this repository
# does not republish those weights (D18). ShadowPublish saves it as a tarball
# and uploads it here instead, one object per commit: shadow/<sha>.tar.gz.
#
# Hardened as the state bucket is (../README.md, "One-time bootstrap") —
# private, encrypted, public access blocked, plaintext transport refused — with
# one difference: this bucket is managed by Terraform. Nothing about it has to
# exist before the first apply, so there is no bootstrap chicken-and-egg.
#
# Versioning is deliberately off. Every key is a commit SHA written once, and a
# noncurrent version would only be a second copy of the weights outliving the
# 60-day expiry below.
resource "aws_s3_bucket" "artifacts" {
  bucket = "mlobs-artifacts-601548053958"
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# SSE-S3, as on the state bucket. New buckets get this by default today; it is
# stated here so the property is in code rather than in an AWS default.
resource "aws_s3_bucket_server_side_encryption_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Refuse plaintext transport. The public access block stops anonymous callers;
# it says nothing about the transport an authorised one uses.
data "aws_iam_policy_document" "artifacts_bucket" {
  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.artifacts.arn,
      "${aws_s3_bucket.artifacts.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  policy = data.aws_iam_policy_document.artifacts_bucket.json

  # A Deny is not a public policy, so BlockPublicPolicy would accept it either
  # way; ordering the two removes the race rather than leaning on that.
  depends_on = [aws_s3_bucket_public_access_block.artifacts]
}

# Sixty days bounds how long any copy of the weights sits here, and is also the
# rollback window for the shadow image: a redeploy of a commit older than that
# fails the deploy workflow's tarball preflight, loudly, rather than half-
# deploying. The multipart rule sweeps the parts of an upload whose run was
# cancelled — an incomplete upload is never a visible object, but it is billed.
resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    id     = "expire-shadow-tarballs"
    status = "Enabled"

    filter {
      prefix = "shadow/"
    }

    expiration {
      days = 60
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# -----------------------------------------------------------------------------
# The deploy document
# -----------------------------------------------------------------------------
#
# The security boundary of the whole pipeline (D24, D29). Whoever can send this
# document can make the host run exactly the script below, as root, with one
# substitution: a commit SHA that SSM rejects unless it is 40 lowercase hex
# characters. That pattern is checked twice by AWS — by the API when the command
# is sent and again by the agent on the host before it runs — so the value that
# reaches the shell cannot carry a quote, a space or a metacharacter. The SHA is
# then re-checked on the host against a freshly fetched origin/main, so only a
# commit on main deploys, whatever the caller claimed — and after the checkout,
# HEAD is compared with the SHA itself. The two checks read the same string
# differently: `merge-base` resolves 40 hex digits as an object id, while
# `checkout` prefers a local branch of that name, so a branch named like a main
# SHA would otherwise pass the ancestry check and put other code under
# apply.sh.
#
# Where this differs from the gate-frozen sequence (the first three items come
# from the security review of PR #57):
#   - the order: the tarball is downloaded and imported before the checkout,
#     so a missing or expired tarball fails with the working tree where it was
#     rather than half-moved to the new commit.
#   - a clean-tree gate before anything changes: a tracked file edited on the
#     host would be carried through the checkout into what apply.sh runs, so
#     the host refuses instead. Untracked files — the host's .env among them —
#     are not considered.
#   - the HEAD check after the checkout, described above.
#   - `set -eu`: runCommand lines are joined into one script, which otherwise
#     carries on past a failing line. The image steps are separate lines, not
#     one `&&` chain, because errexit ignores every command but the last in an
#     AND-list, so a failed gunzip would otherwise go on to run apply.sh.
#   - an explicit PATH: a root script does not inherit its search path from
#     whatever environment the agent happens to have. /snap/bin is included
#     because Ubuntu's own sudo secure_path includes it.
#   - `--no-progress` and `--region` on the copy: the agent keeps only the first
#     24,000 characters of stdout, and a progress meter for a ~500 MB object
#     would spend them before apply.sh's fixed lines — the log this document is
#     judged by — were ever written. The region is pinned rather than resolved.
#
# timeoutSeconds is 840 (14 minutes). A deploy is a few minutes: apply.sh bounds
# each rollout at 180 s and they progress concurrently, and the smoke's target
# poll is bounded at 120 s. The deploy workflow allows 60 s for delivery and
# polls for 16 minutes, so the host always ends the command before the workflow
# stops watching it.
#
# Changing `content` creates a new document version and leaves the old ones
# callable: SendCommand takes a --document-version, and IAM has no condition
# key to pin it. Every change here is therefore followed by deleting the
# superseded versions (docs/PLAN.md D29).
resource "aws_ssm_document" "mlobs_deploy" {
  name            = "mlobs-deploy"
  document_type   = "Command"
  document_format = "YAML"

  content = <<-YAML
    schemaVersion: "2.2"
    description: "mlobs deploy (docs/PLAN.md D24, D29). Checks out one commit of main on the host and runs deploy/k3s/apply.sh with IMAGE_TAG set to it. Fixed script; the only input is a commit SHA."
    parameters:
      Sha:
        type: String
        description: "Full 40-character lowercase commit SHA to deploy. Must be an ancestor of origin/main."
        allowedPattern: "^[0-9a-f]{40}$"
    mainSteps:
      - action: aws:runShellScript
        name: deploy
        inputs:
          timeoutSeconds: "840"
          runCommand:
            - 'set -eu'
            - 'export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin'
            - 'cd /home/ubuntu/ml-observability-system'
            - 'sudo -u ubuntu git fetch origin main'
            - 'sudo -u ubuntu git merge-base --is-ancestor {{Sha}} origin/main || { echo "refusing: {{Sha}} is not an ancestor of origin/main"; exit 1; }'
            - 'test -z "$(sudo -u ubuntu git status --porcelain --untracked-files=no)" || { echo "refusing: tracked files modified on host"; exit 1; }'
            - 'aws s3 cp --no-progress --region ${var.region} s3://${aws_s3_bucket.artifacts.bucket}/shadow/{{Sha}}.tar.gz /tmp/shadow.tar.gz'
            - 'gunzip -f /tmp/shadow.tar.gz'
            - 'k3s ctr images import /tmp/shadow.tar'
            - 'rm -f /tmp/shadow.tar'
            - 'sudo -u ubuntu git checkout {{Sha}}'
            - 'test "$(sudo -u ubuntu git rev-parse HEAD)" = "{{Sha}}" || { echo "refusing: checkout did not land on {{Sha}}"; exit 1; }'
            - 'sudo -u ubuntu env KUBECONFIG=/home/ubuntu/.kube/config IMAGE_TAG={{Sha}} ./deploy/k3s/apply.sh'
  YAML
}

# -----------------------------------------------------------------------------
# mlobs-deploy: the role the deploy workflow assumes
# -----------------------------------------------------------------------------
#
# Trust is the `ec2-deploy` environment subject and nothing else. GitHub issues
# that subject only to a job that declares `environment: ec2-deploy`, and such a
# job does not start until the environment's required reviewer approves it — so
# every session of this role has passed that approval. Not the main-branch
# subject: that one belongs to every job that runs on main, and this role must
# not be one merge away from any of them.
data "aws_iam_policy_document" "deploy_assume_role" {
  statement {
    sid     = "GitHubActionsDeployEnvironment"
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
      values   = ["repo:MuratAlkan06/ml-observability-system:environment:ec2-deploy"]
    }
  }
}

resource "aws_iam_role" "deploy" {
  name               = "mlobs-deploy"
  description        = "Assumed by the ec2-deploy environment job in deploy.yml to send the mlobs-deploy SSM document to the shadow-test host."
  assume_role_policy = data.aws_iam_policy_document.deploy_assume_role.json
}

# What the role cannot do matters as much as what it can. It holds no
# ec2:StartInstances, so a deploy never starts a billed host (D24); no
# ssm:UpdateDocument, so it cannot change what the document runs; and
# SendCommand is scoped to one document and one instance, so neither an AWS-*
# document nor a second host is reachable through it.
data "aws_iam_policy_document" "deploy" {
  # SendCommand is authorised against both the document and each target, and
  # both are named here — the document as the only thing it may run, the host as
  # the only place it may run it.
  statement {
    sid     = "SendDeployDocumentToHost"
    effect  = "Allow"
    actions = ["ssm:SendCommand"]
    resources = [
      aws_ssm_document.mlobs_deploy.arn,
      aws_instance.app.arn,
    ]
  }

  # GetCommandInvocation defines no resource type and no condition key in the
  # Service Authorization Reference, so "*" is the only resource IAM accepts for
  # it: any narrower ARN would simply never match. What it returns is the status
  # and output of a command whose id the caller already holds — read access to
  # Run Command results in this account, the same single-workload acceptance as
  # DescribeEc2 in iam.tf.
  statement {
    sid       = "ReadCommandInvocation"
    effect    = "Allow"
    actions   = ["ssm:GetCommandInvocation"]
    resources = ["*"]
  }

  # The "instance is running" preflight. EC2 Describe actions do not support
  # resource-level permissions (see DescribeEc2 in iam.tf), hence "*".
  statement {
    sid       = "ReadInstanceState"
    effect    = "Allow"
    actions   = ["ec2:DescribeInstances"]
    resources = ["*"]
  }

  # DISCLOSED ADDITION to the gate-frozen P2c role policy (docs/PLAN.md D28),
  # as narrowed by the security review of PR #57. The workflow's tarball
  # preflight only has to learn that shadow/<sha>.tar.gz exists, so it lists
  # shadow/ and looks for the key. The first version headed the object
  # instead, which S3 authorises as s3:GetObject: the right to download the
  # weights, held by a role that never needs them. ListBucket is a bucket-level
  # action, so the prefix condition does the scoping — a list is allowed only
  # with the prefix exactly `shadow/`. What comes back is key names (commit
  # SHAs), sizes and dates, never an object's content.
  #
  # s3:ResourceAccount, here and on the other two grants of this bucket, pins
  # the account that owns the bucket as well as its name. Bucket names are
  # global: if this bucket were ever deleted and the name claimed by another
  # account, a grant by name alone would follow it there — the publish role
  # writing the weights into it, the host importing whatever image it served.
  statement {
    sid       = "ListShadowTarballs"
    effect    = "Allow"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.artifacts.arn]

    condition {
      test     = "StringEquals"
      variable = "s3:prefix"
      values   = ["shadow/"]
    }

    condition {
      test     = "StringEquals"
      variable = "s3:ResourceAccount"
      values   = ["601548053958"]
    }
  }
}

resource "aws_iam_role_policy" "deploy" {
  name   = "mlobs-deploy-send-command"
  role   = aws_iam_role.deploy.id
  policy = data.aws_iam_policy_document.deploy.json
}

# -----------------------------------------------------------------------------
# mlobs-artifact-publish: the role ShadowPublish assumes
# -----------------------------------------------------------------------------
#
# Trust is the `shadow-publish` environment subject: mlobs-deploy's shape,
# without the reviewer. GitHub issues it only to a job that declares
# `environment: shadow-publish`, and the environment's deployment branch rule
# admits only runs on main (infra/README.md, "Deploy pipeline"). Not the
# main-branch subject (security review of PR #57): a subject names a context,
# not a job, and that one is issued to every job that runs on main —
# TerraformPlan among them. It matters because of what this role writes. The
# host imports whatever sits at shadow/<sha>.tar.gz and runs it as the shadow
# scorer, so a write here is code on the host. Behind the trust is one write
# into one prefix: it can add tarballs and do nothing else, not even read
# them back.
data "aws_iam_policy_document" "artifact_publish_assume_role" {
  statement {
    sid     = "GitHubActionsShadowPublishEnvironment"
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
      values   = ["repo:MuratAlkan06/ml-observability-system:environment:shadow-publish"]
    }
  }
}

resource "aws_iam_role" "artifact_publish" {
  name               = "mlobs-artifact-publish"
  description        = "Assumed by the ShadowPublish CI job on pushes to main to upload the shadow-scorer image tarball to the artifacts bucket."
  assume_role_policy = data.aws_iam_policy_document.artifact_publish_assume_role.json
}

# s3:PutObject also authorises the multipart calls `aws s3 cp` makes for an
# object this size. Aborting a failed multipart upload would need
# s3:AbortMultipartUpload; it is not granted, and the lifecycle rule above
# sweeps the parts instead. s3:ResourceAccount as on mlobs-deploy's grant.
data "aws_iam_policy_document" "artifact_publish" {
  statement {
    sid       = "PutShadowTarball"
    effect    = "Allow"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.artifacts.arn}/shadow/*"]

    condition {
      test     = "StringEquals"
      variable = "s3:ResourceAccount"
      values   = ["601548053958"]
    }
  }
}

resource "aws_iam_role_policy" "artifact_publish" {
  name   = "mlobs-artifact-publish-put"
  role   = aws_iam_role.artifact_publish.id
  policy = data.aws_iam_policy_document.artifact_publish.json
}

# -----------------------------------------------------------------------------
# mlobs-host: the instance role and profile
# -----------------------------------------------------------------------------
#
# The SSM agent on the host needs an identity to register with Systems Manager
# and to receive commands at all; AmazonSSMManagedInstanceCore is the AWS-managed
# policy for exactly that. The credentials are served by IMDSv2 with a hop limit
# of 1 (compute.tf), which keeps them out of reach of the pods: a container's
# own network namespace is one hop further than that.
data "aws_iam_policy_document" "host_assume_role" {
  statement {
    sid     = "Ec2AssumeRole"
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "host" {
  name               = "mlobs-host"
  description        = "Instance role for the shadow-test host: SSM agent registration and read of shadow-scorer image tarballs."
  assume_role_policy = data.aws_iam_policy_document.host_assume_role.json
}

resource "aws_iam_role_policy_attachment" "host_ssm_core" {
  role       = aws_iam_role.host.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# DISCLOSED ADDITION to the gate-frozen P2c design (docs/PLAN.md D28). The
# design gave the host AmazonSSMManagedInstanceCore alone, but the deploy
# document downloads the shadow tarball on the host, as root, with the host's
# own credentials — so the host must be able to read it. Read only, only under
# shadow/, and s3:ResourceAccount as on mlobs-deploy's grant. It is the one
# role that still reads the objects: the deploy role lists, it does not read.
data "aws_iam_policy_document" "host_shadow_read" {
  statement {
    sid       = "ReadShadowTarballs"
    effect    = "Allow"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.artifacts.arn}/shadow/*"]

    condition {
      test     = "StringEquals"
      variable = "s3:ResourceAccount"
      values   = ["601548053958"]
    }
  }
}

resource "aws_iam_role_policy" "host_shadow_read" {
  name   = "mlobs-host-shadow-read"
  role   = aws_iam_role.host.id
  policy = data.aws_iam_policy_document.host_shadow_read.json
}

resource "aws_iam_instance_profile" "host" {
  name = "mlobs-host"
  role = aws_iam_role.host.name
}
