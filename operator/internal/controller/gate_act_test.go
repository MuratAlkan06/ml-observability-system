package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
)

// The acting gate's specs (docs/PLAN.md D45, D47; the acting-level ruling on
// #119), on the same harness as the gate in Observe: a fake clock, scripted
// /metrics and pods whose status is written directly.

var (
	actRollback = &servingv1alpha1.GateSpec{
		Mode: servingv1alpha1.GateModeActRollback, ForCanaryImageTag: canaryImageTag,
	}
	actPromote = &servingv1alpha1.GateSpec{
		Mode: servingv1alpha1.GateModeActPromote, ForCanaryImageTag: canaryImageTag,
	}
)

const (
	levelObserve     = servingv1alpha1.GateModeObserve
	levelActRollback = servingv1alpha1.GateModeActRollback
	levelActPromote  = servingv1alpha1.GateModeActPromote
)

// levels is each record's effectiveLevel, oldest first.
func levels(st *servingv1alpha1.GateStatus) []servingv1alpha1.GateMode {
	out := make([]servingv1alpha1.GateMode, 0, len(st.Records))
	for _, rec := range st.Records {
		out = append(out, rec.EffectiveLevel)
	}
	return out
}

// rolledBackMessage is CanaryActive's message on a rolled-back window with
// nothing left to wait on.
func rolledBackMessage(trip servingv1alpha1.GateTrip, interval int32) string {
	return fmt.Sprintf("the gate tripped %s at interval %d and rolled the canary window back; the spec keeps its "+
		"canary fields until the close-window patch clears them, and no window opens before it does", trip, interval)
}

// expectRolledBack asserts the act on h's window, taken at at: CanaryActive
// False with BreachRolledBack since then, naming the trip, and status.gate's
// rollback verdict carrying the act's time. It returns status.gate.
func expectRolledBack(
	h *gateHarness, trip servingv1alpha1.GateTrip, interval int32, at time.Time,
) *servingv1alpha1.GateStatus {
	GinkgoHelper()
	latest := getServingDeployment(h.sd)
	active := condition(latest, servingv1alpha1.ConditionCanaryActive)
	Expect(active.Status).To(Equal(metav1.ConditionFalse))
	Expect(active.Reason).To(Equal(servingv1alpha1.ReasonBreachRolledBack))
	Expect(active.Message).To(Equal(rolledBackMessage(trip, interval)))
	Expect(active.LastTransitionTime.Time).To(BeTemporally("==", at))
	st := latest.Status.Gate
	Expect(st).NotTo(BeNil())
	Expect(st.RollbackVerdict).NotTo(BeNil())
	Expect(st.RollbackVerdict.Trip).To(Equal(trip))
	Expect(st.RollbackVerdict.Interval).To(Equal(interval))
	Expect(st.RollbackVerdict.RolledBackAt).To(HaveValue(HaveField("Time", BeTemporally("==", at))))
	return st
}

// expectOpen asserts h's window is open and its canary serving.
func expectOpen(h *gateHarness) {
	GinkgoHelper()
	active := condition(getServingDeployment(h.sd), servingv1alpha1.ConditionCanaryActive)
	Expect(active.Status).To(Equal(metav1.ConditionTrue))
	Expect(active.Reason).To(Equal(servingv1alpha1.ReasonWindowOpen))
	expectCanaryReplicas(h.ns, 1)
}

// expectStatusWritesOnly asserts that every ServingDeployment write rc saw was
// to its status: the operator never writes a spec (D32).
func expectStatusWritesOnly(rc *recordingClient) {
	GinkgoHelper()
	kind := reflect.TypeFor[servingv1alpha1.ServingDeployment]().Name()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, w := range rc.writes {
		if w.kind == kind {
			Expect(w.subresource).To(Equal("status"), "a ServingDeployment write other than its status: %+v", w)
		}
	}
}

// markStableUnavailable writes the status the Deployment controller reports
// while deployment/api is not Available.
func markStableUnavailable(namespace string) {
	dep := getStable(namespace)
	dep.Status = rolledOutStatus(dep.Generation)
	dep.Status.AvailableReplicas = 0
	dep.Status.Conditions[0].Status = corev1.ConditionFalse
	Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
}

// closeToSteady walks h's closed window back to steady: the canary scaled to
// 0, observed gone, and the shadow returned to 1.
func (h *gateHarness) closeToSteady() {
	GinkgoHelper()
	Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
	expectCanaryReplicas(h.ns, 0)
	markCanaryObserved(h.ns, 0)
	Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
	expectShadowReplicas(h.ns, 1)
}

