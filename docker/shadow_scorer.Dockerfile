# Shadow scorer image — v1.1 Slice A (mirrors docker/api.Dockerfile).
# Frozen rules: python:3.12-slim base; CPU-only torch installed BEFORE
# requirements/shadow_scorer.txt; candidate model baked at BUILD time with a
# label-map sanity assertion; no network at runtime (HF_HUB_OFFLINE=1).
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    HF_HOME=/opt/hf-cache

WORKDIR /app

# 1) CPU-only torch FIRST — the default Linux wheel bundles CUDA and blows the
#    RAM/disk budget for a t3.medium (docs/PLAN.md §1).
RUN pip install --no-cache-dir torch==2.13.0 --index-url https://download.pytorch.org/whl/cpu

# 2) Application requirements (torch==2.13.0 is already satisfied from step 1).
COPY requirements/shadow_scorer.txt requirements/shadow_scorer.txt
RUN pip install --no-cache-dir -r requirements/shadow_scorer.txt

# 3) Application code.
COPY src/shadow_scorer src/shadow_scorer

# 4) Bake the candidate model at build time (network available here). This step
#    (a) retires the label-map risk — two sanity probes must map to
#    positive/negative or the build FAILS; (b) retires the transformers-v5 .bin
#    risk — if the Hub pickle will not load it is converted once to safetensors
#    under /opt/hf-model. The build fails loudly if the model cannot be prepared.
RUN mkdir -p /opt/hf-model && python -m src.shadow_scorer.bake_model

# 5) Enforce offline at runtime — the baked snapshot / converted dir is the only source.
ENV HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1

# 6) Drop root (mirrors api.Dockerfile). The baked caches stay root-owned;
#    `a+rX` adds read + directory-traverse only (never write), so the non-root
#    user loads the snapshot offline without weakening its read-only posture.
#    HOME points at a writable dir because transformers/huggingface_hub may touch
#    lock/cache files under $HOME even when HF_HUB_OFFLINE=1.
RUN useradd --system --no-create-home --user-group app \
    && chmod -R a+rX /opt/hf-cache /opt/hf-model
ENV HOME=/tmp
USER app

# Metrics only (scraped over the compose network; never published off-box).
EXPOSE 9110

CMD ["python", "-m", "src.shadow_scorer"]
