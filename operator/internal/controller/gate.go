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

// The gate's wiring (docs/PLAN.md D42, D45, D47): at each boundary of the
// window's interval grid the controller reads the two pods (package scrape),
// hands the interval to the decision engine (package gate), and records what
// it decided in status.gate. What follows from a decision is the window's
// acting level, by the owner's ruling on #119: sampled when the window opens,
// lowered at the next boundary by a lowered or mismatched spec.gate and at once
// by its removal, and never raised before the next window. At Observe a verdict
// is recorded and nothing acts on it. At ActRollback a tripped line rolls the
// window back: the gate latches CanaryActive False with BreachRolledBack, and
// the window's latch restores steady exactly as for an expired window, with no
// spec write (D47); the promote verdict is recorded, and a human actuates it.
// ActPromote still rolls back, and dispatches the promote verdict to the Deploy
// workflow, latched before it is sent and sent at most once (promote.go; D43).
// With no dispatch token to read it is armed TokenAbsent and runs at
// ActRollback, the level it contains (D42).

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
// degraded, the canary tag an acting gate runs for moved mid-window, the
// window's first breach, and the first verdict of each kind — a rollback
// verdict acted on at an acting level, or recorded only; a promote verdict
// recorded below ActPromote, where at ActPromote its dispatch's Events
// announce it (promote.go).
const (
	eventReasonGateArmed            = "GateArmed"
	eventReasonGateDegraded         = "GateDegraded"
	eventReasonGateCanaryTagChanged = "GateCanaryTagChanged"
	eventReasonGateBreach           = "GateBreach"
	eventReasonGateRolledBack       = "GateRolledBack"
	eventReasonGateWouldRollBack    = "GateWouldRollBack"
	eventReasonGateWouldPromote     = "GateWouldPromote"
)

// gateLevels are the acting levels in the order of D42's lattice, each
// containing the ones before it: the modes, as status.gate records the one in
// effect.
var gateLevels = []servingv1alpha1.GateMode{
	servingv1alpha1.GateModeObserve, servingv1alpha1.GateModeActRollback, servingv1alpha1.GateModeActPromote,
}

// lowerLevel is the lower of two acting levels, a value outside the lattice
// counting as Observe.
func lowerLevel(a, b servingv1alpha1.GateMode) servingv1alpha1.GateMode {
	rank := func(m servingv1alpha1.GateMode) int { return max(slices.Index(gateLevels, m), 0) }
	return gateLevels[min(rank(a), rank(b))]
}

// rollsBack reports whether a window at level rolls back on a tripped line:
// ActRollback, and ActPromote, which still acts on rollback breaches (D42).
func rollsBack(level servingv1alpha1.GateMode) bool {
	return level == servingv1alpha1.GateModeActRollback || level == servingv1alpha1.GateModeActPromote
}

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
	// canaryTag is the spec.canaryImageTag the boundary was read under.
	canaryTag string
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
	// rollBack is the act: an interval this boundary recorded tripped a line
	// at an acting level, and the window is to be rolled back (D47).
	rollBack bool
	// dispatch is the promote dispatch this boundary latched, sent only once
	// the status write carrying its latch has landed (D43); nil when there is
	// none.
	dispatch *dispatchPlan
}

// errNoScraper is a reconciler built without SetupWithManager meeting a gate.
var errNoScraper = errors.New("the gate has no boundary reader; SetupWithManager wires the operator's")

