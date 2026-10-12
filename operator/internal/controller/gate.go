package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/scrape"
)

// The gate's wiring (docs/PLAN.md D42, D45): at each boundary of the window's
// interval grid the controller reads the two pods (package scrape), hands the
// interval to the decision engine (package gate), and records what it decided
// in status.gate. In this version every mode runs as Observe: a verdict is
// recorded, and nothing acts on it — no spec write, no close, no dispatch.

// gateGridInterval is the gate's interval: the O4 interval, watch-window.sh's
// INTERVAL_SECONDS. It is compiled, not a setting.
const gateGridInterval = 60 * time.Second

// gateIntervalEnv names the gate's one test-only knob: set on the operator's
// container, it shortens the grid so that operator/hack/e2e.sh can walk a
// window through its verdicts in minutes. Neither the flattened manifests nor
// the chart set it, and a test holds them to that. It moves nothing else: the
// warmup, the floor, the ratio, N, the read timeout and the TTL are the
// compiled ones, and status.gate.intervalSeconds records the grid a window was
// observed on.
const gateIntervalEnv = "MLOBS_E2E_GATE_INTERVAL"

// gateIntervalFloor is the shortest grid the knob admits: four reads of
// scrape.ReadTimeout, the longest a boundary's reads can take, so a boundary's
// reads always end inside its interval and a pod that never answers still
// costs one unevaluable interval and no more (D41).
const gateIntervalFloor = 4 * scrape.ReadTimeout

// gateRingSize is status.gate's ring: 50 slots, sized to the TTL's 45
// intervals (D42). The CRD's MaxItems on the records holds the same bound.
const gateRingSize = 50

// Event reasons the gate records, on transitions only (D42): the gate armed or
// degraded, the window's first breach, and the first verdict of each kind.
const (
	eventReasonGateArmed         = "GateArmed"
	eventReasonGateDegraded      = "GateDegraded"
	eventReasonGateBreach        = "GateBreach"
	eventReasonGateWouldRollBack = "GateWouldRollBack"
	eventReasonGateWouldPromote  = "GateWouldPromote"
)

// boundaryReader reads the window's two pods at a boundary. In the operator it
// is a *scrape.Scraper, which SetupWithManager wires.
type boundaryReader interface {
	ReadBoundary(ctx context.Context, namespace string) scrape.Boundary
}

// gateIntervalFrom reads the test-only knob through lookup, os.LookupEnv in
// the operator. Unset, the grid is gateGridInterval; set, it must be a whole
// number of seconds from gateIntervalFloor to gateGridInterval.
func gateIntervalFrom(lookup func(string) (string, bool)) (time.Duration, error) {
	value, set := lookup(gateIntervalEnv)
	if !set {
		return gateGridInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", gateIntervalEnv, value, err)
	}
	if interval%time.Second != 0 || interval < gateIntervalFloor || interval > gateGridInterval {
		return 0, fmt.Errorf("%s=%q: the gate's grid must be a whole number of seconds from %s to %s",
			gateIntervalEnv, value, gateIntervalFloor, gateGridInterval)
	}
	return interval, nil
}

// gridInterval is the reconciler's grid interval.
func (r *ServingDeploymentReconciler) gridInterval() time.Duration {
	if r.gateInterval == 0 {
		return gateGridInterval
	}
	return r.gateInterval
}

// grid is a window's interval grid: boundary n is anchor plus n intervals,
// and interval n runs from boundary n to boundary n+1 (D45).
type grid struct {
	anchor   time.Time
	interval time.Duration
}

func (g grid) boundary(n int32) time.Time {
	return g.anchor.Add(time.Duration(n) * g.interval)
}

// latest is the index of the latest boundary at or before now, -1 before the
// grid's anchor.
func (g grid) latest(now time.Time) int32 {
	if now.Before(g.anchor) {
		return -1
	}
	return int32(now.Sub(g.anchor) / g.interval)
}

// onTime reports whether a read at now is boundary n's: one taken within half
// an interval after it, the nearer of the two boundaries around it. Later
// than that, the boundary is missed, and the intervals it ends and starts are
// forfeited rather than measured off the grid.
func (g grid) onTime(n int32, now time.Time) bool {
	return n >= 0 && now.Sub(g.boundary(n)) <= g.interval/2
}

// owns reports whether st is this grid's evidence.
func (g grid) owns(st *servingv1alpha1.GateStatus) bool {
	return st != nil && st.WindowStart.Time.Equal(g.anchor) && st.IntervalSeconds == seconds(g.interval)
}

func seconds(d time.Duration) int32 {
	return int32(d / time.Second)
}

