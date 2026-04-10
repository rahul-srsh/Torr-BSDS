"""
throughput_scaling.py — Experiment 2: throughput vs. relay count.

Ramps concurrency from 10 to 500 in steps of 10, holding each step for
STEP_DURATION seconds. All requests go through full 3-hop circuits.

Stops automatically when p95 latency exceeds P95_THRESHOLD_MS or the
error rate exceeds ERROR_RATE_THRESHOLD. Records the last step that stayed
within bounds as the max sustainable throughput for this relay count.

Environment variables:
  DIRECTORY_URL              Directory server base URL (required)
  ECHO_SERVER_URL            Full destination URL (required)
  RELAY_COUNT                Number of relay nodes in this run (default: 2)
                             Used only for S3 path labelling.
  MIN_USERS                  Starting concurrency (default: 10)
  MAX_USERS                  Maximum concurrency to attempt (default: 500)
  STEP_SIZE                  Users added per step (default: 10)
  STEP_DURATION              Seconds to hold each step (default: 30)
  P95_THRESHOLD_MS           p95 ceiling in ms (default: 500)
  ERROR_RATE_THRESHOLD       Error rate ceiling 0-1 (default: 0.01 = 1%)
  EXPERIMENT_RESULTS_BUCKET  S3 bucket for CSV upload (optional)
  S3_PREFIX                  Overrides default s3 prefix (optional)

Usage:
  RELAY_COUNT=2 locust -f throughput_scaling.py --headless \\
    --csv=results/exp2_relays2 --host $DIRECTORY_URL
"""

import csv
import io
import json
import logging
import os
import time
import threading

import boto3
import requests
from botocore.exceptions import BotoCoreError, ClientError
from locust import HttpUser, LoadTestShape, events, task, between
from locust.exception import RescheduleTask

import circuit_client as cc

log = logging.getLogger(__name__)

# ── Configuration ─────────────────────────────────────────────────────────────

DIRECTORY_URL   = os.getenv("DIRECTORY_URL", "")
ECHO_SERVER_URL = os.getenv("ECHO_SERVER_URL", "http://localhost:8080/echo")
RELAY_COUNT     = int(os.getenv("RELAY_COUNT", "2"))

MIN_USERS  = int(os.getenv("MIN_USERS",  "10"))
MAX_USERS  = int(os.getenv("MAX_USERS",  "500"))
STEP_SIZE  = int(os.getenv("STEP_SIZE",  "10"))
STEP_DURATION = int(os.getenv("STEP_DURATION", "30"))

P95_THRESHOLD_MS     = float(os.getenv("P95_THRESHOLD_MS",     "500"))
ERROR_RATE_THRESHOLD = float(os.getenv("ERROR_RATE_THRESHOLD", "0.01"))

S3_BUCKET = os.getenv("EXPERIMENT_RESULTS_BUCKET", "")
_default_prefix = f"experiment-2/relays-{RELAY_COUNT}"
S3_PREFIX = os.getenv("S3_PREFIX", _default_prefix).rstrip("/")

HOPS = 3

# ── Shared state ──────────────────────────────────────────────────────────────

# Filled in by the shape after each step; read by the quitting handler.
_step_records: list[dict] = []
_ceiling_record: dict | None = None
_ceiling_lock = threading.Lock()


# ── Load shape ────────────────────────────────────────────────────────────────

