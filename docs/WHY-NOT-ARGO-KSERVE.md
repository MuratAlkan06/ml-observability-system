# Why this phase builds an operator instead of adopting Argo Rollouts or KServe

> This write-up is a Phase 3 deliverable of the owner brief (#62;
> [`PHASE3.md`](PHASE3.md), "Settled constraints"). It was written at O3,
> after O0–O2 had built the `ServingDeployment` operator and before O4 runs it
> live. The constraints it cites are decisions in [`PLAN.md`](PLAN.md). The
> tools are described as their own documentation describes them, and as of
> this writing.

## The short answer

Three reasons, in order of weight.

1. **The hardest part of this phase is outside both tools.** A canary window
   here has to pause the shadow scorer first. The shadow goes to 0 and is seen
   gone before the canary's first pod starts, and it comes back only after the
   canary's pods are gone, on every way out of the window: a close, a
   promotion, the TTL (D37 and its O2 addendum). That order exists because two
   torch api pods and the shadow do not fit together under D26's memory bar on
   the 4GB host (D37). Neither Argo Rollouts nor KServe models a sibling
   workload that has to make room. With either tool, this part would be glue
   beside the tool, and it is exactly the part that has to be right.
2. **The live api is adopted in place.** D31 takes over the running
   `deployment/api` by adding an ownerReference and setting its image. The
   name and the rest of the pod template are kept, and a Deployment already at
   the tag is adopted without a restart. Argo Rollouts migrates a Deployment
   by running a second set of pods beside it. KServe replaces the Deployment
   with one of its own. On this host, a second torch api pod beside the stable
   and the shadow is the state D37 rules out.
