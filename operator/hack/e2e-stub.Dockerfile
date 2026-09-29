# The stub the operator's end-to-end check runs in place of the torch api
# (docs/PLAN.md D35): it answers GET /health on 8000 — the probes the
# operator's api Deployment carries — and nothing else. The shell traps TERM
# so a pod goes as soon as it is told to, which the pause and resume steps
# of a canary window wait on. Built from stdin, with no context:
#   docker build -t mlobs-stub:e2e - < operator/hack/e2e-stub.Dockerfile
FROM busybox:1.37
RUN mkdir -p /www && echo ok > /www/health
EXPOSE 8000
CMD ["sh", "-c", "trap 'exit 0' TERM; httpd -f -p 8000 -h /www & wait"]
