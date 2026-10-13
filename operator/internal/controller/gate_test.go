package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/scrape"
)

// The pods' per-interval traffic in these specs: 150 /predict requests a
// minute, 5 rps, three times the floor (D44's arithmetic).
const perInterval = 150

// podTraffic is what one pod's /metrics shows: its cumulative /predict
// requests, each either fast, between 0.1 s and 0.25 s, or slow, between
// 0.25 s and 0.5 s, and the 5xx among the fast ones.
type podTraffic struct {
	pod                gate.Pod
	fast, slow, errors float64
}

// serve adds an interval's traffic.
func (p *podTraffic) serve(fast, slow, errors float64) {
	p.fast += fast
	p.slow += slow
	p.errors += errors
}

// sample is the pod's /metrics as package scrape parses it.
func (p *podTraffic) sample() *gate.Sample {
	n := p.fast + p.slow
	return &gate.Sample{Pod: p.pod, Requests: n, Errors: p.errors, Buckets: []gate.Bucket{
		{UpperBoundSeconds: 0.1, Count: 0},
		{UpperBoundSeconds: 0.25, Count: p.fast},
		{UpperBoundSeconds: 0.5, Count: n},
		{UpperBoundSeconds: math.Inf(1), Count: n},
	}}
}

// scriptedReads stands in for the operator's scrape.Scraper at the gate's
// boundaries. The pods are real: a scrape.Scraper reads them uncached from
// envtest's API server, so the canary's Ready time and its OOM kills come
// from the status these specs write to the pods directly, envtest having no
// kubelet. The pods have no pod IP, so that Scraper's /metrics reads fail
// before any request is made; what each pod's /metrics shows is scripted
// instead, a nil podTraffic being a read that failed.
type scriptedReads struct {
	pods *scrape.Scraper

	mu             sync.Mutex
	stable, canary *podTraffic
	reads          int
}

func newScriptedReads() *scriptedReads {
	return &scriptedReads{pods: &scrape.Scraper{Reader: k8sClient}}
}

func (s *scriptedReads) ReadBoundary(ctx context.Context, namespace string) scrape.Boundary {
	b := s.pods.ReadBoundary(ctx, namespace)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	var read gate.Samples
	if s.stable != nil {
		read.Stable = s.stable.sample()
	}
	if s.canary != nil {
		read.Canary = s.canary.sample()
	}
	b.Current, b.Baseline = read, read
	b.StableErr, b.CanaryErr = nil, nil
	return b
}

func (s *scriptedReads) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// createAPIPod creates one of the window's api pods in ns and writes the
// status a kubelet would report for it: Running, its api container running,
// and Ready since readyAt, or not yet Ready if readyAt is nil. It has no pod
// IP: its /metrics is scriptedReads'.
func createAPIPod(ns, name string, lbls map[string]string, readyAt *time.Time) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbls},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: apiContainerName, Image: stableImage(validImageTag),
		}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  apiContainerName,
			Image: stableImage(validImageTag),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	setReady(&pod.Status, readyAt)
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	return pod
}

func setReady(status *corev1.PodStatus, readyAt *time.Time) {
	status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	status.ContainerStatuses[0].Ready = readyAt != nil
	if readyAt != nil {
		status.Conditions[0].Status = corev1.ConditionTrue
		status.Conditions[0].LastTransitionTime = metav1.NewTime(*readyAt)
	}
}

// updatePodStatus rewrites pod's status through mutate.
func updatePodStatus(pod *corev1.Pod, mutate func(*corev1.PodStatus)) {
	latest := &corev1.Pod{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), latest)).To(Succeed())
	mutate(&latest.Status)
	Expect(k8sClient.Status().Update(ctx, latest)).To(Succeed())
}

func podIdentity(pod *corev1.Pod) gate.Pod {
	return gate.Pod{Name: pod.Name, UID: string(pod.UID)}
}

// setGate sets spec.gate the way the host-side patch that opens a window
// does, with no resourceVersion precondition; a nil spec removes it.
func setGate(sd *servingv1alpha1.ServingDeployment, spec *servingv1alpha1.GateSpec) {
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"gate": spec}})
	Expect(err).NotTo(HaveOccurred())
	target := &servingv1alpha1.ServingDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: sd.Namespace, Name: sd.Name}}
	Expect(k8sClient.Patch(ctx, target, client.RawPatch(types.MergePatchType, body))).To(Succeed())
}

func at(t time.Time) *time.Time { return &t }

// gateHarness is a steady stack whose ServingDeployment is reconciled on a
// fake clock with scriptedReads for its boundary reads, a stable pod Ready
// long before, and boundary walks the gate's grid one boundary at a time.
type gateHarness struct {
	ns     string
	sd     *servingv1alpha1.ServingDeployment
	r      *ServingDeploymentReconciler
	writes *recordingClient
	clock  *testClock
	reads  *scriptedReads
	// n is the boundary the clock was last set to.
	n int32
}

func newGateHarness(spec *servingv1alpha1.GateSpec) *gateHarness {
	ns, sd, r, writes, clock := steadyStack()
	h := &gateHarness{ns: ns, sd: sd, r: r, writes: writes, clock: clock, reads: newScriptedReads()}
	r.scraper = h.reads
	stable := createAPIPod(ns, "api-6f7c9d8b5-q2xkz", apiLabels(), at(windowStart.Add(-time.Hour)))
	h.reads.stable = &podTraffic{pod: podIdentity(stable)}
	if spec != nil {
		setGate(sd, spec)
	}
	return h
}