// gateBaseline is what one boundary's read leaves the interval that starts
// there. The reads live in memory alone, deliberately unpersisted: an
// operator restart loses them, and forfeits the interval that needed them
// (D45).
type gateBaseline struct {
	uid         types.UID
	windowStart time.Time
	boundary    int32
	samples     gate.Samples
}

// gateBaselines holds each ServingDeployment's last boundary read.
type gateBaselines struct {
	mu     sync.Mutex
	byName map[types.NamespacedName]gateBaseline
}

func (b *gateBaselines) get(key types.NamespacedName) (gateBaseline, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	baseline, ok := b.byName[key]
	return baseline, ok
}

func (b *gateBaselines) set(key types.NamespacedName, baseline gateBaseline) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byName == nil {
		b.byName = map[types.NamespacedName]gateBaseline{}
	}
	b.byName[key] = baseline
}

func (b *gateBaselines) forget(key types.NamespacedName) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.byName, key)
}

// gateEvent is an Event the gate records once the status that carries its
// transition is written.
type gateEvent struct {
	eventType, reason, message string
}

// gateStep is one reconcile's work for the gate, held back until the status
// that records it is written: the baseline is kept and the Events recorded
// only then, so a failed status write is retried from the same state.
type gateStep struct {
	// next is the next boundary to read, zero when the gate is not running.
	next time.Time
	// baseline is the read the interval starting at the boundary just read
	// takes its change from, nil when no boundary was read.
	baseline *gateBaseline
	// forget drops the kept baseline: the gate is not running, or the
	// boundary just handled was missed and read nothing.
	forget bool
	events []gateEvent
}

// errNoScraper is a reconciler built without SetupWithManager meeting a gate.
var errNoScraper = errors.New("the gate has no boundary reader; SetupWithManager wires the operator's")

// observe runs the gate for one reconcile of sd, whose window is open or not,
// at now, after setConditions has written CanaryActive: its
// lastTransitionTime anchors the grid. With no spec.gate, or no open window,
// it does nothing at all. Otherwise it reads only at a boundary: the first
// boundary read on time starts the window's evidence, and each later one
// records the interval that ends there. A boundary passed without a read on
// time is missed, and the interval it ends is forfeited, as is the one it
// starts, which has no baseline; so is the interval whose baseline an operator
// restart lost. Between boundaries nothing is read and status.gate is left
// exactly as it is.
func (r *ServingDeploymentReconciler) observe(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, open bool, now time.Time,
) (gateStep, error) {
	if sd.Spec.Gate == nil || !open {
		return gateStep{forget: true}, nil
	}
	if r.scraper == nil {
		return gateStep{forget: true}, errNoScraper
	}
	active := meta.FindStatusCondition(sd.Status.Conditions, servingv1alpha1.ConditionCanaryActive)
	if active == nil {
		return gateStep{forget: true}, nil
	}
	g := grid{anchor: active.LastTransitionTime.Time, interval: r.gridInterval()}
	k := g.latest(now)
	onTime := g.onTime(k, now)
	st := sd.Status.Gate

	if !g.owns(st) {
		step := gateStep{next: g.boundary(k + 1)}
		if !onTime {
			// The window's evidence starts at the first boundary read on
			// time; the intervals before it were never the gate's.
			return step, nil
		}
		b := r.scraper.ReadBoundary(ctx, sd.Namespace)
		st = &servingv1alpha1.GateStatus{
			WindowStart:     metav1.NewTime(g.anchor),
			IntervalSeconds: seconds(g.interval),
			NextBoundary:    k + 1,
		}
		step.events = armGate(sd, st, g, true)
		sd.Status.Gate = st
		step.baseline = &gateBaseline{uid: sd.UID, windowStart: g.anchor, boundary: k, samples: b.Baseline}
		logf.FromContext(ctx).Info("The gate read its first boundary", "boundary", k,
			"stableReadError", errText(b.StableErr), "canaryReadError", errText(b.CanaryErr))
		return step, nil
	}
	step := gateStep{next: g.boundary(max(k+1, st.NextBoundary))}
	if k < st.NextBoundary {
		return step, nil
	}

	step.events = armGate(sd, st, g, false)
	kept, haveKept := r.baselines.get(gateKey(sd))
	session := sessionOf(st)
	for j := st.NextBoundary; j <= k; j++ {
		iv := gate.Interval{Start: g.boundary(j - 1), End: g.boundary(j)}
		read := j == k && onTime
		if read {
			b := r.scraper.ReadBoundary(ctx, sd.Namespace)
			iv.Current, iv.CanaryReadyAt, iv.CanaryOOMKilled = b.Current, b.CanaryReadyAt, b.CanaryOOMKilled
			step.baseline = &gateBaseline{uid: sd.UID, windowStart: g.anchor, boundary: j, samples: b.Baseline}
			if b.StableErr != nil || b.CanaryErr != nil {
				logf.FromContext(ctx).Info("A gate read failed at a boundary", "boundary", j,
					"stableReadError", errText(b.StableErr), "canaryReadError", errText(b.CanaryErr))
			}
		}
		if read && haveKept && kept.uid == sd.UID && kept.windowStart.Equal(g.anchor) && kept.boundary == j-1 {
			iv.Baseline = kept.samples
		} else {
			// Missed at its end, or its baseline lost to a restart or a
			// missed start: forfeited (D45).
			iv.Interrupted = true
		}
		rec := session.Evaluate(iv)
		var d gate.Decision
		session, d = session.Fold(rec)
		step.events = append(step.events, recordInterval(st, j-1, rec, d)...)
		logf.FromContext(ctx).Info("The gate recorded an interval", "interval", j-1,
			"verdict", rec.Verdict, "reasons", rec.Reasons)
	}
	st.NextBoundary = k + 1
	storeSession(st, session)
	step.next = g.boundary(k + 1)
	step.forget = step.baseline == nil
	return step, nil
}

