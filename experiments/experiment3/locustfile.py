"""
locustfile.py — Experiment 3: Failure & Recovery load test.

Runs at steady concurrency and logs per-request metrics including circuit
rebuild events. Designed to coordinate with the failure injection script
(kill happens at ~60s mark).

Each request is logged to CSV with columns:
  timestamp, status, latency_ms, circuit_rebuilt, rebuild_duration_ms

Environment variables:
  DIRECTORY_URL              Directory server base URL (required)
  ECHO_SERVER_URL            Full destination URL (required)
  HOPS                       Hop count: 1 or 3 (default: 3)
  CONCURRENCY                Number of users (default: 50)
  RUN_DURATION               Total run duration in seconds (default: 300)
  HEALTH_CHECK               "true" to enable pre-detection polling (default: "false")
  HEALTH_CHECK_INTERVAL      Seconds between health polls (default: 5)
  EXPERIMENT_RESULTS_BUCKET  S3 bucket for CSV upload (optional)
  S3_PREFIX                  S3 key prefix (default: experiment-3)

Usage:
  # Without pre-detection:
  HEALTH_CHECK=false locust -f locustfile.py --headless \\
    --users 50 --spawn-rate 50 --run-time 300s \\
    --csv=results/no-predetection --host $DIRECTORY_URL

  # With pre-detection:
  HEALTH_CHECK=true locust -f locustfile.py --headless \\
    --users 50 --spawn-rate 50 --run-time 300s \\
    --csv=results/with-predetection --host $DIRECTORY_URL
"""

import csv
import io
import json
import logging
import os
import threading
import time

import boto3
import requests
from botocore.exceptions import BotoCoreError, ClientError
from locust import HttpUser, LoadTestShape, events, task, between
from locust.exception import RescheduleTask

# Import the shared circuit client from the locust directory.
import sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "locust"))
import circuit_client as cc

log = logging.getLogger(__name__)

# ── Configuration ────────────────────────────────────────────────────────────

DIRECTORY_URL = os.getenv("DIRECTORY_URL", "")
ECHO_SERVER_URL = os.getenv("ECHO_SERVER_URL", "http://localhost:8080/echo")
HOPS = int(os.getenv("HOPS", "3"))
CONCURRENCY = int(os.getenv("CONCURRENCY", "50"))
RUN_DURATION = int(os.getenv("RUN_DURATION", "300"))
HEALTH_CHECK = os.getenv("HEALTH_CHECK", "false").lower() == "true"
HEALTH_CHECK_INTERVAL = int(os.getenv("HEALTH_CHECK_INTERVAL", "5"))

S3_BUCKET = os.getenv("EXPERIMENT_RESULTS_BUCKET", "")
S3_PREFIX = os.getenv("S3_PREFIX", "experiment-3").rstrip("/")

# ── Shared CSV writer ────────────────────────────────────────────────────────

_csv_lock = threading.Lock()
_csv_rows = []

CSV_FIELDS = [
    "timestamp",
    "status",
    "latency_ms",
    "circuit_rebuilt",
    "rebuild_duration_ms",
]


def log_csv_row(status, latency_ms, circuit_rebuilt, rebuild_duration_ms):
    row = {
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()),
        "status": status,
        "latency_ms": int(latency_ms),
        "circuit_rebuilt": circuit_rebuilt,
        "rebuild_duration_ms": int(rebuild_duration_ms),
    }
    with _csv_lock:
        _csv_rows.append(row)


# ── Health-check pre-detection ───────────────────────────────────────────────

