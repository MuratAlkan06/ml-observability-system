// Package gate is operator v1's decision engine (docs/PLAN.md D44, D45): the
// O4 abort table of docs/RUNBOOK.md ("During the live canary window: close
// it") transcribed as pure functions over what the operator reads at interval
// boundaries. deploy/k3s/watch-window.sh is the measurement it transcribes,
// line for line: both pods read the same way, the change in their counters
// since the last boundary, the p95 by histogram_quantile's interpolation over
// the bucket changes, the 50-request floor and the 1.25x same-interval ratio.
//
// Nothing here reads a clock, the cluster or the network: time enters as data,
// the interval's boundaries and the canary's Ready time. Reading the pods is
// the controller's (D41), and so is acting on a Decision (D42, D47).
package gate

import (
	"math"
	"slices"
	"time"
)

// The runbook's numbers, as watch-window.sh holds them. They are not settings.
const (
	// RequestFloor is the fewest /predict requests each pod, the stable and the
	// canary both, serves in an interval for line 3 to evaluate it.
	RequestFloor = 50
	// LatencyRatioLimit is line 3's margin: a canary p95 above this multiple of
	// the stable's p95 over the same interval is a breach. The comparison is
	// strict and unrounded.
	LatencyRatioLimit = 1.25
	// Warmup is line 2's exclusion: an interval that starts less than this
	// after the canary pod's latest Ready transition is warmup, so a restarted
	// canary pod gets a warmup of its own (D45).
	Warmup = 60 * time.Second
	// TripRun is line 3's two in a row, latency breaches and unevaluable
	// intervals counted alike.
	TripRun = 2
)

// PromoteAfter is N, the consecutive clean intervals that make the promote
// verdict (D44): Gate 4 passed live on 11 (#68). It is compiled, not a
// setting, and changing it requires a new D-number.
const PromoteAfter = 11

// Verdict classifies one interval. Breach and Unevaluable count toward line
// 3's two in a row, and Clean toward the clean run. Warmup resets line 3's
// counter, as Clean does, and adds nothing to the clean run. Pending counts
// toward neither (D44, D45).
type Verdict string

const (
	// VerdictPending is an interval that ended before any canary pod had been
	// Ready.
	VerdictPending Verdict = "Pending"
	// VerdictWarmup is an interval that started less than Warmup after the
	// canary pod's latest Ready transition, and that line 3 judged ok.
	VerdictWarmup Verdict = "Warmup"
	// VerdictClean is an evaluable interval past warmup that tripped nothing.
	VerdictClean Verdict = "Clean"
	// VerdictBreach is a line 3 latency breach, a canary 5xx past warmup, or a
	// canary OOMKilled.
	VerdictBreach Verdict = "Breach"
	// VerdictUnevaluable is an interval line 3 could not evaluate: a breach
	// toward its two in a row all the same (D45).
	VerdictUnevaluable Verdict = "Unevaluable"
)

// Reason is one entry of an interval record's fixed vocabulary (D42). The
// unevaluable causes are watch-window.sh's, one per pod where the script names
// the pod, plus OperatorInterrupted (D45).
type Reason string

