"""
circuit_3hop.py — Experiment 1: 3-hop circuit load test.

Each Locust user builds its own circuit per request:
  client → guard → relay → exit → destination

Circuit setup time and request time are recorded as separate Locust
events so they can be compared independently.

Environment variables:
  DIRECTORY_URL              Directory server base URL (required)
  ECHO_SERVER_URL            Full destination URL (required)
                             e.g. http://10.0.1.x:8080/echo
  CONCURRENCY_LEVELS         Comma-separated user counts (default: 10,50,100,200)
  STAGE_DURATION             Seconds per level (default: 60)
  EXPERIMENT_RESULTS_BUCKET  S3 bucket for CSV upload (optional)
  S3_PREFIX                  S3 key prefix (default: experiment-1/3-hop)

Usage:
  locust -f circuit_3hop.py --headless --csv=results/3hop \\
         --host $DIRECTORY_URL
"""

import csv
import io
import logging
import os
import time

import boto3
import requests
from botocore.exceptions import BotoCoreError, ClientError
from locust import HttpUser, LoadTestShape, events, task, between
from locust.exception import RescheduleTask

import circuit_client as cc

log = logging.getLogger(__name__)

DIRECTORY_URL = os.getenv("DIRECTORY_URL", "")
ECHO_SERVER_URL = os.getenv("ECHO_SERVER_URL", "http://localhost:8080/echo")

_raw_levels = os.getenv("CONCURRENCY_LEVELS", "10,50,100,200")
CONCURRENCY_LEVELS = [int(x.strip()) for x in _raw_levels.split(",")]
STAGE_DURATION = int(os.getenv("STAGE_DURATION", "60"))

S3_BUCKET = os.getenv("EXPERIMENT_RESULTS_BUCKET", "")
S3_PREFIX = os.getenv("S3_PREFIX", "experiment-1/3-hop").rstrip("/")

HOPS = 3


# ── Load shape ────────────────────────────────────────────────────────────────

class SteppedLoadShape(LoadTestShape):
    def tick(self):
        elapsed = self.get_run_time()
        for i, users in enumerate(CONCURRENCY_LEVELS):
            if elapsed < (i + 1) * STAGE_DURATION:
                return (users, max(users, 10))
        return None


# ── User ──────────────────────────────────────────────────────────────────────

class CircuitThreeHopUser(HttpUser):
    """
    Each user builds a fresh 3-hop circuit per task.
    Uses a plain requests.Session so it can talk to directory server,
    guard, relay, and exit nodes directly.
    """
    wait_time = between(0.01, 0.05)
    host = DIRECTORY_URL or "http://localhost:8080"

    def on_start(self):
        self._session = requests.Session()

    @task
    def run_circuit(self):
        directory_url = DIRECTORY_URL or self.host
        circuit_id = cc.new_circuit_id()

        exit_layer = {
            "url": ECHO_SERVER_URL,
            "method": "GET",
            "headers": {},
            "body": None,
        }

        # ── Setup phase ───────────────────────────────────────────────────────
        t0 = time.monotonic()
        try:
            circuit = cc.get_circuit(self._session, directory_url, HOPS)
            guard_key, relay_key, exit_key = cc.setup_circuit(
                self._session, circuit, circuit_id, HOPS
            )
        except Exception as exc:
            setup_ms = int((time.monotonic() - t0) * 1000)
            events.request.fire(
                request_type="circuit_setup",
                name=f"{HOPS}-hop",
                response_time=setup_ms,
                response_length=0,
                exception=exc,
                context={},
            )
            raise RescheduleTask() from exc

        setup_ms = int((time.monotonic() - t0) * 1000)
        events.request.fire(
            request_type="circuit_setup",
            name=f"{HOPS}-hop",
            response_time=setup_ms,
            response_length=0,
            exception=None,
            context={},
        )

        # ── Request phase ─────────────────────────────────────────────────────
        t1 = time.monotonic()
        try:
            payload = cc.build_onion(guard_key, relay_key, exit_key, exit_layer, circuit, HOPS)
            guard_url = cc._node_url(circuit["guard"])
            onion_resp = cc.send_onion(self._session, guard_url, circuit_id, payload)
            exit_resp = cc.decrypt_response(
                guard_key, relay_key, exit_key, onion_resp["payload"], HOPS
            )
        except Exception as exc:
            req_ms = int((time.monotonic() - t1) * 1000)
            events.request.fire(
                request_type="circuit_request",
                name=f"{HOPS}-hop",
                response_time=req_ms,
                response_length=0,
                exception=exc,
                context={},
            )
            raise RescheduleTask() from exc

        req_ms = int((time.monotonic() - t1) * 1000)
        body_len = len(exit_resp.get("body", "") or "")
        events.request.fire(
            request_type="circuit_request",
            name=f"{HOPS}-hop",
            response_time=req_ms,
            response_length=body_len,
            exception=None,
            context={},
        )


# ── S3 upload on run completion ───────────────────────────────────────────────

@events.quitting.add_listener
def upload_results_to_s3(environment, **kwargs):
    if not S3_BUCKET:
        log.info("[3hop] EXPERIMENT_RESULTS_BUCKET not set — skipping S3 upload")
        return

    stats = environment.runner.stats
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())

    rows = []
    for name, entry in stats.entries.items():
        rows.append({
            "timestamp": timestamp,
            "scenario": "3-hop",
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
        log.warning("[3hop] no stats to upload")
        return

    buf = io.StringIO()
    writer = csv.DictWriter(buf, fieldnames=rows[0].keys())
    writer.writeheader()
    writer.writerows(rows)

    key = f"{S3_PREFIX}/{timestamp}_stats.csv"
    try:
        s3 = boto3.client("s3")
        s3.put_object(Bucket=S3_BUCKET, Key=key, Body=buf.getvalue().encode())
        log.info("[3hop] uploaded results to s3://%s/%s", S3_BUCKET, key)
    except (BotoCoreError, ClientError) as exc:
        log.error("[3hop] S3 upload failed: %s", exc)
