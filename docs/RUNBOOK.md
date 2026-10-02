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
> Its abort criteria are the exception. They sit below, beside the D36
> rollback they lead to.

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

- **Do not scale the canary to 0 by hand while the operator is live.** With
  the stable down, `:8000` goes dark. That trades the api's availability for
  the shadow's data, the reverse of the choice the guard makes. The exception
  is a dead operator: its three-step close ("Closing a window under a dead
  operator") scales the canary to 0 by hand, as its step 2.
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

## Abort criteria (O4 cutover window)

These are the lines #68's cutover and its one live canary window are held
to. Each names a check and what follows when it fails. A check that cannot be
run counts as failed. Abort first and diagnose after. Record every abort on
#68, with the line that tripped it and the time.

There are two ways back, and they differ:

- **From the cutover**, the way back depends on how far it got (the table
  below). D36 is its full form.
- **From a canary window**, the classification table at the end of that part
  decides: the close-window patch, the close under a dead operator and then
  D36, or the pipeline.

### Before the window: do not start

All four hold before #68's step 1, or nothing starts. Nothing has changed
yet, so there is nothing to undo.

1. **The `pg_dump` preflight.** NO-GO if the dump exits non-zero, if the file
   is empty, or if `grep -c '^COPY public\.' <dump>` is not 3: `sql/` defines
   `predictions`, `shadow_predictions` and `drift_runs`, and no other table.
   NO-GO if a live count is below the P2b baseline: predictions 8386,
   shadow_predictions 3628, drift_runs 18884 (`deploy/k3s/README.md`,
   "Migration record"). Nothing in `src/` or `sql/` deletes rows, so the
   counts can only have grown.

   ```bash
   kubectl -n mlobs exec deployment/postgres -- psql -U mlobs -d mlobs -tAc \
     "SELECT (SELECT count(*) FROM predictions), (SELECT count(*) FROM shadow_predictions), (SELECT count(*) FROM drift_runs)"
   ```

   Record the three counts on #68, with the dump's path and size.
2. **The dispatch SHA is published.** `gh run list --branch main --commit <sha>`
   shows the `CI` run for that push `completed` with `success`. NO-GO on
   `cancelled`, which happens when merges land close together (#68), or on no
   run. The SHA is `8dc1a82` or a later commit on `main`: the first post-O2
   SHA with a completed publish. The Deploy workflow's GHCR preflight checks
   `mlobs-operator` with the other three images, but it catches a missing
   image only by failing the dispatch.
3. **The way back exists.** The pre-cutover SHA, from `git rev-parse HEAD` in
   the host checkout, is on #68, and D36's "Before you start" holds for it:
   its shadow tarball is inside S3's 60 days. NO-GO otherwise. Without it, D36
   step 5 has nothing to dispatch.
4. **The measured window rehearsal has passed.** PHASE3.md's O4 acceptance:
   two api pods, the shadow at 0, 5 rps, `MemAvailable` at or above 400MB and
   zero OOM kills. Read the bar as 409,600 kB, the 400 MiB the P2b rehearsal
   read D26's bar in, and sample every 30 s as it did:

   ```bash
   grep MemAvailable /proc/meminfo                     # every sample >= 409600 kB
   sudo dmesg -T | grep -iE 'out of memory|oom-kill'   # no line from the run
   ```

   NO-GO on a miss: no live window opens (item 4 of "Before opening a
   window"). If the rehearsal runs on the cut-over stack, a miss closes its
   window and ends the session there. The cutover stays, in steady state, and
   the live window waits.

### During the cutover: abort and go back

The steps are #68's handoff note, items 1–3.

1. **k3s after the D34 pair.** GO once k3s is back, within 5 minutes of the
   restart. Back is all three of: the node reads `Ready`; the nine workloads
   are restored; and `:8000` answers 200, at this stage still through the
   `hostPort`. `systemctl is-active k3s` reading `active` is not back. The
   pre-cutover `deploy/k3s/smoke.sh` checks the nine and `:8000`. GO also
   needs `br_netfilter` loaded:

   ```bash
   kubectl get nodes                                                        # Ready
   deploy/k3s/smoke.sh                                                      # ends "ok: smoke passed"
   curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8000/health   # 200
   lsmod | grep br_netfilter                                                # the module
   ```

   NO-GO at 5 minutes, on k3s restarting in a loop
   (`sudo journalctl -u k3s -n 50`), or on a red smoke. The node was Ready
   3 s after P2b's install.
2. **The `hostPort` patch.** It costs one Recreate bounce, and `:8000` is then
   dark until the dispatch's NodePort Service lands (#68).
   - GO: `kubectl -n mlobs rollout status deployment/api --timeout=180s`
     completes (180 s is `apply.sh`'s `ROLLOUT_TIMEOUT_SECONDS`), and
     `kubectl -n mlobs get deployment api -o jsonpath='{..hostPort}'` prints
     nothing. Dispatch at once.
   - NO-GO: the rollout has not completed at 180 s.
   - **The dark bound.** `:8000` is dark for 30 minutes at most, the way back
     included. NO-GO at 15 minutes: if
     `curl -fsS http://127.0.0.1:8000/health` has not answered 15 minutes
     after the patch, go back, whatever the run is doing. 15 is the
     30-minute ceiling less the longest way back on record, 13 minutes (the
     deploy run 36517554863, its approval wait included), with about 2
     minutes to spare. 13 minutes is the longest observed, so it is a floor
     on the worst case and not a bound, and the 2 minutes can be used up.
     That is a known risk, accepted for a window with no users.
   - **Why the dark window exists at all.** Nothing can be staged ahead of
     it. D34's range is exactly 8000-8000, so no other NodePort is
     admissible and the Service cannot land on a scratch port first, while
     8000 is the `hostPort`'s until the patch. The D34 tradeoff note is
     recorded in `PLAN.md` at O4's close.
3. **Adoption.** Before dispatching, record
   `kubectl -n mlobs get deployment api -o jsonpath='{.metadata.uid}'`.
   - GO: the conditions command at the top of this runbook shows
     `Ready=True StableAvailable`, the events show `Adopted`, the uid is
     unchanged, and `{.metadata.ownerReferences[0].kind}` on
     `deployment/api` reads `ServingDeployment`.
   - NO-GO: `Ready=False AdoptionFailed`; a Warning `AdoptionFailed` event,
     whose message names the owner already there; or a new uid.
4. **The pipeline deploy.** NO-GO: the run fails anywhere, at a preflight, an
   `apply.sh` `die` line or `smoke.sh`.
5. **State-aware smoke.** GO: the run ends `ok: smoke passed (steady state)`,
   and `deploy/k3s/smoke.sh` on the host ends the same. NO-GO: either is red,
   or names any other state.

**The way back, by how far the cutover got:**

| How far | How to tell | The way back |
|---|---|---|
| k3s restarted, `deployment/api` unpatched | `kubectl -n mlobs get deployment api -o jsonpath='{..hostPort}'` prints `8000`, or k3s is down | Take the D34 pair out of `/etc/rancher/k3s/config.yaml`, restart k3s, run the pre-cutover `smoke.sh` |
| `hostPort` patched, no ServingDeployment | `kubectl -n mlobs get servingdeployment api` reports NotFound, or no such resource type | D36 step 4 if `deployment/operator` exists, then steps 5–6 |
| A ServingDeployment exists | the same command returns it | D36 from step 1 |

- **After a failed adoption, D36 step 3 has nothing to collect.**
  `deployment/api` never took the ServingDeployment's ownerReference, so
  garbage collection leaves it. Check that
  `kubectl -n mlobs get deployment api -o jsonpath='{.metadata.ownerReferences}'`
  names no ServingDeployment, and go on to step 4.
- **The second row and the failed adoption are not rehearsed.** The
  rehearsal's step 5 recreates a garbage-collected `deployment/api`. Here the
  pre-cutover `apply.sh` applies `20-api.yaml` over the object it once
  created. At step 6, also check that `{..hostPort}` prints `8000` again.

### During the live canary window: close it

Closing is the close-window patch ("Closing a window, and promoting"), then
`deploy/k3s/smoke.sh` ending `ok: smoke passed (steady state)`. Which way back
a tripped line takes is the classification table's call, after the lines.

**Before the open patch**, three things on the host:

- **Start the watch.** In a second SSM session, from the same checkout:

  ```bash
  deploy/k3s/watch-window.sh
  ```

  It runs until Ctrl-C and prints a session summary then. It decides
  nothing. Every 60 s it prints the interval's values and a BREACH or ok mark
  for lines 2, 3 and 5 below, and the human acts. Its port-forwards use local
  ports 18000 (the stable) and 18001 (the canary), clear of the 9090 and 8001
  `smoke.sh` opens, so the open's `smoke.sh` runs beside it. Stop it once the
  close has converged.
- **Record the OOM protection k3s runs with.** This is a record, not a gate.
  The kubelet that k3s embeds sets its own process's `oom_score_adj`, so -999
  is expected for k3s. containerd, a child of k3s, is read the same way. Put
  both observed values on #68: the runbook claims only what the host shows.

  ```bash
  k3s_pid="$(systemctl show -p MainPID --value k3s)"
  cat "/proc/${k3s_pid}/oom_score_adj"                                  # expected -999
  cat "/proc/$(pgrep -P "${k3s_pid}" -x containerd)/oom_score_adj"
  ```

  The PID comes from systemd rather than from a process name: it is the
  process `k3s.service` started, whatever title it shows.
- **Record the stable's shape**, for the classification table:

  ```bash
  kubectl -n mlobs get deployment api -o jsonpath='{.metadata.generation} {.spec.replicas}{"\n"}'
  kubectl -n mlobs get service api -o jsonpath='{.spec.selector}{"\n"}'
  ```

**Where to read the two pods.** No dashboard panel separates them. The
*mlobs — API & Inference* panels select no job, so during a window they blend
the two pods (README, "The canary split, stated plainly"). Prometheus cannot
read the stable alone either: in a window the `api` job reaches either pod,
and v0 has no stable-only job (the same README section). So both pods are
read the same way, each off its own `/metrics`, and Prometheus is not the
decision source for lines 2 and 3.

**The evaluation interval** for lines 2 and 3:

- **60 s.** `watch-window.sh` evaluates one interval every 60 s, on its own
  clock.
- **The floor: 50 `/predict` requests on each pod in the interval**, the
  canary and the stable both. At 5 rps split about evenly, each pod sees about
  150 (5 × 60 / 2), three times the floor. The split is per connection, so
  the window's load generator must open a fresh connection for every request:
  a single keep-alive connection pins one pod and starves the other below the
  floor. The simulator holds one connection (README), so it cannot be the
  window's load.
- **The reference: the stable's p95 over the same 60 s.** Same host, same
  interval: what the host does to latency reaches both pods, so the
  comparison controls for it.
- **An interval that cannot be evaluated counts as a breach** for the
  two-in-a-row counter: a pod under the floor, a pod read that fails, a check
  skipped.

**How the two pods are read.** At each interval's end, `watch-window.sh`
reads the stable (`app=api`, no `role` label) and the canary (`role=canary`)
through a port-forward to each, by one function. Both sides get the same two
series, `mlobs_http_requests_total` and
`mlobs_http_request_duration_seconds_bucket` with `endpoint="/predict"`, and
the same math: the change since the last read, and the p95 by
`histogram_quantile`'s own interpolation over the bucket changes. The 5xx
count is the change in the first series' `status` 5xx part. A pod read that
fails, a pod that changed between two reads, or a counter that went down
leaves the interval unevaluable.

The `job="api_canary"` series in Prometheus still show the canary alone, in
Grafana's Explore, for a look. They are not the decision source: their `[1m]`
window is not the watch's interval, and the stable has no series of its own
to set beside them.

Close the window on any of these:

1. **The canary is not serving.** After the open's `smoke.sh` passed in the
   window state, `up{job="api_canary"}` reads 0, or
   `kubectl -n mlobs get deployment api-canary` is not `1/1`.
2. **Canary errors.** Any 5xx from the canary pod, read off its `/metrics`
   as above, in an interval that starts 60 s or more after the canary pod
   turned Ready. The first 60 s after Ready are warmup and are excluded: an
   interval that starts inside them does not count its 5xx. A canary read that
   fails leaves the interval unevaluable, toward line 3's counter. The stable's measured runs
   record zero errors (README, "Load test").
3. **Canary latency.** On two intervals in a row, the canary's p95 is above
   1.25 times the stable's p95 over the same interval. An unevaluable
   interval counts as one of the two. The margin sits over the ~14% the
   README measured between two identical 60 s runs ("Load test", its
   methodology note). Its ≤10% bar was read on matched 120 s windows, which a
   live window is not.
4. **The window state breaks.** While `deployment/api-canary` has a pod:
   `CanaryActive` is not `True WindowOpen`; `ShadowPaused` is not
   `True ShadowScaledToZero`; `kubectl -n mlobs get statefulset shadow-scorer`
   is not `0/0`; or `STATE_TIMEOUT_SECONDS=300 deploy/k3s/smoke.sh` does not
   end `ok: smoke passed (window state)`. A shadow beside the canary is the
   state D26's bar rules out.
5. **Memory.** An interval's minimum `MemAvailable` below 409,600 kB, the
   400 MiB bar of the rehearsal gate, or any oom-kill line in the kernel log
   since the watch started. On an OOM kill, close at once. `watch-window.sh`
   samples `/proc/meminfo` every 10 s and reports each 60 s interval's
   minimum, not an instant value, and it reads the kernel log every
   interval. Prometheus holds no host-memory series: the stack runs no
   node_exporter, by design, so this line is read on the host. There are two
   regimes, and they are not the same risk:
   - **The canary past its own limit.** The canary runs the stable's pod
     template, with a 1Gi request and a 1536Mi limit (D8). They are set in
     `newAPIDeployment`, `operator/internal/controller/stable_deployment.go`,
     and the adopted `deployment/api` carries the same pair from the
     pre-cutover `20-api.yaml`. Past 1536Mi, its cgroup's OOM kill takes the
     canary, deterministically, and nothing else.
   - **The host under pressure, both torch pods inside their limits.** The
     kernel picks the victim by `oom_score_adj`, then RSS. The stable and the
     canary are both Burstable with the same 1Gi request, so their
     `oom_score_adj` ties, and the larger RSS breaks the tie. The victim is
     not deterministic, and it can be the stable. A PriorityClass and
     `kube-reserved` are the fixes. Both are deferred to the hardening issue
     (#60), ranked above its cosmetic items.
6. **The deadline: the operator acts at it.** 45 minutes is the ceiling, not
   the plan. The deadline is in `CanaryActive`'s message,
   `canary window open until <time>`. Above about 6.2 rps it comes sooner:
   `50000 / (3 × rps)` seconds ("The 45-minute TTL, and its arithmetic").
   Close or promote before it. The line trips when the operator has not acted
   within 60 s of the deadline. Acted is either `CanaryActive` False with
   reason `WindowExpired`, or `deployment/api-canary` at `spec.replicas` 0:

   ```bash
   kubectl -n mlobs get servingdeployment api -o jsonpath='{.status.conditions[?(@.type=="CanaryActive")].status} {.status.conditions[?(@.type=="CanaryActive")].reason}{"\n"}'
   kubectl -n mlobs get deployment api-canary -o jsonpath='{.spec.replicas}{"\n"}'
   ```

   A live operator acts at the deadline: its reconcile is requeued for it
   (`RequeueAfter`), and comes milliseconds late. A trip therefore means the
   operator is absent or wedged, which is why the close below assumes a dead
   operator. Once it has acted, the ordered teardown has expectations of its
   own: the canary's pod seen gone within its termination grace plus 30 s of
   slack, 60 s in all, then the shadow scorer back at 1. The grace is
   Kubernetes' 30 s default: neither `newAPIDeployment` nor the pre-cutover
   `20-api.yaml` (`16a8c86`) sets one. A status that flipped over a teardown
   that stalls is not this line. It goes to the table.

**The classification table.** What a tripped line, or the checks, turn up
goes through it, top row first:

| What is seen | At fault | The way back |
|---|---|---|
| Line 6 trips; or `deployment/api`'s replicas, the `api` Service's selector or the stable's pod template changed by anything but the pipeline; or `Ready=False AdoptionFailed`, or an event that names a foreign owner | the operator | the close under a dead operator (below), then D36 |
| Any of lines 1–5 | the canary or the host | close the window: the close-window patch, on the host |
| The stable unhealthy, with the window closed | the stable | through the pipeline: the stable-first guard's case ("The stable-first guard, and what it costs") |

For the first row: a `.metadata.generation` on `deployment/api` that moved
with no pipeline run since the open, or a selector other than
`{"app":"api"}`, against the values recorded before the open.

### Closing a window under a dead operator: D36's step zero

Three steps, in this order. Then, and only then, D36 from step 1.

1. **The close-window patch to the spec.**

   ```bash
   kubectl -n mlobs patch servingdeployment/api --type merge \
     -p '{"spec":{"canaryImageTag":null,"canaryReplicas":0}}'
   ```

   With the operator dead, this changes nothing on the cluster: it is inert.
   It is sent so that an operator that comes back finds the closed shape and
   cannot reopen the window.
2. **The canary to 0, and its pod seen gone.** Wait for it: D37's memory
   order binds a human as it binds the operator, and the canary's pod is gone
   before the shadow returns.

   ```bash
   kubectl -n mlobs scale deployment/api-canary --replicas=0
   kubectl -n mlobs wait --for=delete pod -l app=api,role=canary --timeout=60s
   kubectl -n mlobs get pods -l app=api,role=canary   # no pod left
   ```

3. **The shadow back to 1.**

   ```bash
   kubectl -n mlobs scale statefulset/shadow-scorer --replicas=1
   ```

   This is safe either way: the operator is dead, or it comes back into the
   steady state the closed spec asks for, which agrees.

Two things differ in D36 under a dead operator:

- **Step 1's `smoke.sh` cannot end steady.** It reads the conditions the
  operator writes, and they stay behind the patched spec. Read the objects
  instead: `kubectl -n mlobs get deployment/api-canary statefulset/shadow-scorer`
  shows `0/0` and `1/1`.
- **Step 2's operator check may not read `1/1`, and the delete does not need
  it.** The controller registers no finalizer (O1–O2: nothing under
  `operator/` adds one), and step 3's garbage collection is the cluster's
  garbage collector, which does not depend on the operator. If the delete
  hangs nonetheless, read the finalizers and strip them by hand:

  ```bash
  kubectl -n mlobs get servingdeployment api -o jsonpath='{.metadata.finalizers}{"\n"}'
  kubectl -n mlobs patch servingdeployment/api --type merge -p '{"metadata":{"finalizers":null}}'
  ```

### No EBS snapshot before the cutover (D30)

O4 takes no EBS snapshot: PHASE3.md's O4 acceptance rules it out, and this is
the reasoning, recorded against D30. D30 kept `snap-0f365806e0eaf9fb6` as "the
one rollback floor that does not depend on the pipeline under test", and
retired it once a deploy and its rollback had run green. Its deletion is
recorded on #49. O4 starts from that retired position:

- **The way back does not need one.** The pipeline has a deploy and a
  rollback on record (#49), and D36 is rehearsed green in CI (O3). Nothing in
  the cutover or in D36 deletes the Postgres PVC, and `sql/` is the same on
  both sides of the boundary.
- **The data is covered without one.** The Postgres PVC is the stack's one
  piece of state on disk (D20). The `pg_dump` preflight covers it, through
  D25's restore path. Everything else is rebuilt from the SHA: the GHCR
  images, which do not expire; the shadow tarball in S3; the manifests in
  git.
- **It would not cover the stream.** Redis runs with `--appendonly no` (D20),
  so a disk snapshot holds neither the stream nor its consumer groups'
  positions. Nor does the dump. The consumer recovers by redelivery, not from
  a durable stream, so a snapshot adds nothing there.
- **A consistent one costs a stop.** P2b's was taken with the instance
  stopped. A snapshot of the running host is crash-consistent only, with
  Postgres caught mid-write, which the dump already does better. A stopped
  one adds a full outage and a host restart to the window. The storage is
  cheap, about $1.50 a month for 30 GiB (#49). The stop is not.

The residual, stated plainly: a dump kept on the instance's own volume does
not survive the loss of that volume. Nothing in the cutover touches the
volume.

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