const (
	// ReasonCanaryNotReady: no canary pod has been Ready yet (Pending).
	ReasonCanaryNotReady Reason = "CanaryNotReady"
	// ReasonCanaryWarmup: inside the canary's warmup, its 5xx not counted.
	ReasonCanaryWarmup Reason = "CanaryWarmup"

	// ReasonCanaryOOMKilled: a canary container OOMKilled, line 5's canary
	// regime and the one line 5 signal a pod shows the operator (D46). It trips
	// at once.
	ReasonCanaryOOMKilled Reason = "CanaryOOMKilled"
	// ReasonCanaryErrors: canary 5xx in an interval past warmup, line 2. It
	// trips at once.
	ReasonCanaryErrors Reason = "CanaryErrors"
	// ReasonLatencyRatio: the canary's p95 above LatencyRatioLimit times the
	// stable's, line 3.
	ReasonLatencyRatio Reason = "LatencyRatio"

	// ReasonOperatorInterrupted: an operator restart lost the interval's
	// baseline, so the interval is forfeited (D45).
	ReasonOperatorInterrupted Reason = "OperatorInterrupted"
	// ReasonStableReadFailed and ReasonCanaryReadFailed: the pod's read at the
	// interval's end failed, or its change could not be computed.
	ReasonStableReadFailed Reason = "StableReadFailed"
	ReasonCanaryReadFailed Reason = "CanaryReadFailed"
	// ReasonStableNoBaseline and ReasonCanaryNoBaseline: no read of the pod at
	// the interval's start to take the change from.
	ReasonStableNoBaseline Reason = "StableNoBaseline"
	ReasonCanaryNoBaseline Reason = "CanaryNoBaseline"
	// ReasonStablePodChanged and ReasonCanaryPodChanged: the pod read at the
	// end is not the pod read at the start.
	ReasonStablePodChanged Reason = "StablePodChanged"
	ReasonCanaryPodChanged Reason = "CanaryPodChanged"
	// ReasonStableCounterReset and ReasonCanaryCounterReset: a cumulative
	// counter went down between the two reads.
	ReasonStableCounterReset Reason = "StableCounterReset"
	ReasonCanaryCounterReset Reason = "CanaryCounterReset"
	// ReasonStableBelowFloor and ReasonCanaryBelowFloor: the pod served fewer
	// than RequestFloor /predict requests in the interval.
	ReasonStableBelowFloor Reason = "StableBelowFloor"
	ReasonCanaryBelowFloor Reason = "CanaryBelowFloor"
	// ReasonStableNoP95 and ReasonCanaryNoP95: the pod's bucket changes give no
	// p95.
	ReasonStableNoP95 Reason = "StableNoP95"
	ReasonCanaryNoP95 Reason = "CanaryNoP95"
	// ReasonNoRatio: both p95s read, but the stable's is zero.
	ReasonNoRatio Reason = "NoRatio"
)

// unevaluable reports whether r leaves line 3 unevaluated.
func (r Reason) unevaluable() bool {
	switch r {
	case ReasonOperatorInterrupted,
		ReasonStableReadFailed, ReasonCanaryReadFailed,
		ReasonStableNoBaseline, ReasonCanaryNoBaseline,
		ReasonStablePodChanged, ReasonCanaryPodChanged,
		ReasonStableCounterReset, ReasonCanaryCounterReset,
		ReasonStableBelowFloor, ReasonCanaryBelowFloor,
		ReasonStableNoP95, ReasonCanaryNoP95,
		ReasonNoRatio:
		return true
	}
	return false
}

// line3 reports whether r counts toward line 3's two in a row.
func (r Reason) line3() bool {
	return r == ReasonLatencyRatio || r.unevaluable()
}

// Pod identifies a pod across reads. watch-window.sh compares names; the UID
// also tells apart a pod recreated under the same name.
type Pod struct {
	Name string
	UID  string
}

// Bucket is one bucket of mlobs_http_request_duration_seconds with
// endpoint="/predict".
type Bucket struct {
	// UpperBoundSeconds is the bucket's le, math.Inf(1) for "+Inf".
	UpperBoundSeconds float64
	// Count is the cumulative count of requests at or under the bound.
	Count float64
}

// Sample is one pod's /metrics read at one boundary: cumulative values since
// its process started.
type Sample struct {
	Pod Pod
	// Requests is mlobs_http_requests_total with endpoint="/predict", summed
	// over its status values.
	Requests float64
	// Errors is the 5xx part of Requests.
	Errors float64
	// Buckets are the /predict duration buckets, one per upper bound, in any
	// order.
	Buckets []Bucket
}

// Samples is one boundary's read of the two pods. A nil Sample is a read that
// failed: no single Running pod, a /metrics that did not answer in time, or a
// pod list that could not be read.
type Samples struct {
	Stable *Sample
	Canary *Sample
}

