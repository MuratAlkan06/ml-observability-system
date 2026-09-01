# Infrastructure as code

Terraform for the single EC2 host the README's load-test and shadow-comparison
numbers were certified on. The host was built by hand in July 2026; this
directory adopts it into Terraform **in place** — nothing is recreated, and the
certified instance keeps its id, its volume and its measurements.

Decisions behind everything here are D9–D16 in [`../docs/PLAN.md`](../docs/PLAN.md).

## Layout

```
infra/
  README.md          <- this file: bootstrap runbook, cost note, gaps
  ec2/               <- one flat root module, no submodules (D12)
    versions.tf      Terraform + provider version bands
    backend.tf       S3 remote state (+ bucket/key restated as locals)
    providers.tf     AWS provider, default_tags
    network.tf       default VPC / default subnet, read-only data sources
    compute.tf       instance, security group, one resource per SG rule
    iam.tf           GitHub OIDC provider + scoped read-only plan role
    variables.tf     inputs; ssh_ingress_cidr has no default, on purpose
    outputs.tf       instance_id, sg_id
    imports.tf       import blocks binding each resource to its real id
    .terraform.lock.hcl   committed; pins provider hashes for linux_amd64 + darwin_arm64
```

One root, flat files by concern — deliberately not a `modules/` tree (D12).
There is exactly one instantiation of this configuration and no second consumer
to parameterise for, so a module layer would add indirection and buy nothing.
It becomes worth revisiting when a second environment exists.

## What this root does not do

Stated plainly, because the gap matters more than the coverage:

- **It does not prove the host can be rebuilt from code.** These resources were
  imported, not created by Terraform. A real `apply` from an empty state has
  never been exercised, so "reproducible from source" is not a claim this slice
  earns. That evidence is deferred to the P3 ephemeral cluster run, which
  creates and destroys everything it uses.
- **It does not provision the host.** The instance's `userData` is empty; Docker,
  the compose stack and the `.env` were configured over SSH by hand. Codifying
  that is out of scope here.
- **It does not encrypt the root volume.** The as-found volume is unencrypted,
  and turning encryption on would force a replacement of the certified disk.
  Recorded as a known gap rather than fixed by a drive-by change. The
  account-level half of the remediation is separate and touches nothing that
  exists: `aws ec2 enable-ebs-encryption-by-default --region us-west-2` makes
  every *future* volume in the region encrypted at creation, so the gap stops
  growing while the certified disk stays exactly as measured. It is a regional
  account setting, not Terraform state, which is why it is written here rather
  than added to this root. Retiring the existing unencrypted volume needs a
  snapshot, an encrypted copy and a stop/detach/attach swap — deferred to P3,
  once the numbers that volume carries are no longer the ones being cited.
- **It does not narrow the public ingress.** Ports 8000 and 3000 are open to
  `0.0.0.0/0` as found, which is why the host only runs during a demo window.

## Local use

Terraform `~> 1.14.0`, AWS provider `~> 6.33`. Credentials come from the
operator's normal AWS profile.

```bash
cat > infra/ec2/terraform.tfvars <<'EOF'
ssh_ingress_cidr = "A.B.C.D/32"
EOF

terraform -chdir=infra/ec2 init
terraform -chdir=infra/ec2 plan -input=false
```

`terraform.tfvars` is gitignored and must stay that way: it holds the operator's
home address. Terraform picks it up automatically, so no `-var-file` flag is
needed.

Credential-less checks, exactly what CI runs:

```bash
terraform fmt -check -recursive
terraform -chdir=infra/ec2 init -backend=false
terraform -chdir=infra/ec2 validate
```

### Provider bumps and the lock file

Dependabot watches `infra/ec2` for provider updates. `.terraform.lock.hcl`
records hashes for two platforms on purpose — `linux_amd64` for CI runners and
`darwin_arm64` for the operator's laptop — and Terraform only adds hashes for
the platform it happens to be running on. A lock regenerated on the laptop
therefore carries no Linux hashes, and CI's `init` then fails a checksum check
rather than a version check. The error names the provider and the missing hash,
not the missing platform, so it reads like a supply-chain alarm when it is a
one-line omission.

Check the first Dependabot bump's diff for both platform blocks before merging.
If either is absent, regenerate the lock with both named explicitly and push
that onto the bump branch:

```bash
terraform -chdir=infra/ec2 providers lock \
  -platform=linux_amd64 -platform=darwin_arm64
```

## Handling of the SSH ingress CIDR

`var.ssh_ingress_cidr` is the operator's home IP. It is `sensitive = true`, has
no default, and appears in no committed file. Three things worth knowing:

1. **Terraform's `sensitive` marking is not sufficient on its own.** It hides
   values *derived from* the variable, but `aws_security_group`'s computed
   `ingress` attribute is read back from the EC2 API, so Terraform prints the
   CIDR in clear text whenever the security group appears in a plan diff —
   including the one-time import plan.
2. **What actually keeps it out of the public Actions logs is GitHub's secret
   masking.** Store the value as a repository secret in exactly the form it
   appears on the wire — `A.B.C.D/32`, no spaces, no quotes — and Actions
   replaces every literal occurrence in the log with `***`. A mismatched form
   (bare IP without `/32`, or a trailing newline) silently defeats the masking.
   `var.ssh_ingress_cidr` now carries a validation block rejecting anything but
   `A.B.C.D/32`, so a mismatched secret fails the plan instead of publishing the
   value it was supposed to hide.
3. **CI no longer prints the plan body.** The `TerraformPlan` job writes the
   plan to a file it never echoes, and on a non-no-op emits only resource
   addresses and actions, so masking is the second line of defence rather than
   the only one.

The value also lands in remote state in clear text. That is why the state bucket
is private, versioned and encrypted, and why the bootstrap ends with a state
sweep.

## One-time bootstrap

Run once, by the repository owner, with credentials that can create S3 buckets
and IAM resources. Terraform cannot create its own state bucket — that is the
usual chicken-and-egg, resolved here by four CLI calls rather than a second
Terraform root.

Until this is done, the `TerraformPlan` CI job fails at role assumption. That is
expected and is not a defect in the workflow.

### 1. State bucket

```bash
BUCKET=mlobs-tfstate-601548053958
REGION=us-west-2

aws s3api create-bucket \
  --bucket "$BUCKET" \
  --region "$REGION" \
  --create-bucket-configuration LocationConstraint="$REGION"

aws s3api put-bucket-versioning \
  --bucket "$BUCKET" \
  --versioning-configuration Status=Enabled

aws s3api put-bucket-encryption \
  --bucket "$BUCKET" \
  --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"},"BucketKeyEnabled":true}]}'

aws s3api put-public-access-block \
  --bucket "$BUCKET" \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
```

`us-west-2` requires the explicit `LocationConstraint`; omitting it creates the
bucket in `us-east-1` and the backend then fails with a redirect error.

Two more settings, both about the same fact — this bucket holds the operator's
home address in clear text, and versioning means it holds it more than once.

```bash
# Refuse plaintext transport. BlockPublicAccess stops anonymous callers; it
# says nothing about the transport an authorised one uses.
aws s3api put-bucket-policy --bucket "$BUCKET" --policy "$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "DenyInsecureTransport",
    "Effect": "Deny",
    "Principal": "*",
    "Action": "s3:*",
    "Resource": ["arn:aws:s3:::$BUCKET", "arn:aws:s3:::$BUCKET/*"],
    "Condition": {"Bool": {"aws:SecureTransport": "false"}}
  }]
}
EOF
)"

# Age out superseded state versions.
aws s3api put-bucket-lifecycle-configuration --bucket "$BUCKET" \
  --lifecycle-configuration '{
    "Rules": [{
      "ID": "expire-noncurrent-state",
      "Status": "Enabled",
      "Filter": {"Prefix": ""},
      "NoncurrentVersionExpiration": {"NoncurrentDays": 90},
      "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 7}
    }]
  }'
```

The lifecycle rule is not housekeeping. Versioning is enabled so a corrupted
state can be rolled back, but every apply writes a new object version and the
CIDR is in all of them — so the number of retained copies of that address grows
without bound, and each one is a separate delete if the value ever has to be
scrubbed. Ninety days keeps rollback available across a demo cycle and puts a
ceiling on the history. The `Deny` above applies to the operator too: run the
state commands over HTTPS, which the AWS CLI does by default.

Verify every setting took effect — a bucket holding an IP address in clear text
is worth reading back rather than assuming:

```bash
aws s3api get-bucket-versioning   --bucket "$BUCKET"   # Status: Enabled
aws s3api get-bucket-encryption   --bucket "$BUCKET"   # SSEAlgorithm: AES256
aws s3api get-public-access-block --bucket "$BUCKET"   # all four flags true
aws s3api get-bucket-policy       --bucket "$BUCKET"   # DenyInsecureTransport
aws s3api get-bucket-lifecycle-configuration \
  --bucket "$BUCKET"                                   # NoncurrentDays: 90
```

### 2. Actions secret

```bash
gh secret set SSH_INGRESS_CIDR --repo MuratAlkan06/ml-observability-system
# paste A.B.C.D/32 exactly, no trailing newline
```

The plan role's ARN is **hardcoded** in `.github/workflows/ci.yml` rather than
read from an Actions variable. The account id is already accepted exposure in
this repo (it is in `backend.tf` and `imports.tf`), the role name is fixed by
`iam.tf`, and a variable would add a second place to keep in sync for no
secrecy gain. If the role is ever renamed, the workflow changes with it in the
same commit.

### 3. Init, plan, and the one apply that adopts the host

```bash
terraform -chdir=infra/ec2 init
terraform -chdir=infra/ec2 plan  -input=false
terraform -chdir=infra/ec2 apply -input=false
```

Expected plan summary, verified locally on 2026-09-01 against a scratch local
backend:

```
Plan: 6 to import, 3 to add, 6 to change, 0 to destroy.
```

- **6 to import** — the instance, the security group, and its four rules
  (tcp/8000, tcp/3000, tcp/22, allow-all egress). Every one is
  *updated in-place*; nothing is replaced.
- **3 to add** — the GitHub OIDC provider, the `mlobs-tf-plan` role, and the
  role's inline policy. These genuinely do not exist yet.
- **6 to change** — the two `default_tags` (`project`, `managed-by`) landing on
  the six imported objects, plus `user_data_replace_on_change` on the instance,
  which is Terraform-side only and issues no API call.
- **0 to destroy** — the property that matters. If a plan ever shows a destroy
  or a replacement here, stop and reconcile the configuration with reality
  instead of applying.

#### Before the apply: read back the OIDC provider's audiences

Do this first, every time, even when you expect the provider not to exist. It
is the one step here whose failure mode is silent:

```bash
aws iam get-open-id-connect-provider \
  --open-id-connect-provider-arn \
    arn:aws:iam::601548053958:oidc-provider/token.actions.githubusercontent.com \
  --query 'ClientIDList'
```

- **`NoSuchEntity`** — nothing federates GitHub in this account yet. Proceed;
  the apply creates the provider with the one audience `iam.tf` declares.
- **A list of audiences** — the provider already exists and is shared. Copy
  **every** value the command returns into `client_id_list` in `iam.tf`,
  including any you do not recognise, keeping `sts.amazonaws.com` among them.

An OIDC provider is a single account-wide object, not one per role, and
`client_id_list` is declared as the whole list rather than as a membership
assertion. Import a shared provider while `iam.tf` names only
`sts.amazonaws.com` and Terraform will converge the real object down to that
one value, revoking every audience it does not mention. Roles belonging to
other workflows then fail `AssumeRoleWithWebIdentity` on an audience mismatch,
which surfaces far from here and long after. Terraform reports the change as an
ordinary in-place update, so nothing about the plan summary flags it.

That makes the plan line for this resource worth reading directly. Additions
are fine:

```
~ resource "aws_iam_openid_connect_provider" "github" {
    ~ client_id_list = [
        + "sts.amazonaws.com",
      ]
  }
```

**If any line under `client_id_list` begins with `-`, stop.** That is an
audience being removed from a shared object. Add it to `client_id_list` in
`iam.tf` and re-plan until no `-` line remains under that attribute. Only then
apply.

If `apply` fails with `EntityAlreadyExists` on the OIDC provider, that is the
same condition reported by the API rather than by the read-back above — the
account already federates GitHub. Do not delete it; other roles may trust it.
Add an import block, redo the audience check, and re-run:

```terraform
import {
  to = aws_iam_openid_connect_provider.github
  id = "arn:aws:iam::601548053958:oidc-provider/token.actions.githubusercontent.com"
}
```

