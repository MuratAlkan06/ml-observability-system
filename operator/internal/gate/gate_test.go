package gate

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
)

var (
	// windowOpen is the grid's origin, CanaryActive's lastTransitionTime (D45).
	windowOpen = time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	// canaryReady is the canary pod's Ready transition, 90 s into the window:
	// the first interval starting 60 s or more after it is the fourth.
	canaryReady = windowOpen.Add(90 * time.Second)
	// judgedStart starts an interval well past the canary's warmup.
	judgedStart = windowOpen.Add(10 * time.Minute)
	// restartReady is a restarted canary pod's Ready transition, inside the
	// interval from judgedStart.
	restartReady = judgedStart.Add(40 * time.Second)
	// replacementReady is the Ready transition of a deleted canary pod's
	// replacement, inside the interval after the one from judgedStart.
	replacementReady = judgedStart.Add(100 * time.Second)

	stablePod          = Pod{Name: "api-6f7c9d8b5-q2xkz", UID: "0d6f3b52-1c1e-4a8e-9d7a-2f4b6c8e0a11"}
	restartedStablePod = Pod{Name: "api-6f7c9d8b5-w8ppl", UID: "7e4a2c19-5d3b-4f8e-a1c6-3b5d7f9e1a44"}
	canaryPod          = Pod{Name: "api-canary-7b8d9c6f4-m4tnw", UID: "5c1e8a77-3b2d-4f6a-8c9e-1d3f5a7b9c22"}
	restartedPod       = Pod{Name: "api-canary-7b8d9c6f4-z9hrp", UID: "9a2b4c61-7e8f-4a1b-b2c3-d4e5f6a7b833"}

	// watching is a session whose canary pod has been Ready.
	watching = Session{CanaryReadySeen: true, CanaryPod: canaryPod}

	// bounds are the upper bounds of the /predict duration buckets the
	// samples below carry.
	bounds = []float64{0.1, 0.25, 0.5, 1, math.Inf(1)}
)

// sample is a pod's read: cumulative /predict requests, the 5xx among them,
// and a cumulative count per bound.
func sample(pod Pod, requests, errs float64, cumulative ...float64) *Sample {
	s := &Sample{Pod: pod, Requests: requests, Errors: errs}
	for i, c := range cumulative {
		s.Buckets = append(s.Buckets, Bucket{UpperBoundSeconds: bounds[i], Count: c})
	}
	return s
}

// served is s one interval on: n more /predict requests, errs of them 5xx,
// every one between 0.1 s and 0.25 s, so that the interval's p95 is 0.2425 s
// whatever n is, and two pods served alike have a ratio of 1.
func served(s *Sample, n, errs float64) *Sample {
	next := &Sample{Pod: s.Pod, Requests: s.Requests + n, Errors: s.Errors + errs}
	for _, b := range s.Buckets {
		if b.UpperBoundSeconds > 0.1 {
			b.Count += n
		}
		next.Buckets = append(next.Buckets, b)
	}
	return next
}

// stableRead and canaryRead are the reads at a judged interval's start, of
// pods that have served for a while.
func stableRead() *Sample { return sample(stablePod, 4000, 3, 0, 4000, 4000, 4000, 4000) }
func canaryRead() *Sample { return sample(canaryPod, 1200, 0, 0, 1200, 1200, 1200, 1200) }

func startReads() Samples { return Samples{Stable: stableRead(), Canary: canaryRead()} }

// endReads are the reads at the judged interval's end: stableN and canaryN
// requests served, canaryErrs of the canary's 5xx.
func endReads(stableN, canaryN, canaryErrs float64) Samples {
	return Samples{Stable: served(stableRead(), stableN, 0), Canary: served(canaryRead(), canaryN, canaryErrs)}
}

// interval is the minute from start, base read at its start, cur at its end,
// and the canary's latest Ready transition at readyAt.
func interval(start time.Time, readyAt *time.Time, base, cur Samples) Interval {
	return Interval{Start: start, End: start.Add(time.Minute), Baseline: base, Current: cur, CanaryReadyAt: readyAt}
}

// judged is the minute from judgedStart, well past the canary's warmup.
func judged(base, cur Samples) Interval { return interval(judgedStart, new(canaryReady), base, cur) }

// warming is the minute from 59 s after canaryReady, the last start inside
// the canary's warmup.
func warming(base, cur Samples) Interval {
	return interval(canaryReady.Add(59*time.Second), new(canaryReady), base, cur)
}

// ratioReads are an interval's reads with the stable's p95 at exactly 0.375 s
// and the canary's from k of its 80 requests at 0.25 s or under: k = 48 puts
// the ratio at exactly 1.25, 47 above it and 49 below.
func ratioReads(k float64) (Samples, Samples) {
	return Samples{Stable: sample(stablePod, 520, 0, 0, 500, 520, 520, 520),
			Canary: sample(canaryPod, 1000, 0, 0, 1000, 1000, 1000, 1000)},
		Samples{Stable: sample(stablePod, 600, 0, 0, 572, 600, 600, 600),
			Canary: sample(canaryPod, 1080, 0, 0, 1000+k, 1080, 1080, 1080)}
}

