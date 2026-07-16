# Drift detection service (docs/PLAN.md §5) — python:3.12-slim per PLAN §1.
# Build from the repo root: docker build -f docker/drift.Dockerfile .
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

COPY requirements/drift.txt /tmp/requirements-drift.txt
RUN pip install --no-cache-dir -r /tmp/requirements-drift.txt

COPY src/drift/ /app/src/drift/

# No secrets baked in: SLACK_WEBHOOK_URL comes from the environment at run
# time; baseline.json is mounted read-only by docker-compose.
RUN useradd --create-home --shell /usr/sbin/nologin drift
USER drift

EXPOSE 9109

CMD ["python", "-m", "src.drift"]
