"""Drift detection service (docs/PLAN.md §5).

Compares a frozen reference baseline against a sliding production window of
predictions: Chi-squared on class and token-length distributions, KL
divergence on the confidence distribution. Pure-Python math (no scipy/numpy)
against hard-coded critical values; results exported to Prometheus and
alerted to Slack on threshold breach.
"""
