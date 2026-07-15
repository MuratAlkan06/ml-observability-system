"""Host-side traffic simulator for the ML Observability System.

Runs on the host (not in a container) and drives the inference API by POSTing
``{"text": ...}`` to ``/predict`` at a configurable rate. ``--mode drift``
swaps the input corpus for one designed to trip all three drift tests
(docs/PLAN.md §5). The simulator is stdlib + ``httpx`` only; the
pydantic-settings config convention in §7 applies to the containerized
services, not to this host-side tool.
"""
