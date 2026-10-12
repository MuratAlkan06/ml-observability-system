// Package scrape is operator v1's metric acquisition (docs/PLAN.md D41): what
// the operator reads of the window's two api pods at an interval boundary, in
// the shape the decision engine, package gate, takes. It reads by
// deploy/k3s/watch-window.sh's method verbatim: the pods listed by the
// window's selectors, each one's own /metrics read over plain HTTP at its pod
// IP on port 8000, and the same two series parsed from it. Prometheus is not
// read at all.
//
// The pods are listed through an uncached reader, the manager's APIReader: the
// operator's Role grants get and list on pods and no watch, because reads that
// happen once a minute need no informer (D41). Every read carries a context
// timeout, ReadTimeout, well under the 60 s grid, so a pod that never answers
// costs an unevaluable interval and never a wedged reconcile. A read that
// fails is a nil gate.Sample, which is what gate counts as a failed read.
//
// Nothing here decides or keeps a session: that is gate's, and when to read is
// the controller's.
package scrape

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
)

const (
	// MetricsPort is the api container's port, where /metrics is served
	// beside /predict: watch-window.sh's port-forwards reach each pod on it.
	MetricsPort = 8000
	// ReadTimeout bounds one pod's read at a boundary, the pod list and the
	// /metrics GET together, at the 5 s watch-window.sh allows each of them
	// on its own: `kubectl get pods --request-timeout=5s` and
	// `curl --max-time 5`. It is compiled, not a setting, and well under the
	// 60 s grid (D41): a boundary makes at most four reads, two per pod, so
	// whatever the pods do it holds a reconcile for 20 s at the most, and a
	// pod that never answers costs an unevaluable interval.
	ReadTimeout = 5 * time.Second

	// reasonOOMKilled is the reason the kubelet gives a container the kernel
	// OOM-killed.
	reasonOOMKilled = "OOMKilled"
)

// Role is one of the window's two pods.
type Role string

const (
	// RoleStable is the stable api pod, deployment/api's.
	RoleStable Role = "stable"
	// RoleCanary is the canary api pod, deployment/api-canary's.
	RoleCanary Role = "canary"
)

// selectors are watch-window.sh's pod_selector, verbatim. The stable's
// Deployment selects app=api, which the canary's pods carry too, so the
// stable pod is the one with app=api and no role label; the canary's carries
// role=canary.
var selectors = map[Role]string{
	RoleStable: "app=api,!role",
	RoleCanary: "app=api,role=canary",
}

// Scraper reads the window's two pods at an interval boundary.
type Scraper struct {
	// Reader lists the pods. It must be the manager's APIReader
	// (mgr.GetAPIReader()), which reads the API server directly: the Role
	// grants pods get and list and not watch, so a cached client, whose
	// informer watches, has no grant to run on (D41).
	Reader client.Reader
	// HTTP reads each pod's /metrics; nil means http.DefaultClient. Each
	// request carries its read's context, so the client needs no timeout of
	// its own.
	HTTP *http.Client

	// timeout overrides ReadTimeout for this package's tests; zero means
	// ReadTimeout.
	timeout time.Duration
}

// Boundary is what the operator reads at one interval boundary: the end of
// the interval before it and the start of the interval after it, on the grid
// the controller keeps (D45).
type Boundary struct {
	// Current is each pod's read at the boundary, a nil Sample where the read
	// failed: the interval that ends here takes its change to these. It is
	// gate.Interval.Current for that interval.
	Current gate.Samples
	// Baseline is what the interval that starts here takes its change from:
	// Current, with each pod whose read failed read once more, right after.
	// watch-window.sh retries a failed read at the next interval's start, so
	// that one failed read leaves one interval unevaluable rather than two
	// (the start-of-interval retry, #111). It is gate.Interval.Baseline for
	// the next interval. At the window's first boundary, where no interval
	// ends, Current is the first read and only Baseline is used, as the
	// script's first reads start its first interval.
	Baseline gate.Samples
	// CanaryReadyAt is the latest Ready transition among the canary pods
	// Ready at the boundary, watch-window.sh's canary_ready, nil when none is
	// or the canary pods could not be listed. It is gate.Interval's.
	CanaryReadyAt *time.Time
	// CanaryOOMKilled is whether any container of a canary pod shows an OOM
	// kill in its status, current or last, as read at the boundary. It is
	// gate.Interval's.
	CanaryOOMKilled bool
	// StableErr and CanaryErr say why a read in Current failed, nil where it
	// did not: for the operator's log. The verdict reads only the nil Sample.
	StableErr error
	CanaryErr error
}