// finishGate does what step held back, once the status recording it is
// written: it keeps the boundary's read for the next interval and records the
// transitions' Events.
func (r *ServingDeploymentReconciler) finishGate(sd *servingv1alpha1.ServingDeployment, step gateStep) {
	switch {
	case step.baseline != nil:
		r.baselines.set(gateKey(sd), *step.baseline)
	case step.forget:
		r.baselines.forget(gateKey(sd))
	}
	for _, e := range step.events {
		r.Recorder.Event(sd, e.eventType, e.reason, e.message)
	}
}

// gateKey is sd's key for the kept baselines.
func gateKey(sd *servingv1alpha1.ServingDeployment) types.NamespacedName {
	return types.NamespacedName{Namespace: sd.Namespace, Name: sd.Name}
}

// gateArming decides how the gate arms spec.gate.mode for the canary the spec
// runs. Observe is always armed. An acting mode is armed only for the canary
// tag it names (D42), and armed, it does not act yet in this version.
func gateArming(spec servingv1alpha1.ServingDeploymentSpec) servingv1alpha1.GateArming {
	switch {
	case spec.Gate.Mode == servingv1alpha1.GateModeObserve:
		return servingv1alpha1.GateArmingArmed
	case spec.Gate.ForCanaryImageTag != spec.CanaryImageTag:
		return servingv1alpha1.GateArmingTagMismatch
	default:
		return servingv1alpha1.GateArmingNotYetActive
	}
}

// armGate writes the gate's mode and arming for sd's spec on st, and returns
// the Event that says so when they are new to the window or have changed.
// Whatever the arming, the gate runs as Observe in this version: the acting
// modes' behavior lands with their own slices.
func armGate(sd *servingv1alpha1.ServingDeployment, st *servingv1alpha1.GateStatus, g grid, fresh bool) []gateEvent {
	mode, arming := servingv1alpha1.GateModeObserve, gateArming(sd.Spec)
	if !fresh && st.Mode == mode && st.Arming == arming {
		return nil
	}
	st.Mode, st.Arming = mode, arming
	requested := sd.Spec.Gate.Mode
	switch arming {
	case servingv1alpha1.GateArmingTagMismatch:
		return []gateEvent{{corev1.EventTypeWarning, eventReasonGateDegraded, fmt.Sprintf(
			"spec.gate.mode %s is armed for canary tag %q, not spec.canaryImageTag %s: the gate runs as %s, "+
				"records every interval in status.gate and acts on nothing",
			requested, sd.Spec.Gate.ForCanaryImageTag, sd.Spec.CanaryImageTag, mode)}}
	case servingv1alpha1.GateArmingNotYetActive:
		return []gateEvent{{corev1.EventTypeWarning, eventReasonGateDegraded, fmt.Sprintf(
			"spec.gate.mode %s does not act in this version of the operator: the gate runs as %s for canary tag %s, "+
				"records every interval in status.gate and acts on nothing",
			requested, mode, sd.Spec.CanaryImageTag)}}
	}
	return []gateEvent{{corev1.EventTypeNormal, eventReasonGateArmed, fmt.Sprintf(
		"the gate is armed in %s for canary tag %s: every %s interval from %s is recorded in status.gate, "+
			"and the gate acts on nothing",
		mode, sd.Spec.CanaryImageTag, g.interval, g.anchor.UTC().Format(time.RFC3339))}}
}

