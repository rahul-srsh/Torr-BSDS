"""
generate_charts.py — Generate Experiment 3 visualisations.

Reads per-request CSV data from both experiment runs (with and without
pre-detection) and the kill event JSON, then generates five charts.

Charts generated:
  1. Timeline — requests per second over time, vertical line at kill event
  2. Bar chart — detection time comparison (without vs. with pre-detection)
  3. Bar chart — total request loss comparison
  4. Scatter plot — per-request latency over time
  5. Timeline — circuit rebuild events relative to the kill event

Usage:
  python generate_charts.py \\
    --data-dir experiments/experiment3/results \\
    --kill-event experiments/experiment3/results/kill_event.json \\
    --out-dir docs/charts

  # Optionally upload to S3:
  python generate_charts.py \\
    --data-dir experiments/experiment3/results \\
    --kill-event experiments/experiment3/results/kill_event.json \\
    --out-dir docs/charts \\
    --bucket hopvault-experiment-results \\
    --region us-west-2
"""

import argparse
import csv
import json
import logging
import os
import sys
from datetime import datetime, timezone
from pathlib import Path

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.ticker as mticker
import matplotlib.dates as mdates

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger(__name__)

COLORS = {
    "no-predetection":   "#F44336",   # red
    "with-predetection": "#4CAF50",   # green
}

LABELS = {
    "no-predetection":   "Without pre-detection",
    "with-predetection": "With pre-detection",
}


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description="Generate Experiment 3 charts")
    parser.add_argument("--data-dir", required=True, help="Directory with CSV results")
    parser.add_argument("--kill-event", default="", help="Path to single kill_event.json (used for both modes)")
    parser.add_argument("--kill-event-no-predetection", default="", help="Kill event for no-predetection run")
    parser.add_argument("--kill-event-with-predetection", default="", help="Kill event for with-predetection run")
    parser.add_argument("--out-dir", default="docs/charts", help="Output directory for PNGs")
    parser.add_argument("--bucket", default="", help="S3 bucket for upload (optional)")
    parser.add_argument("--region", default="us-east-1", help="AWS region")
    return parser.parse_args(argv)


def load_csv(path):
    """Load a per-request CSV file into a list of dicts."""
    rows = []
    with open(path, newline="") as f:
        reader = csv.DictReader(f)
        for row in reader:
            row["latency_ms"] = float(row["latency_ms"])
            row["rebuild_duration_ms"] = float(row["rebuild_duration_ms"])
            row["circuit_rebuilt"] = row["circuit_rebuilt"].lower() in ("true", "1", "yes")
            row["_ts"] = datetime.strptime(row["timestamp"], "%Y-%m-%dT%H:%M:%S").replace(
                tzinfo=timezone.utc
            )
            rows.append(row)
    return rows


def load_kill_event(path):
    """Load the kill event JSON."""
    with open(path) as f:
        event = json.load(f)
    event["_ts"] = datetime.fromisoformat(event["killTimestamp"])
    return event


def find_data_files(data_dir):
    """Find the per-request CSV files for each mode.

    Prefers the timestamped custom CSVs (e.g., no-predetection_20260417T201000Z.csv)
    over the Locust-generated stats CSVs (no-predetection_stats.csv).
    """
    files = {}
    for fname in sorted(os.listdir(data_dir)):
        if not fname.endswith(".csv"):
            continue
        # Skip Locust-generated aggregate CSVs.
        if any(x in fname for x in ("_stats.csv", "_stats_history.csv", "_failures.csv", "_exceptions.csv")):
            continue
        if "no-predetection" in fname and "no-predetection" not in files:
            files["no-predetection"] = os.path.join(data_dir, fname)
        elif "with-predetection" in fname and "with-predetection" not in files:
            files["with-predetection"] = os.path.join(data_dir, fname)
    return files


def compute_rps_timeline(rows, bin_seconds=1):
    """Compute requests per second grouped by timestamp bins."""
    if not rows:
        return [], []
    timestamps = sorted(r["_ts"] for r in rows)
    start = timestamps[0]
    bins = {}
    for r in rows:
        offset = int((r["_ts"] - start).total_seconds() / bin_seconds)
        bins[offset] = bins.get(offset, 0) + 1
    max_offset = max(bins.keys()) + 1
    xs = list(range(max_offset))
    ys = [bins.get(x, 0) / bin_seconds for x in xs]
    return xs, ys