class HealthChecker:
    """Polls the directory server for healthy nodes and detects circuit staleness."""

    def __init__(self, session, directory_url, interval=5):
        self._session = session
        self._directory_url = directory_url
        self._interval = interval
        self._stop = threading.Event()
        self._thread = None
        self._current_node_ids = set()
        self._lock = threading.Lock()
        self._needs_rebuild = False

    def set_circuit_nodes(self, node_ids):
        with self._lock:
            self._current_node_ids = set(node_ids)
            self._needs_rebuild = False

    def check_needs_rebuild(self):
        with self._lock:
            return self._needs_rebuild

    def start(self):
        self._thread = threading.Thread(target=self._poll_loop, daemon=True)
        self._thread.start()

    def stop(self):
        self._stop.set()

    def _poll_loop(self):
        while not self._stop.wait(self._interval):
            try:
                resp = self._session.get(
                    f"{self._directory_url}/nodes", timeout=10
                )
                resp.raise_for_status()
                nodes = resp.json()
                healthy_ids = set()
                for node_type in ("guard", "relay", "exit"):
                    for node in nodes.get(node_type, []):
                        healthy_ids.add(node.get("nodeId", ""))

                with self._lock:
                    if self._current_node_ids and not self._current_node_ids.issubset(healthy_ids):
                        missing = self._current_node_ids - healthy_ids
                        log.warning(
                            "[health-check] nodes no longer healthy: %s",
                            missing,
                        )
                        self._needs_rebuild = True
            except Exception as exc:
                log.error("[health-check] poll failed: %s", exc)


# ── Load shape (constant concurrency) ───────────────────────────────────────

class ConstantLoadShape(LoadTestShape):
    def tick(self):
        elapsed = self.get_run_time()
        if elapsed > RUN_DURATION:
            return None
        return (CONCURRENCY, CONCURRENCY)


# ── User ─────────────────────────────────────────────────────────────────────

class FailureRecoveryUser(HttpUser):
    """
    Each user maintains a persistent circuit and sends requests through it.
    On failure, the circuit is rebuilt and the rebuild event is logged.
    """

    wait_time = between(0.05, 0.15)
    host = DIRECTORY_URL or "http://localhost:8080"

    def on_start(self):
        self._session = requests.Session()
        self._directory_url = DIRECTORY_URL or self.host
        self._circuit = None
        self._guard_key = None
        self._relay_key = None
        self._exit_key = None
        self._circuit_id = cc.new_circuit_id()
        self._health_checker = None

        # Build initial circuit.
        self._setup_circuit()

        # Optionally start health-check polling.
        if HEALTH_CHECK:
            self._health_checker = HealthChecker(
                self._session,
                self._directory_url,
                interval=HEALTH_CHECK_INTERVAL,
            )
            self._update_health_checker_nodes()
            self._health_checker.start()

    def on_stop(self):
        if self._health_checker:
            self._health_checker.stop()

    def _setup_circuit(self):
        """Fetch a new circuit and perform key exchange."""
        self._circuit = cc.get_circuit(self._session, self._directory_url, HOPS)
        self._circuit_id = cc.new_circuit_id()
        self._guard_key, self._relay_key, self._exit_key = cc.setup_circuit(
            self._session, self._circuit, self._circuit_id, HOPS
        )

    def _update_health_checker_nodes(self):
        if not self._health_checker or not self._circuit:
            return
        ids = [self._circuit["guard"]["nodeId"]]
        if HOPS == 3:
            ids.append(self._circuit["relay"]["nodeId"])
            ids.append(self._circuit["exit"]["nodeId"])
        self._health_checker.set_circuit_nodes(ids)

    def _rebuild_circuit(self):
        """Discard current circuit and build a fresh one."""
        self._circuit = None
        self._guard_key = None
        self._relay_key = None
        self._exit_key = None
        self._setup_circuit()
        self._update_health_checker_nodes()

    @task
    def send_request(self):
        circuit_rebuilt = False
        rebuild_duration_ms = 0

        # If health checker detected a dead node, proactively rebuild.
        if self._health_checker and self._health_checker.check_needs_rebuild():
            t_rebuild = time.monotonic()
            try:
                self._rebuild_circuit()
                circuit_rebuilt = True
                rebuild_duration_ms = (time.monotonic() - t_rebuild) * 1000
                log.info("[user] proactive rebuild completed in %.0fms", rebuild_duration_ms)
            except Exception as exc:
                rebuild_duration_ms = (time.monotonic() - t_rebuild) * 1000
                log_csv_row("rebuild_failed", rebuild_duration_ms, True, rebuild_duration_ms)
                events.request.fire(
                    request_type="circuit_request",
                    name=f"{HOPS}-hop-failure-recovery",
                    response_time=int(rebuild_duration_ms),
                    response_length=0,
                    exception=exc,
                    context={},
                )
                raise RescheduleTask() from exc

        exit_layer = {
            "url": ECHO_SERVER_URL,
            "method": "GET",
            "headers": {},
            "body": None,
        }

        t0 = time.monotonic()
        try:
            payload = cc.build_onion(
                self._guard_key, self._relay_key, self._exit_key,
                exit_layer, self._circuit, HOPS,
            )
            guard_url = cc._node_url(self._circuit["guard"])
            onion_resp = cc.send_onion(
                self._session, guard_url, self._circuit_id, payload
            )
            exit_resp = cc.decrypt_response(
                self._guard_key, self._relay_key, self._exit_key,
                onion_resp["payload"], HOPS,
            )
            latency_ms = (time.monotonic() - t0) * 1000

            log_csv_row("success", latency_ms, circuit_rebuilt, rebuild_duration_ms)
            events.request.fire(
                request_type="circuit_request",
                name=f"{HOPS}-hop-failure-recovery",
                response_time=int(latency_ms),
                response_length=len(exit_resp.get("body", "") or ""),
                exception=None,
                context={},
            )

        except Exception as exc:
            latency_ms = (time.monotonic() - t0) * 1000

            # Try to rebuild the circuit.
            t_rebuild = time.monotonic()
            try:
                self._rebuild_circuit()
                rebuild_duration_ms = (time.monotonic() - t_rebuild) * 1000
                circuit_rebuilt = True
                log_csv_row("failed_rebuilt", latency_ms + rebuild_duration_ms, True, rebuild_duration_ms)
            except Exception:
                rebuild_duration_ms = (time.monotonic() - t_rebuild) * 1000
                log_csv_row("failed_rebuild_failed", latency_ms + rebuild_duration_ms, True, rebuild_duration_ms)

            events.request.fire(
                request_type="circuit_request",
                name=f"{HOPS}-hop-failure-recovery",
                response_time=int(latency_ms),
                response_length=0,
                exception=exc,
                context={},
            )
            raise RescheduleTask() from exc


