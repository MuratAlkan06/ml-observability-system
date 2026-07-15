"""FastAPI application for the inference service (docs/PLAN.md §2, §3, §6).

``create_app`` is a factory so tests can inject a fake model loader and a fake
redis client; ``app`` is the module-level instance uvicorn runs in the container.
"""

from __future__ import annotations

import json
import logging
import time
import uuid
from contextlib import asynccontextmanager
from typing import Any, Callable, Optional

from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from . import metrics
from .config import Settings
from .model import Model, load_model
from .producer import PredictionProducer
from .schemas import HealthResponse, PredictRequest, PredictResponse

logger = logging.getLogger("inference_service")

_TRACKED_ENDPOINTS = ("/predict", "/health")


class _JsonFormatter(logging.Formatter):
    """Minimal structured (JSON) log formatter."""

    def format(self, record: logging.LogRecord) -> str:
        payload = {
            "ts": self.formatTime(record, "%Y-%m-%dT%H:%M:%S%z"),
            "level": record.levelname,
            "logger": record.name,
            "msg": record.getMessage(),
        }
        if record.exc_info:
            payload["exc"] = self.formatException(record.exc_info)
        return json.dumps(payload)


def _configure_logging(level: str) -> None:
    handler = logging.StreamHandler()
    handler.setFormatter(_JsonFormatter())
    root = logging.getLogger()
    root.handlers = [handler]
    root.setLevel(level.upper())


def _default_redis_client(settings: Settings) -> Any:
    import redis

    return redis.Redis.from_url(
        settings.redis_url,
        socket_timeout=settings.redis_timeout_seconds,
        socket_connect_timeout=settings.redis_timeout_seconds,
    )


def create_app(
    settings: Optional[Settings] = None,
    *,
    model_loader: Callable[[Settings], Model] = load_model,
    redis_factory: Callable[[Settings], Any] = _default_redis_client,
) -> FastAPI:
    settings = settings or Settings()
    _configure_logging(settings.log_level)

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        client = redis_factory(settings)
        app.state.producer = PredictionProducer(
            client, settings.stream_name, settings.stream_maxlen
        )
        try:
            app.state.model = model_loader(settings)
            metrics.MODEL_LOADED.labels(model_version=settings.model_version).set(1)
        except Exception:
            app.state.model = None
            metrics.MODEL_LOADED.labels(model_version=settings.model_version).set(0)
            logger.exception("model failed to load; /predict will return 503")
        try:
            yield
        finally:
            app.state.producer.close()

    app = FastAPI(title="mlobs inference service", lifespan=lifespan)
    app.state.settings = settings

    @app.middleware("http")
    async def _observe(request: Request, call_next: Callable):
        path = request.url.path
        tracked = path in _TRACKED_ENDPOINTS
        in_flight = path == "/predict"
        if in_flight:
            metrics.HTTP_REQUESTS_IN_FLIGHT.inc()
        start = time.perf_counter()
        status = "500"
        try:
            response = await call_next(request)
            status = str(response.status_code)
            return response
        finally:
            if in_flight:
                metrics.HTTP_REQUESTS_IN_FLIGHT.dec()
            if tracked:
                metrics.HTTP_REQUESTS.labels(
                    endpoint=path, method=request.method, status=status
                ).inc()
                metrics.HTTP_REQUEST_DURATION_SECONDS.labels(endpoint=path).observe(
                    time.perf_counter() - start
                )

    @app.exception_handler(Exception)
    async def _unhandled(request: Request, exc: Exception) -> JSONResponse:
        logger.exception("unhandled error on %s", request.url.path)
        return JSONResponse(status_code=500, content={"detail": "internal_error"})

    @app.post("/predict")
    async def predict(payload: PredictRequest, request: Request) -> Response:
        model: Optional[Model] = getattr(request.app.state, "model", None)
        if model is None:
            return JSONResponse(status_code=503, content={"detail": "model_not_loaded"})

        try:
            label, confidence, token_count, latency_ms = model.predict(payload.text)
        except Exception:
            logger.exception("inference failed")
            return JSONResponse(status_code=500, content={"detail": "internal_error"})

        confidence = round(confidence, 6)
        latency_ms = round(latency_ms, 2)
        request_id = str(uuid.uuid4())

        metrics.PREDICTIONS.labels(label=label).inc()
        metrics.PREDICTION_CONFIDENCE_RATIO.observe(confidence)
        metrics.INFERENCE_DURATION_SECONDS.observe(latency_ms / 1000.0)

        event = {
            "request_id": request_id,
            "ts_ms": str(int(time.time() * 1000)),
            "text": payload.text,
            "token_count": str(token_count),
            "label": label,
            "confidence": f"{confidence:.6f}",
            "model_version": model.model_version,
            "latency_ms": f"{latency_ms:.2f}",
        }
        request.app.state.producer.publish(event)

        return JSONResponse(
            content=PredictResponse(
                request_id=request_id,
                label=label,
                confidence=confidence,
                model_version=model.model_version,
                latency_ms=latency_ms,
            ).model_dump()
        )

    @app.get("/health")
    async def health(request: Request) -> JSONResponse:
        cfg: Settings = request.app.state.settings
        model_loaded = getattr(request.app.state, "model", None) is not None
        redis_connected = request.app.state.producer.ping()
        if not model_loaded:
            status, code = "unavailable", 503
        elif not redis_connected:
            status, code = "degraded", 200
        else:
            status, code = "ok", 200
        body = HealthResponse(
            status=status,
            model_loaded=model_loaded,
            redis_connected=redis_connected,
            model_version=cfg.model_version,
        )
        return JSONResponse(status_code=code, content=body.model_dump())

    @app.get("/metrics")
    async def metrics_endpoint() -> Response:
        return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)

    return app


app = create_app()