def compute_detection_time(rows, kill_ts):
    """Time between kill event and first client-side failure."""
    failures = [r for r in rows if r["status"] != "success"]
    if not failures:
        return 0
    first_failure = min(r["_ts"] for r in failures)
    dt = (first_failure - kill_ts).total_seconds()
    return max(dt, 0)


def compute_request_loss(rows):
    """Count total failed requests."""
    return sum(1 for r in rows if r["status"] != "success")


def compute_rebuild_offsets(rows, kill_ts):
    """Return list of (seconds_after_kill, rebuild_duration_ms) for rebuilds."""
    rebuilds = []
    for r in rows:
        if r["circuit_rebuilt"]:
            offset = (r["_ts"] - kill_ts).total_seconds()
            rebuilds.append((offset, r["rebuild_duration_ms"]))
    return rebuilds


# ── Chart 1: RPS Timeline ───────────────────────────────────────────────────

def chart_rps_timeline(data_by_mode, kill_offset_by_mode, out_dir):
    fig, ax = plt.subplots(figsize=(12, 5))
    for mode, rows in data_by_mode.items():
        xs, ys = compute_rps_timeline(rows)
        ax.plot(xs, ys, label=LABELS[mode], color=COLORS[mode], alpha=0.8)
    for mode, offset in kill_offset_by_mode.items():
        ax.axvline(x=offset, color=COLORS[mode], linestyle="--", alpha=0.6,
                   label=f"Kill event ({LABELS[mode]})")
    ax.set_xlabel("Time (seconds)")
    ax.set_ylabel("Requests / second")
    ax.set_title("Experiment 3: Throughput Over Time During Node Failure")
    ax.legend(loc="upper right")
    ax.grid(True, alpha=0.3)
    path = os.path.join(out_dir, "exp3_rps_timeline.png")
    fig.tight_layout()
    fig.savefig(path, dpi=150)
    plt.close(fig)
    log.info("saved %s", path)
    return path


# ── Chart 2: Detection Time Comparison ───────────────────────────────────────

def chart_detection_time(detection_times, out_dir):
    fig, ax = plt.subplots(figsize=(8, 5))
    modes = list(detection_times.keys())
    values = [detection_times[m] for m in modes]
    colors = [COLORS[m] for m in modes]
    labels = [LABELS[m] for m in modes]

    bars = ax.bar(labels, values, color=colors, width=0.5)
    for bar, val in zip(bars, values):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 0.1,
                f"{val:.1f}s", ha="center", va="bottom", fontsize=11)

    ax.set_ylabel("Detection Time (seconds)")
    ax.set_title("Experiment 3: Failure Detection Time")
    ax.grid(True, axis="y", alpha=0.3)
    path = os.path.join(out_dir, "exp3_detection_time.png")
    fig.tight_layout()
    fig.savefig(path, dpi=150)
    plt.close(fig)
    log.info("saved %s", path)
    return path


# ── Chart 3: Request Loss Comparison ─────────────────────────────────────────

def chart_request_loss(request_losses, out_dir):
    fig, ax = plt.subplots(figsize=(8, 5))
    modes = list(request_losses.keys())
    values = [request_losses[m] for m in modes]
    colors = [COLORS[m] for m in modes]
    labels = [LABELS[m] for m in modes]

    bars = ax.bar(labels, values, color=colors, width=0.5)
    for bar, val in zip(bars, values):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 0.3,
                str(val), ha="center", va="bottom", fontsize=11)

    ax.set_ylabel("Failed Requests")
    ax.set_title("Experiment 3: Total Request Loss During Recovery")
    ax.grid(True, axis="y", alpha=0.3)
    path = os.path.join(out_dir, "exp3_request_loss.png")
    fig.tight_layout()
    fig.savefig(path, dpi=150)
    plt.close(fig)
    log.info("saved %s", path)
    return path


# ── Chart 4: Latency Scatter ────────────────────────────────────────────────