// reopen closes h's window by the close-window patch's shape, walks it to
// steady, and opens the next one at at.
func (h *gateHarness) reopen(at time.Time) {
	GinkgoHelper()
	setCanarySpec(h.sd, "", 0)
	h.closeToSteady()
	h.clock.t = at
	setCanarySpec(h.sd, canaryImageTag, 1)
	Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
}

// reconcileReading reconciles h's ServingDeployment once, its reconciler served
// read, a copy taken earlier, in place of the one the API server holds: the
// manager's informer cache, lagging a write for a moment. Everything else is
// read and written as before; the reconcile's error is returned.
func (h *gateHarness) reconcileReading(read *servingv1alpha1.ServingDeployment) error {
	live := h.r.Client
	defer func() { h.r.Client = live }()
	h.r.Client = interceptor.NewClient(live.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if sd, ok := obj.(*servingv1alpha1.ServingDeployment); ok && key == client.ObjectKeyFromObject(read) {
				read.DeepCopyInto(sd)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	_, err := h.r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(h.sd)})
	return err
}

var _ = Describe("The gate in ActRollback (D45, D47)", func() {
	It("rolls the window back on line 3's two in a row: the latch is written first, CanaryActive False with "+
		"BreachRolledBack naming the line, then steady is restored in the close's order, stable first, with no "+
		"spec write", func() {
		h := newGateHarness(actRollback)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		Expect(h.gateEvents()).To(ConsistOf("Normal GateArmed the gate is armed in ActRollback for canary tag " +
			canaryImageTag + ": every 1m0s interval from 2026-09-29T12:00:00Z is recorded in status.gate, and a " +
			"tripped line rolls the window back"))
		h.drainOpenEvents()
		specOpen := getServingDeployment(h.sd)
		writesOpen := writeCount(h.writes)
		h.clean(3)

		By("one breach is not a trip: the window stays open")
		Expect(h.next(0, perInterval, 0).RequeueAfter).To(Equal(time.Minute))
		expectOpen(h)
		Expect(h.gateEvents()).To(ConsistOf(ContainSubstring("Warning GateBreach interval 3 ")))

		By("the second in a row trips line 3 and the gate rolls the window back: the latch is written, and " +
			"nothing in the cluster has moved yet")
		reads := h.reads.count()
		Expect(h.next(0, perInterval, 0).RequeueAfter).To(BeZero(),
			"a closed window is not requeued: the latch's own write brings the next reconcile")
		st := expectRolledBack(h, "Line3CanaryLatency", 4, h.boundaryTime(5))
		Expect(st.RollbackVerdict.At.Time).To(BeTemporally("==", h.boundaryTime(5)))
		Expect(verdicts(st)).To(Equal(verdictList("Clean", "Clean", "Clean", "Breach", "Breach")))
		Expect(st.Records[4].Reasons).To(Equal([]servingv1alpha1.GateReason{
			servingv1alpha1.GateReason(gate.ReasonLatencyRatio),
		}))
		Expect(levels(st)).To(Equal(repeat(levelActRollback, 5)))
		Expect(st.Mode).To(Equal(levelActRollback))
		Expect(st.BreachRun).To(BeEquivalentTo(2))
		Expect(h.gateEvents()).To(ConsistOf("Warning GateRolledBack interval 4 tripped Line3CanaryLatency: in " +
			"ActRollback the gate rolls the canary window back, CanaryActive False with BreachRolledBack; steady is " +
			"restored with no spec write, and no window opens before the close-window patch clears the canary fields"))
		expectCanaryReplicas(h.ns, 1)
		expectShadowReplicas(h.ns, 0)
		expectNoWorkloadWrites(h.writes, writesOpen)

		By("the latch closes the window as it closes an expired one: the canary keeps serving until " +
			"deployment/api is Available")
		markStableUnavailable(h.ns)
		h.clock.t = h.boundaryTime(5).Add(time.Second)
		Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(BeZero())
		expectCanaryReplicas(h.ns, 1)
		active := condition(getServingDeployment(h.sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonBreachRolledBack))
		Expect(active.Message).To(Equal(rolledBackMessage("Line3CanaryLatency", 4) +
			"; deployment/api-canary keeps serving until deployment/api is Available"))

		By("deployment/api Available, the canary goes to 0; the shadow waits for its pods")
		markRolledOut(h.ns)
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		expectCanaryReplicas(h.ns, 0)
		expectShadowReplicas(h.ns, 0)

		By("the canary observed gone, the shadow returns to 1: steady, and the message names the line alone")
		markCanaryObserved(h.ns, 0)
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		expectShadowReplicas(h.ns, 1)
		latest := getServingDeployment(h.sd)
		active = condition(latest, servingv1alpha1.ConditionCanaryActive)
		Expect(active.Message).To(Equal(rolledBackMessage("Line3CanaryLatency", 4)))
		Expect(active.LastTransitionTime.Time).To(BeTemporally("==", h.boundaryTime(5)), "the latch does not move")
		Expect(indexOf(h.writes, "patch", "Deployment", canaryDeploymentName, "")).
			To(BeNumerically("<", indexOf(h.writes, "patch", "StatefulSet", shadowStatefulSetName, shadowResumeBody)))
		expectOnlyShadowScaleWrites(h.writes)

		By("no spec write, no read since the act, and the window's evidence frozen")
		Expect(latest.Spec).To(Equal(specOpen.Spec))
		Expect(latest.Generation).To(Equal(specOpen.Generation))
		expectStatusWritesOnly(h.writes)
		Expect(h.reads.count()).To(Equal(reads+1), "the act's boundary was the last read")
		Expect(latest.Status.Gate).To(Equal(st))
		Expect(h.gateEvents()).To(BeEmpty())
	})

	It("latches the rolled-back window exactly as an expired one: editing its canary fields does not reopen it, the "+
		"cleared shape releases it, and the next window starts fresh evidence at the level sampled at its open", func() {
		h := newGateHarness(actRollback)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(2)
		h.next(0, perInterval, 0)
		h.next(0, perInterval, 0)
		evidence := expectRolledBack(h, "Line3CanaryLatency", 3, h.boundaryTime(4))
		h.closeToSteady()
		h.drainOpenEvents()
		pausesBefore := len(h.writes.statefulSetWrites())
		reads := h.reads.count()

		for _, edit := range []struct {
			by       string
			tag      string
			replicas int32
		}{
			{"a new canary tag on the rolled-back window", thirdCanaryImageTag, 1},
			{"replicas 0 with the tag still set, which is not the cleared shape", thirdCanaryImageTag, 0},
			{"the window's own tag again, not having passed through the cleared shape", canaryImageTag, 1},
		} {
			By(edit.by + ": still latched, still steady, the evidence unchanged")
			setCanarySpec(h.sd, edit.tag, edit.replicas)
			h.clock.t = h.clock.t.Add(time.Minute)
			Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(BeZero())
			latest := getServingDeployment(h.sd)
			active := condition(latest, servingv1alpha1.ConditionCanaryActive)
			Expect(active.Status).To(Equal(metav1.ConditionFalse))
			Expect(active.Reason).To(Equal(servingv1alpha1.ReasonBreachRolledBack))
			Expect(active.Message).To(Equal(rolledBackMessage("Line3CanaryLatency", 3)))
			Expect(latest.Status.Gate).To(Equal(evidence))
			expectCanaryReplicas(h.ns, 0)
			expectShadowReplicas(h.ns, 1)
		}
		Expect(h.writes.statefulSetWrites()).To(HaveLen(pausesBefore), "a latched window pauses nothing")
		Expect(h.reads.count()).To(Equal(reads), "and reads nothing")
		Expect(h.gateEvents()).To(BeEmpty())

		By("the cleared shape releases the latch; the evidence stays")
		setCanarySpec(h.sd, "", 0)
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		latest := getServingDeployment(h.sd)
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).Reason).To(Equal(servingv1alpha1.ReasonNoCanary))
		Expect(latest.Status.Gate).To(Equal(evidence))

		By("the next window opens on a new clock, its evidence fresh and its level sampled at its open")
		next := windowStart.Add(20 * time.Minute)
		h.clock.t = next
		setCanarySpec(h.sd, canaryImageTag, 1)
		Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(Equal(time.Minute))
		latest = getServingDeployment(h.sd)
		active := condition(latest, servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionTrue))
		Expect(active.LastTransitionTime.Time).To(BeTemporally("==", next))
		st := latest.Status.Gate
		Expect(st.WindowStart.Time).To(BeTemporally("==", next))
		Expect(st.Records).To(BeEmpty())
		Expect(st.Totals).To(BeZero())
		Expect(st.RollbackVerdict).To(BeNil())
		Expect(st.Mode).To(Equal(levelActRollback))
		Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingArmed))
		Expect(h.gateEvents()).To(ConsistOf(ContainSubstring("Normal GateArmed the gate is armed in ActRollback")))
	})

	It("holds the latch through a reconcile served the ServingDeployment it was decided from: that reconcile's "+
		"write, Ready having moved since, would put CanaryActive back to WindowOpen over BreachRolledBack and open "+
		"the rolled-back window again; it conflicts instead, and the close goes on to steady (V1-S7)", func() {
		h := newGateHarness(actRollback)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(2)
		h.drainOpenEvents()
		preLatch := getServingDeployment(h.sd)

		By("one canary 5xx past warmup trips line 2 at interval 2: the latch is written")
		h.next(perInterval, 0, 1)
		evidence := expectRolledBack(h, "Line2CanaryErrors", 2, h.boundaryTime(3))
		h.drainOpenEvents()

		By("deployment/api no longer Available, then a reconcile served the object the latch was decided from")
		markStableUnavailable(h.ns)
		h.clock.t = h.boundaryTime(3).Add(time.Second)
		stale := h.reconcileReading(preLatch)

		By("the next reconcile reads the API server: still latched, the canary serving until deployment/api is " +
			"Available")
		h.clock.t = h.boundaryTime(3).Add(2 * time.Second)
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		latest := getServingDeployment(h.sd)
		active := condition(latest, servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionFalse))
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonBreachRolledBack))
		Expect(active.Message).To(Equal(rolledBackMessage("Line2CanaryErrors", 2) +
			"; deployment/api-canary keeps serving until deployment/api is Available"))
		Expect(active.LastTransitionTime.Time).To(BeTemporally("==", h.boundaryTime(3)), "the latch does not move")
		Expect(condition(latest, servingv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		Expect(latest.Status.Gate).To(Equal(evidence))
		Expect(apierrors.IsConflict(stale)).To(BeTrue(), "the stale reconcile's write: %v", stale)
		expectCanaryReplicas(h.ns, 1)

		By("deployment/api Available: the canary goes to 0 and stays there, the window never opened again")
		markRolledOut(h.ns)
		h.closeToSteady()
		h.clock.t = h.boundaryTime(5)
		Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(BeZero())
		expectCanaryReplicas(h.ns, 0)
		Expect(expectRolledBack(h, "Line2CanaryErrors", 2, h.boundaryTime(3))).To(Equal(evidence))
		Expect(h.gateEvents()).To(BeEmpty())
	})

	DescribeTable("rolls back at once on what the canary pod shows: its OOM kill, read from containerStatuses "+
		"written directly, and a 5xx past warmup",
		func(trip func(h *gateHarness, canary *corev1.Pod), want servingv1alpha1.GateTrip) {
			h := newGateHarness(actRollback)
			canary := h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.drainOpenEvents()
			h.clean(2)
			trip(h, canary)
			st := expectRolledBack(h, want, 2, h.boundaryTime(3))
			Expect(verdicts(st)).To(Equal(verdictList("Clean", "Clean", "Breach")))
			Expect(levels(st)).To(Equal(repeat(levelActRollback, 3)))
			Expect(st.BreachRun).To(BeZero(), "not line 3: the trip is at once")
			Expect(h.gateEvents()).To(ConsistOf(ContainSubstring("Warning GateBreach interval 2 "),
				HavePrefix("Warning GateRolledBack interval 2 tripped "+string(want)+": in ActRollback")))
			h.closeToSteady()
			Expect(condition(getServingDeployment(h.sd), servingv1alpha1.ConditionCanaryActive).Message).
				To(Equal(rolledBackMessage(want, 2)))
		},
		Entry("a canary OOMKilled, its last termination written to its containerStatuses",
			func(h *gateHarness, canary *corev1.Pod) {
				updatePodStatus(canary, func(s *corev1.PodStatus) {
					s.ContainerStatuses[0].RestartCount = 1
					s.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
					}
				})
				h.next(perInterval, 0, 0)
			}, servingv1alpha1.GateTrip("Line5CanaryOOMKilled")),
		Entry("one canary 5xx past warmup", func(h *gateHarness, _ *corev1.Pod) {
			h.next(perInterval, 0, 1)
		}, servingv1alpha1.GateTrip("Line2CanaryErrors")),
	)

	It("rolls back on two operator restarts in a row: each interval a restart forfeits is unevaluable, "+
		"OperatorInterrupted, and two in a row trip line 3 (D45)", func() {
		h := newGateHarness(actRollback)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(2)
		for _, n := range []int32{2, 3} {
			By(fmt.Sprintf("the operator restarts 20 s after boundary %d, its baselines gone", n))
			restarted, _, clock := newClockedReconciler()
			restarted.scraper = h.reads
			h.r, h.clock = restarted, clock
			clock.t = h.boundaryTime(n).Add(20 * time.Second)
			Expect(reconcileResult(restarted, h.sd).RequeueAfter).To(Equal(40 * time.Second))
			h.next(perInterval, 0, 0)
		}
		st := expectRolledBack(h, "Line3CanaryLatency", 3, h.boundaryTime(4))
		Expect(verdicts(st)).To(Equal(verdictList("Clean", "Clean", "Unevaluable", "Unevaluable")))
		for _, rec := range st.Records[2:] {
			Expect(rec.Reasons).To(Equal([]servingv1alpha1.GateReason{
				servingv1alpha1.GateReason(gate.ReasonOperatorInterrupted),
			}))
		}
		Expect(levels(st)).To(Equal(repeat(levelActRollback, 4)))
		Expect(st.BreachRun).To(BeEquivalentTo(2))
	})

	It("degrades ActPromote with no dispatch token to ActRollback, the off-season posture: TokenAbsent, named in "+
		"an Event, and a tripped line still rolls the window back (D42, D43)", func() {
		h := newGateHarness(actPromote)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		Expect(h.gateEvents()).To(ConsistOf("Warning GateDegraded spec.gate.mode ActPromote has no dispatch token, " +
			"secret/operator-dispatch-token being absent or its token unreadable: the gate runs as ActRollback for " +
			"canary tag " + canaryImageTag + ", records every interval in status.gate and rolls the window back on a " +
			"tripped line; the promote verdict is recorded and a human actuates it"))
		st := h.gate()
		Expect(st.Mode).To(Equal(levelActRollback))
		Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingTokenAbsent))
		h.clean(1)
		h.next(0, perInterval, 0)
		h.next(0, perInterval, 0)
		st = expectRolledBack(h, "Line3CanaryLatency", 2, h.boundaryTime(3))
		Expect(levels(st)).To(Equal(repeat(levelActRollback, 3)))
		Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingTokenAbsent))
	})

	DescribeTable("records the promote verdict and dispatches nothing: the window stays open and nothing moves (D43)",
		func(spec *servingv1alpha1.GateSpec) {
			h := newGateHarness(spec)
			h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.drainOpenEvents()
			writesOpen := writeCount(h.writes)
			h.clean(12)
			st := h.gate()
			Expect(st.PromoteVerdict).To(HaveField("Interval", BeEquivalentTo(10)))
			Expect(levels(st)).To(Equal(repeat(levelActRollback, 12)))
			Expect(h.gateEvents()).To(ConsistOf("Normal GateWouldPromote interval 10 completed 11 consecutive clean " +
				"intervals, the promote verdict: in ActRollback it is recorded and nothing is dispatched"))
			expectNoWorkloadWrites(h.writes, writesOpen)
			expectOpen(h)
		},
		Entry("ActRollback, where a human actuates it", actRollback),
		Entry("ActPromote with no dispatch token, run as ActRollback (TokenAbsent)", actPromote),
	)

	DescribeTable("expires at 45 minutes in every mode, the TTL unchanged; and when a breach would trip line 3 at "+
		"the very boundary the TTL ends the window at, expiry wins: nothing is read there and nothing acts (D47)",
		func(spec *servingv1alpha1.GateSpec, level servingv1alpha1.GateMode, withToken bool) {
			h := newGateHarness(spec)
			if withToken {
				armDispatch(h)
			}
			h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.clean(43)
			Expect(h.next(0, perInterval, 0).RequeueAfter).To(Equal(time.Minute),
				"interval 43 breaches at 44m00s, the TTL a minute away")
			st := h.gate()
			Expect(st.BreachRun).To(BeEquivalentTo(1))
			Expect(levels(st)).To(Equal(repeat(level, 44)))
			drainEvents(h.r)
			reads := h.reads.count()

			By("at 45m00s, boundary 45 and the deadline at once: WindowExpired, with no read and no record")
			Expect(h.next(0, perInterval, 0).RequeueAfter).To(BeZero())
			latest := getServingDeployment(h.sd)
			active := condition(latest, servingv1alpha1.ConditionCanaryActive)
			Expect(active.Reason).To(Equal(servingv1alpha1.ReasonWindowExpired))
			Expect(active.LastTransitionTime.Time).To(BeTemporally("==", windowStart.Add(canaryWindowTTL)))
			Expect(h.reads.count()).To(Equal(reads), "expiry reads nothing")
			Expect(latest.Status.Gate).To(Equal(st), "interval 44 is never judged")
			Expect(latest.Status.Gate.RollbackVerdict).To(BeNil())
			events := drainEvents(h.r)
			Expect(events).To(ContainElement(ContainSubstring("Normal WindowExpired")))
			Expect(events).NotTo(ContainElement(ContainSubstring("Gate")))
			expectCanaryReplicas(h.ns, 0)
		},
		Entry("Observe", observe, levelObserve, false),
		Entry("ActRollback", actRollback, levelActRollback, false),
		Entry("ActPromote, armed with its token: its verdict dispatched at boundary 11, and the TTL unmoved",
			actPromote, levelActPromote, true),
		Entry("ActPromote with no dispatch token, at ActRollback", actPromote, levelActRollback, false),
	)
})

