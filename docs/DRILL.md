# The Terraform rebuild drill

> The Terraform root in [`infra/ec2`](../infra/ec2) adopted the EC2 host in
> place, through `import` blocks (D10), so for a long time the host was
> *described* by code without ever having been created by it. D10 deferred
> that evidence, P3 did not earn it (D39), and the item sat on issue #60. This
> drill earned it once, on 2026-10-20 UTC. This file is the committed runbook:
> the design the drill ran under, its phases and gates, what the run showed,
> the lessons it left, and a checklist for the next run. The dated evidence is
> on issue #60; the root itself is described in
> [`infra/README.md`](../infra/README.md).

## What the drill claims, and what it does not

The claim, with its qualifier: on 2026-10-20 UTC the host slice was rebuilt
once — infra from a clean state via `terraform apply`; host provisioned by
hand per the migration runbook; data restored count-exact; smoke green, twice.
Where this repository says "rebuilt from code", it means exactly that chain —
infra from a clean state via `terraform apply`, host provisioned by hand per
the migration runbook, data restored count-exact, smoke green twice — and
nothing more.

What it does not claim:

- **That provisioning the host is code.** The root still does not provision
  the host (`infra/README.md`, "What this root does not do"). k3s, the
  checkout and the `.env` were put on the drill host by hand, replaying the
  [migration record](../deploy/k3s/README.md#migration-record-and-rehearsal-results-2026-09-27)
  in `deploy/k3s/README.md`.
- **That the whole root was re-created.** The drill applied the host slice
  only: the instance, its security group and three rules, the host role with
  its SSM attachment, and the instance profile. The account singletons — the
  GitHub OIDC provider, the plan, deploy and publish roles, the SSM document,
  the buckets — were left out on purpose. Terraform created them at bootstrap
  and at P2c, and under their fixed names a second copy cannot coexist with
  the first.
- **That the deploy pipeline was rebuilt.** Its roles, environments and SSM
  document are among those singletons; the drill deployed with `apply.sh` on
  the host instead.
- **More than one run.** It is one drill, not a recurring check.

## The isolation design

The drill never touched the live host, its checkout or its state. Isolation
came from a second checkout rather than a Terraform workspace or a second
backend key: those share one configuration directory with the live root, and
the configuration itself had to differ for the drill.

- **A second, disposable checkout** at a pinned commit, with exactly three
  local changes in its `infra/ec2/`:
  1. `imports.tf` deleted, so a fresh state cannot adopt the live objects;
  2. a `backend_override.tf` declaring a local backend, so the S3 backend is
     never configured in the drill directory;
  3. a `terraform.tfvars` setting `name_suffix = "-drill"` and a dummy `/32`
     for `ssh_ingress_cidr` — the drill host is reached over SSM only, so the
     operator's address never enters drill state.
- **A targeted, create-only apply.** Five `-target` addresses —
  `aws_instance.app`, the three security-group rules (`api`, `ssh`, `all`)
  and `aws_iam_role_policy_attachment.host_ssm_core`. Their dependency
  closure adds the security group, the host role and the instance profile:
  eight resources in all.
- **The drill SHA is the live host's deployed SHA,** so the images, the shadow
  tarball and the restored data all belong to one commit.

## Phases and gates

| Phase | What happens | Gate or abort point |
|---|---|---|
| 0 — backup | In the live host's normal demo window: `pg_dump --clean --if-exists`, gzipped, uploaded to the artifacts bucket's `backups/` prefix by the host's own write-only role (#155); row counts of `predictions`, `shadow_predictions` and `drift_runs` captured as the drill's acceptance numbers; the deployed SHA recorded; the host stopped again. | Abort if the SHA's shadow tarball has expired or its images are missing. |
| 1 — isolate | Second checkout; the three-item delta; `git status --porcelain` shows exactly that delta; `terraform init` against the local backend. | **G1**, **G2**. Abort on any other `git status` output. |
| 2 — build | `terraform plan -out` with the five targets, then `terraform apply` of that saved plan — never a re-plan. | **G3**. |
| 3 — provision, restore, smoke | By hand, per the migration record: k3s `v1.36.4+k3s1` with the `config.yaml` pair; the repo cloned at the drill SHA; a fresh `.env`; the shadow tarball imported; `IMAGE_TAG=<sha> deploy/k3s/apply.sh`; smoke; then the Phase-0 dump restored, the four services holding Postgres connections restarted, counts compared, and smoke again. | Abort on a rollout timeout: inspect, never widen scope. |
| 4 — teardown | Targeted destroy, then a bare destroy of what remains (see the lessons); `terraform state list` empty; residue sweep by the `-drill` names; **G4**; the drill checkout and its local state deleted. | **G4**. |

The four gates exist to prove one thing: the drill wrote nothing to the live
account outside its own eight resources.

| Gate | Phase | Check | Pass |
|---|---|---|---|
| G1 | 1 | `terraform state list` in the drill directory | Prints nothing. The live state is never empty, so a drill directory wired to it fails here, before any plan. |
| G2 | 1 | `LastModified` of the live state key (`s3api head-object`) | Recorded, as G4's baseline. |
| G3 | 2 | The saved plan's summary and text | Exactly `Plan: 8 to add, 0 to change, 0 to destroy.`; no "to import"; the plan names `mlobs-demo-sg-drill` and neither the live instance id nor the live security group id. |
| G4 | 4 | The live state key, and a plan from the live checkout | `LastModified` equal to G2's; `plan -detailed-exitcode` exits 0. |

## The 2026-10-20 run

Phase 0 ran first, in its own host window, and the drill followed the same
day. Every gate passed. The live host stayed stopped throughout.

| Step | Result |
|---|---|
| G2 | Live state key `LastModified` 2026-10-20T05:36:41Z. |
| Apply (G3 passed first) | `8 added, 0 changed, 0 destroyed`; drill instance `i-0e24761866b655ab5`; every suffixable name ending `-drill`; the dummy `/32` in drill state. |
| Provision | k3s `v1.36.4+k3s1` with the config pair; repo at `badf284`; a fresh `.env`, mode 0600, never logged; the shadow tarball and the dump fetched by presigned URL. |
| Deploy and first smoke | `apply.sh` rolled out all nine workloads; smoke printed `ok: smoke passed (steady state)`. |
| Restore | Error-free and count-exact against Phase 0: `predictions` 408605, `shadow_predictions` 403847, `drift_runs` 69225. |
| Second smoke | After the restart, the same line, verbatim. |
| Teardown | 5 + 3 = 8 destroyed; `terraform state list` empty. |
| Residue sweep | Instance `terminated` only; security group, role, instance profile and volumes gone. |
| G4 | `LastModified` unchanged at 2026-10-20T05:36:41Z; a fresh plan from the live checkout a no-op. |
| Cleanup | Drill checkout and its local state deleted. |

## Lessons

### The name collision that produced `name_suffix` (#153, #154)

Applied a second time in the same account, the root as it stood would have
collided with the live host. A security group's name is unique within its VPC
(the drill uses the same default VPC), and IAM role and instance profile names
are unique within the account. Worse than a collision, `imports.tf` run
against an empty state adopts the live instance and security group into it,
after which a suffixed plan would schedule the live group's destroy. Hence the
three-item delta deletes `imports.tf`, and G3 searches the plan text for the
live ids.

`var.name_suffix` (#153, merged in #154) is the one repository change the
drill needed. It suffixes the security group's name, the `Name` tags on the
group, the instance and its root volume, and the host role and instance
profile. Its default, the empty string, reproduces every live name unchanged,
and `TerraformPlan` against the live account stayed a no-op. Two quirks of its
validation, both by design: the suffix tops out at 13 characters including
the leading hyphen, and a trailing hyphen is accepted.

### `host_shadow_read` cannot be a drill target

The live host reads the shadow tarball under its own role, through
`aws_iam_role_policy.host_shadow_read`. Adding that policy to the drill's
targets, so the drill host could read the tarball the same way, was rejected
by the plan guard: the policy document references the artifacts bucket's ARN,
so `-target` pulled `aws_s3_bucket.artifacts` into the create closure. That is
the live bucket, an account singleton, planned as a resource to create in the
drill's empty state: a collision on apply and, had it ever entered drill
state, a bucket the drill's own teardown would destroy. The plan stopped
matching G3, and nothing was applied. `host_backups_write` references the
bucket the same way, so the same holds for it.

### Artifacts by presigned URL, and the exposure that comes with it

With the bucket-read path closed, and no SSH to copy files over (the drill
host was SSM-only), the shadow tarball and the Phase-0 dump reached the drill
host as presigned GET URLs:

- minted on the operator's laptop just before use, with `aws s3 presign`;
- signed by the bootstrap session, because no long-lived IAM user exists in
  this account — within the gate's intent;
- a 30-minute expiry;
- silent in output: never echoed, and fetched on the host with a silent
  `curl`.

The disclosed exposure: the URLs were passed to the host inside SSM commands,
so they transited SSM's command history and stayed readable there, to anyone
who can read that history, until they expired. Each one grants a GET of a
single object, and only until its expiry, which is why the expiry is short.
No URL appears in this file or in the evidence.

### A targeted destroy leaves the dependencies behind

`-target` is not symmetric. On create, the five targets pulled in what they
depend on, for eight resources. On destroy, the same five targets destroyed
five: a targeted destroy removes the targets and anything that depends on
them, not what they depend on. The security group, the host role and the
instance profile were left in state, and a follow-up bare
`terraform destroy` removed those three.

The bare destroy was safe only because the drill's local state held nothing
but drill resources, checked with `terraform state list` before each destroy.
Never run a bare destroy against a state that holds anything else — and never
in the live checkout.

### Host-side gotchas

- **`git` and `safe.directory`.** SSM runs commands as root, and the
  checkout belongs to `ubuntu`, so `git` run by root fails its
  dubious-ownership check. In Phase 0 this broke the on-host lookup of the
  deployed SHA, and the backup object was re-keyed from the laptop with the
  independently confirmed SHA. Before running `git` as root there:
  `git config --global --add safe.directory /home/ubuntu/ml-observability-system`.
- **`sudo -u` needs `-H`.** In the drill's SSM commands `HOME` was unset, and
  `sudo -u ubuntu git clone` failed with `fatal: $HOME not set`. Use
  `sudo -u ubuntu -H`, which sets `HOME` to the target user's. Root's own
  `git config --global`, the `safe.directory` line above among it, needs
  `HOME` too: export `HOME=/root` first.

## Next-drill checklist

1. **Pick the SHA:** the live host's deployed SHA. Confirm its shadow tarball
   is still in the bucket (60-day expiry) and its images exist on GHCR.
2. **Phase 0:** dump with `pg_dump --clean --if-exists`, upload to `backups/`
   with the host role, capture the three counts, record the SHA, stop the
   host.
3. **Isolate:** a second checkout; delete `imports.tf`; add the local-backend
   override; `terraform.tfvars` with `name_suffix = "-drill"` and a dummy
   `/32`, never the operator's address. `git status --porcelain` shows
   exactly that.
4. **G1 and G2** before any plan.
5. **Five targets only.** Never add `host_shadow_read` or
   `host_backups_write`: both pull the live bucket into the closure.
6. **G3, exactly;** apply the saved plan file, never a re-plan.
7. **Host:** export `HOME=/root`; set `safe.directory`; run user commands with
   `sudo -u ubuntu -H`; write the `.env` at mode 0600 with fresh passwords,
   never echoed.
8. **Artifacts:** mint the presigned URLs just before use, 30-minute expiry,
   never echoed; note the expiry and disclose the SSM-history exposure in the
   evidence.
9. **Deploy, smoke, restore, restart, smoke:** `apply.sh`; smoke; restore the
   dump; restart `consumer`, `shadow-scorer`, `drift` and `drift-shadow`;
   `predictions` and `shadow_predictions` equal Phase 0's, and `drift_runs`
   may exceed it only by fresh drift cycles; smoke again.
10. **Teardown:** the targeted destroy (expect 5); `terraform state list`
    shows only the three dependencies; a bare destroy (expect 3);
    `terraform state list` empty.
11. **Residue sweep** by the `-drill` names: instance `terminated` only; no
    security group, role, instance profile or volume.
12. **G4,** then delete the drill checkout and its local state.
13. **Evidence:** a dated comment on issue #60, with the claim stated with its
    qualifier.