// addCanary creates the canary pod, Ready since readyAt or not yet Ready, and
// scripts its /metrics.
func (h *gateHarness) addCanary(readyAt *time.Time) *corev1.Pod {
	pod := createAPIPod(h.ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels(), readyAt)
	h.reads.mu.Lock()
	defer h.reads.mu.Unlock()
	h.reads.canary = &podTraffic{pod: podIdentity(pod)}
	return pod
}

// interval is the reconciler's grid interval.
func (h *gateHarness) interval() time.Duration { return h.r.gridInterval() }

func (h *gateHarness) boundaryTime(n int32) time.Time {
	return windowStart.Add(time.Duration(n) * h.interval())
}

// open opens the window at windowStart, boundary 0, and walks it to the
// window state.
func (h *gateHarness) open() {
	h.clock.t = windowStart
	openWindowFully(h.ns, h.sd, h.r)
}

// serve has each pod whose read is scripted serve one interval's traffic, the
// stable all fast and the canary as given.
func (h *gateHarness) serve(fast, slow, errors float64) {
	h.reads.mu.Lock()
	defer h.reads.mu.Unlock()
	h.reads.stable.serve(perInterval, 0, 0)
	if h.reads.canary != nil {
		h.reads.canary.serve(fast, slow, errors)
	}
}

// next serves an interval of the canary's traffic and reconciles at the next
// boundary.
func (h *gateHarness) next(fast, slow, errors float64) reconcile.Result {
	h.serve(fast, slow, errors)
	h.n++
	h.clock.t = h.boundaryTime(h.n)
	return reconcileResult(h.r, h.sd)
}

// clean walks count boundaries of clean intervals.
func (h *gateHarness) clean(count int) {
	for range count {
		h.next(perInterval, 0, 0)
	}
}

func (h *gateHarness) gate() *servingv1alpha1.GateStatus {
	st := getServingDeployment(h.sd).Status.Gate
	Expect(st).NotTo(BeNil(), "no status.gate")
	return st
}

// gateEvents returns the gate's Events the reconciler has recorded since the
// last call.
func (h *gateHarness) gateEvents() []string {
	var out []string
	for _, e := range drainEvents(h.r) {
		if strings.Contains(e, " Gate") {
			out = append(out, e)
		}
	}
	return out
}

func verdicts(st *servingv1alpha1.GateStatus) []servingv1alpha1.GateVerdict {
	out := make([]servingv1alpha1.GateVerdict, 0, len(st.Records))
	for _, rec := range st.Records {
		out = append(out, rec.Verdict)
	}
	return out
}

func indexes(st *servingv1alpha1.GateStatus) []int32 {
	out := make([]int32, 0, len(st.Records))
	for _, rec := range st.Records {
		out = append(out, rec.Index)
	}
	return out
}

