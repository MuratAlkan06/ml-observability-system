"""Inference service (docs/PLAN.md §2, producer half of §3, API rows of §6).

Self-hosted DistilBERT SST-2 sentiment API: ``POST /predict``, ``GET /health``,
``GET /metrics``. Owns the Redis Streams producer and the API metric inventory.
"""
