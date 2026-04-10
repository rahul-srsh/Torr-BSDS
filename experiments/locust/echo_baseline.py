"""
echo_baseline.py — Experiment 1 direct baseline load test.

Hits the echo server directly (no circuit, no encryption).
Drives four concurrency stages automatically via LoadTestShape.

Environment variables:
  TARGET_URL             Echo server base URL (required)
  TARGET_PATH            Path to hit (default: /echo)
  REQUEST_METHOD         GET or POST (default: GET)
  PAYLOAD                POST body (default: baseline)
  CONCURRENCY_LEVELS     Comma-separated user counts (default: 10,50,100,200)
  STAGE_DURATION         Seconds to hold each level (default: 60)
  EXPERIMENT_RESULTS_BUCKET  S3 bucket for CSV upload (optional)
  S3_PREFIX              S3 key prefix (default: experiment-1/direct)

Usage:
  locust -f echo_baseline.py --headless --csv=results/direct \\
         --host $TARGET_URL
"""

import csv
import io
import logging
import os
import time

import boto3
from botocore.exceptions import BotoCoreError, ClientError
from locust import HttpUser, LoadTestShape, events, task, between

log = logging.getLogger(__name__)

# ── Configuration ─────────────────────────────────────────────────────────────

TARGET_PATH = os.getenv("TARGET_PATH", "/echo")
REQUEST_METHOD = os.getenv("REQUEST_METHOD", "GET").upper()
PAYLOAD = os.getenv("PAYLOAD", "baseline")

_raw_levels = os.getenv("CONCURRENCY_LEVELS", "10,50,100,200")
CONCURRENCY_LEVELS = [int(x.strip()) for x in _raw_levels.split(",")]
STAGE_DURATION = int(os.getenv("STAGE_DURATION", "60"))

S3_BUCKET = os.getenv("EXPERIMENT_RESULTS_BUCKET", "")
S3_PREFIX = os.getenv("S3_PREFIX", "experiment-1/direct").rstrip("/")


# ── Load shape ────────────────────────────────────────────────────────────────

class SteppedLoadShape(LoadTestShape):
    """
    Drives concurrency through CONCURRENCY_LEVELS, holding each level for
    STAGE_DURATION seconds, then stops.
    """

    def tick(self):
        elapsed = self.get_run_time()
        for i, users in enumerate(CONCURRENCY_LEVELS):
            stage_start = i * STAGE_DURATION
            stage_end = (i + 1) * STAGE_DURATION
            if elapsed < stage_end:
                spawn_rate = max(users, 10)
                return (users, spawn_rate)
        return None  # all stages done — stop


# ── User ──────────────────────────────────────────────────────────────────────

class EchoBaselineUser(HttpUser):
    wait_time = between(0.01, 0.05)

    @task
    def hit_target(self):
        if REQUEST_METHOD == "POST":
            self.client.post(
                TARGET_PATH,
                data=PAYLOAD,
                headers={"Content-Type": "text/plain"},
                name=TARGET_PATH,
            )
        else:
            self.client.get(TARGET_PATH, name=TARGET_PATH)


# ── S3 upload on run completion ───────────────────────────────────────────────

@events.quitting.add_listener
def upload_results_to_s3(environment, **kwargs):
    if not S3_BUCKET:
        log.info("[baseline] EXPERIMENT_RESULTS_BUCKET not set — skipping S3 upload")
        return

    stats = environment.runner.stats
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())

    rows = []
    for name, entry in stats.entries.items():
        rows.append({
            "timestamp": timestamp,
            "scenario": "direct",
            "name": name[0],
            "method": name[1],
            "requests": entry.num_requests,
            "failures": entry.num_failures,
            "rps": round(entry.current_rps, 3),
            "p50_ms": entry.get_response_time_percentile(0.50),
            "p95_ms": entry.get_response_time_percentile(0.95),
            "p99_ms": entry.get_response_time_percentile(0.99),
            "avg_ms": round(entry.avg_response_time, 2),
        })

    if not rows:
        log.warning("[baseline] no stats to upload")
        return

    buf = io.StringIO()
    writer = csv.DictWriter(buf, fieldnames=rows[0].keys())
    writer.writeheader()
    writer.writerows(rows)

    key = f"{S3_PREFIX}/{timestamp}_stats.csv"
    try:
        s3 = boto3.client("s3")
        s3.put_object(Bucket=S3_BUCKET, Key=key, Body=buf.getvalue().encode())
        log.info("[baseline] uploaded results to s3://%s/%s", S3_BUCKET, key)
    except (BotoCoreError, ClientError) as exc:
        log.error("[baseline] S3 upload failed: %s", exc)
