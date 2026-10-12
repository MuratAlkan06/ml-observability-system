package scrape

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
)

// The pages under testdata/ are /metrics as the api serves it: the api's own
// metric inventory, src/inference_service/metrics.py, imported in the api
// image (prometheus-client 0.25.0, the pinned version) and driven the way the
// app's _observe middleware records a request — /predict at 200, 422, 500 and
// 503, and /health — then rendered by generate_latest, the call the app's
// /metrics route makes. fresh is a pod before its first /predict; the start
// and end pages are one pod at two boundaries a minute apart.
//
// The expected values are not this package's: they are what watch-window.sh's
// own parse_exposition and pod_delta programs, cut verbatim from the script,
// print for those pages.

// page reads a testdata page.
func page(name string) string {
	data, err := os.ReadFile(filepath.Join("testdata", name+".metrics"))
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

// scriptSnapshot parses parse_exposition's output, "<count> <5xx>
// <le>:<cumulative>,..." with "-" for no buckets, as the script's pod_delta
// does.
func scriptSnapshot(printed string) gate.Sample {
	fields := strings.Fields(printed)
	Expect(fields).To(HaveLen(3))
	var s gate.Sample
	s.Requests = parseFloat(fields[0])
	s.Errors = parseFloat(fields[1])
	if fields[2] != "-" {
		for item := range strings.SplitSeq(fields[2], ",") {
			le, c, ok := strings.Cut(item, ":")
			Expect(ok).To(BeTrue())
			s.Buckets = append(s.Buckets, gate.Bucket{UpperBoundSeconds: parseFloat(le), Count: parseFloat(c)})
		}
	}
	return s
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	Expect(err).NotTo(HaveOccurred())
	return v
}

// parsed is parseExposition over text, which must succeed.
func parsed(text string) gate.Sample {
	s, err := parseExposition(strings.NewReader(text))
	Expect(err).NotTo(HaveOccurred())
	return s
}

var _ = Describe("A /metrics page, read as watch-window.sh's parse_exposition reads it", func() {
	DescribeTable("each api page gives what the script prints for it",
		func(name, printed string) {
			Expect(parsed(page(name))).To(Equal(scriptSnapshot(printed)))
		},
		Entry("a pod before its first /predict: zero, and no buckets", "fresh", "0.0 0.0 -"),
		Entry("the stable at the start", "stable-start",
			"4004.0 3.0 0.01:2.0,0.025:366.0,0.05:2029.0,0.075:3189.0,0.1:3667.0,0.15:3953.0,0.2:3997.0,"+
				"0.3:4004.0,0.5:4004.0,1.0:4004.0,2.5:4004.0,+Inf:4004.0"),
		Entry("the stable a minute on", "stable-end",
			"4155.0 3.0 0.01:3.0,0.025:373.0,0.05:2113.0,0.075:3315.0,0.1:3813.0,0.15:4103.0,0.2:4147.0,"+
				"0.3:4155.0,0.5:4155.0,1.0:4155.0,2.5:4155.0,+Inf:4155.0"),
		Entry("the canary at the start", "canary-start",
			"1201.0 0.0 0.01:1.0,0.025:37.0,0.05:384.0,0.075:746.0,0.1:984.0,0.15:1154.0,0.2:1190.0,"+
				"0.3:1199.0,0.5:1201.0,1.0:1201.0,2.5:1201.0,+Inf:1201.0"),
		Entry("the canary a minute on, its one 500 a 5xx and its 422s not", "canary-end",
			"1353.0 1.0 0.01:2.0,0.025:42.0,0.05:430.0,0.075:839.0,0.1:1102.0,0.15:1296.0,0.2:1340.0,"+
				"0.3:1351.0,0.5:1353.0,1.0:1353.0,2.5:1353.0,+Inf:1353.0"),
	)

	DescribeTable("two pages, through the engine, give the change the script's pod_delta prints",
		func(start, end string, requests, errs int64, p95 float64) {
			var baseline, current gate.Samples
			baseline.Stable, current.Stable = new(parsed(page(start))), new(parsed(page(end)))
			at := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
			rec := gate.Session{}.Evaluate(gate.Interval{
				Start: at, End: at.Add(time.Minute), Baseline: baseline, Current: current, CanaryReadyAt: &at,
			})
			Expect(rec.Stable).To(Equal(&gate.Delta{Requests: requests, Errors: errs, P95Seconds: p95}))
		},
		Entry("the stable's minute: ok 151 0 0.0968125", "stable-start", "stable-end", int64(151), int64(0), 0.0968125),
		Entry("the canary's minute: ok 152 1 0.16500000000000004",
			"canary-start", "canary-end", int64(152), int64(1), 0.16500000000000004),
		Entry("from a fresh pod's read, every bucket from zero: ok 4004 3 0.12391608391608387",
			"fresh", "stable-start", int64(4004), int64(3), 0.12391608391608387),
		Entry("from a fresh pod's read: ok 1201 0 0.14616176470588235",
			"fresh", "canary-start", int64(1201), int64(0), 0.14616176470588235),
	)

	It("reads only the two series, and only their /predict lines", func() {
		s := parsed(strings.Join([]string{
			`# HELP mlobs_http_requests_total HTTP requests to the API (/predict, /health).`,
			`mlobs_http_requests_total{endpoint="/health",method="GET",status="500"} 7.0`,
			`mlobs_http_requests_total{endpoint="/predict",method="POST",status="200"} 10.0`,
			`mlobs_http_requests_created{endpoint="/predict",method="POST",status="200"} 1.7918251951e+09`,
			`mlobs_http_request_duration_seconds_count{endpoint="/predict"} 10.0`,
			`mlobs_http_request_duration_seconds_sum{endpoint="/predict"} 1.5`,
			`mlobs_http_request_duration_seconds_bucket{endpoint="/health",le="+Inf"} 7.0`,
			`mlobs_http_request_duration_seconds_bucket{endpoint="/predict",le="+Inf"} 10.0`,
			`mlobs_inference_duration_seconds_bucket{le="+Inf"} 10.0`,
		}, "\n"))
		Expect(s).To(Equal(gate.Sample{Requests: 10, Buckets: []gate.Bucket{{UpperBoundSeconds: math.Inf(1), Count: 10}}}))
	})

	DescribeTable("a status is a 5xx by the script's pattern, status=\"5[0-9][0-9]\"",
		func(status string, errs float64) {
			s := parsed(`mlobs_http_requests_total{endpoint="/predict",method="POST",status="` + status + `"} 4.0`)
			Expect(s.Requests).To(Equal(4.0))
			Expect(s.Errors).To(Equal(errs))
		},
		Entry("500", "500", 4.0),
		Entry("503", "503", 4.0),
		Entry("200", "200", 0.0),
		Entry("422", "422", 0.0),
		Entry("5000, not three digits", "5000", 0.0),
	)

	It("keeps the last line for an le named twice and sorts by upper bound, as the script's dict and sort do", func() {
		s := parsed(strings.Join([]string{
			`mlobs_http_request_duration_seconds_bucket{endpoint="/predict",le="+Inf"} 9.0`,
			`mlobs_http_request_duration_seconds_bucket{endpoint="/predict",le="0.5"} 3.0`,
			`mlobs_http_request_duration_seconds_bucket{endpoint="/predict",le="0.1"} 1.0`,
			`mlobs_http_request_duration_seconds_bucket{endpoint="/predict",le="0.5"} 4.0`,
		}, "\n"))
		Expect(s.Buckets).To(Equal([]gate.Bucket{
			{UpperBoundSeconds: 0.1, Count: 1}, {UpperBoundSeconds: 0.5, Count: 4}, {UpperBoundSeconds: math.Inf(1), Count: 9},
		}))
	})

	It("reads CRLF lines, and a value with whitespace around it, as Python's float() does", func() {
		s := parsed("mlobs_http_requests_total{endpoint=\"/predict\",method=\"POST\",status=\"200\"}  \t12.0\r\n" +
			"mlobs_http_requests_total{endpoint=\"/predict\",method=\"POST\",status=\"500\"} 1e1\r\n")
		Expect(s.Requests).To(Equal(22.0))
		Expect(s.Errors).To(Equal(10.0))
	})

	It("takes the value from after the line's last space, as the script's rsplit does, "+
		"past a label value with a space in it", func() {
		s := parsed(`mlobs_http_requests_total{endpoint="/predict",method="POST",status="200",note="a b c"} 5.0`)
		Expect(s.Requests).To(Equal(5.0))
	})

	It("reads an empty page as zero requests and no buckets, as the script does", func() {
		Expect(parsed("")).To(Equal(gate.Sample{}))
	})

	DescribeTable("fails the read where the script's Python raises",
		func(line string) {
			_, err := parseExposition(strings.NewReader("# a page\n" + line + "\n"))
			Expect(err).To(HaveOccurred())
		},
		Entry("a value that is not a number",
			`mlobs_http_requests_total{endpoint="/predict",method="POST",status="200"} many`),
		Entry("no value at all", `mlobs_http_requests_total{endpoint="/predict",method="POST",status="200"}`),
		Entry("a bucket with no le", `mlobs_http_request_duration_seconds_bucket{endpoint="/predict"} 3.0`),
		Entry("an le that is not a number", `mlobs_http_request_duration_seconds_bucket{endpoint="/predict",le="soon"} 3.0`),
	)

	It("fails the read on a line past its bound, rather than buffering without one", func() {
		long := `mlobs_http_requests_total{endpoint="/predict",method="` + strings.Repeat("P", maxLineBytes) + `"} 1.0`
		_, err := parseExposition(strings.NewReader(long))
		Expect(err).To(HaveOccurred())
	})
})