// recordInterval appends interval index's record to st's ring, counts it,
// latches the first verdict of each kind, and returns the Events of the
// transitions it made: the window's first breach, and each first verdict.
func recordInterval(st *servingv1alpha1.GateStatus, index int32, rec gate.Record, d gate.Decision) []gateEvent {
	var events []gateEvent
	st.Records = append(st.Records, gateRecord(index, rec))
	if n := len(st.Records); n > gateRingSize {
		st.Records = slices.Clone(st.Records[n-gateRingSize:])
	}
	if rec.Verdict == gate.VerdictBreach && st.Totals.Breach == 0 {
		events = append(events, gateEvent{corev1.EventTypeWarning, eventReasonGateBreach, fmt.Sprintf(
			"interval %d (%s to %s) is the window's first breach: %s",
			index, stamp(rec.Start), stamp(rec.End), joinReasons(rec.Reasons))})
	}
	countVerdict(&st.Totals, rec.Verdict)
	st.LastVerdict = servingv1alpha1.GateVerdict(rec.Verdict)
	at := metav1.NewTime(rec.End)
	if d.Rollback != "" && st.RollbackVerdict == nil {
		st.RollbackVerdict = &servingv1alpha1.GateRollbackVerdict{
			Interval: index, At: at, Trip: servingv1alpha1.GateTrip(d.Rollback),
		}
		events = append(events, gateEvent{corev1.EventTypeWarning, eventReasonGateWouldRollBack, fmt.Sprintf(
			"interval %d tripped %s: an acting gate would close the window here; in %s nothing is done, "+
				"and the window stays open until its TTL or a close",
			index, d.Rollback, st.Mode)})
	}
	if d.Promote && st.PromoteVerdict == nil {
		st.PromoteVerdict = &servingv1alpha1.GatePromoteVerdict{Interval: index, At: at}
		events = append(events, gateEvent{corev1.EventTypeNormal, eventReasonGateWouldPromote, fmt.Sprintf(
			"interval %d completed %d consecutive clean intervals, the promote verdict: in %s it is recorded "+
				"and nothing is dispatched",
			index, gate.PromoteAfter, st.Mode)})
	}
	return events
}

func countVerdict(t *servingv1alpha1.GateTotals, v gate.Verdict) {
	switch v {
	case gate.VerdictPending:
		t.Pending++
	case gate.VerdictWarmup:
		t.Warmup++
	case gate.VerdictClean:
		t.Clean++
	case gate.VerdictBreach:
		t.Breach++
	case gate.VerdictUnevaluable:
		t.Unevaluable++
	}
}

// gateRecord is rec as status.gate's ring keeps it, interval index on the
// grid. Its numbers are written as the decimals that read back as exactly the
// numbers the engine decided on, and a p95 or ratio that is not a number is
// left out.
func gateRecord(index int32, rec gate.Record) servingv1alpha1.GateRecord {
	out := servingv1alpha1.GateRecord{
		Index:   index,
		Start:   metav1.NewTime(rec.Start),
		End:     metav1.NewTime(rec.End),
		Stable:  podDelta(rec.Stable),
		Canary:  podDelta(rec.Canary),
		Ratio:   decimal(rec.Ratio),
		Verdict: servingv1alpha1.GateVerdict(rec.Verdict),
	}
	for _, reason := range rec.Reasons {
		out.Reasons = append(out.Reasons, servingv1alpha1.GateReason(reason))
	}
	return out
}

func podDelta(d *gate.Delta) *servingv1alpha1.GatePodDelta {
	if d == nil {
		return nil
	}
	return &servingv1alpha1.GatePodDelta{Requests: d.Requests, Errors: d.Errors, P95Seconds: decimal(d.P95Seconds)}
}

// decimal is v as the shortest decimal that parses back to it, empty when v
// is not a finite number.
func decimal(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// sessionOf is the engine's session as st keeps it: everything the engine
// carries from one interval to the next, so a restarted operator resumes it.
func sessionOf(st *servingv1alpha1.GateStatus) gate.Session {
	s := gate.Session{
		BreachRun:       int(st.BreachRun),
		CleanRun:        int(st.CleanRun),
		CanaryReadySeen: st.CanaryReadySeen,
	}
	if st.CanaryPod != nil {
		s.CanaryPod = gate.Pod{Name: st.CanaryPod.Name, UID: st.CanaryPod.UID}
	}
	return s
}

// storeSession writes s on st.
func storeSession(st *servingv1alpha1.GateStatus, s gate.Session) {
	st.BreachRun, st.CleanRun = int32(s.BreachRun), int32(s.CleanRun)
	st.CanaryReadySeen = s.CanaryReadySeen
	st.CanaryPod = nil
	if s.CanaryPod != (gate.Pod{}) {
		st.CanaryPod = &servingv1alpha1.GatePod{Name: s.CanaryPod.Name, UID: s.CanaryPod.UID}
	}
}

func joinReasons(reasons []gate.Reason) string {
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, string(reason))
	}
	return strings.Join(parts, ", ")
}

func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