func oomKilled(iv Interval) Interval {
	iv.CanaryOOMKilled = true
	return iv
}

func forfeited(iv Interval) Interval {
	iv.Interrupted = true
	return iv
}

// boundary is one boundary's read, as the controller would hand it over.
type boundary struct {
	reads       Samples
	readyAt     *time.Time
	interrupted bool
}

// walk evaluates and folds, from s, the intervals between consecutive
// boundaries read every 60 s from origin, and returns each interval's record
// and decision and the session after it.
func walk(s Session, origin time.Time, bs ...boundary) ([]Record, []Session, []Decision) {
	n := len(bs) - 1
	recs, sessions, decisions := make([]Record, 0, n), make([]Session, 0, n), make([]Decision, 0, n)
	for k := 1; k < len(bs); k++ {
		iv := interval(origin.Add(time.Duration(k-1)*time.Minute), bs[k].readyAt, bs[k-1].reads, bs[k].reads)
		iv.Interrupted = bs[k].interrupted
		rec := s.Evaluate(iv)
		var d Decision
		s, d = s.Fold(rec)
		recs, sessions, decisions = append(recs, rec), append(sessions, s), append(decisions, d)
	}
	return recs, sessions, decisions
}

func verdicts(recs []Record) []Verdict {
	out := make([]Verdict, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Verdict)
	}
	return out
}

func cleanRuns(sessions []Session) []int {
	out := make([]int, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.CleanRun)
	}
	return out
}

func breachRuns(sessions []Session) []int {
	out := make([]int, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.BreachRun)
	}
	return out
}

// seedingBoundaries are a window's first five boundaries: the stable read at
// the open; pendingRead for the canary a minute in, before it is Ready; the
// canary Ready at canaryReady, 90 s in, with 75 requests served by the next
// boundary and 150 an interval after that.
func seedingBoundaries(pendingRead *Sample) []boundary {
	s0 := stableRead()
	s1 := served(s0, 150, 0)
	s2 := served(s1, 150, 0)
	s3 := served(s2, 150, 0)
	c2 := sample(canaryPod, 75, 0, 0, 75, 75, 75, 75)
	c3 := served(c2, 150, 0)
	return []boundary{
		{reads: Samples{Stable: s0}},
		{reads: Samples{Stable: s1, Canary: pendingRead}},
		{reads: Samples{Stable: s2, Canary: c2}, readyAt: new(canaryReady)},
		{reads: Samples{Stable: s3, Canary: c3}, readyAt: new(canaryReady)},
		{reads: Samples{Stable: served(s3, 150, 0), Canary: served(c3, 150, 0)}, readyAt: new(canaryReady)},
	}
}

// inOrder matches a reason list exactly and in order. A table entry with no
// reasons hands over an empty list, and Evaluate a nil one.
func inOrder(want []Reason) types.GomegaMatcher {
	if len(want) == 0 {
		return BeEmpty()
	}
	return Equal(want)
}

func record(v Verdict, reasons ...Reason) Record {
	return Record{Verdict: v, Reasons: reasons, Ratio: math.NaN()}
}

