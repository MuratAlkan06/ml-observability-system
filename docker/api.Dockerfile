# Inference service image — docs/PLAN.md §1, §2.
# Frozen rules: python:3.12-slim base; CPU-only torch installed BEFORE
# requirements/api.txt; HF model snapshot baked at BUILD time into HF_HOME;
# no network at runtime (HF_HUB_OFFLINE=1).
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
COPY requirements/api.txt requirements/api.txt
RUN pip install --no-cache-dir -r requirements/api.txt

# 3) Application code.
COPY src/inference_service/ inference_service/

# 4) Bake the pinned model snapshot into HF_HOME at build time (network available
#    here; the build fails loudly if the download does not complete).
RUN python -m inference_service.bake_model

# 5) Enforce offline at runtime — the baked snapshot is the only source.
ENV HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1

# 6) Drop root (security review F2). The baked HF cache stays root-owned; `a+rX`
#    only adds read (and directory traverse) — never write — so the non-root user
#    can load the snapshot offline without weakening its read-only posture. HOME
#    points at a writable dir because transformers/huggingface_hub may touch
#    lock/cache files under $HOME even when HF_HUB_OFFLINE=1.
RUN useradd --system --no-create-home --user-group app \
    && chmod -R a+rX /opt/hf-cache
ENV HOME=/tmp
USER app

EXPOSE 8000

# Single uvicorn worker (docs/PLAN.md §0 constraint).
CMD ["uvicorn", "inference_service.app:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]