func repeat[T any](v T, n int) []T {
	out := make([]T, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func span(from, to int32) []int32 {
	out := make([]int32, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// expectNoWorkloadWrites asserts that the only writes the reconciler sent
// since the first `from` were to a ServingDeployment's status: the gate
// writes no spec, and moves no Deployment and no StatefulSet.
func expectNoWorkloadWrites(rc *recordingClient, from int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, w := range rc.writes[from:] {
		Expect(w.kind == "ServingDeployment" && w.subresource == "status").To(BeTrue(),
			"a write other than the ServingDeployment's status: %+v", w)
	}
}

func writeCount(rc *recordingClient) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.writes)
}

func parseDecimal(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	Expect(err).NotTo(HaveOccurred())
	return v
}

var observe = &servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeObserve}

var _ = Describe("The gate in Observe (D42, D45)", func() {
	It("leaves a ServingDeployment with no spec.gate exactly v0: through a whole window it reads no pod, "+
		"writes no status.gate, records no gate Event and requeues for the TTL alone", func() {
		h := newGateHarness(nil)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		for n := int32(1); n <= 5; n++ {
			Expect(h.next(perInterval, 0, 0).RequeueAfter).To(Equal(canaryWindowTTL - time.Duration(n)*time.Minute))
		}
		h.clock.t = windowStart.Add(canaryWindowTTL)
		Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(BeZero())

		latest := getServingDeployment(h.sd)
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).Reason).To(Equal(servingv1alpha1.ReasonWindowExpired))
		Expect(latest.Status.Gate).To(BeNil())
		Expect(h.reads.count()).To(BeZero(), "a gateless ServingDeployment reads no pod")
		Expect(h.gateEvents()).To(BeEmpty())
	})

	It("observes a clean window: pending until the canary is Ready, warmup for its first minute, then eleven "+
		"clean intervals and the promote verdict, recorded with one Event and acted on by nothing", func() {
		h := newGateHarness(observe)
		h.open()

		By("the window's first boundary starts the evidence: one read, though the window took three reconciles to open")
		st := h.gate()
		Expect(st.WindowStart.Time).To(BeTemporally("==", windowStart))
		Expect(st.IntervalSeconds).To(BeEquivalentTo(60))
		Expect(st.Mode).To(Equal(servingv1alpha1.GateModeObserve))
		Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingArmed))
		Expect(st.NextBoundary).To(BeEquivalentTo(1))
		Expect(st.Records).To(BeEmpty())
		Expect(h.reads.count()).To(Equal(1))
		Expect(h.gateEvents()).To(ConsistOf(
			"Normal GateArmed the gate is armed in Observe for canary tag " + canaryImageTag +
				": every 1m0s interval from 2026-09-29T12:00:00Z is recorded in status.gate, and the gate acts on nothing"))
		writesOpen := writeCount(h.writes)
		specOpen := getServingDeployment(h.sd)

		By("the canary pod comes up Running inside interval 0, not yet Ready: interval 0 is pending")
		canary := h.addCanary(nil)
		Expect(h.next(perInterval, 0, 0).RequeueAfter).To(Equal(time.Minute))

		By("Ready at 1m30s: the intervals starting before 2m30s are warmup, the rest clean")
		updatePodStatus(canary, func(s *corev1.PodStatus) { setReady(s, at(windowStart.Add(90*time.Second))) })
		for range 2 + 10 {
			Expect(h.next(perInterval, 0, 0).RequeueAfter).To(Equal(time.Minute))
		}
		st = h.gate()
		Expect(verdicts(st)).To(Equal(append(
			[]servingv1alpha1.GateVerdict{gateVerdict(gate.VerdictPending), gateVerdict(gate.VerdictWarmup),
				gateVerdict(gate.VerdictWarmup)},
			repeat(gateVerdict(gate.VerdictClean), 10)...)))
		Expect(st.CleanRun).To(BeEquivalentTo(10))
		Expect(st.PromoteVerdict).To(BeNil(), "not at 10")
		Expect(h.gateEvents()).To(BeEmpty())

		By("the eleventh clean interval is the promote verdict, recorded once, with one Event")
		h.next(perInterval, 0, 0)
		st = h.gate()
		Expect(st.PromoteVerdict).NotTo(BeNil())
		Expect(st.PromoteVerdict.Interval).To(BeEquivalentTo(13))
		Expect(st.PromoteVerdict.At.Time).To(BeTemporally("==", windowStart.Add(14*time.Minute)))
		Expect(h.gateEvents()).To(ConsistOf("Normal GateWouldPromote interval 13 completed 11 consecutive clean " +
			"intervals, the promote verdict: in Observe it is recorded and nothing is dispatched"))
		h.next(perInterval, 0, 0)
		st = h.gate()
		Expect(st.PromoteVerdict.Interval).To(BeEquivalentTo(13), "the first verdict stays the one recorded")
		Expect(h.gateEvents()).To(BeEmpty(), "the verdict recurs in the engine, and is not announced again")

		By("the record: interval by interval, D42's shape")
		Expect(indexes(st)).To(Equal(span(0, 14)))
		Expect(st.Records[0].Reasons).To(Equal([]servingv1alpha1.GateReason{"CanaryNotReady"}))
		Expect(st.Records[1].Reasons).To(Equal([]servingv1alpha1.GateReason{"CanaryWarmup"}))
		third := st.Records[3]
		Expect(third.Start.Time).To(BeTemporally("==", windowStart.Add(3*time.Minute)))
		Expect(third.End.Time).To(BeTemporally("==", windowStart.Add(4*time.Minute)))
		Expect(third.Verdict).To(Equal(gateVerdict(gate.VerdictClean)))
		Expect(third.Reasons).To(BeEmpty())
		for _, pod := range []*servingv1alpha1.GatePodDelta{third.Stable, third.Canary} {
			Expect(pod.Requests).To(BeEquivalentTo(perInterval))
			Expect(pod.Errors).To(BeZero())
			Expect(parseDecimal(pod.P95Seconds)).To(BeNumerically("~", 0.2425, 1e-12))
		}
		Expect(third.Ratio).To(Equal("1"))
		Expect(st.CleanRun).To(BeEquivalentTo(12))
		Expect(st.BreachRun).To(BeZero())
		Expect(st.CanaryReadySeen).To(BeTrue())
		Expect(st.CanaryPod).To(Equal(&servingv1alpha1.GatePod{Name: canary.Name, UID: string(canary.UID)}))
		Expect(st.Totals).To(Equal(servingv1alpha1.GateTotals{Pending: 1, Warmup: 2, Clean: 12}))
		Expect(st.LastVerdict).To(Equal(gateVerdict(gate.VerdictClean)))
		Expect(st.RollbackVerdict).To(BeNil())

		By("acted on nothing: no spec write, no workload moved, the window open")
		expectNoWorkloadWrites(h.writes, writesOpen)
		latest := getServingDeployment(h.sd)
		Expect(latest.Spec).To(Equal(specOpen.Spec))
		Expect(latest.Generation).To(Equal(specOpen.Generation))
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).Status).To(Equal(metav1.ConditionTrue))
		expectCanaryReplicas(h.ns, 1)
	})

	It("leaves the status byte-identical across a reconcile between boundaries: no read, no ring append, "+
		"no timestamp moved, and so no status write", func() {
		h := newGateHarness(observe)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(3)
		before := getServingDeployment(h.sd)
		beforeJSON, err := json.Marshal(before.Status)
		Expect(err).NotTo(HaveOccurred())
		reads, writes := h.reads.count(), writeCount(h.writes)

		for _, offset := range []time.Duration{time.Second, 30 * time.Second, 59*time.Second + 999*time.Millisecond} {
			h.clock.t = h.boundaryTime(3).Add(offset)
			Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(Equal(time.Minute-offset),
				"requeued for the next boundary, %s on", offset)
		}
		after := getServingDeployment(h.sd)
		afterJSON, err := json.Marshal(after.Status)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(afterJSON)).To(Equal(string(beforeJSON)))
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "the DeepEqual guard sent no write")
		Expect(h.reads.count()).To(Equal(reads), "no pod read between boundaries")
		Expect(writeCount(h.writes)).To(Equal(writes))
	})

	It("records a latency breach two intervals in a row as the rollback verdict and acts on nothing: the canary "+
		"keeps serving and CanaryActive stays True, until the TTL closes the window exactly as in v0", func() {
		h := newGateHarness(observe)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.drainOpenEvents()
		writesOpen := writeCount(h.writes)
		specOpen := getServingDeployment(h.sd).Spec
		h.clean(3)

		By("a slow canary: the first breach is announced, and one is not yet a trip")
		h.next(0, perInterval, 0)
		st := h.gate()
		breach := st.Records[3]
		Expect(breach.Verdict).To(Equal(gateVerdict(gate.VerdictBreach)))
		Expect(breach.Reasons).To(Equal([]servingv1alpha1.GateReason{servingv1alpha1.GateReason(gate.ReasonLatencyRatio)}))
		Expect(parseDecimal(breach.Ratio)).To(BeNumerically(">", gate.LatencyRatioLimit))
		Expect(st.BreachRun).To(BeEquivalentTo(1))
		Expect(st.CleanRun).To(BeZero())
		Expect(st.RollbackVerdict).To(BeNil())
		Expect(h.gateEvents()).To(ConsistOf(
			"Warning GateBreach interval 3 (2026-09-29T12:03:00Z to 2026-09-29T12:04:00Z) is the window's first breach: " +
				"LatencyRatio"))

		By("the second in a row trips line 3: the rollback verdict, recorded with one Event")
		h.next(0, perInterval, 0)
		st = h.gate()
		Expect(st.RollbackVerdict).NotTo(BeNil())
		Expect(st.RollbackVerdict.Interval).To(BeEquivalentTo(4))
		Expect(st.RollbackVerdict.At.Time).To(BeTemporally("==", windowStart.Add(5*time.Minute)))
		Expect(st.RollbackVerdict.Trip).To(BeEquivalentTo("Line3CanaryLatency"))
		Expect(h.gateEvents()).To(ConsistOf("Warning GateWouldRollBack interval 4 tripped Line3CanaryLatency: an " +
			"acting gate would close the window here; in Observe nothing is done, and the window stays open until " +
			"its TTL or a close"))
		h.next(0, perInterval, 0)
		st = h.gate()
		Expect(st.BreachRun).To(BeEquivalentTo(3))
		Expect(st.RollbackVerdict.Interval).To(BeEquivalentTo(4))
		Expect(h.gateEvents()).To(BeEmpty(), "transitions only")

		By("through the breach the canary keeps serving and CanaryActive stays True")
		latest := getServingDeployment(h.sd)
		active := condition(latest, servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionTrue))
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonWindowOpen))
		Expect(latest.Spec).To(Equal(specOpen))
		expectCanaryReplicas(h.ns, 1)
		expectShadowReplicas(h.ns, 0)
		expectNoWorkloadWrites(h.writes, writesOpen)

		By("the TTL closes the window at 45 minutes as WindowExpired, as in v0, with no read; the evidence stays")
		reads := h.reads.count()
		h.clock.t = windowStart.Add(canaryWindowTTL)
		Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(BeZero())
		latest = getServingDeployment(h.sd)
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).Reason).To(Equal(servingv1alpha1.ReasonWindowExpired))
		expectCanaryReplicas(h.ns, 0)
		Expect(h.reads.count()).To(Equal(reads))
		Expect(latest.Status.Gate).To(Equal(st))
		Expect(drainEvents(h.r)).To(ContainElement(ContainSubstring("Normal WindowExpired")))
	})

	DescribeTable("trips at once on what the canary pod shows, its OOM kill read from containerStatuses written "+
		"directly and a 5xx past warmup: recorded as the rollback verdict, acted on by nothing",
		func(trip func(h *gateHarness, canary *corev1.Pod), reason servingv1alpha1.GateReason,
			want servingv1alpha1.GateTrip) {
			h := newGateHarness(observe)
			canary := h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.clean(2)
			trip(h, canary)
			st := h.gate()
			last := st.Records[len(st.Records)-1]
			Expect(last.Verdict).To(Equal(gateVerdict(gate.VerdictBreach)))
			Expect(last.Reasons).To(ContainElement(reason))
			Expect(st.RollbackVerdict).To(HaveField("Trip", want))
			Expect(st.RollbackVerdict.Interval).To(BeEquivalentTo(2))
			Expect(condition(getServingDeployment(h.sd), servingv1alpha1.ConditionCanaryActive).Status).
				To(Equal(metav1.ConditionTrue))
			expectCanaryReplicas(h.ns, 1)
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
			}, servingv1alpha1.GateReason("CanaryOOMKilled"), servingv1alpha1.GateTrip("Line5CanaryOOMKilled")),
		Entry("one canary 5xx past warmup", func(h *gateHarness, _ *corev1.Pod) {
			h.next(perInterval, 0, 1)
		}, servingv1alpha1.GateReason("CanaryErrors"), servingv1alpha1.GateTrip("Line2CanaryErrors")),
	)

	It("resumes from status across an operator restart: the counters, the session and the ring carry on, and the "+
		"interval whose baseline the restart lost is forfeited as OperatorInterrupted — after one breach, the second "+
		"in a row", func() {
		h := newGateHarness(observe)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(3)
		h.next(0, perInterval, 0)
		before := h.gate()
		Expect(before.BreachRun).To(BeEquivalentTo(1))
		h.drainOpenEvents()

		By("a restarted operator, its baselines gone, reconciles mid-interval: nothing to read, nothing written")
		restarted, _, clock := newClockedReconciler()
		restarted.scraper = h.reads
		h.r, h.clock = restarted, clock
		clock.t = h.boundaryTime(4).Add(20 * time.Second)
		rv := getServingDeployment(h.sd).ResourceVersion
		Expect(reconcileResult(restarted, h.sd).RequeueAfter).To(Equal(40 * time.Second))
		Expect(getServingDeployment(h.sd).ResourceVersion).To(Equal(rv))

		By("at the next boundary the interval it interrupted is forfeited, and line 3's counter resumed from status")
		h.next(perInterval, 0, 0)
		st := h.gate()
		forfeited := st.Records[4]
		Expect(forfeited.Verdict).To(Equal(gateVerdict(gate.VerdictUnevaluable)))
		Expect(forfeited.Reasons).To(Equal([]servingv1alpha1.GateReason{"OperatorInterrupted"}))
		Expect(forfeited.Start.Time).To(BeTemporally("==", windowStart.Add(4*time.Minute)))
		Expect(st.BreachRun).To(BeEquivalentTo(2))
		Expect(st.RollbackVerdict).To(HaveField("Trip", servingv1alpha1.GateTrip("Line3CanaryLatency")))
		Expect(st.RollbackVerdict.Interval).To(BeEquivalentTo(4))
		Expect(h.gateEvents()).To(ConsistOf(ContainSubstring("Warning GateWouldRollBack interval 4")))

		By("the read it took there is the next interval's baseline: clean again, and the ring unbroken")
		h.next(perInterval, 0, 0)
		st = h.gate()
		Expect(indexes(st)).To(Equal(span(0, 5)))
		Expect(st.LastVerdict).To(Equal(gateVerdict(gate.VerdictClean)))
		Expect(st.BreachRun).To(BeZero())
		Expect(st.CleanRun).To(BeEquivalentTo(1))
		Expect(st.Totals).To(Equal(servingv1alpha1.GateTotals{Clean: 4, Breach: 1, Unevaluable: 1}))
		Expect(st.WindowStart).To(Equal(before.WindowStart))
		Expect(st.CanaryPod).To(Equal(before.CanaryPod))
	})

	DescribeTable("forfeits what an operator restart or a missed boundary costs, as OperatorInterrupted, and keeps "+
		"the grid anchored to CanaryActive: a read later than half an interval after its boundary misses it",
		func(downUntil time.Duration, wantReads int, want []servingv1alpha1.GateVerdict, wantNext time.Duration) {
			h := newGateHarness(observe)
			h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.clean(1)
			reads := h.reads.count()

			By("the operator goes down after boundary 1, and a new one starts at " + downUntil.String())
			restarted, _, clock := newClockedReconciler()
			restarted.scraper = h.reads
			h.r, h.clock = restarted, clock
			h.serve(perInterval, 0, 0)
			clock.t = windowStart.Add(downUntil)
			Expect(reconcileResult(restarted, h.sd).RequeueAfter).To(Equal(wantNext))
			Expect(h.reads.count()-reads).To(Equal(wantReads), "a missed boundary is not read late")

			By("then on to boundary 6 on time, the canary clean throughout")
			h.n = h.gate().NextBoundary - 1
			for h.n < 6 {
				h.next(perInterval, 0, 0)
			}

			st := h.gate()
			Expect(verdicts(st)).To(Equal(want))
			Expect(indexes(st)).To(Equal(span(0, 5)))
			for i, rec := range st.Records {
				Expect(rec.Start.Time).To(BeTemporally("==", windowStart.Add(time.Duration(i)*time.Minute)))
				Expect(rec.End.Time).To(BeTemporally("==", windowStart.Add(time.Duration(i+1)*time.Minute)))
				if rec.Verdict == gateVerdict(gate.VerdictUnevaluable) {
					Expect(rec.Reasons).To(Equal([]servingv1alpha1.GateReason{"OperatorInterrupted"}))
				}
			}
		},
		Entry("back 10 s after boundary 2: on time; only interval 1, which it interrupted, is lost",
			2*time.Minute+10*time.Second, 1,
			verdictList("Clean", "Unevaluable", "Clean", "Clean", "Clean", "Clean"), 50*time.Second),
		Entry("back 30 s after boundary 2, half an interval: still on time",
			2*time.Minute+30*time.Second, 1,
			verdictList("Clean", "Unevaluable", "Clean", "Clean", "Clean", "Clean"), 30*time.Second),
		Entry("back 31 s after boundary 2: boundary 2 is missed, and interval 2, which it starts, has no baseline",
			2*time.Minute+31*time.Second, 0,
			verdictList("Clean", "Unevaluable", "Unevaluable", "Clean", "Clean", "Clean"), 29*time.Second),
		Entry("back after boundaries 2, 3 and 4 went by: each interval they end is forfeited at once, "+
			"and so is the one the next boundary ends",
			4*time.Minute+40*time.Second, 0,
			verdictList("Clean", "Unevaluable", "Unevaluable", "Unevaluable", "Unevaluable", "Clean"), 20*time.Second),
	)

	It("keeps at most 50 records in its ring, checked past 50 intervals: the oldest go first, the totals count on", func() {
		h := newGateHarness(observe)
		h.r.gateInterval = gateIntervalFloor
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(55)
		st := h.gate()
		Expect(st.IntervalSeconds).To(BeEquivalentTo(20))
		Expect(st.Records).To(HaveLen(gateRingSize))
		Expect(indexes(st)).To(Equal(span(5, 54)))
		Expect(st.Records[0].Start.Time).To(BeTemporally("==", windowStart.Add(5*gateIntervalFloor)))
		Expect(st.Totals.Clean).To(BeEquivalentTo(55))
		Expect(st.NextBoundary).To(BeEquivalentTo(56))
		encoded, err := json.Marshal(st)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(encoded)).To(BeNumerically("<", 16<<10), "about 12.5 KB, trivial against etcd's bound (D42)")
	})

	DescribeTable("arms an acting mode only for the canary tag it names, and runs Observe, and an acting mode "+
		"armed for no tag or another tag, as Observe, saying so in one Event per window",
		func(spec servingv1alpha1.GateSpec, want servingv1alpha1.GateArming, event any) {
			h := newGateHarness(&spec)
			h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			Expect(h.gateEvents()).To(ConsistOf(event))
			h.next(0, perInterval, 0)
			h.next(0, perInterval, 0)
			st := h.gate()
			Expect(st.Mode).To(Equal(servingv1alpha1.GateModeObserve))
			Expect(st.Arming).To(Equal(want))
			Expect(st.RollbackVerdict).NotTo(BeNil(), "it observes: the verdict recorded")
			Expect(h.gateEvents()).NotTo(ContainElement(Or(
				ContainSubstring("GateArmed"), ContainSubstring("GateDegraded"))), "once per window")
			Expect(condition(getServingDeployment(h.sd), servingv1alpha1.ConditionCanaryActive).Status).
				To(Equal(metav1.ConditionTrue), "and acts on nothing")
			expectCanaryReplicas(h.ns, 1)
		},
		Entry("Observe, which needs no tag: armed",
			servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeObserve},
			servingv1alpha1.GateArmingArmed, ContainSubstring("Normal GateArmed the gate is armed in Observe")),
		Entry("ActRollback for another canary's tag: degraded to Observe (D42)",
			servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeActRollback, ForCanaryImageTag: laterCanaryImageTag},
			servingv1alpha1.GateArmingTagMismatch, Equal("Warning GateDegraded spec.gate.mode ActRollback is armed for "+
				"canary tag \""+laterCanaryImageTag+"\", not spec.canaryImageTag "+canaryImageTag+": the gate runs as "+
				"Observe, records every interval in status.gate and acts on nothing")),
		Entry("ActPromote with no tag: degraded to Observe",
			servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeActPromote},
			servingv1alpha1.GateArmingTagMismatch,
			ContainSubstring("Warning GateDegraded spec.gate.mode ActPromote is armed for canary tag \"\"")),
	)

	It("re-arms at a boundary when the canary tag changes mid-window, with one Event on the transition", func() {
		h := newGateHarness(&servingv1alpha1.GateSpec{
			Mode: servingv1alpha1.GateModeActRollback, ForCanaryImageTag: canaryImageTag,
		})
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		Expect(h.gateEvents()).To(ConsistOf(ContainSubstring("Normal GateArmed the gate is armed in ActRollback")))
		h.clean(1)
		setCanarySpec(h.sd, laterCanaryImageTag, 1)
		h.clean(1)
		Expect(h.gate().Arming).To(Equal(servingv1alpha1.GateArmingTagMismatch))
		Expect(h.gateEvents()).To(ConsistOf(ContainSubstring(
			"GateDegraded spec.gate.mode ActRollback is armed for canary tag \"" + canaryImageTag + "\", not " +
				"spec.canaryImageTag " + laterCanaryImageTag)))
		h.clean(2)
		Expect(h.gateEvents()).To(BeEmpty())
	})

	It("ends with its window, keeping the evidence, and starts afresh with the next window's clock", func() {
		h := newGateHarness(observe)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(3)
		h.drainOpenEvents()
		first := h.gate()

		By("closed by the close-window patch: no read while it is closed, and the evidence kept")
		setCanarySpec(h.sd, "", 0)
		h.clean(1)
		markCanaryObserved(h.ns, 0)
		h.clean(1)
		Expect(h.reads.count()).To(Equal(4), "the window's four boundaries, and none after")
		Expect(h.gate()).To(Equal(first))

		By("the next window anchors a new grid and a new record")
		setCanarySpec(h.sd, canaryImageTag, 1)
		h.clock.t = windowStart.Add(10 * time.Minute)
		Expect(reconcileServingDeployment(h.r, h.sd)).To(Succeed())
		st := h.gate()
		Expect(st.WindowStart.Time).To(BeTemporally("==", windowStart.Add(10*time.Minute)))
		Expect(st.Records).To(BeEmpty())
		Expect(st.Totals).To(BeZero())
		Expect(h.gateEvents()).To(ConsistOf(ContainSubstring("Normal GateArmed")))
	})

	It("round-trips every field of the engine's session through status", func() {
		for _, s := range []gate.Session{
			{},
			{BreachRun: 2, CleanRun: 0, CanaryReadySeen: true, CanaryPod: gate.Pod{Name: "api-canary-x", UID: "u-1"}},
			{BreachRun: 0, CleanRun: 11, CanaryReadySeen: true, CanaryPod: gate.Pod{Name: "api-canary-y", UID: "u-2"}},
		} {
			st := &servingv1alpha1.GateStatus{}
			storeSession(st, s)
			Expect(sessionOf(st)).To(Equal(s))
		}
		// Every field of gate.Session is one of the four above: a new one must
		// be kept in status too, or a restart would drop it (D45).
		var fields []string
		for field := range reflect.TypeFor[gate.Session]().Fields() {
			fields = append(fields, field.Name)
		}
		Expect(fields).To(Equal([]string{"BreachRun", "CleanRun", "CanaryReadySeen", "CanaryPod"}))
	})
})