var _ = Describe("The acting level, sampled at window open (#119)", func() {
	DescribeTable("never raises the level inside a window: a raise is recorded in status and takes effect from the "+
		"next window, so through breaches that trip line 3 this window only records",
		func(atOpen *servingv1alpha1.GateSpec, raise func(h *gateHarness), wantArming servingv1alpha1.GateArming,
			raiseEvent any, before []servingv1alpha1.GateMode) {
			h := newGateHarness(atOpen)
			h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.clean(2)
			h.drainOpenEvents()
			raise(h)
			for range 3 {
				h.next(0, perInterval, 0)
			}

			st := h.gate()
			Expect(getServingDeployment(h.sd).Spec.Gate).To(Equal(actRollback), "the raise is in the spec")
			Expect(st.Mode).To(Equal(levelObserve))
			Expect(st.Arming).To(Equal(wantArming))
			Expect(levels(st)).To(Equal(append(before, repeat(levelObserve, len(st.Records)-len(before))...)))
			Expect(st.RollbackVerdict).To(HaveField("Trip", servingv1alpha1.GateTrip("Line3CanaryLatency")))
			Expect(st.RollbackVerdict.RolledBackAt).To(BeNil(), "recorded, and not acted on")
			events := h.gateEvents()
			Expect(events).To(ContainElement(HavePrefix("Warning GateWouldRollBack ")))
			Expect(events).NotTo(ContainElement(ContainSubstring("GateRolledBack")))
			if raiseEvent != nil {
				Expect(events).To(ContainElement(raiseEvent))
			} else {
				Expect(events).NotTo(ContainElement(ContainSubstring("GateDegraded")))
			}
			expectOpen(h)

			By("the next window opens at the raised level")
			h.reopen(windowStart.Add(30 * time.Minute))
			Expect(h.gate().Mode).To(Equal(levelActRollback))
		},
		Entry("spec.gate set mid-window, the window having opened with none",
			nil, func(h *gateHarness) { setGate(h.sd, actRollback) }, servingv1alpha1.GateArmingArmed,
			Equal("Warning GateDegraded spec.gate.mode ActRollback is armed for canary tag "+canaryImageTag+" from the "+
				"next window: a window acts at the level sampled when it opened, so the gate runs this one as Observe: "+
				"it records every interval in status.gate and acts on nothing"),
			[]servingv1alpha1.GateMode(nil)),
		Entry("Observe raised to ActRollback, the arming unchanged and so no Event: status alone records it",
			observe, func(h *gateHarness) { setGate(h.sd, actRollback) }, servingv1alpha1.GateArmingArmed,
			nil, repeat(levelObserve, 2)),
		Entry("ActRollback armed for another tag, the tag put right",
			&servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeActRollback, ForCanaryImageTag: laterCanaryImageTag},
			func(h *gateHarness) { setGate(h.sd, actRollback) }, servingv1alpha1.GateArmingArmed,
			HavePrefix("Warning GateDegraded spec.gate.mode ActRollback is armed for canary tag "+canaryImageTag+
				" from the next window"),
			repeat(levelObserve, 2)),
		Entry("spec.gate removed, which lowers at once, and set again: the gap the removal forfeits trips line 3 "+
			"with the next breach, and is only recorded (#117, disposition (d))",
			actRollback, func(h *gateHarness) {
				setGate(h.sd, nil)
				h.clock.t = h.boundaryTime(2).Add(10 * time.Second)
				Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
				Expect(h.gate().Mode).To(Equal(levelObserve))
				Expect(h.gateEvents()).To(ConsistOf("Warning GateDegraded spec.gate was removed from the open window: " +
					"the gate stops at once and acts on nothing, status.gate keeps the window's evidence at Observe, " +
					"and a gate set again before the window closes runs as Observe"))
				setGate(h.sd, actRollback)
			}, servingv1alpha1.GateArmingArmed, nil, repeat(levelActRollback, 2)),
	)

	DescribeTable("lowers the level at the next boundary: the interval it ends is evaluated at the lowered level, so "+
		"the second breach in a row there is recorded and not acted on, the ring shows the drop, and a raise back "+
		"waits for the next window",
		func(lower func(h *gateHarness), wantArming servingv1alpha1.GateArming, lowerEvent any,
			restore func(h *gateHarness), restoreEvent any) {
			h := newGateHarness(actRollback)
			h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.clean(2)
			h.next(0, perInterval, 0)
			h.drainOpenEvents()

			By("lowered mid-interval, after one breach at ActRollback")
			lower(h)
			h.next(0, perInterval, 0)
			st := h.gate()
			Expect(verdicts(st)).To(Equal(verdictList("Clean", "Clean", "Breach", "Breach")))
			Expect(levels(st)).To(Equal([]servingv1alpha1.GateMode{
				levelActRollback, levelActRollback, levelActRollback, levelObserve,
			}))
			Expect(st.Mode).To(Equal(levelObserve))
			Expect(st.Arming).To(Equal(wantArming))
			Expect(st.RollbackVerdict).To(HaveField("Interval", BeEquivalentTo(3)))
			Expect(st.RollbackVerdict.RolledBackAt).To(BeNil())
			Expect(h.gateEvents()).To(ConsistOf(lowerEvent, HavePrefix(
				"Warning GateWouldRollBack interval 3 tripped Line3CanaryLatency: an acting gate would close the "+
					"window here; in Observe nothing is done")))
			expectOpen(h)

			By("raised back mid-window: still Observe until the window closes")
			restore(h)
			h.next(0, perInterval, 0)
			st = h.gate()
			Expect(st.Mode).To(Equal(levelObserve))
			Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingArmed))
			Expect(levels(st)[4]).To(Equal(levelObserve))
			if restoreEvent != nil {
				Expect(h.gateEvents()).To(ConsistOf(restoreEvent))
			} else {
				Expect(h.gateEvents()).To(BeEmpty())
			}
			expectOpen(h)
		},
		Entry("spec.gate.mode lowered to Observe",
			func(h *gateHarness) { setGate(h.sd, observe) }, servingv1alpha1.GateArmingArmed,
			Equal("Normal GateArmed the gate is armed in Observe for canary tag "+canaryImageTag+": every 1m0s "+
				"interval from 2026-09-29T12:00:00Z is recorded in status.gate, and the gate acts on nothing"),
			func(h *gateHarness) { setGate(h.sd, actRollback) }, nil),
		Entry("a mismatch brought in by a new canary tag",
			func(h *gateHarness) { setCanarySpec(h.sd, laterCanaryImageTag, 1) }, servingv1alpha1.GateArmingTagMismatch,
			HavePrefix("Warning GateDegraded spec.gate.mode ActRollback is armed for canary tag \""+canaryImageTag+
				"\", not spec.canaryImageTag "+laterCanaryImageTag+": the gate runs as Observe"),
			func(h *gateHarness) { setCanarySpec(h.sd, canaryImageTag, 1) },
			HavePrefix("Warning GateDegraded spec.gate.mode ActRollback is armed for canary tag "+canaryImageTag+
				" from the next window")),
	)

	It("keeps the level sampled at the open through reconciles served a stale ServingDeployment: a status write "+
		"is locked to what its reconcile read, so one served the object from before the open conflicts, and "+
		"neither the grid's anchor nor the level moves (V1-S7, CI run 38790492622)", func() {
		h := newGateHarness(actRollback)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		setCanarySpec(h.sd, canaryImageTag, 1)
		beforeOpen := getServingDeployment(h.sd)

		By("the reconcile that opens the window samples ActRollback")
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		opening := getServingDeployment(h.sd)
		markStatefulSetObserved(h.ns, shadowStatefulSetName)

		By("a second, a second later, is served the ServingDeployment from before the open")
		h.clock.t = windowStart.Add(time.Second)
		staleOpen := h.reconcileReading(beforeOpen)

		By("a third is served the opening's write, and nothing written since conflicts its own")
		h.clock.t = windowStart.Add(2 * time.Second)
		Expect(h.reconcileReading(opening)).To(Succeed())

		By("a fourth reads the API server: the gate armed once, at the open's anchor and level")
		h.clock.t = windowStart.Add(3 * time.Second)
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		Expect(h.gateEvents()).To(ConsistOf("Normal GateArmed the gate is armed in ActRollback for canary tag " +
			canaryImageTag + ": every 1m0s interval from 2026-09-29T12:00:00Z is recorded in status.gate, and a " +
			"tripped line rolls the window back"))
		latest := getServingDeployment(h.sd)
		Expect(latest.Status.Gate.Mode).To(Equal(levelActRollback))
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).LastTransitionTime.Time).
			To(BeTemporally("==", windowStart))
		Expect(latest.Status.Gate.WindowStart.Time).To(BeTemporally("==", windowStart))
		Expect(apierrors.IsConflict(staleOpen)).To(BeTrue(), "the stale open's write: %v", staleOpen)

		By("the window acts at that level: line 3's two in a row roll it back, every record at ActRollback")
		h.clean(2)
		h.next(0, perInterval, 0)
		h.next(0, perInterval, 0)
		st := expectRolledBack(h, "Line3CanaryLatency", 3, h.boundaryTime(4))
		Expect(levels(st)).To(Equal(repeat(levelActRollback, 4)))
		Expect(st.Mode).To(Equal(levelActRollback))
	})

	It("never arms a raise in the window it was made in, whatever a stale reconcile writes: one served the "+
		"ServingDeployment from before the window was requested would put CanaryActive back to NoCanary over "+
		"the open window, and the next reconcile would open it again at the raised level; its write conflicts "+
		"instead (V1-S7)", func() {
		h := newGateHarness(observe)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		beforeRequest := getServingDeployment(h.sd)
		h.open()
		h.clean(2)
		h.drainOpenEvents()
		evidence := h.gate()

		By("spec.gate raised to ActRollback mid-window, then a reconcile served the object from before the request")
		setGate(h.sd, actRollback)
		h.clock.t = h.boundaryTime(2).Add(10 * time.Second)
		stale := h.reconcileReading(beforeRequest)

		By("the next reconcile reads the API server: the window open on its own anchor, its evidence at Observe")
		h.clock.t = h.boundaryTime(2).Add(20 * time.Second)
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		Expect(h.gateEvents()).To(BeEmpty())
		latest := getServingDeployment(h.sd)
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).LastTransitionTime.Time).
			To(BeTemporally("==", windowStart))
		Expect(latest.Status.Gate).To(Equal(evidence))
		Expect(apierrors.IsConflict(stale)).To(BeTrue(), "the stale reconcile's write: %v", stale)
		expectOpen(h)

		By("the next boundary records at Observe")
		h.clean(1)
		st := h.gate()
		Expect(st.Mode).To(Equal(levelObserve))
		Expect(levels(st)).To(Equal(repeat(levelObserve, 3)))
	})
})

