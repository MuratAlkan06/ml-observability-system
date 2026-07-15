# Consumer service image (docs/PLAN.md §1, §7). Build context = repo root.
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

COPY requirements/consumer.txt requirements/consumer.txt
RUN pip install --no-cache-dir -r requirements/consumer.txt

COPY src/consumer src/consumer

CMD ["python", "-m", "src.consumer"]
