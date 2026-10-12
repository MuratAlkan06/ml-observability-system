package scrape

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
)

// The two series watch-window.sh reads, on /predict alone: the api's
// mlobs_http_requests counter, which prometheus-client exposes with its _total
// suffix, and the buckets of its mlobs_http_request_duration_seconds histogram
// (src/inference_service/metrics.py).
const (
	predictLabel   = `endpoint="/predict"`
	requestsPrefix = "mlobs_http_requests_total{"
	bucketsPrefix  = "mlobs_http_request_duration_seconds_bucket{"
)

var (
	// errorStatus marks a request line whose status is a 5xx.
	errorStatus = regexp.MustCompile(`status="5[0-9][0-9]"`)
	// upperBound captures a bucket line's le.
	upperBound = regexp.MustCompile(`le="([^"]+)"`)
)

// The bounds a page is read within. The script's Python reads past each of
// them; here the read fails instead, so each is a named deviation from
// watch-window.sh, and each errs toward an unevaluable interval.
const (
	// maxLineBytes bounds one line of a /metrics page. The api's longest
	// runs to 115 bytes; a longer line than this fails the read rather than
	// growing the buffer.
	maxLineBytes = 16 << 10
	// maxBuckets bounds the distinct le values the /predict bucket lines
	// name. The api's histogram has 12; a page naming more than this fails
	// the read rather than growing the map.
	maxBuckets = 64
	// maxCount bounds a count at 2^53-1, the largest integer a float64 holds
	// exactly with both its neighbours: from 2^53 on, two consecutive counts
	// read as the same number, and a change between two counts can pass the
	// int64 range, where gate's conversion to a count gives whatever the
	// architecture makes of it. A count past the bound, below zero, where no
	// counter or bucket goes, or not a number at all (NaN, ±Inf), fails the
	// read.
	maxCount = 1<<53 - 1
)

// parseExposition is watch-window.sh's parse_exposition, line for line, over
// a /metrics page in the text exposition format: of the lines that carry
// endpoint="/predict", each mlobs_http_requests_total line adds its value to
// the request count, and to the 5xx count too when its status is a 5xx; each
// mlobs_http_request_duration_seconds_bucket line sets its le's cumulative
// count, a later line for the same le replacing an earlier one. The value is
// the line's last space-separated field. Every other line is skipped, and a
// page without a /predict line is a read of zero requests and no buckets, as
// the script prints it before a pod's first /predict.
//
// Where the script's Python would raise — a value that is not a number, a
// bucket line with no le, an le that is not a number — the read fails, as
// the script's does. It also fails past the bounds above, where the script's
// would not: a line over maxLineBytes, more than maxBuckets bounds, and a
// count, a line's or the request sum, that is not a number from 0 to
// maxCount; the 5xx sum lies between zero and the request sum. The buckets
// come back sorted by upper bound, the script's order, ties in the order the
// page first named them. The pod is left for the caller.
func parseExposition(page io.Reader) (gate.Sample, error) {
	var (
		sample gate.Sample
		order  []string
		counts = map[string]float64{}
	)
	lines := bufio.NewScanner(page)
	lines.Buffer(nil, maxLineBytes)
	for n := 1; lines.Scan(); n++ {
		line := lines.Text()
		if !strings.Contains(line, predictLabel) {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			return gate.Sample{}, fmt.Errorf("line %d has no value", n)
		}
		// Python's float() strips the whitespace around a number, the
		// line's newline among it.
		value, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		if err != nil {
			return gate.Sample{}, fmt.Errorf("line %d: %w", n, err)
		}
		requests, bucket := strings.HasPrefix(line, requestsPrefix), strings.HasPrefix(line, bucketsPrefix)
		if (requests || bucket) && !isCount(value) {
			return gate.Sample{}, fmt.Errorf("line %d: %v is not a count from 0 to 2^53-1", n, value)
		}
		switch {
		case requests:
			sample.Requests += value
			if errorStatus.MatchString(line) {
				sample.Errors += value
			}
		case bucket:
			m := upperBound.FindStringSubmatch(line)
			if m == nil {
				return gate.Sample{}, fmt.Errorf("line %d is a bucket with no le", n)
			}
			// A copy, not the substring: a key kept from the line would keep
			// the whole line, and a map assignment stores its key anew.
			le := strings.Clone(m[1])
			if _, seen := counts[le]; !seen {
				if len(order) == maxBuckets {
					return gate.Sample{}, fmt.Errorf("line %d names more than %d bucket bounds", n, maxBuckets)
				}
				order = append(order, le)
			}
			counts[le] = value
		}
	}
	if err := lines.Err(); err != nil {
		return gate.Sample{}, err
	}
	// Each line's count is within the bound; their sum must be too. The 5xx
	// sum needs no check of its own: it adds some of the same counts, none
	// below zero, so it lies between zero and the request sum, an order a
	// float sum's rounding, being monotonic, keeps.
	if !isCount(sample.Requests) {
		return gate.Sample{}, fmt.Errorf("the request sum, %v, is not a count from 0 to 2^53-1", sample.Requests)
	}
	for _, le := range order {
		bound, err := strconv.ParseFloat(le, 64)
		if err != nil {
			return gate.Sample{}, fmt.Errorf("bucket le=%q: %w", le, err)
		}
		sample.Buckets = append(sample.Buckets, gate.Bucket{UpperBoundSeconds: bound, Count: counts[le]})
	}
	slices.SortStableFunc(sample.Buckets, func(a, b gate.Bucket) int {
		return cmp.Compare(a.UpperBoundSeconds, b.UpperBoundSeconds)
	})
	return sample, nil
}

// isCount reports whether v is a number from 0 to maxCount. NaN compares
// false with everything, so it is not.
func isCount(v float64) bool {
	return v >= 0 && v <= maxCount
}
