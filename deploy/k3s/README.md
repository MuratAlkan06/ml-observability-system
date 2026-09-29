# k3s deployment

Kubernetes manifests and deploy scripts for running the ML Observability stack
on k3s, ported from `docker-compose.yml` (Phase 2 P2a; decisions D17–D27 in
`docs/PLAN.md`).

The Compose file remains the source of truth for *what each service is* — env
blocks, images and startup ordering are transcribed here rather than
reinterpreted, and every manifest names the Compose service it came from. It
also stays installed on the host as a fallback runtime for the duration of
Phase 2 (D25).

Why the stack moved, what changed and when to revisit are in `docs/K3S.md`.
This file covers the manifests, the two scripts, how to rehearse them locally,
and the record of the on-host cutover (P2b).

## Layout

```
deploy/k3s/
  manifests/        plain YAML, applied with kubectl apply -f
  apply.sh          render + deploy + wait + smoke
  smoke.sh          post-deploy / post-rollback check, runs standalone
```

Nine workloads in namespace `mlobs`: `api`, `grafana`, `postgres`, `redis`,
`prometheus`, `drift`, `drift-shadow` (Deployments) and `consumer`,
`shadow-scorer` (StatefulSets). Eight ClusterIP Services, named exactly as the
Compose DNS names so `prometheus/prometheus.yml` is mounted unchanged (D21).
One PVC, for Postgres (D20). No Ingress and no LoadBalancer: `api` and `grafana`
publish host ports 8000 and 3000, and traefik and servicelb are disabled on the
host (D19).

Nothing under this directory is a copy of anything. `apply.sh` builds every
ConfigMap from the canonical files — `prometheus/prometheus.yml`,
`grafana/provisioning/**`, `grafana/dashboards/*.json`, `sql/init.sql` plus
`sql/migrations/*.sql`, and `baseline/*.json`.

## Deploying

```bash
IMAGE_TAG=<commit-sha> deploy/k3s/apply.sh
```

| Variable | Default | Meaning |
|---|---|---|
| `IMAGE_TAG` | *(required)* | Tag for the four service images. No default on purpose — deploying "whatever `latest` means today" is the failure this slice removes. |
| `IMAGE_PREFIX` | `ghcr.io/muratalkan06` | Registry and owner. CI overrides it to `docker.io/library`, which is how containerd names an image imported from a local build. |
| `ENV_FILE` | `<repo>/.env` | Holds `POSTGRES_PASSWORD` and `GF_ADMIN_PASSWORD` (both required), plus optional `GF_ADMIN_USER` and `SLACK_WEBHOOK_URL`. Same file Compose used; still gitignored. |
| `NAMESPACE` | `mlobs` | |
| `ROLLOUT_TIMEOUT_SECONDS` | `180` | Per-workload bound. Exceeding it fails the deploy. |

`apply.sh` applies the namespace, generates the Secret and the ConfigMaps,
renders the manifests into a temporary directory with the image refs and the
Prometheus config hash substituted, applies them, waits for all nine rollouts,
and finishes by running `smoke.sh`. Its exit status is the deploy's.

`smoke.sh` also runs on its own — that is the point of it. It is the check after
a `kubectl rollout undo` or a redeploy of a previous SHA (D25):

```bash
deploy/k3s/smoke.sh
```

It asserts the nine rollouts, `GET /health` 200, a `POST /predict` round trip
carrying `request_id` + `label` + `confidence`, that Prometheus reports exactly
the jobs configured in `prometheus/prometheus.yml` as up, and Grafana
`/api/health` 200. Needs `kubectl`, `curl` and `python3` on PATH.

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
| `docker compose stop shadow-scorer` (the latency A/B "off" switch) | `kubectl -n mlobs scale statefulset/shadow-scorer --replicas=0` |
| `docker compose start shadow-scorer` | `kubectl -n mlobs scale statefulset/shadow-scorer --replicas=1` |
| `docker compose logs -f api` | `kubectl -n mlobs logs -f deployment/api` |
| `docker compose logs -f consumer` | `kubectl -n mlobs logs -f statefulset/consumer` |
| `docker compose restart drift` | `kubectl -n mlobs rollout restart deployment/drift` |
| `docker compose exec -T postgres psql -U mlobs -d mlobs` | `kubectl -n mlobs exec -it deployment/postgres -- psql -U mlobs -d mlobs` |
| `127.0.0.1:9090` (Prometheus loopback publish) | `kubectl -n mlobs port-forward svc/prometheus 9090:9090` |
| `127.0.0.1:6379` (Redis loopback publish) | `kubectl -n mlobs port-forward svc/redis 6379:6379` |
| `localhost:8000` / `localhost:3000` | unchanged — both are host ports |
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

This mirrors the `K3sSmoke` CI job step for step; if it passes here it should
pass there. Requires Docker, `kubectl` and roughly 20GB of free disk — the two
torch images are large and each is stored twice, once by the docker daemon and
again in the cluster's containerd.

```bash
# 1. k3d, pinned to the version CI uses
curl -fsSL -o /tmp/k3d \
  https://github.com/k3d-io/k3d/releases/download/v5.9.0/k3d-linux-amd64
sudo install -m 0755 /tmp/k3d /usr/local/bin/k3d

# 2. a cluster shaped like the host: same k3s, same add-ons disabled,
#    the two host ports published through to your machine
k3d cluster create mlobs-dev \
  --image rancher/k3s:v1.36.4-k3s1 \
  -p '8000:8000@server:0' \
  -p '3000:3000@server:0' \
  --k3s-arg '--disable=traefik@server:*' \
  --k3s-arg '--disable=servicelb@server:*' \
  --k3s-arg '--disable=metrics-server@server:*' \
  --wait

# 3. build the four images
docker build -f docker/api.Dockerfile           -t mlobs-api:dev .
docker build -f docker/consumer.Dockerfile      -t mlobs-consumer:dev .
docker build -f docker/drift.Dockerfile         -t mlobs-drift:dev .
docker build -f docker/shadow_scorer.Dockerfile -t mlobs-shadow-scorer:dev .

# 4. import them. containerd normalises a bare name:tag to
#    docker.io/library/name:tag, which is why IMAGE_PREFIX is set that way below.
k3d image import -c mlobs-dev \
  mlobs-api:dev mlobs-consumer:dev mlobs-drift:dev mlobs-shadow-scorer:dev

# 5. deploy (needs a .env; cp .env.example .env and fill it in)
IMAGE_PREFIX=docker.io/library IMAGE_TAG=dev deploy/k3s/apply.sh

# 6. tear down
k3d cluster delete mlobs-dev
```

To rehearse a redeploy the way CI does, retag the images to `:dev2`, re-import,
and run `apply.sh` again with `IMAGE_TAG=dev2`. Editing
`prometheus/prometheus.yml` first is worth doing too: it should move the
`mlobs/config-hash` annotation and roll exactly the Prometheus pod.

```bash
kubectl -n mlobs get deployment prometheus \
  -o jsonpath="{.spec.template.metadata.annotations['mlobs/config-hash']}"
```

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
A/B switch is the scale idiom from the table above, exercised live; the shadow
scorer was restored to `replicas=1` afterwards.

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