// drainOpenEvents discards the Events the window's opening recorded.
func (h *gateHarness) drainOpenEvents() { drainEvents(h.r) }

func gateVerdict(v gate.Verdict) servingv1alpha1.GateVerdict { return servingv1alpha1.GateVerdict(v) }

func verdictList(vs ...string) []servingv1alpha1.GateVerdict {
	out := make([]servingv1alpha1.GateVerdict, 0, len(vs))
	for _, v := range vs {
		out = append(out, servingv1alpha1.GateVerdict(v))
	}
	return out
}

var _ = Describe("Wiring the gate", func() {
	It("reads through a scrape.Scraper holding the manager's APIReader and nothing else, so its /metrics reads "+
		"go through package scrape's own hardened client and none of the operator's (#116)", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:     k8sClient.Scheme(),
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{"default": {}}},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		r := &ServingDeploymentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: &record.FakeRecorder{}}
		Expect(r.SetupWithManager(mgr)).To(Succeed())

		s, ok := r.scraper.(*scrape.Scraper)
		Expect(ok).To(BeTrue(), "the scraper is a %T", r.scraper)
		Expect(s.Reader).To(BeIdenticalTo(mgr.GetAPIReader()), "the uncached reader: no informer, so no watch (D41)")
		Expect(s.Reader).NotTo(BeIdenticalTo(mgr.GetClient()))
		// Package scrape's own spec pins what a Scraper with only Reader set
		// reads with: its metricsClient, never a caller's. Here: nothing but
		// Reader is set, unexported fields included.
		v := reflect.ValueOf(s).Elem()
		for i := range v.NumField() {
			if name := v.Type().Field(i).Name; name != "Reader" {
				Expect(v.Field(i).IsZero()).To(BeTrue(), "Scraper.%s is set", name)
			}
		}
		Expect(r.gridInterval()).To(Equal(gateGridInterval))
	})

	It("holds the grid to watch-window.sh's interval", func() {
		script, err := os.ReadFile(filepath.Join(k3sDir, "watch-window.sh"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(script)).To(ContainSubstring(
			fmt.Sprintf("\nINTERVAL_SECONDS=%d\n", int(gateGridInterval.Seconds()))))
	})

	DescribeTable("reads the grid from its test-only knob, a whole number of seconds from four read timeouts "+
		"to the O4 interval, and refuses anything else",
		func(value *string, want time.Duration, wantErr string) {
			interval, err := gateIntervalFrom(func(name string) (string, bool) {
				Expect(name).To(Equal(gateIntervalEnv))
				if value == nil {
					return "", false
				}
				return *value, true
			})
			if wantErr != "" {
				Expect(err).To(MatchError(ContainSubstring(wantErr)))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(interval).To(Equal(want))
		},
		Entry("unset: the O4 interval", nil, time.Minute, ""),
		Entry("20s, the floor", new("20s"), 20*time.Second, ""),
		Entry("1m", new("1m"), time.Minute, ""),
		Entry("19s, under the floor", new("19s"), time.Duration(0), "from 20s to 1m0s"),
		Entry("61s, longer than the O4 interval", new("61s"), time.Duration(0), "from 20s to 1m0s"),
		Entry("a fraction of a second", new("20500ms"), time.Duration(0), "whole number of seconds"),
		Entry("not a duration", new("fast"), time.Duration(0), "invalid duration"),
	)

	It("requeues an open window for the earlier of its TTL and the gate's next boundary", func() {
		h := newGateHarness(observe)
		h.r.gateInterval = 40 * time.Second
		h.addCanary(at(windowStart.Add(-time.Minute)))
		h.open()
		h.clean(65)
		By("at 44m00s, the next boundary is 40 s away and the TTL 60 s")
		Expect(h.next(perInterval, 0, 0).RequeueAfter).To(Equal(40 * time.Second))
		By("at 44m40s, the TTL is 20 s away and the next boundary 40 s")
		Expect(h.next(perInterval, 0, 0).RequeueAfter).To(Equal(20 * time.Second))
		h.clock.t = windowStart.Add(canaryWindowTTL)
		Expect(reconcileResult(h.r, h.sd).RequeueAfter).To(BeZero())
		Expect(condition(getServingDeployment(h.sd), servingv1alpha1.ConditionCanaryActive).Reason).
			To(Equal(servingv1alpha1.ReasonWindowExpired))
		Expect(h.gate().NextBoundary).To(BeEquivalentTo(68), "the TTL beat boundary 68: nothing past it is recorded")
	})
})