// Interval is one evaluation interval, as the controller hands it over: its
// two boundaries on the grid, which anchors to CanaryActive's
// lastTransitionTime (D45), and what was read at each.
type Interval struct {
	Start time.Time
	End   time.Time
	// Baseline is what was read at Start, the previous boundary.
	Baseline Samples
	// Current is what was read at End.
	Current Samples
	// CanaryReadyAt is the latest Ready transition among the canary pods Ready
	// at End, nil when none is: watch-window.sh's canary_ready.
	CanaryReadyAt *time.Time
	// CanaryOOMKilled is whether a canary container was OOMKilled, as read at
	// End.
	CanaryOOMKilled bool
	// Interrupted marks an interval whose baseline an operator restart lost:
	// the snapshots live in memory, deliberately unpersisted (D45).
	Interrupted bool
}

// Delta is one pod's change over an interval: watch-window.sh's pod_delta.
type Delta struct {
	// Requests is the /predict requests the pod served in the interval.
	Requests int64
	// Errors is the 5xx among them.
	Errors int64
	// P95Seconds is histogram_quantile(0.95) over the bucket changes, NaN when
	// they give none, as the script prints nan.
	P95Seconds float64
}

// Record is one interval's evidence and verdict, the content of a
// status.gate ring slot (D42).
type Record struct {
	Start time.Time
	End   time.Time
	// Stable and Canary are each pod's change, nil where none was taken.
	Stable *Delta
	Canary *Delta
	// Ratio is the canary's p95 over the stable's, NaN when there is none.
	Ratio   float64
	Verdict Verdict
	// Reasons explain the verdict, in the order watch-window.sh prints its
	// lines: line 5's canary OOM, then line 2, then line 3.
	Reasons []Reason
	// CanaryReadyAt is the warmup's anchor, as read at End.
	CanaryReadyAt *time.Time
	// CanaryPod is the canary pod read at End, nil when that read failed.
	CanaryPod *Pod
}

// Session is what a window carries from one interval to the next. None of it
// is in-memory only: the controller keeps it in status, so it resumes across
// an operator restart (D45).
type Session struct {
	// BreachRun is line 3's counter: consecutive intervals with a latency
	// breach or unevaluable.
	BreachRun int
	// CleanRun is the consecutive clean intervals (D44).
	CleanRun int
	// CanaryReadySeen is whether a canary pod has been Ready at a boundary of
	// the window. Until one has, intervals are pending (D45).
	CanaryReadySeen bool
	// CanaryPod is the canary pod last read, the zero Pod before any.
	CanaryPod Pod
}

// Trip names the line of the table that tripped.
type Trip string

const (
	// TripCanaryOOMKilled is line 5's canary regime: an OOM kill closes the
	// window at once.
	TripCanaryOOMKilled Trip = "Line5CanaryOOMKilled"
	// TripCanaryErrors is line 2: a canary 5xx past warmup closes it at once.
	TripCanaryErrors Trip = "Line2CanaryErrors"
	// TripCanaryLatency is line 3: two intervals in a row.
	TripCanaryLatency Trip = "Line3CanaryLatency"
)

// Decision is what one folded interval asks of the controller. Whether it is
// acted on is the gate mode's question, not this package's (D42, D47).
type Decision struct {
	// Rollback names the line that tripped, empty when none did.
	Rollback Trip
	// Promote is the promote verdict: a clean interval with CleanRun at
	// PromoteAfter or beyond. It recurs on each later clean interval; the
	// controller latches the first (D43, D47).
	Promote bool
}