def chart_latency_scatter(data_by_mode, kill_offset_by_mode, out_dir):
    fig, ax = plt.subplots(figsize=(12, 5))
    for mode, rows in data_by_mode.items():
        if not rows:
            continue
        start = min(r["_ts"] for r in rows)
        xs = [(r["_ts"] - start).total_seconds() for r in rows]
        ys = [r["latency_ms"] for r in rows]
        ax.scatter(xs, ys, s=4, alpha=0.4, label=LABELS[mode], color=COLORS[mode])

    for mode, offset in kill_offset_by_mode.items():
        ax.axvline(x=offset, color=COLORS[mode], linestyle="--", alpha=0.6)

    ax.set_xlabel("Time (seconds)")
    ax.set_ylabel("Latency (ms)")
    ax.set_title("Experiment 3: Per-Request Latency During Node Failure")
    ax.legend(loc="upper right")
    ax.grid(True, alpha=0.3)
    path = os.path.join(out_dir, "exp3_latency_scatter.png")
    fig.tight_layout()
    fig.savefig(path, dpi=150)
    plt.close(fig)
    log.info("saved %s", path)
    return path


# ── Chart 5: Rebuild Timeline ───────────────────────────────────────────────

def chart_rebuild_timeline_multi(data_by_mode, kill_ts_by_mode, out_dir):
    """Rebuild timeline using per-mode kill timestamps."""
    fig, ax = plt.subplots(figsize=(12, 5))
    y_offset = 0
    yticks = []
    ytick_labels = []

    for mode, rows in data_by_mode.items():
        kill_ts = kill_ts_by_mode.get(mode)
        if not kill_ts:
            continue
        rebuilds = compute_rebuild_offsets(rows, kill_ts)
        if not rebuilds:
            continue
        offsets = [r[0] for r in rebuilds]
        durations = [r[1] for r in rebuilds]
        ax.barh(
            [y_offset] * len(rebuilds),
            durations,
            left=offsets,
            height=0.4,
            color=COLORS[mode],
            alpha=0.7,
            label=LABELS[mode],
        )
        yticks.append(y_offset)
        ytick_labels.append(LABELS[mode])
        y_offset += 1

    ax.axvline(x=0, color="black", linestyle="--", linewidth=1.5, label="Kill event")
    ax.set_xlabel("Time Relative to Kill Event (seconds)")
    ax.set_ylabel("")
    ax.set_yticks(yticks)
    ax.set_yticklabels(ytick_labels)
    ax.set_title("Experiment 3: Circuit Rebuild Timing Relative to Kill Event")
    ax.legend(loc="upper right")
    ax.grid(True, axis="x", alpha=0.3)
    path = os.path.join(out_dir, "exp3_rebuild_timeline.png")
    fig.tight_layout()
    fig.savefig(path, dpi=150)
    plt.close(fig)
    log.info("saved %s", path)
    return path


def chart_rebuild_timeline(data_by_mode, kill_ts, out_dir):
    fig, ax = plt.subplots(figsize=(12, 5))
    y_offset = 0
    yticks = []
    ytick_labels = []

    for mode, rows in data_by_mode.items():
        rebuilds = compute_rebuild_offsets(rows, kill_ts)
        if not rebuilds:
            continue
        offsets = [r[0] for r in rebuilds]
        durations = [r[1] for r in rebuilds]
        ax.barh(
            [y_offset] * len(rebuilds),
            durations,
            left=offsets,
            height=0.4,
            color=COLORS[mode],
            alpha=0.7,
            label=LABELS[mode],
        )
        yticks.append(y_offset)
        ytick_labels.append(LABELS[mode])
        y_offset += 1

    ax.axvline(x=0, color="black", linestyle="--", linewidth=1.5, label="Kill event")
    ax.set_xlabel("Time Relative to Kill Event (seconds)")
    ax.set_ylabel("")
    ax.set_yticks(yticks)
    ax.set_yticklabels(ytick_labels)
    ax.set_title("Experiment 3: Circuit Rebuild Timing Relative to Kill Event")
    ax.legend(loc="upper right")
    ax.grid(True, axis="x", alpha=0.3)
    path = os.path.join(out_dir, "exp3_rebuild_timeline.png")
    fig.tight_layout()
    fig.savefig(path, dpi=150)
    plt.close(fig)
    log.info("saved %s", path)
    return path


