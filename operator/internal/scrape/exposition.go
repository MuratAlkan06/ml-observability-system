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

// maxLineBytes bounds one line of a /metrics page. The api's lines run to a
// few hundred bytes; a longer one fails the read rather than growing the
// buffer without bound.
const maxLineBytes = 1 << 20

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
// the script's does. The buckets come back sorted by upper bound, the
// script's order, ties in the order the page first named them. The pod is
// left for the caller.
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
		switch {
		case strings.HasPrefix(line, requestsPrefix):
			sample.Requests += value
			if errorStatus.MatchString(line) {
				sample.Errors += value
			}
		case strings.HasPrefix(line, bucketsPrefix):
			m := upperBound.FindStringSubmatch(line)
			if m == nil {
				return gate.Sample{}, fmt.Errorf("line %d is a bucket with no le", n)
			}
			if _, seen := counts[m[1]]; !seen {
				order = append(order, m[1])
			}
			counts[m[1]] = value
		}
	}
	if err := lines.Err(); err != nil {
		return gate.Sample{}, err
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