class ThroughputRampShape(LoadTestShape):
    """
    Ramps users from MIN_USERS to MAX_USERS in STEP_SIZE increments.
    After each STEP_DURATION window, samples live stats and checks thresholds.
    Stops when the ceiling is hit or MAX_USERS is reached.
    """

    def __init__(self):
        super().__init__()
        self._last_sampled_step = -1
        self._breached = False

    def tick(self):
        global _ceiling_record

        if self._breached:
            return None

        elapsed   = self.get_run_time()
        step_idx  = int(elapsed // STEP_DURATION)
        users     = MIN_USERS + step_idx * STEP_SIZE

        if users > MAX_USERS:
            return None

        # Sample once per step transition (after the first full step)
        if step_idx > 0 and step_idx != self._last_sampled_step:
            self._last_sampled_step = step_idx
            prev_users = MIN_USERS + (step_idx - 1) * STEP_SIZE
            self._sample_step(prev_users, elapsed)

            if self._breached:
                return None

        spawn_rate = max(STEP_SIZE, 10)
        return (users, spawn_rate)

    def _sample_step(self, users: int, elapsed: float):
        global _ceiling_record

        runner = self.runner
        if runner is None:
            return

        stats = runner.stats.total
        total_reqs = stats.num_requests + stats.num_failures
        if total_reqs == 0:
            return

        p95     = stats.get_response_time_percentile(0.95) or 0
        err_rate = stats.num_failures / total_reqs if total_reqs else 0
        rps     = stats.current_rps

        record = {
            "relay_count":  RELAY_COUNT,
            "users":        users,
            "elapsed_s":    round(elapsed, 1),
            "rps":          round(rps, 3),
            "p50_ms":       stats.get_response_time_percentile(0.50) or 0,
            "p95_ms":       round(p95, 1),
            "p99_ms":       stats.get_response_time_percentile(0.99) or 0,
            "requests":     stats.num_requests,
            "failures":     stats.num_failures,
            "error_rate":   round(err_rate, 4),
            "within_bounds": p95 <= P95_THRESHOLD_MS and err_rate <= ERROR_RATE_THRESHOLD,
        }

        with _ceiling_lock:
            _step_records.append(record)

            if p95 > P95_THRESHOLD_MS or err_rate > ERROR_RATE_THRESHOLD:
                log.warning(
                    "[exp2] threshold breached at %d users — p95=%.1fms err=%.2f%%",
                    users, p95, err_rate * 100,
                )
                self._breached = True
            else:
                _ceiling_record = record
                log.info(
                    "[exp2] step ok: %d users, %.1f rps, p95=%.1fms, err=%.2f%%",
                    users, rps, p95, err_rate * 100,
                )


# ── User ──────────────────────────────────────────────────────────────────────

class ThroughputScalingUser(HttpUser):
    wait_time = between(0.01, 0.05)
    host = DIRECTORY_URL or "http://localhost:8080"

    def on_start(self):
        self._session = requests.Session()

    @task
    def run_circuit(self):
        directory_url = DIRECTORY_URL or self.host
        circuit_id    = cc.new_circuit_id()

        exit_layer = {
            "url":     ECHO_SERVER_URL,
            "method":  "GET",
            "headers": {},
            "body":    None,
        }

        # ── Setup ─────────────────────────────────────────────────────────────
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
                name="3-hop",
                response_time=setup_ms,
                response_length=0,
                exception=exc,
                context={},
            )
            raise RescheduleTask() from exc

        setup_ms = int((time.monotonic() - t0) * 1000)
        events.request.fire(
            request_type="circuit_setup",
            name="3-hop",
            response_time=setup_ms,
            response_length=0,
            exception=None,
            context={},
        )

        # ── Request ───────────────────────────────────────────────────────────
        t1 = time.monotonic()
        try:
            payload    = cc.build_onion(guard_key, relay_key, exit_key, exit_layer, circuit, HOPS)
            guard_url  = cc._node_url(circuit["guard"])
            onion_resp = cc.send_onion(self._session, guard_url, circuit_id, payload)
            exit_resp  = cc.decrypt_response(
                guard_key, relay_key, exit_key, onion_resp["payload"], HOPS
            )
        except Exception as exc:
            req_ms = int((time.monotonic() - t1) * 1000)
            events.request.fire(
                request_type="circuit_request",
                name="3-hop",
                response_time=req_ms,
                response_length=0,
                exception=exc,
                context={},
            )
            raise RescheduleTask() from exc

        req_ms   = int((time.monotonic() - t1) * 1000)
        body_len = len(exit_resp.get("body", "") or "")

        # Log which relay handled this circuit for load-distribution audit.
        relay_id = circuit.get("relay", {}).get("nodeId", "unknown")
        log.debug("[exp2] circuit %s routed via relay %s", circuit_id, relay_id)

        events.request.fire(
            request_type="circuit_request",
            name="3-hop",
            response_time=req_ms,
            response_length=body_len,
            exception=None,
            context={},
        )


