# Operator runbook: canary windows, the TTL, and the rollback across the boundary

> Phase 3 O3. This is the host procedure for the `ServingDeployment` operator:
> opening and closing a canary window, what the 45-minute TTL does and why it
> is 45 minutes, the one tradeoff the operator makes on its own, and D36's
> rollback. The decisions are D31–D37 in [`PLAN.md`](PLAN.md). The operator
> itself is described in [`operator/README.md`](../operator/README.md), and
> what the traffic split does and does not promise is set out in the README,
> under [The canary split, stated plainly](../README.md#the-canary-split-stated-plainly).
>
> It starts where O4 leaves the host (issue #68): the D34 pair in
> `/etc/rancher/k3s/config.yaml`, the `hostPort` gone from `deployment/api`,
> and a post-O2 SHA deployed, so that the operator has adopted the api. #68
> owns that cutover and its own preflights, and none of it is repeated here.

Some steps run on the host and some through the pipeline. **Host** steps run
in an SSM session on the instance, as `ubuntu`, from
`/home/ubuntu/ml-observability-system`, with
`KUBECONFIG=/home/ubuntu/.kube/config`. That is the user, checkout and
kubeconfig the deploy document uses (`infra/ec2/deploy.tf`), so
`deploy/k3s/smoke.sh` there is the smoke check of the SHA that is deployed.
**Pipeline** steps are a reviewer-gated dispatch of the Deploy workflow
(`infra/README.md`, "Deploy pipeline").

## Where each step runs

| Step | Where | Who |
|---|---|---|
| Deploy a SHA: sets `spec.imageTag`, closes any open window | pipeline | a human dispatches |
| Open a window | host | a human |
| Close a window by hand | host | a human |
| Promote the canary | pipeline: dispatch the canary's SHA | a human dispatches; the operator orders it |
| Close a window at its TTL | in-cluster | the operator |
| Clear the canary fields an expired window leaves | pipeline, or host | either |
| D36 rollback, steps 1–4 | host | a human |
| D36 rollback, steps 5–6 | pipeline | a human dispatches |

To see where the operator stands at any point:

```bash
kubectl -n mlobs get servingdeployment api -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl -n mlobs get events --field-selector involvedObject.kind=ServingDeployment --sort-by=.lastTimestamp
```

## The 45-minute TTL, and its arithmetic

A window pauses the shadow scorer (D37): two torch api pods and the shadow do
not fit together under D26's memory bar on the 4GB host. While it is paused,
the api keeps appending to the stream, `XADD mlobs:predictions MAXLEN ~ 50000`
(PLAN §3; `src/inference_service/producer.py`), and the shadow's consumer group
reads nothing. Once the entries that arrived after the shadow's last read
outnumber the 50,000 the stream keeps, trimming starts removing entries it
never scored. That gap in
`shadow_predictions` is permanent, and nothing reports it: when the shadow
resumes, it reads on from the oldest entry still there.

- **The horizon** is `50000 / rps` seconds, where `rps` is the rate at which
  entries enter the stream: one per successful `/predict`.
- **The rule** (D37, frozen): the TTL stays at or below a third of the
  horizon, and is recomputed before any run at a higher rate.
- **At the frozen 5 rps** the horizon is 10,000 s, about 2.8 h. A third of it
  is 3,333 s, about 55 min, so 45 minutes holds.

| Observed rps | Horizon | A third of it | 45-min TTL within the rule? |
|---|---|---|---|
| 1 | 50,000 s (13.9 h) | 16,667 s (4.6 h) | yes |
| 5 (the simulator's default) | 10,000 s (2.8 h) | 3,333 s (55.6 min) | yes |
| 6 | 8,333 s (2.3 h) | 2,778 s (46.3 min) | yes, barely |
| 7 | 7,143 s (2.0 h) | 2,381 s (39.7 min) | **no** |
| 10 | 5,000 s (83.3 min) | 1,667 s (27.8 min) | **no** |

The break-even is `50000 / (3 × 2700)` ≈ 6.2 rps. The TTL is a compiled
constant (`canaryWindowTTL`, `operator/internal/controller/window.go`), not a
setting. Above about 6.2 rps the rule is therefore kept by hand: close the
window by `50000 / (3 × rps)` seconds (27 minutes at 10 rps), or do not open
one at that rate. Changing the constant is a code change with a `PLAN.md`
entry, not a runbook step.

**Measure the rate before opening**, in the steady state. The `api` job is
then the one stable pod, and during a window it is not (see the README
section linked above):

```bash
kubectl -n mlobs port-forward svc/prometheus 9090:9090 >/dev/null 2>&1 &
curl -sG http://127.0.0.1:9090/api/v1/query \
  --data-urlencode 'query=sum(rate(mlobs_stream_events_published_total{job="api"}[10m]))'
kill %1
```

**Watch the horizon during a window** from Redis, not from Prometheus:

```bash
kubectl -n mlobs exec deployment/redis -- redis-cli XLEN mlobs:predictions
kubectl -n mlobs exec deployment/redis -- redis-cli XINFO GROUPS mlobs:predictions
```

While the shadow is paused, the `shadow_scorer` group's `lag` grows by one per
request. It never exceeds the stream's length. **A `lag` equal to `XLEN` means
the shadow has reached the horizon**, and from there each new request pushes
one entry it never read out of the stream. Redis stops `lag` at the length
rather than reporting it as unknown. That was checked on the Redis 7.4 the
stack runs, against a stream trimmed past a paused group.

## Before opening a window

1. **The stack is steady.** `deploy/k3s/smoke.sh` ends
   `ok: smoke passed (steady state)`.
2. **The rate allows 45 minutes**, as above.
3. **The canary tag is a published SHA.** It must be a full 40-hex commit on
   `main` whose `GhcrPublish` completed. That is the rule #68 sets for the
   cutover dispatch, and it applies here for the same reason: the operator
   writes `ghcr.io/muratalkan06/mlobs-api:<tag>`, and the host pulls it when
   the canary starts. Check with `gh run list --branch main --commit <sha>`.
4. **The host has been shown to hold the window state.** O4's measured window
   rehearsal (#68: two api pods, the shadow at 0, 5 rps, `MemAvailable` at or
   above 400MB) is that evidence. Until it has passed, no live window is
   opened. Keep `canaryReplicas` at 1: each further replica is another torch
   pod the rehearsal did not measure.

## Opening a window (host)

```bash
kubectl -n mlobs patch servingdeployment/api --type merge \
  -p '{"spec":{"canaryImageTag":"<40-hex sha>","canaryReplicas":1}}'
STATE_TIMEOUT_SECONDS=300 deploy/k3s/smoke.sh   # ends "ok: smoke passed (window state)"
```

The operator scales the shadow scorer to 0 and waits to see it gone before it
brings `deployment/api-canary` up. `CanaryActive` turns True on the reconcile
that starts the pause, and its `lastTransitionTime` is the TTL's clock.
`ShadowPaused` turns True once the shadow is observed at 0. The deadline is in
`CanaryActive`'s message, `canary window open until <time>`. The longer
`STATE_TIMEOUT_SECONDS` covers the canary's model load.

## Closing a window, and promoting

- **By hand (host).** The constant close-window patch, the same one `apply.sh`
  sends (D32):

  ```bash
  kubectl -n mlobs patch servingdeployment/api --type merge \
    -p '{"spec":{"canaryImageTag":null,"canaryReplicas":0}}'
  deploy/k3s/smoke.sh   # ends "ok: smoke passed (steady state)"
  ```

- **By any pipeline deploy.** It prints
  `ok: canary window closed by the close-window patch (D32)`. A deploy of a
  different SHA also moves the stable, and that ends the comparison the window
  was opened for.
- **Promotion** is that same pipeline deploy, with the canary's own SHA:

  ```bash
  gh workflow run deploy.yml --ref main -f sha=<the canary's sha>
  ```

  The stable rolls to the promoted tag and completes while the canary still
  serves. Only then does the canary go to 0 and the shadow come back to 1
  (D32, and D37's O2 addendum). No host step is involved. It is a whole
  deploy of that SHA: `IMAGE_TAG` is one tag for all five images, so the
  consumer, the drift jobs, the shadow scorer and the operator move to it
  with the api.

Every close follows the same order: the canary's pods are gone before the
shadow returns.

## When the TTL lapses

At 45 minutes from `CanaryActive`'s last False→True transition, the operator
brings the stack back to steady by itself, in the same order as any close and
under the same stable-first guard (below). It sets `CanaryActive` False with
reason `WindowExpired` and records a `WindowExpired` event. It does not touch
the spec, because it never writes one (the D32 clarification of O2). The
canary fields therefore stay. That spec is legal and equivalent to steady:
`smoke.sh` judges from cluster state and conditions, and it passes.

- **The latch.** While those fields stay, editing them does not reopen the
  window. A new window needs the spec to pass through the closed shape first,
  and a reconcile has to see it there. Send the close patch above, and wait
  until `CanaryActive`'s reason reads `NoCanary`:

  ```bash
  kubectl -n mlobs get servingdeployment api \
    -o jsonpath='{.status.conditions[?(@.type=="CanaryActive")].reason}'
  ```

  Then send the open patch. If both are sent back to back, one reconcile can
  see only the second, and the latch holds.
- **Clearing the fields.** Either the close patch by hand or the next pipeline
  deploy clears them. The deploy prints the "closed" fixed line even though
  the TTL already closed the window: the line reports the fields it cleared,
  not the condition.

## The stable-first guard, and what it costs

Every close waits for `deployment/api` to be Available before it scales the
canary to 0. That covers a close by hand, by the pipeline, by promotion and by
the TTL alike. This is D32's no-serving-gap rule, and in the ordinary case it
costs nothing.

**The named tradeoff:** if the stable is not Available when a window closes,
the canary keeps serving and the shadow stays paused. That happens when the
stable is crash-looping at the TTL, or when a promotion's new stable never
becomes ready. It lasts as long as the stable stays down, past 45 minutes if
it comes to that. The operator puts availability ahead of shadow data:
`/predict` keeps answering from the canary while the shadow's horizon runs.

How it shows:

- `CanaryActive` is False, but canary pods run.
- `CanaryActive`'s message ends
  `deployment/api-canary keeps serving until deployment/api is Available`.
- `ShadowPaused` stays True.
- `smoke.sh` fails on `the stack is in neither the steady nor the window state`.

What to do:

1. Look at the stable: `kubectl -n mlobs describe deployment api`, and
   `kubectl -n mlobs logs deployment/api`.
2. Fix it through the pipeline. Dispatch the last SHA known good or, if the
   canary is the one to trust, the canary's SHA, which is a promotion. The
   guard releases as soon as `deployment/api` is Available: the canary goes,
   then the shadow comes back.
3. Watch the horizon meanwhile (`XINFO GROUPS`, above). If the stable cannot
   be fixed before `lag` reaches `XLEN`, the gap is the price of keeping the
   api up. Record when it started and how long it lasted on the issue that
   covers the window.

What not to do:

- **Do not scale the canary to 0 by hand.** With the stable down, `:8000`
  goes dark. That trades the api's availability for the shadow's data, the
  reverse of the choice the guard makes.
- **Do not scale the shadow up by hand.** While the guard holds, the operator
  leaves the shadow's scale alone, so nothing would put it back. Beside a
  serving canary and a restarting stable, that is the state D26's bar rules
  out.

## The shadow scorer down outside a window: a detection gap

Outside a window, the operator holds the shadow scorer's scale at 1. It
enforces the scale, not the shadow's health. A shadow that crash-loops at 1
leaves `ShadowPaused` False, with reason `ShadowRunning`, because the
condition reads the StatefulSet's scale and pod count. Nothing alerts on it.
Its data gap is the same as a paused shadow's: permanent past the horizon.

`deploy/k3s/smoke.sh` catches it. The steady state requires the shadow scorer
ready and the `shadow_scorer` target up. But it catches it only when someone
runs it: after every deploy it runs by itself, and it is worth running after
an instance start or any other host event.

An alert is **recommended and not deployed**. The stack has no Alertmanager,
and Slack alerting comes from the drift job alone. Two rules would cover it:

```yaml
# The shadow down while no window is open.
- alert: ShadowScorerDownOutsideWindow
  expr: up{job="shadow_scorer"} == 0 unless on() up{job="api_canary"} == 1
  for: 10m
# The shadow paused longer than any window: a window held open by the
# stable-first guard, or an operator that stopped reconciling.
- alert: ShadowScorerPausedTooLong
  expr: up{job="shadow_scorer"} == 0
  for: 60m
```

These are candidates for the phase-close hardening pass (#60), not a
commitment of this slice.

## Rolling back across the boundary (D36)

**When:** the operator itself has to come out, because it misbehaves in a way
no deploy fixes, or because O4's cutover is being undone. A bad api release is
not this case. Dispatch the previous post-cutover SHA instead, and the
operator rolls the stable back (`infra/README.md`, "Rolling back").

**Before you start:**

- **The pre-cutover SHA.** This is the SHA the host ran before O4's cutover.
  O4 records it on #68, from the host checkout's `git rev-parse HEAD` taken
  before the cutover dispatch. The CI rehearsal pins `16a8c86`, the last
  deploy on record before O4.
- **It must still be deployable.** Its GHCR images do not expire, but its
  shadow tarball in S3 does, 60 days after the push that uploaded it.
  `16a8c86`'s dates from 2026-09-27, and the last pre-O2 push, `32bb140`, from
  2026-09-29. By the end of November 2026 no pre-cutover SHA can pass the
  deploy preflight, and step 5 then needs a new commit on `main` whose tree is
  pre-cutover, which is a revert with its own review. **This fallback has a
  shelf life.**
- **The database needs nothing.** `sql/` is identical between `16a8c86` and
  O2, so no schema change crosses this boundary and D25's restore is not
  needed. O4's `pg_dump` preflight stands regardless.
- **`:8000` goes dark from step 3 until step 5's api is ready.** That is the
  length of a pipeline run plus a model load: minutes. Schedule for it.

**Steps 1–4, on the host:**

1. **The canary to 0**, by the close-window patch, and check that the stack is
   steady. With no window open this is a no-op. Send it anyway.

   ```bash
   kubectl -n mlobs patch servingdeployment/api --type merge \
     -p '{"spec":{"canaryImageTag":null,"canaryReplicas":0}}'
   deploy/k3s/smoke.sh   # ends "ok: smoke passed (steady state)"
   ```

   If this stalls on the stable-first guard, deal with that first (above). The
   rehearsal runs step 1 from a healthy stable only.
2. **Delete the ServingDeployment while the operator still runs.**

   ```bash
   kubectl -n mlobs get deployment operator   # READY 1/1
   kubectl -n mlobs delete servingdeployment/api
   ```

   Use a plain delete. `--cascade=orphan` would leave `deployment/api` running
   with no owner, and skip the proof that step 3 waits for.
3. **Wait, to a bound, for garbage collection.**

   ```bash
   kubectl -n mlobs wait --for=delete deployment/api deployment/api-canary --timeout=180s
   kubectl -n mlobs get pods -l app=api   # no api pod left
   ```

   `deployment/api` gone through its ownerReference is the proof that the
   adoption is undone. If the bound passes and it is still there, stop: the
   adoption is not undone. Read its `ownerReferences` before anything else.
4. **The operator down, the CRD left inert.**

   ```bash
   kubectl -n mlobs scale deployment/operator --replicas=0
   kubectl -n mlobs get pods -l app=operator                  # none
   kubectl get servingdeployments --all-namespaces            # none
   ```

   To remove the operator for good instead, run
   `kubectl -n mlobs delete deployment/operator`. Then, last, and only with no
   ServingDeployment left anywhere, run
   `kubectl delete crd servingdeployments.serving.mlobs.dev`. Deleting the CRD
   earlier would take every CR with it, outside D36's order. The rehearsal
   runs the scale-down.

**Steps 5–6, the pipeline:**

5. **Dispatch the pre-cutover SHA.**

   ```bash
   gh workflow run deploy.yml --ref main -f sha=<pre-cutover sha>
   ```

   Then approve the run. That tree's `apply.sh` recreates `deployment/api`
   from its `20-api.yaml`, as a new object with no owner and with
   `hostPort: 8000`, and turns `service/api` back into a ClusterIP.
6. **`smoke.sh` green.** The run's output ends `ok: smoke passed`: the
   pre-cutover `smoke.sh`, which is not state-aware, so the line has no state
   suffix. Then run `deploy/k3s/smoke.sh` on the host on its own. The checkout
   is now the pre-cutover tree, so this is that tree's check.

**Left in place, all inert:** the CRD; `deployment/operator` at 0; its
ServiceAccount, Role, RoleBinding and leader-election Lease; and
`service/api-canary`, which selects no pod. The D34 pair stays in `/etc/rancher/k3s/config.yaml`. The pre-cutover
tree uses no NodePort, and the rehearsal runs it on a k3s that carries the
pair.

**Going forward again** is #68's cutover from its step 2. The `hostPort` patch
comes first, because the recreated `deployment/api` carries `hostPort: 8000`
again. Step 1's flags are already in place. The dispatch re-applies the CRD
and the operator at one replica, and the new ServingDeployment adopts the new
`deployment/api`. This path is not rehearsed on its own. The rehearsal's
setup runs the same two steps, but on a cluster that had never had the
operator.

### What the rehearsal proves, and what it does not

CI's `RollbackRehearsal` job runs `deploy/k3s/rehearse-rollback.sh` on the
pinned k3s with the D34 pair. It deploys the pre-cutover tree through its own
`apply.sh`, runs #68's steps 2–3 so that the operator adopts `deployment/api`
in place (same uid), and opens a window. Then it runs the six steps above in
order, asserting each on what it leaves: conditions, replica counts, owners
and uids, the Service's type, and the scripts' fixed lines. The local recipe
is in `deploy/k3s/README.md`.

It does not cover:

- **The pipeline around `apply.sh`.** The script calls each tree's `apply.sh`
  directly, as the SSM document does after its checkout. The dispatch, the
  approval, the preflights and the tarball import are not in it.
- **The old images.** It deploys this commit's builds under the pre-cutover
  tag. The inputs they are built from are unchanged since `16a8c86`, which the
  job checks and reports.
- **The host's memory.** A k3d cluster on a CI runner says nothing about the
  4GB host. That is O4's measured rehearsal.
- **Step 1 from a stable that is down.**