### 4. State secret sweep

The apply writes the SSH CIDR into remote state. Confirm nothing *else*
sensitive went with it:

```bash
# The pulled state holds the CIDR in clear text, so it does not go to a
# predictable world-readable path. `umask` must precede the redirect: the shell
# creates the file with the mode in force when it opens it, and tightening
# permissions afterwards leaves a window where it was readable.
umask 077
work=$(mktemp -d)
terraform -chdir=infra/ec2 state pull > "$work/state.json"

# Expected: only the known SSH /32, in the SG rule and the SG's ingress list.
# The array alternative is the point: `cidr_blocks` is a JSON list, so an
# extraction that stops at the first comma prints one element and hides the
# rest — which is precisely where an unexpected second range would sit.
grep -oE '"(cidr_ipv4|cidr_blocks|ssh_ingress_cidr)": *(\[[^]]*\]|"[^"]*"|null)' \
  "$work/state.json"

# Expected: no output.
# The exclusion is what makes that expectation reachable. The OIDC provider's
# URL, its ARN and both trust-policy condition keys all contain the literal
# "token", so the unfiltered pattern always matched: the step as written could
# never pass, and a check nobody can pass is a check nobody runs. Filtering the
# one known-benign string restores "no output" as a real result.
grep -niE 'password|passwd|secret|token|private_key|BEGIN [A-Z ]*PRIVATE KEY|aws_access_key|webhook' \
  "$work/state.json" \
  | grep -v 'token\.actions\.githubusercontent\.com'

rm -rf "$work"
```

`rm -rf`, not `rm -P`: `-P` is a BSD flag absent from GNU coreutils, so the
original line fails outright on Linux, and on APFS or any SSD its overwrite is
not a meaningful erase anyway. The protection that does hold is the private
`mktemp -d` directory plus the `umask` above.

### 5. Evidence: a plan that reports nothing to do

```bash
terraform -chdir=infra/ec2 plan -input=false -detailed-exitcode; echo "exit=$?"
```

`-detailed-exitcode` returns **0** for no changes, 2 for pending changes, 1 for
an error. Only 0 is acceptable — it is the machine-checkable statement that the
committed configuration and the running account agree.

Run it **twice: once with the instance stopped, once with it running.** The host
spends most of its life stopped, and attributes such as `public_ip` and
`instance_state` only populate when it is up; a configuration that is a no-op in
one state and drifts in the other is not actually codified.

```bash
INSTANCE=i-0ed558a5144e76f4d

# stopped (the usual resting state)
terraform -chdir=infra/ec2 plan -input=false -detailed-exitcode; echo "stopped exit=$?"

# running
aws ec2 start-instances --instance-ids "$INSTANCE" --region us-west-2
aws ec2 wait instance-running --instance-ids "$INSTANCE" --region us-west-2
terraform -chdir=infra/ec2 plan -input=false -detailed-exitcode; echo "running exit=$?"

aws ec2 stop-instances --instance-ids "$INSTANCE" --region us-west-2
```

### 6. Re-run CI

Re-run the `TerraformPlan` job on the open pull request. It should go green;
that is the acceptance evidence for this slice.

If it instead fails on an IAM `AccessDenied` during refresh, the plan role is
missing a read action that the provider needs but the runbook did not
anticipate. Add the specific action to the matching statement in `iam.tf` — do
not substitute the AWS-managed `ReadOnlyAccess` policy, which is far wider than
this root requires.

## Cost

us-west-2 on-demand, September 2026 list prices:

| Item | Rate | Monthly if left on |
| --- | --- | --- |
| t3.medium, running | $0.0416 / hour | ≈ $30.40 |
| 30 GiB gp3 root volume | $0.08 / GB-month | $2.40 |
| S3 state bucket | a handful of small objects | < $0.01 |

The volume is billed whether the instance runs or not, so a **stopped** host
costs ≈ **$2.40/month** and a host left running costs ≈ **$32.80/month**.

There is no Elastic IP, no NAT gateway and no DynamoDB lock table — the three
line items that usually make an idle demo environment expensive. State locking
uses an S3 object instead, and the instance takes a fresh public IP on each
start, which is also why no output here publishes one.

Actual spend follows the demo pattern: the host is started for a demo or a load
test and stopped afterwards, landing at roughly **$3/month**.