var _ = Describe("The decision engine: the O4 table transcribed from watch-window.sh (D44, D45)", func() {
	It("compiles the runbook's numbers and N = 11, which changes only with a new D-number (D44)", func() {
		Expect(RequestFloor).To(Equal(50))
		Expect(LatencyRatioLimit).To(Equal(1.25))
		Expect(Warmup).To(Equal(60 * time.Second))
		Expect(TripRun).To(Equal(2))
		Expect(PromoteAfter).To(Equal(11))
	})

	DescribeTable("the 50-request floor holds on each pod: 49 against 50, unevaluable against evaluable",
		func(cur Samples, want Verdict, reasons ...Reason) {
			rec := watching.Evaluate(judged(startReads(), cur))
			Expect(rec.Verdict).To(Equal(want))
			Expect(rec.Reasons).To(inOrder(reasons))
		},
		Entry("both pods at 50: evaluable, and clean", endReads(50, 50, 0), VerdictClean),
		Entry("the stable at 49", endReads(49, 50, 0), VerdictUnevaluable, ReasonStableBelowFloor),
		Entry("the canary at 49", endReads(50, 49, 0), VerdictUnevaluable, ReasonCanaryBelowFloor),
		Entry("both at 49", endReads(49, 49, 0), VerdictUnevaluable, ReasonStableBelowFloor, ReasonCanaryBelowFloor),
		Entry("the stable's own 5xx: line 2 reads the canary alone",
			Samples{Stable: served(stableRead(), 150, 4), Canary: served(canaryRead(), 150, 0)}, VerdictClean),
	)

	// The stable serves 80 requests in the interval, 72 at 0.25 s or under and
	// all 80 at 0.5 s or under: its p95 interpolates to exactly 0.375 s. The
	// canary serves 80 too, k of them at 0.25 s or under. With k = 48 its p95
	// is exactly 0.46875 s, the ratio exactly 1.25; one request either way
	// moves it across. Each canary history is chosen so that the cumulative
	// histogram would land on the other side of 1.25 from the bucket changes.
	DescribeTable("line 3 compares the canary's p95 with 1.25 times the stable's over the same interval, "+
		"each from its bucket changes through histogram_quantile's interpolation",
		func(canaryBase *Sample, k float64, wantRatio float64, cumulativeSide string, want Verdict, reasons ...Reason) {
			stableBase := sample(stablePod, 520, 0, 0, 500, 520, 520, 520)
			stableEnd := sample(stablePod, 600, 0, 0, 572, 600, 600, 600)
			end := canaryBase.Buckets
			canaryEnd := sample(canaryPod, canaryBase.Requests+80, 0,
				0, end[1].Count+k, end[2].Count+80, end[3].Count+80, end[4].Count+80)

			rec := watching.Evaluate(judged(Samples{Stable: stableBase, Canary: canaryBase},
				Samples{Stable: stableEnd, Canary: canaryEnd}))
			Expect(rec.Verdict).To(Equal(want))
			Expect(rec.Reasons).To(inOrder(reasons))
			Expect(rec.Stable.Requests).To(BeEquivalentTo(80))
			Expect(rec.Canary.Requests).To(BeEquivalentTo(80))
			Expect(rec.Stable.P95Seconds).To(Equal(0.375))
			Expect(rec.Ratio).To(BeNumerically("~", wantRatio, 1e-6))
			Expect(p95Seconds(canaryEnd.Buckets)/p95Seconds(stableEnd.Buckets)).
				To(BeNumerically(cumulativeSide, LatencyRatioLimit), "the cumulative histogram's ratio")
		},
		Entry("1.2473 after a slow history: clean",
			sample(canaryPod, 100, 0, 0, 10, 30, 100, 100), 49.0, 1.247312, ">", VerdictClean),
		Entry("exactly 1.25 after a slow history: not above it, clean",
			sample(canaryPod, 100, 0, 0, 10, 30, 100, 100), 48.0, 1.25, ">", VerdictClean),
		Entry("1.2525 after a slow history: a breach",
			sample(canaryPod, 100, 0, 0, 10, 30, 100, 100), 47.0, 1.252525, ">", VerdictBreach, ReasonLatencyRatio),
		Entry("1.2473 after a fast history: clean",
			sample(canaryPod, 1000, 0, 0, 1000, 1000, 1000, 1000), 49.0, 1.247312, "<", VerdictClean),
		Entry("1.2525 after a fast history: a breach",
			sample(canaryPod, 1000, 0, 0, 1000, 1000, 1000, 1000), 47.0, 1.252525, "<", VerdictBreach,
			ReasonLatencyRatio),
	)

	It("compares exactly 1.25 as not above the limit, on every architecture: "+
		"the ratio is unrounded, the comparison strict, and no multiply is fused", func() {
		r := watching.Evaluate(judged(
			Samples{Stable: sample(stablePod, 520, 0, 0, 500, 520, 520, 520),
				Canary: sample(canaryPod, 100, 0, 0, 10, 30, 100, 100)},
			Samples{Stable: sample(stablePod, 600, 0, 0, 572, 600, 600, 600),
				Canary: sample(canaryPod, 180, 0, 0, 58, 110, 180, 180)}))
		Expect(r.Stable.P95Seconds).To(Equal(0.375))
		Expect(r.Canary.P95Seconds).To(Equal(0.46875))
		Expect(r.Ratio).To(Equal(LatencyRatioLimit))
		Expect(r.Verdict).To(Equal(VerdictClean))
	})

	DescribeTable("each unevaluable cause counts toward line 3's two in a row, and two in a row trip it (D45)",
		func(iv Interval, reasons ...Reason) {
			rec := watching.Evaluate(iv)
			Expect(rec.Verdict).To(Equal(VerdictUnevaluable))
			Expect(rec.Reasons).To(inOrder(reasons))

			once, d := watching.Fold(rec)
			Expect(once.BreachRun).To(Equal(1))
			Expect(d).To(Equal(Decision{}))
			twice, d := once.Fold(rec)
			Expect(twice.BreachRun).To(Equal(2))
			Expect(d).To(Equal(Decision{Rollback: TripCanaryLatency}))
		},
		Entry("the stable's read failed",
			judged(startReads(), Samples{Canary: served(canaryRead(), 150, 0)}), ReasonStableReadFailed),
		Entry("the canary's read failed",
			judged(startReads(), Samples{Stable: served(stableRead(), 150, 0)}), ReasonCanaryReadFailed),
		Entry("both reads failed: a metric-source outage",
			judged(startReads(), Samples{}), ReasonStableReadFailed, ReasonCanaryReadFailed),
		Entry("a count that is not a number cannot be computed: a failed read",
			judged(startReads(), Samples{Stable: served(stableRead(), 150, 0),
				Canary: &Sample{Pod: canaryPod, Requests: math.NaN(), Buckets: served(canaryRead(), 150, 0).Buckets}}),
			ReasonCanaryReadFailed),
		Entry("no stable read at the start to take the change from",
			judged(Samples{Canary: canaryRead()}, endReads(150, 150, 0)), ReasonStableNoBaseline),
		Entry("no canary read at the start to take the change from",
			judged(Samples{Stable: stableRead()}, endReads(150, 150, 0)), ReasonCanaryNoBaseline),
		Entry("the stable pod changed between the reads",
			judged(startReads(), Samples{Stable: sample(restartedStablePod, 150, 0, 0, 150, 150, 150, 150),
				Canary: served(canaryRead(), 150, 0)}), ReasonStablePodChanged),
		Entry("the canary pod changed between the reads",
			judged(startReads(), Samples{Stable: served(stableRead(), 150, 0),
				Canary: sample(restartedPod, 150, 0, 0, 150, 150, 150, 150)}), ReasonCanaryPodChanged),
		Entry("the stable's 5xx counter went down",
			judged(startReads(), Samples{Stable: sample(stablePod, 4150, 0, 0, 4150, 4150, 4150, 4150),
				Canary: served(canaryRead(), 150, 0)}), ReasonStableCounterReset),
		Entry("the canary's request counter went down: its process restarted in place",
			judged(startReads(), Samples{Stable: served(stableRead(), 150, 0),
				Canary: sample(canaryPod, 140, 0, 0, 140, 140, 140, 140)}), ReasonCanaryCounterReset),
		Entry("one canary bucket went down",
			judged(startReads(), Samples{Stable: served(stableRead(), 150, 0),
				Canary: sample(canaryPod, 1350, 0, 0, 1100, 1350, 1350, 1350)}), ReasonCanaryCounterReset),
		Entry("the stable under the floor", judged(startReads(), endReads(49, 150, 0)), ReasonStableBelowFloor),
		Entry("the canary under the floor", judged(startReads(), endReads(150, 49, 0)), ReasonCanaryBelowFloor),
		Entry("the stable's buckets have no +Inf: no p95",
			judged(startReads(), Samples{
				Stable: &Sample{Pod: stablePod, Requests: 4150, Errors: 3,
					Buckets: []Bucket{{0.1, 0}, {0.25, 4150}, {0.5, 4150}, {1, 4150}}},
				Canary: served(canaryRead(), 150, 0)}), ReasonStableNoP95),
		Entry("the canary's buckets did not move: no p95",
			judged(startReads(), Samples{Stable: served(stableRead(), 150, 0),
				Canary: sample(canaryPod, 1350, 0, 0, 1200, 1200, 1200, 1200)}), ReasonCanaryNoP95),
		Entry("the stable's p95 is zero: no ratio",
			judged(Samples{Stable: &Sample{Pod: stablePod, Requests: 4000, Buckets: []Bucket{{0, 4000}, {math.Inf(1), 4000}}},
				Canary: canaryRead()},
				Samples{Stable: &Sample{Pod: stablePod, Requests: 4150, Buckets: []Bucket{{0, 4150}, {math.Inf(1), 4150}}},
					Canary: served(canaryRead(), 150, 0)}), ReasonNoRatio),
		Entry("an operator restart lost the interval's baseline: OperatorInterrupted",
			forfeited(judged(startReads(), endReads(150, 150, 0))), ReasonOperatorInterrupted),
	)

	DescribeTable("a post-warmup canary 5xx and a canary OOMKilled each trip at once",
		func(s Session, iv Interval, trip Trip, reasons ...Reason) {
			rec := s.Evaluate(iv)
			Expect(rec.Verdict).To(Equal(VerdictBreach))
			Expect(rec.Reasons).To(inOrder(reasons))
			_, d := s.Fold(rec)
			Expect(d).To(Equal(Decision{Rollback: trip}))
		},
		Entry("one canary 5xx past warmup: line 2", watching,
			judged(startReads(), endReads(150, 150, 1)), TripCanaryErrors, ReasonCanaryErrors),
		Entry("a canary 5xx under the floor: line 2 counts any 5xx, line 3 unevaluable beside it", watching,
			judged(startReads(), endReads(150, 30, 1)), TripCanaryErrors, ReasonCanaryErrors, ReasonCanaryBelowFloor),
		Entry("a canary OOMKilled, its container restarting and not Ready: line 5's canary regime", watching,
			oomKilled(interval(judgedStart, nil, startReads(), Samples{Stable: served(stableRead(), 150, 0),
				Canary: sample(canaryPod, 20, 0, 0, 20, 20, 20, 20)})),
			TripCanaryOOMKilled, ReasonCanaryOOMKilled, ReasonCanaryCounterReset),
		Entry("a canary OOMKilled in warmup: the runbook's close at once has no warmup", watching,
			oomKilled(interval(canaryReady, new(canaryReady), startReads(), endReads(150, 150, 0))),
			TripCanaryOOMKilled, ReasonCanaryOOMKilled, ReasonCanaryWarmup),
		Entry("a canary OOMKilled before any canary Ready: the script reads line 5 before it waits for one",
			Session{}, oomKilled(interval(windowOpen, nil, Samples{Stable: stableRead()},
				Samples{Stable: served(stableRead(), 150, 0)})),
			TripCanaryOOMKilled, ReasonCanaryOOMKilled, ReasonCanaryNotReady),
		Entry("an OOM kill and a 5xx together: line 5 named, as the script prints it first", watching,
			oomKilled(judged(startReads(), endReads(150, 150, 2))),
			TripCanaryOOMKilled, ReasonCanaryOOMKilled, ReasonCanaryErrors),
		Entry("a canary OOMKilled in an interval an operator restart forfeited", watching,
			oomKilled(forfeited(judged(startReads(), endReads(150, 150, 0)))),
			TripCanaryOOMKilled, ReasonCanaryOOMKilled, ReasonOperatorInterrupted),
	)

	DescribeTable("warmup: an interval starting 59 s after the canary pod's latest Ready is warmup and its 5xx "+
		"uncounted, one starting at 60 s counts them, and the rule applies again after a restart",
		func(pod Pod, readyAt time.Time, offset time.Duration, canaryErrs float64, want Verdict, reasons ...Reason) {
			base := Samples{Stable: stableRead(), Canary: sample(pod, 600, 0, 0, 600, 600, 600, 600)}
			cur := Samples{Stable: served(base.Stable, 150, 0), Canary: served(base.Canary, 150, canaryErrs)}
			s := Session{CanaryReadySeen: true, CanaryPod: pod}

			rec := s.Evaluate(interval(readyAt.Add(offset), &readyAt, base, cur))
			Expect(rec.Verdict).To(Equal(want))
			Expect(rec.Reasons).To(inOrder(reasons))
			Expect(rec.Canary.Errors).To(BeEquivalentTo(canaryErrs), "the 5xx are recorded, counted or not")
		},
		Entry("59 s after the first Ready, 5xx in it: warmup",
			canaryPod, canaryReady, 59*time.Second, 2.0, VerdictWarmup, ReasonCanaryWarmup),
		Entry("60 s after the first Ready, 5xx in it: line 2",
			canaryPod, canaryReady, 60*time.Second, 2.0, VerdictBreach, ReasonCanaryErrors),
		Entry("59 s after the first Ready, no 5xx: warmup",
			canaryPod, canaryReady, 59*time.Second, 0.0, VerdictWarmup, ReasonCanaryWarmup),
		Entry("60 s after the first Ready, no 5xx: clean",
			canaryPod, canaryReady, 60*time.Second, 0.0, VerdictClean),
		Entry("59 s after a restarted pod's Ready, 5xx in it: warmup again",
			restartedPod, restartReady, 59*time.Second, 2.0, VerdictWarmup, ReasonCanaryWarmup),
		Entry("60 s after a restarted pod's Ready, 5xx in it: line 2",
			restartedPod, restartReady, 60*time.Second, 2.0, VerdictBreach, ReasonCanaryErrors),
	)

	// The script's line 3 runs on every interval past pending, warmup included:
	// warmup excludes line 2's 5xx and nothing else. A warmup interval line 3
	// judged ok resets its counter but adds nothing to the clean run (D44).
	DescribeTable("warmup excludes the canary's 5xx and nothing else: line 3 judges the interval, "+
		"and its two in a row runs through warmup",
		func(iv Interval, want Verdict, reasons ...Reason) {
			rec := watching.Evaluate(iv)
			Expect(rec.Verdict).To(Equal(want))
			Expect(rec.Reasons).To(inOrder(reasons))

			afterBreach := watching
			afterBreach.BreachRun = 1
			afterBreach, d := afterBreach.Fold(rec)
			cleanRun := watching
			cleanRun.CleanRun = 4
			cleanRun, _ = cleanRun.Fold(rec)
			if want == VerdictWarmup {
				Expect(afterBreach.BreachRun).To(BeZero(), "line 3 judged it ok: its counter resets")
				Expect(d).To(Equal(Decision{}))
				Expect(cleanRun.CleanRun).To(Equal(4), "warmup adds nothing to the clean run (D44)")
				return
			}
			Expect(afterBreach.BreachRun).To(Equal(2))
			Expect(d).To(Equal(Decision{Rollback: TripCanaryLatency}), "the second in a row, in warmup")
			Expect(cleanRun.CleanRun).To(BeZero())
		},
		Entry("line 3 ok, 5xx in it: warmup, the 5xx uncounted",
			warming(startReads(), endReads(150, 150, 2)), VerdictWarmup, ReasonCanaryWarmup),
		Entry("the canary under the floor: unevaluable",
			warming(startReads(), endReads(150, 49, 0)), VerdictUnevaluable, ReasonCanaryWarmup, ReasonCanaryBelowFloor),
		Entry("the stable under the floor: unevaluable",
			warming(startReads(), endReads(49, 150, 0)), VerdictUnevaluable, ReasonCanaryWarmup, ReasonStableBelowFloor),
		Entry("the canary under the floor with 5xx in it: line 3 counts it, line 2 does not trip",
			warming(startReads(), endReads(150, 30, 2)), VerdictUnevaluable, ReasonCanaryWarmup, ReasonCanaryBelowFloor),
		Entry("the canary's read failed: unevaluable",
			warming(startReads(), Samples{Stable: served(stableRead(), 150, 0)}), VerdictUnevaluable,
			ReasonCanaryWarmup, ReasonCanaryReadFailed),
		Entry("no earlier canary read to take the change from: the script's no earlier read, unevaluable",
			warming(Samples{Stable: stableRead()}, endReads(150, 150, 0)), VerdictUnevaluable,
			ReasonCanaryWarmup, ReasonCanaryNoBaseline),
		Entry("the ratio at 1.2525: a breach", warming(ratioReads(47)), VerdictBreach,
			ReasonCanaryWarmup, ReasonLatencyRatio),
		Entry("the ratio at exactly 1.25: not above it, ok", warming(ratioReads(48)), VerdictWarmup, ReasonCanaryWarmup),
		Entry("the ratio at 1.2473: ok", warming(ratioReads(49)), VerdictWarmup, ReasonCanaryWarmup),
		Entry("an operator restart lost the baseline: unevaluable, OperatorInterrupted",
			forfeited(warming(startReads(), endReads(150, 150, 0))), VerdictUnevaluable,
			ReasonCanaryWarmup, ReasonOperatorInterrupted),
	)

	DescribeTable("a deleted canary pod trips line 3 across its replacement's warmup: the interval that finds "+
		"no canary pod is unevaluable, and the replacement's first interval, in warmup, is the second in a row",
		func(base, cur *Sample, want Verdict, reasons ...Reason) {
			s := Session{CanaryReadySeen: true, CanaryPod: canaryPod, CleanRun: 7}
			stableEnd := served(stableRead(), 150, 0)
			gone := s.Evaluate(interval(judgedStart, nil, startReads(), Samples{Stable: stableEnd}))
			Expect(gone.Verdict).To(Equal(VerdictUnevaluable))
			Expect(gone.Reasons).To(Equal([]Reason{ReasonCanaryReadFailed}))
			s, d := s.Fold(gone)
			Expect(s).To(Equal(Session{BreachRun: 1, CanaryReadySeen: true, CanaryPod: canaryPod}))
			Expect(d).To(Equal(Decision{}))

			rec := s.Evaluate(interval(judgedStart.Add(time.Minute), new(replacementReady),
				Samples{Stable: stableEnd, Canary: base}, Samples{Stable: served(stableEnd, 150, 0), Canary: cur}))
			Expect(rec.Verdict).To(Equal(want))
			Expect(rec.Reasons).To(inOrder(reasons))
			s, d = s.Fold(rec)
			Expect(s).To(Equal(Session{BreachRun: 2, CanaryReadySeen: true, CanaryPod: restartedPod}))
			Expect(d).To(Equal(Decision{Rollback: TripCanaryLatency}))
		},
		Entry("no earlier read of the replacement to take the change from",
			nil, sample(restartedPod, 75, 0, 0, 75, 75, 75, 75),
			VerdictUnevaluable, ReasonCanaryWarmup, ReasonCanaryNoBaseline),
		Entry("the replacement read at the interval's start, Ready too late in it to reach the floor",
			sample(restartedPod, 0, 0, 0, 0, 0, 0, 0), sample(restartedPod, 30, 0, 0, 30, 30, 30, 30),
			VerdictUnevaluable, ReasonCanaryWarmup, ReasonCanaryBelowFloor),
		Entry("the replacement slow, its p95 above 1.25 times the stable's",
			sample(restartedPod, 0, 0, 0, 0, 0, 0, 0), sample(restartedPod, 75, 0, 0, 0, 75, 75, 75),
			VerdictBreach, ReasonCanaryWarmup, ReasonLatencyRatio),
	)

	DescribeTable("pending: an interval ending before any canary pod has been Ready counts toward neither counter (D45)",
		func(s Session, canary *Sample, readyAt *time.Time, want Verdict, reasons ...Reason) {
			base := Samples{Stable: stableRead()}
			cur := Samples{Stable: served(base.Stable, 150, 0), Canary: canary}
			rec := s.Evaluate(interval(windowOpen, readyAt, base, cur))
			Expect(rec.Verdict).To(Equal(want))
			Expect(rec.Reasons).To(inOrder(reasons))

			next, d := s.Fold(rec)
			Expect(d).To(Equal(Decision{}))
			if want == VerdictPending {
				Expect(next.BreachRun).To(Equal(s.BreachRun))
				Expect(next.CleanRun).To(Equal(s.CleanRun))
			}
		},
		Entry("no canary pod yet, so no canary read: pending, not a failed read",
			Session{}, nil, nil, VerdictPending, ReasonCanaryNotReady),
		Entry("the canary pod Running and not yet Ready",
			Session{}, sample(canaryPod, 0, 0, 0, 0, 0, 0, 0), nil, VerdictPending, ReasonCanaryNotReady),
		Entry("the canary Ready a second after the interval ends",
			Session{}, sample(canaryPod, 0, 0, 0, 0, 0, 0, 0), new(windowOpen.Add(61*time.Second)),
			VerdictPending, ReasonCanaryNotReady),
		Entry("the canary Ready as the interval ends: no longer pending but warmup, and line 3 judges it: "+
			"with no earlier canary read, unevaluable",
			Session{}, sample(canaryPod, 0, 0, 0, 0, 0, 0, 0), new(windowOpen.Add(time.Minute)),
			VerdictUnevaluable, ReasonCanaryWarmup, ReasonCanaryNoBaseline),
		Entry("once a canary has been Ready nothing is pending, and with none Ready now nothing is excluded",
			watching, nil, nil, VerdictUnevaluable, ReasonCanaryReadFailed),
	)

	DescribeTable("seeds the canary's baseline from its first read: "+
		"the first post-warmup interval carries a real change and no free breach (D44)",
		func(pendingRead *Sample, want []Verdict, wantBreachRuns []int) {
			recs, sessions, decisions := walk(Session{}, windowOpen, seedingBoundaries(pendingRead)...)
			Expect(verdicts(recs)).To(Equal(want))
			Expect(recs[3].Canary.Requests).To(BeEquivalentTo(150),
				"the change over the interval, not the 375 the canary has served since it started")
			Expect(breachRuns(sessions)).To(Equal(wantBreachRuns))
			Expect(decisions).To(HaveEach(Decision{}))
			Expect(sessions[3]).To(Equal(Session{CleanRun: 1, CanaryReadySeen: true, CanaryPod: canaryPod}))
		},
		Entry("the canary read while Running and not yet Ready: both warmup intervals judged ok",
			sample(canaryPod, 0, 0, 0, 0, 0, 0, 0),
			[]Verdict{VerdictPending, VerdictWarmup, VerdictWarmup, VerdictClean}, []int{0, 0, 0, 0}),
		Entry("the canary first read once Ready: watch-window.sh's no earlier read leaves the first warmup "+
			"interval unevaluable, and the second, judged ok, resets the counter",
			nil, []Verdict{VerdictPending, VerdictUnevaluable, VerdictWarmup, VerdictClean}, []int{0, 1, 0, 0}),
	)

	It("reaches the promote verdict at exactly 11 consecutive clean intervals, not at 10 (D44)", func() {
		bs := seedingBoundaries(nil)
		last := bs[len(bs)-1].reads
		for range 10 {
			last = Samples{Stable: served(last.Stable, 150, 0), Canary: served(last.Canary, 150, 0)}
			bs = append(bs, boundary{reads: last, readyAt: new(canaryReady)})
		}
		recs, sessions, decisions := walk(Session{}, windowOpen, bs...)

		Expect(recs).To(HaveLen(14))
		Expect(cleanRuns(sessions)[3:]).To(Equal([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}))
		for i, d := range decisions[:13] {
			Expect(d).To(Equal(Decision{}), "interval %d", i+1)
		}
		Expect(sessions[12].CleanRun).To(Equal(10))
		Expect(decisions[13]).To(Equal(Decision{Promote: true}))
	})

	DescribeTable("a canary restart resets the clean run, and the new pod warms up again (D44, D45)",
		func(interrupted bool, wantVerdicts []Verdict, first []Reason, wantBreachRuns []int) {
			restarted := sample(restartedPod, 50, 0, 0, 50, 50, 50, 50)
			b0 := startReads()
			b1 := Samples{Stable: served(b0.Stable, 150, 0), Canary: restarted}
			b2 := Samples{Stable: served(b1.Stable, 150, 0), Canary: served(restarted, 150, 0)}
			b3 := Samples{Stable: served(b2.Stable, 150, 0), Canary: served(b2.Canary, 150, 0)}
			recs, sessions, decisions := walk(Session{CanaryReadySeen: true, CanaryPod: canaryPod, CleanRun: 7}, judgedStart,
				boundary{reads: b0, readyAt: new(canaryReady)},
				boundary{reads: b1, readyAt: new(restartReady), interrupted: interrupted},
				boundary{reads: b2, readyAt: new(restartReady)},
				boundary{reads: b3, readyAt: new(restartReady)},
			)

			Expect(verdicts(recs)).To(Equal(wantVerdicts))
			Expect(recs[0].Reasons).To(Equal(first))
			Expect(cleanRuns(sessions)).To(Equal([]int{0, 0, 1}))
			Expect(breachRuns(sessions)).To(Equal(wantBreachRuns))
			Expect(sessions[2].CanaryPod).To(Equal(restartedPod))
			Expect(decisions).To(HaveEach(Decision{}))
		},
		Entry("the replacement shows as a changed pod: unevaluable, then its own warmup, judged ok, then clean",
			false, []Verdict{VerdictUnevaluable, VerdictWarmup, VerdictClean},
			[]Reason{ReasonCanaryWarmup, ReasonCanaryPodChanged}, []int{1, 0, 0}),
		Entry("an operator restart forfeited the interval that shows it: unevaluable, and the clean run resets "+
			"all the same",
			true, []Verdict{VerdictUnevaluable, VerdictWarmup, VerdictClean},
			[]Reason{ReasonCanaryWarmup, ReasonOperatorInterrupted}, []int{1, 0, 0}),
	)

	DescribeTable("folds the counters: line 3's two in a row and the clean run",
		func(s Session, recs []Record, want Session, last Decision) {
			var d Decision
			for _, r := range recs {
				s, d = s.Fold(r)
			}
			Expect(s).To(Equal(want))
			Expect(d).To(Equal(last))
		},
		Entry("a latency breach and an unevaluable interval count alike: two in a row trip line 3",
			Session{}, []Record{record(VerdictBreach, ReasonLatencyRatio), record(VerdictUnevaluable, ReasonCanaryBelowFloor)},
			Session{BreachRun: 2}, Decision{Rollback: TripCanaryLatency}),
		Entry("a clean interval between two breaches resets line 3's counter",
			Session{}, []Record{record(VerdictBreach, ReasonLatencyRatio), record(VerdictClean),
				record(VerdictBreach, ReasonLatencyRatio)},
			Session{BreachRun: 1}, Decision{}),
		Entry("a warmup interval line 3 judged ok between two resets line 3's counter, as the script's does",
			Session{}, []Record{record(VerdictBreach, ReasonLatencyRatio), record(VerdictWarmup, ReasonCanaryWarmup),
				record(VerdictUnevaluable, ReasonStableReadFailed)},
			Session{BreachRun: 1}, Decision{}),
		Entry("a latency breach and an unevaluable interval in warmup count alike: two in a row trip line 3",
			Session{}, []Record{record(VerdictBreach, ReasonCanaryWarmup, ReasonLatencyRatio),
				record(VerdictUnevaluable, ReasonCanaryWarmup, ReasonCanaryBelowFloor)},
			Session{BreachRun: 2}, Decision{Rollback: TripCanaryLatency}),
		Entry("a pending interval counts toward neither counter",
			Session{BreachRun: 1, CleanRun: 4}, []Record{record(VerdictPending, ReasonCanaryNotReady)},
			Session{BreachRun: 1, CleanRun: 4}, Decision{}),
		Entry("a warmup interval line 3 judged ok leaves the clean run alone: warmup never adds to it (D44)",
			Session{CleanRun: 5}, []Record{record(VerdictWarmup, ReasonCanaryWarmup)},
			Session{CleanRun: 5}, Decision{}),
		Entry("a canary 5xx with line 3 clean trips line 2 and resets line 3's counter, the script's lines apart",
			Session{BreachRun: 1, CleanRun: 3}, []Record{record(VerdictBreach, ReasonCanaryErrors)},
			Session{}, Decision{Rollback: TripCanaryErrors}),
		Entry("an OOM kill in warmup, line 3 judged ok beside it, resets line 3's counter and breaks the clean run",
			Session{BreachRun: 1, CleanRun: 2}, []Record{record(VerdictBreach, ReasonCanaryOOMKilled, ReasonCanaryWarmup)},
			Session{}, Decision{Rollback: TripCanaryOOMKilled}),
		Entry("the 10th clean interval: no promote verdict",
			Session{CleanRun: 9}, []Record{record(VerdictClean)}, Session{CleanRun: 10}, Decision{}),
		Entry("the 11th: the promote verdict",
			Session{CleanRun: 10}, []Record{record(VerdictClean)}, Session{CleanRun: 11}, Decision{Promote: true}),
		Entry("past 11 the verdict recurs, for the controller to latch once",
			Session{CleanRun: 11}, []Record{record(VerdictClean)}, Session{CleanRun: 12}, Decision{Promote: true}),
		Entry("an unevaluable interval breaks the clean run",
			Session{CleanRun: 10}, []Record{record(VerdictUnevaluable, ReasonOperatorInterrupted)},
			Session{BreachRun: 1}, Decision{}),
		Entry("two forfeited intervals in a row trip line 3: two operator restarts close the window",
			Session{}, []Record{record(VerdictUnevaluable, ReasonOperatorInterrupted),
				record(VerdictUnevaluable, ReasonOperatorInterrupted)},
			Session{BreachRun: 2}, Decision{Rollback: TripCanaryLatency}),
	)
})
