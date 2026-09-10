# k3s deployment

Kubernetes manifests and deploy scripts for running the ML Observability stack
on k3s, ported from `docker-compose.yml` (Phase 2 P2a; decisions D17–D27 in
`docs/PLAN.md`).

The Compose file remains the source of truth for *what each service is* — env
blocks, images and startup ordering are transcribed here rather than
reinterpreted, and every manifest names the Compose service it came from. It
also stays installed on the host as a fallback runtime for the duration of
Phase 2 (D25).

The on-host cutover runbook lands in `docs/K3S.md` with slice P2b. This file
covers the manifests, the two scripts, and how to rehearse them locally.

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
`IMAGE_PREFIX` for all four, the shadow image is built on the host (or fetched
from private S3 in P2c) and tagged into containerd under the same
`ghcr.io/muratalkan06/mlobs-shadow-scorer:<tag>` name the manifest expects. The
manifests set `imagePullPolicy: IfNotPresent`, so an image already present is
used and the registry is never consulted for it. The P2b runbook spells this
out.

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

## See also

- `docs/PLAN.md` — decisions D17–D27.
- `docs/K3S.md` — on-host cutover and rollback runbook (arrives with P2b).
- `PRINCIPLES.md` — binding engineering rules.
