# The stub the operator's end-to-end check runs in place of the torch api
# (docs/PLAN.md D35). It answers GET /health on 8000 — the probes the
# operator's api Deployment carries — and GET /metrics, a page holding the two
# /predict series the gate reads (D41) in the api's exposition shape, and
# nothing else. It serves no request: a generator adds to the /predict
# counters once a second what /www/profile names, three counts a second —
# fast requests (between 0.1 s and 0.25 s), slow ones (between 0.25 s and
# 0.5 s) and 5xx (fast) — so operator/hack/e2e.sh shapes each pod's request,
# 5xx and histogram bucket changes, and through them the p95 ratio, by writing
# that file into the pod (kubectl exec). The default, 10 fast requests a
# second, is a clean pod. While /www/hang exists, /metrics is a FIFO nothing
# writes: a read of it never answers, while /health still does. The shell
# traps TERM so a pod goes as soon as it is told to, which the pause and
# resume steps of a canary window wait on.
#
# The same image is the e2e's stand-in for GitHub's REST API (D43), through
# httpd's CGI: /cgi-bin/github answers the two calls the operator's promote
# dispatch makes, under the path the test-only knob MLOBS_E2E_GITHUB_API_URL
# points it at. A Deploy runs list is answered with the count /www/github-runs
# holds, 0 unless operator/hack/e2e.sh writes another, for every status; a
# dispatch of deploy.yml with 204, GitHub's answer under the API version the
# operator pins; anything else with 404. Every request is appended to
# /tmp/github-requests — method, path and query, the Authorization header,
# the body — for the e2e to read. Built from stdin, with no context:
#   docker build -t mlobs-stub:e2e - < operator/hack/e2e-stub.Dockerfile
FROM busybox:1.37
RUN mkdir -p /www/cgi-bin /usr/local/bin \
  && echo ok > /www/health \
  && echo '10 0 0' > /www/profile \
  && echo 0 > /www/github-runs \
  && printf '%s\n' \
    '#!/bin/sh' \
    '# github: see operator/hack/e2e-stub.Dockerfile.' \
    'workflow=/repos/MuratAlkan06/ml-observability-system/actions/workflows/deploy.yml' \
    'body=""' \
    'if [ "${CONTENT_LENGTH:-0}" -gt 0 ]; then body="$(head -c "$CONTENT_LENGTH")"; fi' \
    'echo "${REQUEST_METHOD} ${PATH_INFO}?${QUERY_STRING} ${HTTP_AUTHORIZATION:-none} ${body}" >> /tmp/github-requests' \
    'case "${REQUEST_METHOD} ${PATH_INFO}" in' \
    '  "GET ${workflow}/runs")' \
    '    printf "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n{\"total_count\":%s,\"workflow_runs\":[]}\n" "$(cat /www/github-runs)" ;;' \
    '  "POST ${workflow}/dispatches")' \
    '    printf "HTTP/1.0 204 No Content\r\n\r\n" ;;' \
    '  *)' \
    '    printf "HTTP/1.0 404 Not Found\r\n\r\n" ;;' \
    'esac' \
    > /www/cgi-bin/github \
  && chmod 0755 /www/cgi-bin/github \
  && printf '%s\n' \
    '#!/bin/sh' \
    '# stub-metrics: see operator/hack/e2e-stub.Dockerfile.' \
    'fast=0 slow=0 errors=0' \
    'publish() {' \
    '  n=$((fast + slow + errors))' \
    '  {' \
    '    echo "mlobs_http_requests_total{endpoint=\"/predict\",method=\"POST\",status=\"200\"} $((fast + slow))"' \
    '    echo "mlobs_http_requests_total{endpoint=\"/predict\",method=\"POST\",status=\"500\"} ${errors}"' \
    '    echo "mlobs_http_request_duration_seconds_bucket{endpoint=\"/predict\",le=\"0.1\"} 0"' \
    '    echo "mlobs_http_request_duration_seconds_bucket{endpoint=\"/predict\",le=\"0.25\"} $((fast + errors))"' \
    '    echo "mlobs_http_request_duration_seconds_bucket{endpoint=\"/predict\",le=\"0.5\"} ${n}"' \
    '    echo "mlobs_http_request_duration_seconds_bucket{endpoint=\"/predict\",le=\"+Inf\"} ${n}"' \
    '  } > /tmp/metrics' \
    '  mv -f /tmp/metrics /www/metrics' \
    '}' \
    'publish' \
    'httpd -f -p 8000 -h /www &' \
    'while sleep 1; do' \
    '  f=0 s=0 e=0' \
    '  read -r f s e < /www/profile' \
    '  case "${f}${s}${e}" in *[!0-9]*) f=0 s=0 e=0 ;; esac' \
    '  fast=$((fast + ${f:-0})) slow=$((slow + ${s:-0})) errors=$((errors + ${e:-0}))' \
    '  if [ -e /www/hang ]; then' \
    '    [ -p /www/metrics ] || { rm -f /www/metrics; mkfifo /www/metrics; }' \
    '  else' \
    '    publish' \
    '  fi' \
    'done' \
    > /usr/local/bin/stub-metrics \
  && chmod 0755 /usr/local/bin/stub-metrics
EXPOSE 8000
# stub-metrics writes the first page before it starts httpd, so no read finds
# /metrics missing.
CMD ["sh", "-c", "trap 'exit 0' TERM; stub-metrics & wait"]