// observe runs the gate for one reconcile of sd, whose window is open or not,
// at now, after setConditions has written CanaryActive: its
// lastTransitionTime anchors the grid. opened marks the reconcile that opened
// the window, where its acting level is sampled. With no open window it does
// nothing at all, and with no spec.gate nothing but lower the level of a
// window whose gate was removed (disarm). Otherwise it reads only at a
// boundary: the first boundary read on time starts the window's evidence, and
// each later one records the interval that ends there, at the window's level,
// and asks for the act if one tripped a line at an acting level. A boundary
// passed without a read on time is missed, and the interval it ends is
// forfeited, as is the one it starts, which has no baseline; so is the
// interval whose baseline an operator restart lost. At ActPromote, a boundary
// read with the promote verdict latched takes the verdict's dispatch its next
// step (promote). Between boundaries nothing is read, nothing is dispatched,
// and status.gate is left exactly as it is.
func (r *ServingDeploymentReconciler) observe(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, open, opened bool, now time.Time,
) (gateStep, error) {
	if !open {
		return gateStep{forget: true}, nil
	}
	if sd.Spec.Gate == nil {
		return gateStep{forget: true, events: r.disarm(sd)}, nil
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
		step.events = armGate(sd, st, g, true, opened, r.tokenPresent(sd))
		sd.Status.Gate = st
		step.baseline = &gateBaseline{
			uid: sd.UID, windowStart: g.anchor, boundary: k, samples: b.Baseline, canaryTag: sd.Spec.CanaryImageTag,
		}
		logf.FromContext(ctx).Info("The gate read its first boundary", "boundary", k,
			"stableReadError", errText(b.StableErr), "canaryReadError", errText(b.CanaryErr))
		return step, nil
	}
	step := gateStep{next: g.boundary(max(k+1, st.NextBoundary))}
	if k < st.NextBoundary {
		return step, nil
	}

	kept, haveKept := r.baselines.get(gateKey(sd))
	step.events = armGate(sd, st, g, false, false, r.tokenPresent(sd))
	if step.events == nil {
		step.events = tagMoved(sd, st, g, kept, haveKept)
	}
	level := st.Mode
	session := sessionOf(st)
	// b is this boundary's read, and last the decision on the interval it
	// ended: what the promote arm acts on.
	var (
		b    scrape.Boundary
		last gate.Decision
	)
	for j := st.NextBoundary; j <= k; j++ {
		iv := gate.Interval{Start: g.boundary(j - 1), End: g.boundary(j)}
		read := j == k && onTime
		if read {
			b = r.scraper.ReadBoundary(ctx, sd.Namespace)
			iv.Current, iv.CanaryReadyAt, iv.CanaryOOMKilled = b.Current, b.CanaryReadyAt, b.CanaryOOMKilled
			step.baseline = &gateBaseline{
				uid: sd.UID, windowStart: g.anchor, boundary: j, samples: b.Baseline, canaryTag: sd.Spec.CanaryImageTag,
			}
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
		session, last = session.Fold(rec)
		step.events = append(step.events, recordInterval(st, j-1, rec, last, level)...)
		step.rollBack = step.rollBack || (last.Rollback != "" && rollsBack(level))
		logf.FromContext(ctx).Info("The gate recorded an interval", "interval", j-1,
			"verdict", rec.Verdict, "reasons", rec.Reasons, "level", level)
	}
	st.NextBoundary = k + 1
	storeSession(st, session)
	step.next = g.boundary(k + 1)
	step.forget = step.baseline == nil
	if step.rollBack && st.RollbackVerdict.RolledBackAt == nil {
		// The act's time, stamped as CanaryActive's False transition is.
		st.RollbackVerdict.RolledBackAt = new(metav1.NewTime(now.Truncate(time.Second)))
		logf.FromContext(ctx).Info("The gate tripped a line at an acting level; rolling the window back",
			"interval", st.RollbackVerdict.Interval, "trip", st.RollbackVerdict.Trip, "level", level)
	}
	if level == servingv1alpha1.GateModeActPromote && st.PromoteVerdict != nil && onTime && !step.rollBack {
		deadline := active.LastTransitionTime.Add(canaryWindowTTL)
		step.events = append(step.events, r.promote(ctx, sd, st, b, last.Promote, deadline, &step)...)
	}
	return step, nil
}

// tokenPresent reports whether sd's gate asks for ActPromote and a dispatch
// token can be read for it now. Only ActPromote reads the token's file.
func (r *ServingDeploymentReconciler) tokenPresent(sd *servingv1alpha1.ServingDeployment) bool {
	return sd.Spec.Gate.Mode == servingv1alpha1.GateModeActPromote && r.dispatcher != nil && r.dispatcher.tokenPresent()
}

// disarm lowers the acting level of sd's open window to Observe when its
// spec.gate is removed, a lower that takes effect at once (#119): the gate
// stops reading, and its evidence stays, so a gate set again before the window
// closes is a raise, which waits for the next window. It changes nothing for a
// window the gate never ran in, or already ran at Observe, and returns the
// Event of the lower.
func (r *ServingDeploymentReconciler) disarm(sd *servingv1alpha1.ServingDeployment) []gateEvent {
	st := sd.Status.Gate
	active := meta.FindStatusCondition(sd.Status.Conditions, servingv1alpha1.ConditionCanaryActive)
	if active == nil || st == nil || !rollsBack(st.Mode) {
		return nil
	}
	if !(grid{anchor: active.LastTransitionTime.Time, interval: r.gridInterval()}).owns(st) {
		return nil
	}
	st.Mode = servingv1alpha1.GateModeObserve
	return []gateEvent{{corev1.EventTypeWarning, eventReasonGateDegraded, fmt.Sprintf(
		"spec.gate was removed from the open window: the gate stops at once and acts on nothing, status.gate "+
			"keeps the window's evidence at %s, and a gate set again before the window closes runs as %s",
		st.Mode, st.Mode)}}
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
	r.recordHeld(sd, step.events)
}

// recordHeld records events, held back until the status write that carries
// their transitions landed.
func (r *ServingDeploymentReconciler) recordHeld(sd *servingv1alpha1.ServingDeployment, events []gateEvent) {
	for _, e := range events {
		r.Recorder.Event(sd, e.eventType, e.reason, e.message)
	}
}

// gateKey is sd's key for the kept baselines.
func gateKey(sd *servingv1alpha1.ServingDeployment) types.NamespacedName {
	return types.NamespacedName{Namespace: sd.Namespace, Name: sd.Name}
}

// gateArming decides how the gate arms spec.gate.mode for the canary the spec
// runs, token being whether a dispatch token can be read. Observe is always
// armed. An acting mode is armed only for the canary tag it names (D42);
// armed, ActRollback acts, and so does ActPromote while it has its token,
// without which it degrades to verdict-only, the off-season posture (D43).
func gateArming(spec servingv1alpha1.ServingDeploymentSpec, token bool) servingv1alpha1.GateArming {
	switch {
	case spec.Gate.Mode == servingv1alpha1.GateModeObserve:
		return servingv1alpha1.GateArmingArmed
	case spec.Gate.ForCanaryImageTag != spec.CanaryImageTag:
		return servingv1alpha1.GateArmingTagMismatch
	case spec.Gate.Mode == servingv1alpha1.GateModeActPromote && !token:
		return servingv1alpha1.GateArmingTokenAbsent
	default:
		return servingv1alpha1.GateArmingArmed
	}
}

// armedLevel is the acting level spec.gate asks for, as armed: a mismatched
// tag degrades an acting mode to Observe (D42), and ActPromote with no token
// runs at ActRollback, the level it contains, so its rollback still acts.
func armedLevel(spec servingv1alpha1.ServingDeploymentSpec, arming servingv1alpha1.GateArming) servingv1alpha1.GateMode {
	switch arming {
	case servingv1alpha1.GateArmingTagMismatch:
		return servingv1alpha1.GateModeObserve
	case servingv1alpha1.GateArmingTokenAbsent:
		return servingv1alpha1.GateModeActRollback
	}
	return spec.Gate.Mode
}

// gateDoes says what the gate does at level.
func gateDoes(level servingv1alpha1.GateMode) string {
	switch level {
	case servingv1alpha1.GateModeActPromote:
		return "records every interval in status.gate, rolls the window back on a tripped line, and dispatches " +
			"the promote verdict to " + deployWorkflow + " at most once"
	case servingv1alpha1.GateModeActRollback:
		return "records every interval in status.gate and rolls the window back on a tripped line"
	}
	return "records every interval in status.gate and acts on nothing"
}

// armGate writes on st the gate's arming for sd's spec and the window's acting
// level, and returns the Event that says so when they are new to the window or
// have changed. The level is the owner's ruling on #119: sampled when the
// window opens — fresh evidence started on the reconcile that opened it, while
// evidence started mid-window is for a window that opened with no gate, at
// Observe — and at each later boundary the lower of the level in effect and
// the one armed now, so a lower takes effect at once and a raise only from the
// next window. token is whether a dispatch token can be read now.
func armGate(
	sd *servingv1alpha1.ServingDeployment, st *servingv1alpha1.GateStatus, g grid, fresh, opened, token bool,
) []gateEvent {
	arming := gateArming(sd.Spec, token)
	armed := armedLevel(sd.Spec, arming)
	level := armed
	switch {
	case fresh && !opened:
		level = servingv1alpha1.GateModeObserve
	case !fresh:
		level = lowerLevel(st.Mode, armed)
	}
	if !fresh && st.Mode == level && st.Arming == arming {
		return nil
	}
	st.Mode, st.Arming = level, arming
	requested := sd.Spec.Gate.Mode
	switch {
	case arming == servingv1alpha1.GateArmingTagMismatch:
		return []gateEvent{{corev1.EventTypeWarning, eventReasonGateDegraded, fmt.Sprintf(
			"spec.gate.mode %s is armed for canary tag %q, not spec.canaryImageTag %s: the gate runs as %s, %s",
			requested, sd.Spec.Gate.ForCanaryImageTag, sd.Spec.CanaryImageTag, level, gateDoes(level))}}
	case level != armed:
		return []gateEvent{{corev1.EventTypeWarning, eventReasonGateDegraded, fmt.Sprintf(
			"spec.gate.mode %s is armed for canary tag %s from the next window: a window acts at the level "+
				"sampled when it opened, so the gate runs this one as %s: it %s",
			requested, sd.Spec.CanaryImageTag, level, gateDoes(level))}}
	case arming == servingv1alpha1.GateArmingTokenAbsent:
		return []gateEvent{{corev1.EventTypeWarning, eventReasonGateDegraded, fmt.Sprintf(
			"spec.gate.mode %s has no dispatch token, secret/%s being absent or its token unreadable: the gate "+
				"runs as %s for canary tag %s, %s; the promote verdict is recorded and a human actuates it",
			requested, dispatchTokenSecret, level, sd.Spec.CanaryImageTag, gateDoes(level))}}
	}
	acts := "the gate acts on nothing"
	switch level {
	case servingv1alpha1.GateModeActPromote:
		acts = "a tripped line rolls the window back, and the promote verdict is dispatched to " + deployWorkflow +
			" at most once"
	case servingv1alpha1.GateModeActRollback:
		acts = "a tripped line rolls the window back"
	}
	return []gateEvent{{corev1.EventTypeNormal, eventReasonGateArmed, fmt.Sprintf(
		"the gate is armed in %s for canary tag %s: every %s interval from %s is recorded in status.gate, and %s",
		level, sd.Spec.CanaryImageTag, g.interval, g.anchor.UTC().Format(time.RFC3339), acts)}}
}

// tagMoved returns the Event of a tag pair moved under an acting gate, when
// armGate has none: spec.canaryImageTag and spec.gate.forCanaryImageTag both
// changed since kept, this window's last boundary read, was taken, and the
// arming and the level stayed as they were, so nothing else says so. It
// records what was observed and changes nothing: the level, the arming and the
// evidence are what they would be without it (#60, V1-S7; D37). The tag is
// kept beside the read, in memory alone (D45), so a boundary with no kept read
// of this window — after an operator restart or a missed boundary — has
// nothing to compare, and records none.
func tagMoved(
	sd *servingv1alpha1.ServingDeployment, st *servingv1alpha1.GateStatus, g grid, kept gateBaseline, haveKept bool,
) []gateEvent {
	from, to := kept.canaryTag, sd.Spec.CanaryImageTag
	if !haveKept || kept.uid != sd.UID || !kept.windowStart.Equal(g.anchor) || from == "" || from == to ||
		!rollsBack(st.Mode) {
		return nil
	}
	return []gateEvent{{corev1.EventTypeNormal, eventReasonGateCanaryTagChanged, fmt.Sprintf(
		"spec.canaryImageTag and spec.gate.forCanaryImageTag both moved mid-window, from %s at the gate's last "+
			"boundary to %s at this one: the gate runs on as %s, now for canary tag %s: it %s",
		from, to, st.Mode, to, gateDoes(st.Mode))}}
}

// recordInterval appends interval index's record, evaluated at level, to st's
// ring, counts it, latches the first verdict of each kind, and returns the
// Events of the transitions it made: the window's first breach, and each first
// verdict — a rollback verdict at an acting level as the act.
func recordInterval(
	st *servingv1alpha1.GateStatus, index int32, rec gate.Record, d gate.Decision, level servingv1alpha1.GateMode,
) []gateEvent {
	var events []gateEvent
	st.Records = append(st.Records, gateRecord(index, rec, level))
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
		if rollsBack(level) {
			events = append(events, gateEvent{corev1.EventTypeWarning, eventReasonGateRolledBack, fmt.Sprintf(
				"interval %d tripped %s: in %s the gate rolls the canary window back, CanaryActive False with %s; "+
					"steady is restored with no spec write, and no window opens before the close-window patch "+
					"clears the canary fields%s",
				index, d.Rollback, level, servingv1alpha1.ReasonBreachRolledBack, sentDispatch(st))})
		} else {
			events = append(events, gateEvent{corev1.EventTypeWarning, eventReasonGateWouldRollBack, fmt.Sprintf(
				"interval %d tripped %s: an acting gate would close the window here; in %s nothing is done, "+
					"and the window stays open until its TTL or a close",
				index, d.Rollback, level)})
		}
	}
	if d.Promote && st.PromoteVerdict == nil {
		st.PromoteVerdict = &servingv1alpha1.GatePromoteVerdict{Interval: index, At: at}
		if level != servingv1alpha1.GateModeActPromote {
			events = append(events, gateEvent{corev1.EventTypeNormal, eventReasonGateWouldPromote, fmt.Sprintf(
				"interval %d completed %d consecutive clean intervals, the promote verdict: in %s it is recorded "+
					"and nothing is dispatched",
				index, gate.PromoteAfter, level)})
		}
	}
	return events
}