var _ = Describe("The rolled-back window's latch and evidence", func() {
	DescribeTable("decideWindow latches BreachRolledBack exactly as WindowExpired",
		func(spec servingv1alpha1.ServingDeploymentSpec, wantReason string) {
			w := decideWindow(spec,
				canaryActive(metav1.ConditionFalse, servingv1alpha1.ReasonBreachRolledBack, windowStart),
				windowStart.Add(3*time.Hour))
			Expect(w.open).To(BeFalse())
			Expect(w.expiredNow).To(BeFalse())
			Expect(w.reason).To(Equal(wantReason))
		},
		Entry("latched while the spec requests a window", windowSpec(thirdCanaryImageTag),
			servingv1alpha1.ReasonBreachRolledBack),
		Entry("latched on a half-cleared spec", servingv1alpha1.ServingDeploymentSpec{CanaryImageTag: canaryImageTag},
			servingv1alpha1.ReasonBreachRolledBack),
		Entry("released by the cleared shape", servingv1alpha1.ServingDeploymentSpec{},
			servingv1alpha1.ReasonNoCanary),
	)

	It("ships each record's effectiveLevel and the act's rolledBackAt in the CRD: the level enum-validated as the "+
		"modes, both optional so a record an earlier operator wrote stays valid", func() {
		Expect(crdEnum(strings.Split("status.properties.gate.properties.records.items.properties.effectiveLevel", ".")...)).To(ConsistOf(
			string(servingv1alpha1.GateModeObserve), string(servingv1alpha1.GateModeActRollback),
			string(servingv1alpha1.GateModeActPromote)))

		sd := &servingv1alpha1.ServingDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "schema-acting", Namespace: metav1.NamespaceDefault},
			Spec:       servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag},
		}
		Expect(k8sClient.Create(ctx, sd)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, sd)).To(Succeed()) })
		write := func(mutate func(*servingv1alpha1.GateStatus)) error {
			latest := getServingDeployment(sd)
			latest.Status.Gate = schemaGateStatus()
			mutate(latest.Status.Gate)
			return k8sClient.Status().Update(ctx, latest)
		}
		Expect(write(func(st *servingv1alpha1.GateStatus) {
			st.Mode = levelActRollback
			st.Records[0].EffectiveLevel = levelActRollback
			st.RollbackVerdict.RolledBackAt = new(metav1.NewTime(windowStart.Add(5 * time.Minute)))
		})).To(Succeed())
		Expect(write(func(st *servingv1alpha1.GateStatus) { st.Records[0].EffectiveLevel = "" })).
			To(Succeed(), "a record without the field, as an earlier operator wrote it")
		Expect(write(func(st *servingv1alpha1.GateStatus) { st.Records[0].EffectiveLevel = "Rollback" })).
			To(MatchError(ContainSubstring("effectiveLevel")))
	})
})