# ── S3 upload on run completion ───────────────────────────────────────────────

@events.quitting.add_listener
def upload_results_to_s3(environment, **kwargs):
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())

    with _ceiling_lock:
        steps   = list(_step_records)
        ceiling = _ceiling_record

    if ceiling:
        log.info(
            "[exp2] max sustainable throughput: %.1f rps at %d users "
            "(p95=%.1fms, relays=%d)",
            ceiling["rps"], ceiling["users"], ceiling["p95_ms"], RELAY_COUNT,
        )
    else:
        log.warning("[exp2] no step stayed within bounds — ceiling not determined")

    if not S3_BUCKET:
        log.info("[exp2] EXPERIMENT_RESULTS_BUCKET not set — skipping S3 upload")
        return

    s3 = boto3.client("s3")

    # Upload per-step CSV
    if steps:
        buf = io.StringIO()
        writer = csv.DictWriter(buf, fieldnames=steps[0].keys())
        writer.writeheader()
        writer.writerows(steps)
        key = f"{S3_PREFIX}/{timestamp}_steps.csv"
        try:
            s3.put_object(Bucket=S3_BUCKET, Key=key, Body=buf.getvalue().encode())
            log.info("[exp2] uploaded steps to s3://%s/%s", S3_BUCKET, key)
        except (BotoCoreError, ClientError) as exc:
            log.error("[exp2] S3 step upload failed: %s", exc)

    # Upload ceiling summary JSON
    summary = {
        "relay_count":           RELAY_COUNT,
        "timestamp":             timestamp,
        "ceiling":               ceiling,
        "p95_threshold_ms":      P95_THRESHOLD_MS,
        "error_rate_threshold":  ERROR_RATE_THRESHOLD,
        "total_steps_sampled":   len(steps),
    }
    key = f"{S3_PREFIX}/{timestamp}_summary.json"
    try:
        s3.put_object(
            Bucket=S3_BUCKET,
            Key=key,
            Body=json.dumps(summary, indent=2).encode(),
        )
        log.info("[exp2] uploaded summary to s3://%s/%s", S3_BUCKET, key)
    except (BotoCoreError, ClientError) as exc:
        log.error("[exp2] S3 summary upload failed: %s", exc)

    # Also upload the full Locust stats for this run
    stats = environment.runner.stats
    rows  = []
    for name, entry in stats.entries.items():
        rows.append({
            "timestamp":  timestamp,
            "relay_count": RELAY_COUNT,
            "name":        name[0],
            "method":      name[1],
            "requests":    entry.num_requests,
            "failures":    entry.num_failures,
            "rps":         round(entry.current_rps, 3),
            "p50_ms":      entry.get_response_time_percentile(0.50),
            "p95_ms":      entry.get_response_time_percentile(0.95),
            "p99_ms":      entry.get_response_time_percentile(0.99),
            "avg_ms":      round(entry.avg_response_time, 2),
        })

    if rows:
        buf = io.StringIO()
        writer = csv.DictWriter(buf, fieldnames=rows[0].keys())
        writer.writeheader()
        writer.writerows(rows)
        key = f"{S3_PREFIX}/{timestamp}_locust_stats.csv"
        try:
            s3.put_object(Bucket=S3_BUCKET, Key=key, Body=buf.getvalue().encode())
            log.info("[exp2] uploaded locust stats to s3://%s/%s", S3_BUCKET, key)
        except (BotoCoreError, ClientError) as exc:
            log.error("[exp2] S3 locust stats upload failed: %s", exc)