var _ = Describe("Shipping the gate", func() {
	It("validates status.gate against the decision engine's own vocabularies: the CRD's enums are package "+
		"gate's verdicts, reasons and trips, no more and no fewer", func() {
		consts := gateConstants()
		Expect(consts).To(HaveKey("Verdict"))
		Expect(consts).To(HaveKey("Reason"))
		Expect(consts).To(HaveKey("Trip"))
		gateProperties := []string{"status", "properties", "gate", "properties"}
		record := append(append([]string(nil), gateProperties...), "records", "items", "properties")
		for _, c := range []struct {
			path   []string
			consts string
		}{
			{append(append([]string(nil), record...), "verdict"), "Verdict"},
			{append(append([]string(nil), gateProperties...), "lastVerdict"), "Verdict"},
			{append(append([]string(nil), record...), "reasons", "items"), "Reason"},
			{append(append([]string(nil), gateProperties...), "rollbackVerdict", "properties", "trip"), "Trip"},
		} {
			Expect(crdEnum(c.path...)).To(ConsistOf(consts[c.consts]), "%v", c.path)
		}
		Expect(crdEnum("spec", "properties", "gate", "properties", "mode")).To(ConsistOf(
			string(servingv1alpha1.GateModeObserve), string(servingv1alpha1.GateModeActRollback),
			string(servingv1alpha1.GateModeActPromote)))
		maxItems, found, err := unstructured.NestedFieldNoCopy(crdSchema(),
			append(append([]string(nil), gateProperties...), "records", "maxItems")...)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(maxItems).To(BeNumerically("==", gateRingSize))
	})

	It("ships no manifest and no chart file that sets the gate's test-only knob", func() {
		chart := filepath.Join("..", "..", "..", "deploy", "helm", "mlobs-operator")
		var files int
		for _, root := range []string{filepath.Join(k3sDir, "manifests"), chart} {
			Expect(filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(data)).NotTo(ContainSubstring(gateIntervalEnv), path)
				files++
				return nil
			})).To(Succeed())
		}
		Expect(files).To(BeNumerically(">", 10))
	})

	It("renders the ServingDeployment the pipeline applies with no gate key: spec.imageTag alone (D32, D42)", func() {
		path := filepath.Join(k3sDir, "manifests", "40-servingdeployment.yaml")
		docs := decodeDocuments(path, testImagePrefix, validImageTag)
		Expect(docs).To(HaveLen(1))
		spec, found, err := unstructured.NestedMap(docs[0].Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(Equal(map[string]any{"imageTag": validImageTag}))
		raw, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(MatchRegexp(`(?m)^\s*gate:`))
	})
})

