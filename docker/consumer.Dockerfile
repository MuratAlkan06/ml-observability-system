# Consumer service image (docs/PLAN.md §1, §7). Build context = repo root.
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

COPY requirements/consumer.txt requirements/consumer.txt
RUN pip install --no-cache-dir -r requirements/consumer.txt

COPY src/consumer src/consumer

# Drop root (security review N1), mirroring api/drift.Dockerfile. The consumer
# only reads Redis/Postgres and serves metrics on 9108 (>1024, so no privileged
# bind); no writable paths needed beyond its home.
RUN useradd --create-home --shell /usr/sbin/nologin consumer
USER consumer

CMD ["python", "-m", "src.consumer"]
