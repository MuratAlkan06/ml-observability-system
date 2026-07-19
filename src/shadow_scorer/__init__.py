"""Shadow second-model scorer (v1.1 Slice A).

Joins the ``mlobs:predictions`` stream with its own consumer group, re-scores
each event's ``text`` with the candidate model, and writes one row per event to
``shadow_predictions``. Reuses the primary path's at-least-once + request_id
idempotency machinery with zero impact on the primary prediction path.
"""
