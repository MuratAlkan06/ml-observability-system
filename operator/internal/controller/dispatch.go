package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// The promote dispatch's GitHub client (docs/PLAN.md D43). Of everything the
// token could do — its residual capabilities are enumerated in D43 and
// docs/RUNBOOK.md — the code path uses one capability, a single
// workflow_dispatch of deploy.yml on main, and, before it, the Actions read
// that lists the Deploy runs a dispatch must not evict. The repository, the
// workflow, the ref and the API version are compiled, not settings.
//
// The token is read from the file the operator Deployment mounts from its
// Secret, at every arming check and again at every dispatch, never kept: a
// rotated Secret is picked up without a restart, and a deleted one degrades
// ActPromote to verdict-only at the next boundary.
//
// The client is held to the same discipline as package scrape's (#116): it is
// its own, never http.DefaultClient or the manager's, whose transport carries
// the ServiceAccount token; it goes through no proxy whatever the environment
// names, so the token reaches GitHub alone; it follows no redirect, so the
// token is sent to the one URL built here and a 3xx is the answer read; it
// keeps no connection between calls a window apart; and it bounds the
// response headers and body it reads. Every exchange runs under
// dispatchTimeout. No error or Event carries the token or a response body.

const (
	// githubAPI is the GitHub REST API the dispatch goes to. Only the e2e's
	// test-only knob, githubAPIEnv, moves it.
	githubAPI = "https://api.github.com"
	// githubAPIEnv names the dispatch's one test-only knob: set on the
	// operator's container, it points the GitHub calls at
	// operator/hack/e2e.sh's in-cluster stand-in. Neither the flattened
	// manifests nor the chart set it, and a test holds them to that. It moves
	// nothing else: the repository, the workflow, the ref, the token's file
	// and every bound are the compiled ones.
	githubAPIEnv = "MLOBS_E2E_GITHUB_API_URL"
	// githubAPIVersion pins the REST API version. Under it a dispatch is
	// answered 204 with no body, naming no run (D43); the 2026-03-10 version
	// answers 200 with the run's id and URL.
	githubAPIVersion = "2022-11-28"

	// deployRepository, deployWorkflow and deployRef are the dispatch's one
	// target: .github/workflows/deploy.yml in this repository, on main. Its
	// one input is sha.
	deployRepository = "MuratAlkan06/ml-observability-system"
	deployWorkflow   = "deploy.yml"
	deployRef        = "main"

	// dispatchTokenSecret is the Secret, created on the host and never
	// through CI or the repository (D43), that the operator Deployment alone
	// mounts, optionally, at dispatchTokenPath's directory: its key token is
	// the file. No Role grants a read of it: a mount is not an API read.
	dispatchTokenSecret = "operator-dispatch-token"
	// dispatchTokenPath is the token's file.
	dispatchTokenPath = "/var/run/secrets/mlobs/dispatch/token"

	// dispatchTimeout bounds each exchange with GitHub — the runs listed
	// before a dispatch, all of them, or the dispatch itself — so a GitHub
	// that never answers costs a reconcile at most that long, and never
	// wedges it.
	dispatchTimeout = 10 * time.Second
	// maxTokenBytes bounds the token's file. GitHub's tokens run to under a
	// hundred bytes.
	maxTokenBytes = 4 << 10
	// maxGitHubBodyBytes bounds a response body read, after the transport
	// has inflated it. A runs page of one run is a few tens of KiB.
	maxGitHubBodyBytes = 1 << 20
	// maxGitHubHeaderBytes bounds a response's headers, which from GitHub run
	// to a few KiB.
	maxGitHubHeaderBytes = 64 << 10
)

// pendingRunStatuses are the statuses a Deploy run is listed by before a
// dispatch, a run in any of them deferring it. Queued and waiting are D43's.
// Pending is a run held by deploy.yml's concurrency group, which keeps only
// the newest pending run: dispatching beside one would evict it. Requested is
// a run about to be queued. In progress is a deploy under way, which closes
// the window when its apply.sh runs; a dispatch beside it would wait behind
// it and land after the window, as an ordinary deploy (D47). GitHub's filter
// takes one status a request.
var pendingRunStatuses = []string{"queued", "waiting", "pending", "requested", "in_progress"}