// sentDispatch is what a rollback's Event adds when a promote dispatch was
// latched before the trip: its run may be pending, and the reviewer's reject
// is the human override (D47).
func sentDispatch(st *servingv1alpha1.GateStatus) string {
	if st.PromoteVerdict == nil || st.PromoteVerdict.Dispatch == nil || st.PromoteVerdict.Dispatch.AttemptedAt == nil {
		return ""
	}
	switch d := st.PromoteVerdict.Dispatch; d.State {
	case servingv1alpha1.GateDispatchAttempted, servingv1alpha1.GateDispatchDispatched:
		return fmt.Sprintf("; the promote dispatch sent at %s may have a pending Deploy run: check Actions and "+
			"reject it", stamp(d.AttemptedAt.Time))
	}
	return ""
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
// grid, evaluated at level. Its numbers are written as the decimals that read
// back as exactly the numbers the engine decided on, and a p95 or ratio that
// is not a number is left out.
func gateRecord(index int32, rec gate.Record, level servingv1alpha1.GateMode) servingv1alpha1.GateRecord {
	out := servingv1alpha1.GateRecord{
		Index:          index,
		Start:          metav1.NewTime(rec.Start),
		End:            metav1.NewTime(rec.End),
		Stable:         podDelta(rec.Stable),
		Canary:         podDelta(rec.Canary),
		Ratio:          decimal(rec.Ratio),
		Verdict:        servingv1alpha1.GateVerdict(rec.Verdict),
		EffectiveLevel: level,
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
