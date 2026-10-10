# k3s on the EC2 host — why, and when to revisit

> Rung 2 of the frozen stretch ladder in [`PLAN.md`](PLAN.md) is "k3s migration
> **with a written why/when doc**". This is that doc. The decisions are D17–D27
> in `PLAN.md`; the cutover as executed on 2026-09-27, with the memory rehearsal
> and the raw numbers, is recorded in
> [`deploy/k3s/README.md`](../deploy/k3s/README.md#migration-record-and-rehearsal-results-2026-09-27)
> and issue #48.

## Why

The move is the next rung of the ladder, taken in order after the shadow
comparison (rung 1, `v1.1.0`). The owner ruled k3s on 2026-09-01 as honouring
the frozen ladder as written, so it carries no deviation entry (`PHASE2.md`).
It is not a capacity argument: this is one node running one uvicorn worker, and
k3s itself costs about 282 MB of RAM at idle on this host.

What the move buys is a runtime that is declared and reconciled rather than
remembered. Compose left three things implicit on this host, and one of them
failed in plain sight:

- **Restart after an instance start.** Seven of the nine services in
  `docker-compose.yml` carried `restart: unless-stopped`; `prometheus` and
  `grafana` carried no restart policy. After the instance was started on
  2026-09-21 those two never came back: the pre-cutover check on 2026-09-27
  found 7/9 containers up and Grafana — the project's only UI — dark for about
  six days. Two lines of YAML would have closed that particular gap. The point
  is that under Compose each service opts in, and two had not; under k3s
  nothing opts in, because k3s is an enabled systemd service and every workload
  is a controller-managed object that comes back with it.
- **Durability.** Compose declared no named volume. Postgres data lived in an
  anonymous volume that a plain `docker compose down` would have orphaned (PLAN
  erratum 2026-09-27). D20's PVC makes the durability the prose always claimed
  a property of the manifests.
- **The deploy as an artifact.** Compose built images on the host from whatever
  tree was checked out. Under k3s a deploy names an image tag, waits on nine
  rollouts under a hard bound, and ends in the same `smoke.sh` the CI rehearsal
  runs against the same pinned k3s (D17, D22, D25). That is also the foothold
  P2c's OIDC/SSM pipeline needs (D24).

## What changed

Same Dockerfiles, same ports, same scrape file, same `.env` — a different
runtime underneath.

| | Compose (to 2026-09-27) | k3s `v1.36.4+k3s1` (from 2026-09-27) |
|---|---|---|
| Images | built on the host (`up --build`) | `api`, `consumer`, `drift` from GHCR, built by CI; `shadow-scorer` built off-host and imported into containerd (D18) |
| Ports | `:8000`, `:3000` published; `:9090`, `:6379` on loopback | `:8000`, `:3000` as hostPorts, security group untouched (D19); Prometheus and Redis via `kubectl port-forward`. From Phase 3 O2 the api's `:8000` is a NodePort Service on the exact 8000-8000 range instead (D34 and the D19 erratum); on the host from O4 |
| Scrape config | `prometheus/prometheus.yml` bind-mounted | the same file as a ConfigMap, mounted verbatim and hash-annotated (D21) |
| Postgres data | anonymous volume | `local-path` PVC, 5Gi; everything else ephemeral (D20) |
| Secrets | `.env` interpolated by Compose | the same `.env` rendered into one Secret (D23) |
| After an instance start | per-service restart policy; two services had none | k3s systemd unit; all nine workloads controller-managed |
| Shadow A/B switch | `docker compose stop` / `start shadow-scorer` | `kubectl -n mlobs scale statefulset/shadow-scorer --replicas=0` / `1` — exercised live on 2026-09-27; retired in Phase 3 O2, when the operator became the sole writer of the shadow scorer's scale: it pauses the shadow only inside a canary window and puts a hand scale back (D37) |

The demo history crossed the cutover by `pg_dump --clean --if-exists` and
restore: predictions 8386 → 8386, shadow_predictions 3628 → 3628, drift_runs
18884 → 18885 (the +1 is a fresh k3s drift cycle). The D26 memory rehearsal
passed with a 4.7× margin, so the host stays a `t3.medium`.

The "after an instance start" row holds by construction. The P2b evidence does
not include a stop/start of the migrated host, so the first one is its live
test.

## When to revisit

- **P3 — EKS and Helm.** Plain manifests are the right size for one
  instantiation (D17). P3's ephemeral EKS run is where a chart has something to
  prove; revisit D17 there, including whether these manifests become the
  chart's source or give way to it. D38 settled the chart half in Phase 3
  H1: these manifests stay the authority, and the operator chart at
  `deploy/helm/mlobs-operator/` is a CI-checked projection of three of them
  (`01-` to `03-`), with no Helm in the k3s path. The EKS run is H2 (D39).
  Its config and owner-run script, never CI, are in `deploy/eks/`.
- **Compose retirement — executed at P3 close (H3, D40).** Until then Compose
  was the host's fallback runtime (D25) and the README quick start for local
  development. Retiring `docker-compose.yml` was a change with its own risk
  (D27), so it shipped as its own slice: the file is deleted, its last shipping
  commit `f33b65909820d9a4659e291f166e448077dab677` is recorded in
  `deploy/k3s/README.md`, D25's fallback runtime ended with it, and the README
  quick start is the local k3d recipe.
- **k3s version bumps.** The pin (D19) lives in three places — the host
  install, `K3S_IMAGE` in CI, and the local k3d recipe — and moves in one
  change, with `K3sSmoke` green on the new pin before the host moves. Each bump
  is recorded as a `PLAN.md` erratum against D19. D34's node-port range,
  `--service-node-port-range=8000-8000`, is pinned in the same three places
  as an atomic pair with `--disable-network-policy` (the D34 erratum of O2:
  k3s's network-policy controller refuses a single-port range and the server
  crash-loops): the host's `/etc/rancher/k3s/config.yaml`
  (`deploy/k3s/README.md`, "Host k3s flags"; the host gains the pair inside
  O4's gated window, issue #68), the `K3sSmoke` k3d arguments, and the local
  recipe. A bump re-verifies `127.0.0.1:8000`, which reaches the NodePort
  through kube-proxy's iptables-mode `route_localnet`, and re-checks whether
  the network-policy controller still refuses the exact range.
- **A second environment.** D12, D17 and D23 all name it as their trigger: a
  module layer for Terraform, a templating layer for the manifests, a sealing
  tool for secrets. None of them earns its keep with one host.

## Numbers policy (D27)

- Compose-era measurements are historical. They stay in `README.md` as
  measured, annotated in place with the runtime and the instance type they were
  taken on; they are never deleted and never reused as k3s figures.
- k3s measurements are appended beside them with full methodology — host and
  instance type, tool and version, warm-up, window, reproduce block
  (`PRINCIPLES.md` §6). EC2 and local numbers still never mix.
- A runtime change is a methodology change. A later runtime or instance type
  repeats the pattern: annotate, re-measure, append.

The re-measurement, taken on 2026-09-27 on the EC2 `t3.medium` under k3s
`v1.36.4+k3s1`: `hey` 0.1.5 on-instance, 15 s warm-up then 120 s measured,
`-c 1 -q 5`, fixed payload — the README methodology unchanged.

| Window | req/s | p50 | p95 | p99 |
|---|---|---|---|---|
| shadow ON (`replicas=1`) | 4.96 | 62.2 ms | 111.2 ms | 300.6 ms |
| shadow OFF (`replicas=0`) | 4.95 | 59.8 ms | 101.0 ms | 243.9 ms |

The shadow scorer's cost to the primary path under k3s is ≈2.4 ms at p50 and
≈10 ms at p95. At p95 that computes to +10.1% (111.2 vs 101.0 ms), just outside
the v1.1 ≤10% criterion the compose-era run met at −1.0%. It comes from one
matched pair of 120 s windows and is published as measured, not re-certified.
