# Baseline Experiment Files

This folder contains the code and scripts for the initial baseline measurements
before full onion routing is implemented.

## Included files

- `locust/echo_baseline.py`
  A parameterized Locust workload for either:
  - direct echo requests
  - one-hop forwarding requests through `guard-node`

- `scripts/extract_locust_metrics.py`
  Reads Locust `*_stats.csv` output and appends a single summary row with
  p50/p95/p99 latency into `experiments/results/summary.csv`.

- `scripts/generate_latency_charts.py`
  Turns `summary.csv` into chart PNGs in `docs/charts/`.

## Expected output files

- `experiments/results/<scenario>-c<users>_stats.csv`
- `experiments/results/<scenario>-c<users>_stats_history.csv`
- `experiments/results/<scenario>-c<users>_failures.csv`
- `experiments/results/summary.csv`
- `docs/charts/direct-latency-vs-concurrency.png`
- `docs/charts/one-hop-latency-vs-concurrency.png`
- `docs/charts/direct-vs-one-hop-p95.png`

## S3 prefixes

Recommended upload layout in the experiment-results bucket:

- `experiment-1/locust/`
- `experiment-1/summaries/`
- `charts/`