// Evaluate classifies one interval of the window s has folded so far. It
// transcribes watch-window.sh's classification in the script's order, and
// follows D45 where it rules on forfeiture. Warmup is the script's, by the
// owner ruling of 2026-10-12, where D44's text has warmup count toward
// nothing:
//
//   - A canary OOMKilled is a breach in any interval, pending and warmup
//     included: the script reads line 5 before it looks for a Ready canary.
//   - Pending: the interval ends before any canary pod has been Ready, and
//     nothing else is judged (D45).
//   - Line 2: in warmup, an interval that starts less than Warmup after the
//     canary pod's latest Ready transition, the canary's 5xx are not counted;
//     with no canary pod Ready at End, nothing is excluded. Past warmup, any
//     canary 5xx trips at once. Warmup excludes the 5xx and nothing else.
//   - Line 3, in warmup and past it alike: a read that failed, no baseline, a
//     changed pod, a counter that went down, a pod under RequestFloor or no
//     p95 leaves the interval unevaluable; else the ratio above
//     LatencyRatioLimit is a breach. Baselines seed from the first read, so a
//     warmup interval with one is judged in full, and one without is the
//     script's no earlier read, unevaluable.
//   - An interval whose baseline an operator restart lost cannot be judged on
//     line 3, warmup or not: it is unevaluable, OperatorInterrupted (D45).
func (s Session) Evaluate(iv Interval) Record {
	rec := Record{Start: iv.Start, End: iv.End, CanaryReadyAt: iv.CanaryReadyAt}
	if iv.Current.Canary != nil {
		pod := iv.Current.Canary.Pod
		rec.CanaryPod = &pod
	}
	baseline := iv.Baseline
	if iv.Interrupted {
		baseline = Samples{}
	}
	var stableCause, canaryCause Reason
	rec.Stable, stableCause = delta(baseline.Stable, iv.Current.Stable, stableCauses)
	rec.Canary, canaryCause = delta(baseline.Canary, iv.Current.Canary, canaryCauses)
	rec.Ratio = ratio(rec.Stable, rec.Canary)

	if iv.CanaryOOMKilled {
		rec.Reasons = append(rec.Reasons, ReasonCanaryOOMKilled)
	}
	if !s.CanaryReadySeen && !readyBy(iv.CanaryReadyAt, iv.End) {
		rec.Reasons = append(rec.Reasons, ReasonCanaryNotReady)
		rec.Verdict = verdictOf(rec.Reasons)
		return rec
	}
	switch {
	case iv.CanaryReadyAt != nil && iv.Start.Before(iv.CanaryReadyAt.Add(Warmup)):
		rec.Reasons = append(rec.Reasons, ReasonCanaryWarmup)
	case rec.Canary != nil && rec.Canary.Errors > 0:
		rec.Reasons = append(rec.Reasons, ReasonCanaryErrors)
	}
	if iv.Interrupted {
		rec.Reasons = append(rec.Reasons, ReasonOperatorInterrupted)
	} else {
		rec.Reasons = append(rec.Reasons, latency(rec, stableCause, canaryCause)...)
	}
	rec.Verdict = verdictOf(rec.Reasons)
	return rec
}

// Fold folds one interval's record into the session and returns what it asks
// of the controller. Line 3's counter counts a latency breach and an
// unevaluable interval alike, warmup or not, and is reset by any interval
// line 3 judged ok, warmup included, as the script's two in a row is. The
// clean run counts clean intervals, which are past warmup, and is reset by
// any breach or unevaluable one; a warmup interval judged ok leaves it alone
// (D44), and a pending interval leaves both counters alone. A canary restart,
// seen as a new canary pod, also resets the clean run: a new process restarts
// the evidence (D44). Line 5's canary OOM and line 2 trip at once, line 3 at
// TripRun in a row, and the promote verdict comes at PromoteAfter.
func (s Session) Fold(rec Record) (Session, Decision) {
	next := s
	if rec.CanaryPod != nil {
		if s.CanaryPod != (Pod{}) && *rec.CanaryPod != s.CanaryPod {
			next.CleanRun = 0
		}
		next.CanaryPod = *rec.CanaryPod
	}
	if readyBy(rec.CanaryReadyAt, rec.End) {
		next.CanaryReadySeen = true
	}

	breach := slices.ContainsFunc(rec.Reasons, Reason.line3)
	switch {
	case breach:
		next.BreachRun++
	case !slices.Contains(rec.Reasons, ReasonCanaryNotReady):
		next.BreachRun = 0
	}
	switch rec.Verdict {
	case VerdictClean:
		next.CleanRun++
	case VerdictBreach, VerdictUnevaluable:
		next.CleanRun = 0
	}

	var d Decision
	switch {
	case slices.Contains(rec.Reasons, ReasonCanaryOOMKilled):
		d.Rollback = TripCanaryOOMKilled
	case slices.Contains(rec.Reasons, ReasonCanaryErrors):
		d.Rollback = TripCanaryErrors
	case breach && next.BreachRun >= TripRun:
		d.Rollback = TripCanaryLatency
	}
	d.Promote = rec.Verdict == VerdictClean && next.CleanRun >= PromoteAfter
	return next, d
}