// ReadBoundary reads the window's two pods in namespace at one boundary, the
// stable then the canary as the script does, then reads once more each pod
// whose read failed, for Baseline. Every read is bounded by ReadTimeout, and
// ReadBoundary always returns: a read that fails or times out is a nil Sample.
func (s *Scraper) ReadBoundary(ctx context.Context, namespace string) Boundary {
	var b Boundary
	b.Current.Stable, _, b.StableErr = s.read(ctx, namespace, RoleStable)
	var canaryPods []corev1.Pod
	b.Current.Canary, canaryPods, b.CanaryErr = s.read(ctx, namespace, RoleCanary)
	b.CanaryReadyAt = latestReady(canaryPods)
	b.CanaryOOMKilled = oomKilled(canaryPods)

	b.Baseline = b.Current
	if b.Baseline.Stable == nil {
		b.Baseline.Stable, _, _ = s.read(ctx, namespace, RoleStable)
	}
	if b.Baseline.Canary == nil {
		b.Baseline.Canary, _, _ = s.read(ctx, namespace, RoleCanary)
	}
	return b
}

// read is watch-window.sh's pod_snapshot for role, under one ReadTimeout:
// the role's pods listed in namespace, and the /metrics of the one Running
// pod among them read and parsed. It returns the sample, nil when the read
// failed, and the pods the list returned, nil when it failed.
func (s *Scraper) read(ctx context.Context, namespace string, role Role) (*gate.Sample, []corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, s.readTimeout())
	defer cancel()

	selector, err := labels.Parse(selectors[role])
	if err != nil {
		return nil, nil, fmt.Errorf("the %s selector: %w", role, err)
	}
	list := &corev1.PodList{}
	if err := s.Reader.List(ctx, list, client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, fmt.Errorf("the %s pod list is unreadable: %w", role, err)
	}
	pods := list.Items

	// The script lists with --field-selector=status.phase=Running; the phase
	// is filtered here instead, so the canary's one list also serves its
	// Ready time and OOM kills, which the script reads from all its pods.
	var running []*corev1.Pod
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning {
			running = append(running, &pods[i])
		}
	}
	if len(running) != 1 {
		return nil, pods, fmt.Errorf("%d Running %s pods, not 1", len(running), role)
	}
	pod := running[0]
	sample, err := s.metrics(ctx, pod)
	if err != nil {
		return nil, pods, fmt.Errorf("%s pod/%s /metrics unreadable: %w", role, pod.Name, err)
	}
	sample.Pod = gate.Pod{Name: pod.Name, UID: string(pod.UID)}
	return &sample, pods, nil
}

// metrics GETs pod's /metrics at its pod IP on MetricsPort and parses it. A
// status other than 200 fails the read, as curl -f fails one of 400 or
// above; the api's /metrics answers 200 or not at all.
func (s *Scraper) metrics(ctx context.Context, pod *corev1.Pod) (gate.Sample, error) {
	if pod.Status.PodIP == "" {
		return gate.Sample{}, fmt.Errorf("pod/%s has no pod IP", pod.Name)
	}
	url := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(MetricsPort)) + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return gate.Sample{}, err
	}
	httpClient := s.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return gate.Sample{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return gate.Sample{}, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return parseExposition(resp.Body)
}

func (s *Scraper) readTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return ReadTimeout
}

// latestReady is watch-window.sh's canary_ready: the latest Ready transition
// among pods whose Ready condition is True, nil when none is.
func latestReady(pods []corev1.Pod) *time.Time {
	var latest *time.Time
	for i := range pods {
		for _, c := range pods[i].Status.Conditions {
			if c.Type != corev1.PodReady || c.Status != corev1.ConditionTrue {
				continue
			}
			if t := c.LastTransitionTime.Time; latest == nil || t.After(*latest) {
				latest = &t
			}
		}
	}
	return latest
}

// oomKilled reports whether any container of pods, init containers included,
// was OOMKilled: terminated by it now, or last terminated by it before a
// restart. It reads the kubelet's status, the one line 5 signal a pod shows
// the operator (D46).
func oomKilled(pods []corev1.Pod) bool {
	for i := range pods {
		for _, cs := range slices.Concat(pods[i].Status.InitContainerStatuses, pods[i].Status.ContainerStatuses) {
			for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
				if t != nil && t.Reason == reasonOOMKilled {
					return true
				}
			}
		}
	}
	return false
}
