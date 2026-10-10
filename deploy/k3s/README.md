# k3s deployment

Kubernetes manifests and deploy scripts for running the ML Observability stack
on k3s, ported from `docker-compose.yml` (Phase 2 P2a; decisions D17–D27 in
`docs/PLAN.md`).

The manifests stand alone: they are the description of *what each service is*.
Their env blocks, images and startup ordering were transcribed from the Compose
file rather than reinterpreted, and every manifest still names the Compose
service it came from, as provenance. Compose is retired (D40), and that truth
now lives at a recorded commit, not in the tree: the last shipping version is
`docker-compose.yml` at `f33b65909820d9a4659e291f166e448077dab677`, readable with
`git show f33b65909820d9a4659e291f166e448077dab677:docker-compose.yml`. D25's
fallback runtime on the host ended with the file.

Why the stack moved, what changed and when to revisit are in `docs/K3S.md`.
This file covers the manifests, the two scripts, how to rehearse them locally,
and the record of the on-host cutover (P2b).

## Layout

```
deploy/k3s/
  manifests/            plain YAML, applied with kubectl apply -f
  apply.sh              render + deploy + wait + smoke
  smoke.sh              post-deploy / post-rollback check, runs standalone
  rehearse-rollback.sh  D36's rollback across the operator boundary, on k3d
```

Nine workloads in namespace `mlobs`: `api`, `grafana`, `postgres`, `redis`,
`prometheus`, `drift`, `drift-shadow` (Deployments) and `consumer`,
`shadow-scorer` (StatefulSets). Beside them runs the `ServingDeployment`
operator (Phase 3, [`operator/`](../../operator/README.md)), which since O2
owns `deployment/api`, created or adopted from the `ServingDeployment` in
`manifests/40-servingdeployment.yaml` rather than rendered from a manifest, and
`deployment/api-canary`, which runs only inside a canary window. Eight Services
named exactly as the Compose DNS names so `prometheus/prometheus.yml` is mounted
unchanged (D21), plus `api-canary`, the canary's own scrape address. One PVC,
for Postgres (D20). No Ingress and no LoadBalancer: `grafana` publishes host
port 3000, the api is a NodePort Service on 8000 (D34), and traefik and
servicelb are disabled on the host (D19).

Nothing under this directory is a copy of anything. `apply.sh` builds every
ConfigMap from the canonical files — `prometheus/prometheus.yml`,
`grafana/provisioning/**`, `grafana/dashboards/*.json`, `sql/init.sql` plus
`sql/migrations/*.sql`, and `baseline/*.json`. The operator's CRD and Role are
generated, not copied: controller-gen's output sits below a marker line in
`manifests/01-servingdeployment-crd.yaml` and `manifests/02-operator-rbac.yaml`,
and `make -C operator verify-manifests` (CI job `OperatorManifestSync`) fails
if either drifts from the Go source (D31).

## Deploying

```bash
IMAGE_TAG=<commit-sha> deploy/k3s/apply.sh
```

| Variable | Default | Meaning |
|---|---|---|
| `IMAGE_TAG` | *(required)* | Tag for the four service images and the operator's: a full 40-character lowercase commit SHA, because it becomes the `ServingDeployment`'s `spec.imageTag`, whose schema admits nothing else. No default on purpose — deploying "whatever `latest` means today" is the failure this slice removes. |
| `IMAGE_PREFIX` | `ghcr.io/muratalkan06` | Registry and owner. CI overrides it to `docker.io/library`, which is how containerd names an image imported from a local build. The operator is handed the same prefix for the api images it writes. |
| `ENV_FILE` | `<repo>/.env` | Holds `POSTGRES_PASSWORD` and `GF_ADMIN_PASSWORD` (both required), plus optional `GF_ADMIN_USER` and `SLACK_WEBHOOK_URL`. Same file Compose used; still gitignored. |
| `NAMESPACE` | `mlobs` | |
| `ROLLOUT_TIMEOUT_SECONDS` | `180` | Per-workload bound. Exceeding it fails the deploy. |