// readyBy reports whether a canary pod was Ready at t. watch-window.sh reads
// the Ready time at the boundary, so a transition after t is not yet seen.
func readyBy(readyAt *time.Time, t time.Time) bool {
	return readyAt != nil && !readyAt.After(t)
}

// latency is line 3's reasons for an interval, warmup or not, in
// watch-window.sh's order: the reads, the floors, the p95s, and with none of
// them, the ratio.
func latency(rec Record, causes ...Reason) []Reason {
	var reasons []Reason
	for _, c := range causes {
		if c != "" {
			reasons = append(reasons, c)
		}
	}
	if rec.Stable != nil && rec.Stable.Requests < RequestFloor {
		reasons = append(reasons, ReasonStableBelowFloor)
	}
	if rec.Canary != nil && rec.Canary.Requests < RequestFloor {
		reasons = append(reasons, ReasonCanaryBelowFloor)
	}
	if rec.Stable != nil && !isNumber(rec.Stable.P95Seconds) {
		reasons = append(reasons, ReasonStableNoP95)
	}
	if rec.Canary != nil && !isNumber(rec.Canary.P95Seconds) {
		reasons = append(reasons, ReasonCanaryNoP95)
	}
	switch {
	case len(reasons) > 0:
		return reasons
	case math.IsNaN(rec.Ratio):
		return []Reason{ReasonNoRatio}
	case rec.Ratio > LatencyRatioLimit:
		return []Reason{ReasonLatencyRatio}
	}
	return nil
}

// verdictOf summarizes an interval's reasons. An immediate trip is a breach
// in any interval; otherwise pending, then an unevaluable cause, then the line
// 3 ratio, and only an interval line 3 judged ok is warmup or clean.
func verdictOf(reasons []Reason) Verdict {
	switch {
	case slices.Contains(reasons, ReasonCanaryOOMKilled), slices.Contains(reasons, ReasonCanaryErrors):
		return VerdictBreach
	case slices.Contains(reasons, ReasonCanaryNotReady):
		return VerdictPending
	case slices.ContainsFunc(reasons, Reason.unevaluable):
		return VerdictUnevaluable
	case slices.Contains(reasons, ReasonLatencyRatio):
		return VerdictBreach
	case slices.Contains(reasons, ReasonCanaryWarmup):
		return VerdictWarmup
	}
	return VerdictClean
}

// ratio is the canary's p95 over the stable's: NaN unless both are numbers
// and the stable's is above zero, as watch-window.sh computes it.
func ratio(stable, canary *Delta) float64 {
	if stable == nil || canary == nil || !isNumber(stable.P95Seconds) || !isNumber(canary.P95Seconds) ||
		stable.P95Seconds <= 0 {
		return math.NaN()
	}
	return canary.P95Seconds / stable.P95Seconds
}

// isNumber is watch-window.sh's is_number for a p95: a finite value, zero or
// above.
func isNumber(v float64) bool {
	return finite(v) && v >= 0
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