# ── S3 upload ────────────────────────────────────────────────────────────────

def upload_to_s3(bucket, region, chart_paths):
    if not bucket:
        return
    try:
        import boto3
        s3 = boto3.client("s3", region_name=region)
        for path in chart_paths:
            key = f"experiment-3/{os.path.basename(path)}"
            s3.upload_file(path, bucket, key, ExtraArgs={"ContentType": "image/png"})
            log.info("uploaded s3://%s/%s", bucket, key)
    except Exception as exc:
        log.error("S3 upload failed: %s", exc)


# ── Main ─────────────────────────────────────────────────────────────────────

def main(argv=None):
    args = parse_args(argv)
    os.makedirs(args.out_dir, exist_ok=True)

    # Load kill event(s). Support per-mode kill events for sequential experiments.
    kill_ts_by_mode = {}
    if args.kill_event:
        kill_event = load_kill_event(args.kill_event)
        kill_ts_by_mode["no-predetection"] = kill_event["_ts"]
        kill_ts_by_mode["with-predetection"] = kill_event["_ts"]
        log.info("single kill event at %s", kill_event["_ts"].isoformat())
    if args.kill_event_no_predetection:
        evt = load_kill_event(args.kill_event_no_predetection)
        kill_ts_by_mode["no-predetection"] = evt["_ts"]
        log.info("no-predetection kill event at %s", evt["_ts"].isoformat())
    if args.kill_event_with_predetection:
        evt = load_kill_event(args.kill_event_with_predetection)
        kill_ts_by_mode["with-predetection"] = evt["_ts"]
        log.info("with-predetection kill event at %s", evt["_ts"].isoformat())

    if not kill_ts_by_mode:
        log.error("no kill event provided")
        sys.exit(1)

    # Find and load data files.
    data_files = find_data_files(args.data_dir)
    if not data_files:
        log.error("no CSV files found in %s", args.data_dir)
        sys.exit(1)

    data_by_mode = {}
    for mode, path in data_files.items():
        log.info("loading %s from %s", mode, path)
        data_by_mode[mode] = load_csv(path)
        log.info("  %d rows", len(data_by_mode[mode]))

    # Compute kill offset relative to each run's start time (using per-mode kill ts).
    kill_offset_by_mode = {}
    for mode, rows in data_by_mode.items():
        if rows and mode in kill_ts_by_mode:
            start = min(r["_ts"] for r in rows)
            kill_offset_by_mode[mode] = (kill_ts_by_mode[mode] - start).total_seconds()

    # Detection times (per-mode kill ts).
    detection_times = {}
    for mode, rows in data_by_mode.items():
        if mode in kill_ts_by_mode:
            detection_times[mode] = compute_detection_time(rows, kill_ts_by_mode[mode])
            log.info("  %s detection time: %.1fs", mode, detection_times[mode])

    # Request losses.
    request_losses = {}
    for mode, rows in data_by_mode.items():
        request_losses[mode] = compute_request_loss(rows)
        log.info("  %s request loss: %d", mode, request_losses[mode])

    # Use the first available kill_ts for the rebuild timeline chart.
    primary_kill_ts = next(iter(kill_ts_by_mode.values()))

    # Generate charts.
    chart_paths = []
    chart_paths.append(chart_rps_timeline(data_by_mode, kill_offset_by_mode, args.out_dir))
    chart_paths.append(chart_detection_time(detection_times, args.out_dir))
    chart_paths.append(chart_request_loss(request_losses, args.out_dir))
    chart_paths.append(chart_latency_scatter(data_by_mode, kill_offset_by_mode, args.out_dir))

    # For rebuild timeline, use per-mode kill timestamps.
    chart_paths.append(chart_rebuild_timeline_multi(data_by_mode, kill_ts_by_mode, args.out_dir))

    log.info("generated %d charts in %s", len(chart_paths), args.out_dir)

    # Optional S3 upload.
    upload_to_s3(args.bucket, args.region, chart_paths)


if __name__ == "__main__":
    main()
