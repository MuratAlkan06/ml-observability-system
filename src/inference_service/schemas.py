"""Pydantic request/response schemas for the HTTP API (docs/PLAN.md §2).

Frozen contract: request rejects unknown fields (``extra="forbid"``), text is
required, 1..1000 chars, and must contain at least one non-whitespace character.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field, field_validator


class PredictRequest(BaseModel):
    """``POST /predict`` request body."""

    model_config = ConfigDict(extra="forbid")

    text: str = Field(..., min_length=1, max_length=1000)

    @field_validator("text")
    @classmethod
    def _non_whitespace(cls, value: str) -> str:
        if value.strip() == "":
            raise ValueError("text must contain at least one non-whitespace character")
        return value


class PredictResponse(BaseModel):
    """``POST /predict`` 200 response (all fields non-nullable)."""

    model_config = ConfigDict(protected_namespaces=())

    request_id: str
    label: str
    confidence: float
    model_version: str
    latency_ms: float


class HealthResponse(BaseModel):
    """``GET /health`` response body."""

    model_config = ConfigDict(protected_namespaces=())

    status: str
    model_loaded: bool
    redis_connected: bool
    model_version: str