3. **The brief asks for a controller.** Phase 3 was re-sequenced so that a CRD
   and a Go controller ship before the P3 stretch (#62). That is a reason in
   its own right, and it is given as one here rather than presented as a
   technical verdict. The two points above are what make building the
   operator defensible for this repository. They are not what put it on the
   plan.

Neither tool is wrong for the job it was built for. The rest of this document
says what each would have bought, what each would have cost under this
repository's constraints, and what would change the answer.

## What Argo Rollouts would have bought

- **The canary mechanics, already built.** A `Rollout` resource with canary
  steps: set a weight, pause for a duration or until a human promotes, abort.
  There is a `kubectl` plugin and a dashboard to drive them.
- **Metric-driven promote and rollback.** `AnalysisTemplate`s query Prometheus
  and fail or pass a step. That is what v1 of this operator is meant to do
  (the metric split in `PHASE3.md`: health metrics for rollback, agreement and
  confidence for promotion). It is the piece this repository would most
  directly re-implement.
- **Request-level weights, given a traffic router.** With a mesh, an ingress
  controller or the Gateway API plugin, a 5% canary means 5% of requests.
- **A controller with users.** Argo Rollouts has users who have found its bugs.
  This operator is about a thousand lines of Go
  (`operator/internal/controller`, tests excluded), and the only evidence for
  it is its own tests: envtest, the k3d e2e, and the D36 rehearsal.

Two things it would **not** have bought here, stated so that they are not
counted against it by mistake:

- **A better split on this node.** Without a traffic router, Argo's own
  documentation says the canary makes a "best effort attempt" at a weight by
  scaling ReplicaSets, and a Service then spreads connections across pods.
  That is the same mechanism as D34's replica-ratio split. Getting
  request-level weights needs a router this single node does not run.
- **A narrower grant.** RBAC is not a reason against Argo. It ships a
  namespaced install that needs only namespace-level privileges, with its CRDs
  applied separately. That is the same shape D33 gives this operator: one
  namespaced Role, and the CRD applied by `apply.sh` under the host's admin
  credentials.

## What KServe would have bought

- **A model-serving platform.** An `InferenceService` resource, model runtimes
  and the standard inference protocols, a storage initializer that fetches
  model artifacts, transformers and explainers, and inference graphs.
- **Revision-based canaries and autoscaling**, scale-to-zero included, through
  Knative.

Its canary, though, is not available on the path that fits this host. KServe's
documentation states that its canary rollout strategy "is only supported in
serverless deployment mode". Serverless mode means Knative Serving and a
networking layer beside KServe's own controller and cert-manager. Its
Standard mode runs plain Deployments and Services and has no canary. The
feature this phase needs arrives only with the heaviest install.

## What each would have cost here

| Constraint | Argo Rollouts | KServe | This operator |
|---|---|---|---|
| **Memory, 4GB single node (D26).** Measured under k3s at 5 rps: api 565M RSS, shadow 439M, k3s server 529M. The window state trades the shadow for a second api pod, and O4 measures it against `MemAvailable` ≥ 400MB. | One more controller pod. | KServe's controller, Knative Serving, a networking layer and cert-manager, all running all the time. | One pod with a 64Mi request. |
| **In-place adoption (D31).** The live api keeps its pods. | Migrating an existing Deployment "spin[s] up the required number of Pods side-by-side with the Deployment Pods", in Argo's own words: a second torch api pod beside the stable and the shadow. | The api becomes a predictor container in an `InferenceService`, whose Deployment KServe creates. Adoption becomes replacement. | ownerReference plus image; no pod restarted when the tag already matches. |
| **Control flow (D32).** The pipeline moves the stable; a human opens a window; any deploy during a window closes it. | Every change to the pod template is itself a rollout through the canary steps. There is no separate, human-opened window, and a deploy mid-rollout moves the canary on to the newer revision rather than closing it. That is the "window survives onto a new stable" hazard D32 rejects. | Traffic moves between revisions of one `InferenceService`. The same objection applies. | Built to D32: the pipeline renders only `spec.imageTag`, and its constant patch can close a window and never open one. |
| **Two states, the shadow's pause ordered (D37).** | Not modelled. It would take an analysis Job or a step plugin holding its own grant on the shadow scorer, and the reverse order on abort would be that extension's to get right. | Not modelled. It would be a controller of its own, running beside KServe's. | The reconcile loop, with one extra grant: `patch` on the scale subresource of the one named StatefulSet (the D33 addendum). |
| **Minimal RBAC, no webhooks in v0 (D33).** | The namespaced install fits, as above. | A cluster-scoped controller with admission webhooks; cert-manager, which provisions the webhooks' certificates, is in its dependency list. | One namespaced Role, no ClusterRole, no webhooks, no write on CRDs. |
| **Budget, 4–6 weekends (`PHASE3.md`).** | Installing it is cheaper than writing an operator. Bending D31, D32 and D37 into it is not free. | The install and its dependencies are the cost before any canary exists. | O0–O3, spent on the thing the brief asks for. |

The table is not meant as a scorecard. Argo Rollouts comes closest. If this
host had room for a second api pod beside the shadow, and if D32's
opt-in window were not a decision, most of the argument against it would
narrow to the brief itself.

## What the operator gives up

- **Request-level weights.** The split is per connection and set by replica
  ratio (D34; the README's "The canary split, stated plainly"). A 5% canary is
  not expressible, and a keep-alive client sticks to one pod.
- **Automated analysis.** v0 promotes and rolls back by hand. v1 has to write
  the metric loop that Argo's `AnalysisTemplate` already provides.
- **Maturity and ecosystem.** There are no dashboards, no plugin and no
  community. The evidence is this repository's tests and rehearsals, and it is
  only as good as they are.

## What would change the answer

- **P3's EKS pass.** An ephemeral EKS cluster is not bound by the 4GB host's
  memory bar that dominates the table above. That is the natural place to run
  Argo Rollouts against the same api image beside the Helm-packaged operator,
  and to record what expressing D37's shadow pause in it actually takes.
  Whether P3 does so is for its own design freeze.
- **v1.** Before writing the metric-driven promote and rollback loop, look at
  Argo's analysis again: that loop is the part of Argo this repository would
  most directly rebuild.
- **A percentage that matters.** The moment a canary has to take a set share
  of requests rather than of connections, a traffic router is needed: the
  Gateway API behind Argo, or Knative behind KServe. The cost of either tool
  then falls relative to what it adds.
- **More models, or GPUs.** Several models served side by side, model
  runtimes, and scale-to-zero belong to KServe.
- **A bigger host or a second environment.** This is the trigger D12, D17 and
  D23 already name for their own deferred tools. With memory to spare, the
  second-pod migration and the always-on controllers stop being disqualifying.