// gateConstants is package gate's string constants by their type's name, read
// from its source.
func gateConstants() map[string][]string {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "gate", "gate.go"), nil, 0)
	Expect(err).NotTo(HaveOccurred())
	out := map[string][]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			typ, ok := value.Type.(*ast.Ident)
			if !ok {
				continue
			}
			for _, v := range value.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				Expect(err).NotTo(HaveOccurred())
				out[typ.Name] = append(out[typ.Name], s)
			}
		}
	}
	return out
}

// crdSchema is the openAPIV3Schema of the CRD apply.sh ships.
func crdSchema() map[string]any {
	docs := decodeDocuments(filepath.Join(k3sDir, "manifests", "01-servingdeployment-crd.yaml"),
		testImagePrefix, validImageTag)
	Expect(docs).To(HaveLen(1))
	versions, found, err := unstructured.NestedSlice(docs[0].Object, "spec", "versions")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	Expect(versions).To(HaveLen(1))
	schema, found, err := unstructured.NestedMap(versions[0].(map[string]any), "schema", "openAPIV3Schema", "properties")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	return schema
}

// crdEnum is the enum of the shipped schema's property at path, nil if it has
// none.
func crdEnum(path ...string) []string {
	enum, found, err := unstructured.NestedStringSlice(crdSchema(), append(path, "enum")...)
	Expect(err).NotTo(HaveOccurred())
	if !found {
		return nil
	}
	return enum
}
