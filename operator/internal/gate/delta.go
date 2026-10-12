package gate

import (
	"cmp"
	"math"
	"slices"
)

// latencyQuantile is line 3's quantile, the p95.
const latencyQuantile = 0.95

// podCauses are one pod's reasons its change cannot be taken.
type podCauses struct {
	readFailed, noBaseline, podChanged, counterReset Reason
}

var (
	stableCauses = podCauses{
		ReasonStableReadFailed, ReasonStableNoBaseline, ReasonStablePodChanged, ReasonStableCounterReset,
	}
	canaryCauses = podCauses{
		ReasonCanaryReadFailed, ReasonCanaryNoBaseline, ReasonCanaryPodChanged, ReasonCanaryCounterReset,
	}
)

// delta is watch-window.sh's pod_delta: one pod's change from prev, read at
// the interval's start, to cur, read at its end, or the reason it cannot be
// taken. The checks run in the script's order: a read, a read to take the
// change from, the same pod, and no counter gone down, the request and 5xx
// counts and every bucket present at the end, a bucket absent at the start
// counting from zero. A change that is not a finite number is one the script
// cannot compute, and counts as a failed read. Counts are rounded half to
// even, as the script's round() does.
func delta(prev, cur *Sample, causes podCauses) (*Delta, Reason) {
	switch {
	case cur == nil:
		return nil, causes.readFailed
	case prev == nil:
		return nil, causes.noBaseline
	case cur.Pod != prev.Pod:
		return nil, causes.podChanged
	}
	before := make(map[float64]float64, len(prev.Buckets))
	for _, b := range prev.Buckets {
		before[b.UpperBoundSeconds] = b.Count
	}
	buckets := slices.Clone(cur.Buckets)
	slices.SortFunc(buckets, func(a, b Bucket) int { return cmp.Compare(a.UpperBoundSeconds, b.UpperBoundSeconds) })
	wentDown := cur.Requests < prev.Requests || cur.Errors < prev.Errors
	for i := range buckets {
		buckets[i].Count -= before[buckets[i].UpperBoundSeconds]
		wentDown = wentDown || buckets[i].Count < 0
	}
	requests, errs := cur.Requests-prev.Requests, cur.Errors-prev.Errors
	switch {
	case wentDown:
		return nil, causes.counterReset
	case !finite(requests) || !finite(errs):
		return nil, causes.readFailed
	}
	return &Delta{
		Requests:   int64(math.RoundToEven(requests)),
		Errors:     int64(math.RoundToEven(errs)),
		P95Seconds: p95Seconds(buckets),
	}, ""
}

// p95Seconds is histogram_quantile(0.95) over bucket changes sorted by upper
// bound, as watch-window.sh computes it: NaN with fewer than two buckets,
// without +Inf last, or with nothing observed; the second-highest bound when
// the rank falls in +Inf; the first bucket's bound when that bound is zero or
// below; and otherwise a linear interpolation inside the bucket the rank falls
// in, the first bucket interpolating from zero. Every operation rounds as the
// script's Python does: the explicit conversion keeps the compiler from fusing
// the rank's multiply into a later subtraction, which the Go spec allows and
// arm64 does, and which moves a ratio of exactly 1.25 past it.
func p95Seconds(buckets []Bucket) float64 {
	n := len(buckets)
	if n < 2 || !math.IsInf(buckets[n-1].UpperBoundSeconds, 0) || buckets[n-1].Count <= 0 {
		return math.NaN()
	}
	rank := float64(latencyQuantile * buckets[n-1].Count)
	b := slices.IndexFunc(buckets, func(x Bucket) bool { return x.Count >= rank })
	switch {
	case b < 0:
		// A NaN count: the script's computation fails, and the interval is
		// unevaluable either way.
		return math.NaN()
	case b == n-1:
		return buckets[n-2].UpperBoundSeconds
	case b == 0 && buckets[0].UpperBoundSeconds <= 0:
		return buckets[0].UpperBoundSeconds
	}
	var start, below float64
	if b > 0 {
		start, below = buckets[b-1].UpperBoundSeconds, buckets[b-1].Count
	}
	end, count := buckets[b].UpperBoundSeconds, buckets[b].Count
	return start + (end-start)*(rank-below)/(count-below)
}