var (
	// commitSHA is a full commit SHA, the one shape the Deploy workflow's sha
	// input and the CRD's tags take.
	commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// tokenShape is the characters a GitHub token is made of, and all a
	// header value built from one may hold.
	tokenShape = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// errNoDispatcher is a reconciler built without SetupWithManager, which has
// no token to read.
var errNoDispatcher = errors.New("no dispatcher: SetupWithManager wires the operator's")

// githubStatusError is a GitHub answer other than the one a call expects. It
// names the status by its code: the reason phrase the server sent is not
// carried into a log line or an Event.
type githubStatusError struct{ code int }

func (e *githubStatusError) Error() string {
	return fmt.Sprintf("GitHub answered %d %s", e.code, http.StatusText(e.code))
}

// dispatchClient is the client every GitHub call is made with, the only one a
// dispatcher outside this package's tests has: see the discipline above. It
// has no timeout of its own; each exchange's context carries dispatchTimeout.
var dispatchClient = newDispatchClient()

func newDispatchClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: maxGitHubHeaderBytes,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dispatcher makes the promote dispatch's GitHub calls.
type dispatcher struct {
	// baseURL is the REST API's: githubAPI unless the test-only knob moves
	// it.
	baseURL string
	// tokenPath is the token's file: dispatchTokenPath unless this package's
	// tests set another.
	tokenPath string
	// client overrides dispatchClient for this package's tests; nil means
	// dispatchClient.
	client *http.Client
}

func (d *dispatcher) httpClient() *http.Client {
	if d.client != nil {
		return d.client
	}
	return dispatchClient
}

// token reads the token from its file, bounded and trimmed. An absent file is
// an absent Secret, or one without the token key.
func (d *dispatcher) token() (string, error) {
	if d == nil {
		return "", errNoDispatcher
	}
	f, err := os.Open(d.tokenPath)
	if err != nil {
		return "", fmt.Errorf("the dispatch token is not readable: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("the dispatch token is not readable: %w", err)
	}
	if len(data) > maxTokenBytes {
		return "", fmt.Errorf("the dispatch token's file is longer than %d bytes", maxTokenBytes)
	}
	token := strings.TrimSpace(string(data))
	switch {
	case token == "":
		return "", errors.New("the dispatch token's file is empty")
	case !tokenShape.MatchString(token):
		return "", errors.New("the dispatch token's file holds a character no token has")
	}
	return token, nil
}

// tokenPresent reports whether a token can be read now.
func (d *dispatcher) tokenPresent() bool {
	_, err := d.token()
	return err == nil
}

// workflowURL is the deploy workflow's URL under the API, with path appended.
func (d *dispatcher) workflowURL(path string) string {
	return d.baseURL + "/repos/" + deployRepository + "/actions/workflows/" + deployWorkflow + path
}

// do sends req with token and GitHub's headers, and returns the response
// with its body bounded at maxGitHubBodyBytes.
func (d *dispatcher) do(req *http.Request, token string) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "mlobs-operator")
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	// MaxBytesReader is io.LimitReader that fails past its limit instead of
	// ending at it. Given no ResponseWriter, it only reads.
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxGitHubBodyBytes)
	return resp, nil
}

// pendingRun lists the Deploy runs in each of pendingRunStatuses, under one
// dispatchTimeout, and returns the first status that has any, empty when none
// does. Any list that fails fails it: a queued human deploy cannot then be
// ruled out.
func (d *dispatcher) pendingRun(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	for _, status := range pendingRunStatuses {
		n, err := d.countRuns(ctx, token, status)
		if err != nil {
			return "", fmt.Errorf("listing the %s Deploy runs: %w", status, err)
		}
		if n > 0 {
			return status, nil
		}
	}
	return "", nil
}

// countRuns is the number of Deploy runs in status: the total_count of a
// one-run page.
func (d *dispatcher) countRuns(ctx context.Context, token, status string) (int, error) {
	query := url.Values{"status": {status}, "per_page": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.workflowURL("/runs?"+query.Encode()), nil)
	if err != nil {
		return 0, err
	}
	resp, err := d.do(req, token)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, &githubStatusError{code: resp.StatusCode}
	}
	var page struct {
		TotalCount *int `json:"total_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return 0, fmt.Errorf("the runs page is unreadable: %w", err)
	}
	if page.TotalCount == nil || *page.TotalCount < 0 {
		return 0, errors.New("the runs page has no total_count")
	}
	return *page.TotalCount, nil
}

// dispatch sends the one workflow_dispatch: deploy.yml on main, its sha input
// sha. A sha that is not a full commit SHA is refused before any request is
// made, whatever validated it before. It returns the status GitHub answered
// with, 0 when none came, and an error unless the status is 2xx.
func (d *dispatcher) dispatch(ctx context.Context, token, sha string) (int, error) {
	if !commitSHA.MatchString(sha) {
		return 0, fmt.Errorf("the sha %q is not a 40-character lowercase hex commit SHA, and is not dispatched", sha)
	}
	body, err := json.Marshal(map[string]any{"ref": deployRef, "inputs": map[string]string{"sha": sha}})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.workflowURL("/dispatches"), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.do(req, token)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained, not read: under the pinned version a dispatch has
	// none, and a refusal's is not trusted into a status or an Event.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &githubStatusError{code: resp.StatusCode}
	}
	return resp.StatusCode, nil
}

// githubAPIFrom reads the test-only knob through lookup, os.LookupEnv in the
// operator. Unset, the API is githubAPI; set, it must be an http or https URL
// with a host and nothing past its path.
func githubAPIFrom(lookup func(string) (string, bool)) (string, error) {
	value, set := lookup(githubAPIEnv)
	if !set {
		return githubAPI, nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s=%q: the GitHub API must be an http or https URL with a host, and no user, "+
			"query or fragment", githubAPIEnv, value)
	}
	return strings.TrimSuffix(value, "/"), nil
}