`apply.sh` runs D32's deploy sequence, in this order and no other. It renders
the manifests into a temporary directory with the image refs and the
Prometheus config hash substituted, and runs a preflight that changes nothing
(see [Host k3s flags](#host-k3s-flags)). Then it applies the namespace,
generates the Secret and the ConfigMaps, applies the `ServingDeployment` CRD
and waits for it to be `Established`, applies the operator and waits for its
rollout, and applies the rest of the stack. It applies the `ServingDeployment`
itself, which renders `spec.imageTag` and nothing else, and sends the constant
close-window patch: a pipeline deploy during an open canary window closes it
and says so on a fixed line, `ok: canary window closed by the close-window
patch (D32)`, and no deploy can open one. It waits for the resource's `Ready`
condition at its current generation, then for the nine rollouts,
`deployment/api` first, and finishes by running `smoke.sh`. Its exit status is
the deploy's. `deployment/api` is no longer rendered from a manifest: the
operator creates it, or adopts the one that exists, from the
`ServingDeployment` (D31).

`smoke.sh` also runs on its own — that is the point of it. It is the check after
a `kubectl rollout undo` or a redeploy of a previous SHA (D25):

```bash
deploy/k3s/smoke.sh
```

It is state-aware (D37). The stack is in exactly one of two states, and
`smoke.sh` decides which from the cluster: the `ServingDeployment`'s
`CanaryActive` and `ShadowPaused` conditions at its current generation, the
canary Deployment and the shadow scorer as observed — never the spec's canary
fields, which an expired window leaves behind until the close-window patch
clears them.

| State | Conditions | Workloads | Prometheus jobs up |
|---|---|---|---|
| steady | `CanaryActive` False, `ShadowPaused` False | `api-canary` absent or at 0 with no pods; `shadow-scorer` at 1, ready | every configured job except `api_canary` |
| window | `CanaryActive` True, `ShadowPaused` True | `shadow-scorer` at 0 with no pods; `api-canary` at 1 or more, all ready | every configured job except `shadow_scorer` |

Any other combination, such as a transition still settling, is polled for up
to `STATE_TIMEOUT_SECONDS` (default 180) and then rejected, so a state put
together by hand fails the check. In either state it then asserts the
rollouts, `GET /health` 200 and a `POST /predict` round trip carrying
`request_id` + `label` + `confidence` through the api's node port; that
Prometheus reports exactly the state's jobs up, `drift_shadow` included in
both; and Grafana `/api/health` 200. In a window it also sends twenty
`/predict` connections through the same port and requires the canary's own
predictions counter to have moved: the canary is serving behind the shared
Service, not only running. Needs `kubectl`, `curl` and `python3` on PATH.

### Host k3s flags

The manifests assume a k3s started with the flags below. Each is pinned in
three places that move together (`docs/K3S.md`): the host, here; the
`K3sSmoke` k3d arguments in `.github/workflows/ci.yml`; and the local k3d
recipe in [Rehearsing locally with k3d](#rehearsing-locally-with-k3d).

| Flag | Why |
|---|---|
| `--disable traefik,servicelb,metrics-server` | D19: add-ons the manifests never reference, and RAM on a 4GB host. Set at install, 2026-09-27 (see the migration record below). |
| `--service-node-port-range=8000-8000` with `--disable-network-policy` | D34: the api is a NodePort Service on 8000, the port the security group already admits; an exact range keeps it there. The two are one atomic pair, never set apart (the D34 erratum of O2): k3s's bundled network-policy controller refuses a single-port range and the server crash-loops. The stack defines no NetworkPolicy, so nothing enforced is lost; one added later would go unenforced until the controller returns. |

On the host the pair is two lines in `/etc/rancher/k3s/config.yaml`, which k3s
reads beside its install flags, followed by a k3s restart:

```yaml
service-node-port-range: "8000-8000"
disable-network-policy: true
```

That change was O4's, made inside its gated cutover window (issue #68,
completed 2026-10-05), not a pipeline step; the host was the third pin site,
deferred there on purpose until the cutover. With network policy off, `br_netfilter` is loaded by k3s's own
startup and by nothing else, and pods reach Services only while it is loaded;
the restart is followed by `lsmod | grep br_netfilter` and a pod resolving a
Service by name (in k3d, where k3s cannot load modules, CI loads it on the
runner). On any cluster that lacks the pair, `apply.sh` stops at its preflight,
before anything is applied: it reads the range k3s records in each server
node's `k3s.io/node-args` annotation and stops on the fixed line naming the
pair when that range does not admit 8000, and the running stack is left as it
was. On a cluster that is not k3s the annotation is absent; the preflight
warns that it cannot read the range there and goes on to its server-side
dry-run of the api Service, which catches a port collision but not the range.

### A note on the shadow-scorer image

`api`, `consumer` and `drift` are published to GHCR by CI (D18). `shadow-scorer`
is **not**, because the model it bakes carries no upstream licence and this
repository does not republish those weights. Since `apply.sh` takes one
`IMAGE_PREFIX` for all four, the shadow image has to be in containerd already,
under the same `ghcr.io/muratalkan06/mlobs-shadow-scorer:<tag>` name the
manifest expects. The manifests set `imagePullPolicy: IfNotPresent`, so an
image already present is used and the registry is never consulted for it.

As of P2c that step is automated, and the S3 path supersedes `scp` (D28). CI's
`ShadowPublish` job saves the image on every push to `main` and uploads it to a
private S3 bucket as `shadow/<sha>.tar.gz`; the SSM deploy document downloads
it on the host and imports it with `k3s ctr` before running `apply.sh` with
`IMAGE_TAG=<sha>` — see "Deploy pipeline" in `infra/README.md`. The P2b interim
it replaces, a local `linux/amd64` build copied up with `scp`, was exercised
live in the cutover — see
[Migration record and rehearsal results](#migration-record-and-rehearsal-results-2026-09-27).

## Compose idiom to k3s equivalent

Everything below assumes `-n mlobs`.

| Compose | k3s |
|---|---|
| `docker compose up -d` | `IMAGE_TAG=<sha> deploy/k3s/apply.sh` |
| `docker compose ps` | `kubectl -n mlobs get pods` |
| `docker compose stop` / `start shadow-scorer` (the latency A/B switch) | none since Phase 3 O2. The operator is the sole writer of the shadow scorer's scale: it holds it at 1, pauses it only inside a canary window, and puts a hand `kubectl scale` back on its next reconcile (D37). The switch this row used to name, `kubectl -n mlobs scale statefulset/shadow-scorer --replicas=0` / `1`, is retired. |
| `docker compose logs -f api` | `kubectl -n mlobs logs -f deployment/api` |
| `docker compose logs -f consumer` | `kubectl -n mlobs logs -f statefulset/consumer` |
| `docker compose restart drift` | `kubectl -n mlobs rollout restart deployment/drift` |
| `docker compose exec -T postgres psql -U mlobs -d mlobs` | `kubectl -n mlobs exec -it deployment/postgres -- psql -U mlobs -d mlobs` |
| `127.0.0.1:9090` (Prometheus loopback publish) | `kubectl -n mlobs port-forward svc/prometheus 9090:9090` |
| `127.0.0.1:6379` (Redis loopback publish) | `kubectl -n mlobs port-forward svc/redis 6379:6379` |
| `localhost:8000` / `localhost:3000` | unchanged — `:3000` is Grafana's host port, and `:8000` the api's NodePort (D34) |
| `docker compose up -d --build` | rebuild, push or import, then re-run `apply.sh` with the new `IMAGE_TAG` |
| `docker compose down` | `kubectl delete namespace mlobs` (also deletes the PVC) |

Prometheus and Redis lost their loopback publishes on purpose: a port-forward
opens a socket only while someone is using it, which is strictly less exposure
than a permanently bound port (D19).

### Schema changes

Schema changes are never hand-patched onto a live volume (`PRINCIPLES.md` §4).
The k3s equivalent of `docker compose down -v && docker compose up -d` is to
delete the PVC and let `apply.sh` re-run `sql/init.sql` and the migrations from
a freshly generated ConfigMap:

```bash
kubectl -n mlobs scale deployment/postgres --replicas=0   # release the volume
kubectl -n mlobs delete pvc postgres-data
IMAGE_TAG=<sha> deploy/k3s/apply.sh                       # recreates it, re-runs initdb
kubectl -n mlobs rollout restart \
  statefulset/consumer statefulset/shadow-scorer \
  deployment/drift deployment/drift-shadow
```

The last step is needed because those four hold Postgres connections that the
recreated database will not honour, and `apply.sh` restarts nothing when the
image tag has not changed. **This destroys the data.** On a host with data worth
keeping, take a `pg_dump` first (D25).

## Rehearsing locally with k3d

This mirrors the `K3sSmoke` CI job step for step, its canary window included
(below the recipe); if it passes here it should pass there. The operator's own
end-to-end check, `operator/hack/e2e.sh`, runs against a local cluster too
(`operator/README.md`). Requires Docker, `kubectl` and roughly 20GB of free disk — the two
torch images are large and each is stored twice, once by the docker daemon and
again in the cluster's containerd.

```bash
# 1. k3d, pinned to the version CI uses
curl -fsSL -o /tmp/k3d \
  https://github.com/k3d-io/k3d/releases/download/v5.9.0/k3d-linux-amd64
sudo install -m 0755 /tmp/k3d /usr/local/bin/k3d

# 2. a cluster shaped like the host: same k3s, same add-ons disabled, the
#    same exact node-port range with network policy off (D34 and its O2
#    erratum; the two lines are a pair, never one without the other), and
#    :8000 and :3000 published through to your machine. On a Linux machine,
#    load br_netfilter first: k3s loads it itself on a real host but cannot
#    from inside a k3d node, and with network policy off nothing else does,
#    so no pod would reach a Service. Docker Desktop already has it loaded.
sudo modprobe br_netfilter   # Linux only
k3d cluster create mlobs-dev \
  --image rancher/k3s:v1.36.4-k3s1 \
  -p '8000:8000@server:0' \
  -p '3000:3000@server:0' \
  --k3s-arg '--disable=traefik@server:*' \
  --k3s-arg '--disable=servicelb@server:*' \
  --k3s-arg '--disable=metrics-server@server:*' \
  --k3s-arg '--service-node-port-range=8000-8000@server:*' \
  --k3s-arg '--disable-network-policy@server:*' \
  --wait

# 3. build the five images under a full commit SHA: apply.sh renders the tag
#    into the ServingDeployment, whose spec.imageTag admits nothing else
TAG="$(git rev-parse HEAD)"
docker build -f docker/api.Dockerfile           -t "mlobs-api:${TAG}" .
docker build -f docker/consumer.Dockerfile      -t "mlobs-consumer:${TAG}" .
docker build -f docker/drift.Dockerfile         -t "mlobs-drift:${TAG}" .
docker build -f docker/shadow_scorer.Dockerfile -t "mlobs-shadow-scorer:${TAG}" .
docker build -t "mlobs-operator:${TAG}" operator

# 4. import them. containerd normalises a bare name:tag to
#    docker.io/library/name:tag, which is why IMAGE_PREFIX is set that way below.
k3d image import -c mlobs-dev "mlobs-api:${TAG}" "mlobs-consumer:${TAG}" \
  "mlobs-drift:${TAG}" "mlobs-shadow-scorer:${TAG}" "mlobs-operator:${TAG}"

# 5. deploy (needs a .env; cp .env.example .env and fill it in)
IMAGE_PREFIX=docker.io/library IMAGE_TAG="${TAG}" deploy/k3s/apply.sh

# 6. tear down
k3d cluster delete mlobs-dev
```

To rehearse a redeploy the way CI does, retag the five images under a second
40-character hex tag, re-import, and run `apply.sh` again with that tag. Editing
`prometheus/prometheus.yml` first is worth doing too: it should move the
`mlobs/config-hash` annotation and roll exactly the Prometheus pod.

```bash
kubectl -n mlobs get deployment prometheus \
  -o jsonpath="{.spec.template.metadata.annotations['mlobs/config-hash']}"
```

`K3sSmoke` also takes the stack through a canary window, and so can a local
cluster. A window is opened the way a human opens one on the host, with a
merge patch setting both canary fields (D32); here the canary is the api
image under a second 40-character tag, as in CI:

```bash
CANARY_TAG="$(printf '%s' "${TAG}-canary" | shasum | cut -c1-40)"
docker tag "mlobs-api:${TAG}" "mlobs-api:${CANARY_TAG}"
k3d image import -c mlobs-dev "mlobs-api:${CANARY_TAG}"
kubectl -n mlobs patch servingdeployment/api --type merge \
  -p "{\"spec\":{\"canaryImageTag\":\"${CANARY_TAG}\",\"canaryReplicas\":1}}"
STATE_TIMEOUT_SECONDS=300 deploy/k3s/smoke.sh   # ends "ok: smoke passed (window state)"
```

Any `apply.sh` run then closes the window on its fixed line, and its own
`smoke.sh` ends in the steady state. The window's 45-minute TTL applies here
too.

### The D36 rollback

`rehearse-rollback.sh` rehearses undoing the operator (D36) the way CI's
`RollbackRehearsal` job runs it. It deploys the pre-cutover tree through that
tree's own `apply.sh`, cuts over to this one as O4 did on the host (issue
#68), so that the operator adopts `deployment/api` in place, and opens a canary
window. Then it runs D36's six steps and asserts each on the state it leaves:
the canary to 0 by host patch, the `ServingDeployment` deleted,
`deployment/api` garbage-collected to a bound, the operator scaled to 0, the
pre-cutover tree redeployed by its own `apply.sh`, and its `smoke.sh` green.
The host procedure it rehearses is in [`docs/RUNBOOK.md`](../../docs/RUNBOOK.md).

It needs a cluster that has had nothing deployed — it refuses one that
already holds the `mlobs` namespace — so on the cluster from step 2 it runs in
place of step 5. Besides the five images of step 3 it needs two more tags on
the same builds: the pre-cutover SHA, which the job pins to the SHA the host
ran before O4, and a canary tag. The pre-cutover tree is read from git history
with `git archive`, so a shallow clone has to be deepened first.

```bash
PRE=16a8c860af1fdc89bb7db96f497aeb542e1b40c9   # RollbackRehearsal's PRE_CUTOVER_SHA
CANARY_TAG="$(printf '%s' "${TAG}-canary" | shasum | cut -c1-40)"
for svc in api consumer drift shadow-scorer; do
  docker tag "mlobs-${svc}:${TAG}" "mlobs-${svc}:${PRE}"
done
docker tag "mlobs-api:${TAG}" "mlobs-api:${CANARY_TAG}"
k3d image import -c mlobs-dev "mlobs-api:${PRE}" "mlobs-consumer:${PRE}" \
  "mlobs-drift:${PRE}" "mlobs-shadow-scorer:${PRE}" "mlobs-api:${CANARY_TAG}"
IMAGE_PREFIX=docker.io/library PRE_CUTOVER_SHA="${PRE}" IMAGE_TAG="${TAG}" \
  CANARY_TAG="${CANARY_TAG}" deploy/k3s/rehearse-rollback.sh
# ends "ok: D36 rollback rehearsal passed"
```

On a machine where something else already holds `:8000`, `:3000` and `:9090`
(a Compose-era stack left running, say), publish the cluster on other ports
(`-p '18000:8000@server:0'`, `-p '13000:3000@server:0'`) and point both trees'
`smoke.sh` there through the environment the rehearsal passes on:
`API_URL=http://127.0.0.1:18000`, `GRAFANA_URL=http://127.0.0.1:13000` and
`PROMETHEUS_LOCAL_PORT=19090`.
Without the last one, the port-forward to the cluster's Prometheus cannot bind
`:9090`, and `smoke.sh`'s readiness probe is answered by whatever holds
`:9090` instead.

## Migration record and rehearsal results (2026-09-27)

The on-host cutover as executed across two owner sessions, 2026-09-21 and
2026-09-27. The raw evidence is on issue #48; the reasoning is in
`docs/K3S.md`.

**Abort floor.** EBS snapshot `snap-0f365806e0eaf9fb6` — 30 GiB, created
2026-09-21 with the instance stopped, completed 100% — taken before any
destructive step. Above it sat the D25 path: Compose still installed on the
host, and a `pg_dump` to restore into whichever runtime came back. The abort
path was never needed.

**State found.** Compose was degraded: 7/9 containers up, with `prometheus`
and `grafana` down since the 2026-09-21 instance start. Neither has a restart
policy in `docker-compose.yml`, so this was the reboot gap observed live —
Grafana dark for about six days. The instance had also idled for six days after
an interrupted session, ≈$6 of unplanned compute, recorded here so the cost
note stays honest. The host repository was fast-forwarded `feb211e` →
`7a270be` before the cutover.

**Cutover.**

1. Baseline: predictions 8386, shadow_predictions 3628, drift_runs 18884.
   `pg_dump --clean --if-exists` → 18M at `~/mlobs-backup-2026-09-27.sql`.
2. `docker compose down`: 10/10 removed; ports verified free.
3. k3s `v1.36.4+k3s1`: hash-verified install, systemd-enabled,
   `--disable traefik,servicelb,metrics-server`. Node Ready in 3s.
4. Shadow image: the interim path from the note above, exercised live — a
   `linux/amd64` build on the operator machine, copied up with `scp` and
   imported with `k3s ctr` (487M gzipped). P2c replaces it with the private S3
   tarball (D18).
5. `IMAGE_TAG=main ./deploy/k3s/apply.sh`: all ok-lines, nine workloads rolled
   out, Prometheus targets 5/5 (`api`, `consumer`, `drift`, `drift_shadow`,
   `shadow_scorer`), smoke passed. `main` is the moving GHCR tag; SHA-pinned
   deploys arrive with P2c (D24).
6. Restore: predictions 8386, shadow_predictions 3628, drift_runs 18885 — the
   +1 is a fresh k3s drift cycle.
7. Standalone `smoke.sh`: passed.

**k3s idle overhead.** Used memory 648 → 930 MB (≈282 MB) after install,
before the stack.

**D26 rehearsal: PASS.** 12 minutes at simulator 5 rps with the shadow scorer
on, ~3.6k requests served. 24/24 samples; MemAvailable 1,917,348–1,961,112 kB,
minimum ≈1872 MiB against the ≥400 MiB bar — a 4.7× margin. dmesg OOM scan:
zero events. Top RSS under load:

| Process | RSS |
|---|---|
| api | 565M |
| k3s-server | 529M |
| shadow | 439M |
| grafana | 240M |
| containerd | 186M |

Verdict: **t3.medium retained.** No instance-type change, so
`var.instance_type` in the P1 root is untouched.

**D27 re-measurement.** `hey` 0.1.5 on-instance, the README methodology
unchanged: 15 s warm-up then 120 s measured, `-c 1 -q 5`, fixed payload. The
A/B switch was `kubectl -n mlobs scale statefulset/shadow-scorer`, exercised
live (the idiom retired in Phase 3 O2, when the operator took over the shadow
scorer's scale); the shadow scorer was restored to `replicas=1` afterwards.

| Window | req/s | p50 | p95 | p99 |
|---|---|---|---|---|
| shadow ON (`replicas=1`) | 4.96 | 62.2 ms | 111.2 ms | 300.6 ms |
| shadow OFF (`replicas=0`) | 4.95 | 59.8 ms | 101.0 ms | 243.9 ms |

Shadow primary-path cost ≈2.4 ms p50 / ≈10 ms p95. How the p95 figure reads
against the v1.1 ≤10% criterion is set out in the README's k3s re-measurement
section.

**Host facts.** ssm-agent was present at migration (snap 3.3.4793.0,
latest/stable), confirming the P2c dependency; refreshed to latest/candidate
3.3.5226.0 at the P2c owner steps to meet the CVE-2026-89049 floor (issue #49).

**SSH ingress churn.** The SSH `/32` rotated twice during this slice, each time
through the P1 root: plan `0 add/1 change/0 destroy`, apply, then the Actions
secret synced. That recurring friction is recorded as the concrete motivation
for P2c's SSM channel (D24), which needs no inbound SSH.

## See also

- `docs/PLAN.md` — decisions D17–D27, and D28–D30 for the deploy pipeline.
- `infra/README.md`, "Deploy pipeline" — dispatching a deploy, the environment
  gate, rollback.
- `docs/K3S.md` — why k3s, what changed, when to revisit, and the numbers
  policy (D27).
- `PRINCIPLES.md` — binding engineering rules.