# ── CSV export + S3 upload on completion ─────────────────────────────────────

@events.quitting.add_listener
def export_results(environment, **kwargs):
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    mode = "with-predetection" if HEALTH_CHECK else "no-predetection"

    with _csv_lock:
        rows = list(_csv_rows)

    if not rows:
        log.warning("[exp3] no CSV rows to export")
        return

    # Write local CSV.
    csv_path = f"results/{mode}_{timestamp}.csv"
    os.makedirs("results", exist_ok=True)
    with open(csv_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=CSV_FIELDS)
        writer.writeheader()
        writer.writerows(rows)
    log.info("[exp3] wrote %d rows to %s", len(rows), csv_path)

    # Upload to S3 if configured.
    if not S3_BUCKET:
        log.info("[exp3] EXPERIMENT_RESULTS_BUCKET not set — skipping S3 upload")
        return

    buf = io.StringIO()
    writer = csv.DictWriter(buf, fieldnames=CSV_FIELDS)
    writer.writeheader()
    writer.writerows(rows)

    key = f"{S3_PREFIX}/{mode}/{timestamp}_requests.csv"
    try:
        s3 = boto3.client("s3")
        s3.put_object(Bucket=S3_BUCKET, Key=key, Body=buf.getvalue().encode())
        log.info("[exp3] uploaded to s3://%s/%s", S3_BUCKET, key)
    except (BotoCoreError, ClientError) as exc:
        log.error("[exp3] S3 upload failed: %s", exc)

    # Also upload Locust aggregated stats.
    stats = environment.runner.stats
    stat_rows = []
    for name, entry in stats.entries.items():
        stat_rows.append({
            "timestamp": timestamp,
            "scenario": f"{HOPS}-hop-{mode}",
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

    if stat_rows:
        stat_buf = io.StringIO()
        stat_writer = csv.DictWriter(stat_buf, fieldnames=stat_rows[0].keys())
        stat_writer.writeheader()
        stat_writer.writerows(stat_rows)

        stat_key = f"{S3_PREFIX}/{mode}/{timestamp}_stats.csv"
        try:
            s3 = boto3.client("s3")
            s3.put_object(Bucket=S3_BUCKET, Key=stat_key, Body=stat_buf.getvalue().encode())
            log.info("[exp3] uploaded stats to s3://%s/%s", S3_BUCKET, stat_key)
        except (BotoCoreError, ClientError) as exc:
            log.error("[exp3] stats S3 upload failed: %s", exc)
