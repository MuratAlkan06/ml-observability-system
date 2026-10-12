package gate

import (
	"math"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("A pod's change over an interval, as watch-window.sh's pod_delta takes it", func() {
	DescribeTable("p95Seconds is histogram_quantile(0.95) with the script's rules",
		func(buckets []Bucket, want float64) {
			got := p95Seconds(buckets)
			if math.IsNaN(want) {
				Expect(math.IsNaN(got)).To(BeTrue(), "got %v", got)
				return
			}
			Expect(got).To(BeNumerically("~", want, 1e-12))
		},
		Entry("fewer than two buckets: NaN", []Bucket{{math.Inf(1), 10}}, math.NaN()),
		Entry("no +Inf bucket last: NaN", []Bucket{{0.1, 5}, {0.25, 10}}, math.NaN()),
		Entry("nothing observed: NaN", []Bucket{{0.1, 0}, {math.Inf(1), 0}}, math.NaN()),
		Entry("the rank in +Inf: the highest finite bound", []Bucket{{0.1, 1}, {0.25, 2}, {math.Inf(1), 100}}, 0.25),
		Entry("a first bound of zero: that bound", []Bucket{{0, 100}, {math.Inf(1), 100}}, 0.0),
		Entry("the first bucket interpolates from zero", []Bucket{{0.5, 100}, {math.Inf(1), 100}}, 0.475),
		Entry("linear inside the bucket the rank falls in", []Bucket{{0.25, 72}, {0.5, 80}, {math.Inf(1), 80}}, 0.375),
	)

	It("counts the buckets of a read taken before the first /predict, which has none, from zero", func() {
		d, cause := delta(&Sample{Pod: canaryPod}, sample(canaryPod, 80, 0, 0, 72, 80, 80, 80), canaryCauses)
		Expect(cause).To(BeEmpty())
		Expect(d.Requests).To(BeEquivalentTo(80))
		Expect(d.P95Seconds).To(Equal(0.375))
	})

	It("sorts the buckets by upper bound, as the script's parser does", func() {
		end := sample(canaryPod, 80, 0, 0, 72, 80, 80, 80)
		slices.Reverse(end.Buckets)
		d, cause := delta(sample(canaryPod, 0, 0, 0, 0, 0, 0, 0), end, canaryCauses)
		Expect(cause).To(BeEmpty())
		Expect(d.P95Seconds).To(Equal(0.375))
	})

	It("rounds the counts half to even, as the script's round() does", func() {
		end := sample(canaryPod, 48.5, 2.5, 0, 48, 48, 48, 48)
		d, cause := delta(sample(canaryPod, 0, 0, 0, 0, 0, 0, 0), end, canaryCauses)
		Expect(cause).To(BeEmpty())
		Expect(d.Requests).To(BeEquivalentTo(48))
		Expect(d.Errors).To(BeEquivalentTo(2))
	})
})
