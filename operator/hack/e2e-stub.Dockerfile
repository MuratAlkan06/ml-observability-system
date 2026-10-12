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
# resume steps of a canary window wait on. Built from stdin, with no context:
#   docker build -t mlobs-stub:e2e - < operator/hack/e2e-stub.Dockerfile
FROM busybox:1.37
RUN mkdir -p /www /usr/local/bin \
  && echo ok > /www/health \
  && echo '10 0 0' > /www/profile \
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
