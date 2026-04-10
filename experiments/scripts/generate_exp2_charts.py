"""
generate_exp2_charts.py — Generate Experiment 2 visualisations.

Reads throughput_scaling.py summary JSONs and CloudWatch metric CSVs from
S3 (or a local directory), produces four publication-quality charts, and
saves PNGs to both S3 and docs/charts/.

Charts generated:
  1. Max sustainable throughput (req/s) vs relay count       (line)
  2. p95 latency at max throughput vs relay count            (line)
  3. CPU utilisation per service type vs relay count         (grouped bar)
  4. Throughput per relay node vs relay count                (line + linear reference)

Usage:
  python generate_exp2_charts.py \\
    --bucket hopvault-experiment-results-123456789-us-east-1 \\
    --region us-east-1 \\
    --out-dir docs/charts

  # or read from local directory:
  python generate_exp2_charts.py --local-dir experiments/results/exp2
"""

import argparse
import io
import json
import logging
import os
import sys
from pathlib import Path

import boto3
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.ticker as mticker
import numpy as np
import pandas as pd
from botocore.exceptions import BotoCoreError, ClientError

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger(__name__)

RELAY_COUNTS = [2, 5, 10, 20]

# Service types and their display colours for Chart 3
SERVICE_COLORS = {
    "guard-node":       "#2196F3",
    "relay-node":       "#FF9800",
    "exit-node":        "#F44336",
    "directory-server": "#9C27B0",
}

SERVICE_LABELS = {
    "guard-node":       "Guard",
    "relay-node":       "Relay",
    "exit-node":        "Exit",
    "directory-server": "Directory",
}


# ── Data loading ──────────────────────────────────────────────────────────────

def _s3_list_keys(s3, bucket: str, prefix: str) -> list[str]:
    resp = s3.list_objects_v2(Bucket=bucket, Prefix=prefix.rstrip("/") + "/")
    return [obj["Key"] for obj in resp.get("Contents", [])]


def _s3_read_json(s3, bucket: str, key: str) -> dict:
    obj = s3.get_object(Bucket=bucket, Key=key)
    return json.loads(obj["Body"].read())


def _s3_read_csv(s3, bucket: str, key: str) -> pd.DataFrame:
    obj  = s3.get_object(Bucket=bucket, Key=key)
    data = obj["Body"].read().decode()
    return pd.read_csv(io.StringIO(data))


def load_summaries_from_s3(s3, bucket: str) -> dict[int, dict]:
    """
    Return {relay_count: ceiling_record} by reading *_summary.json files
    from experiment-2/relays-{count}/.
    """
    summaries = {}
    for count in RELAY_COUNTS:
        prefix = f"experiment-2/relays-{count}"
        keys   = _s3_list_keys(s3, bucket, prefix)
        json_keys = [k for k in keys if k.endswith("_summary.json")]
        if not json_keys:
            log.warning("No summary JSON found for relay_count=%d", count)
            continue
        # Use the most recent summary (last alphabetically by timestamp prefix)
        key  = sorted(json_keys)[-1]
        data = _s3_read_json(s3, bucket, key)
        summaries[count] = data.get("ceiling") or {}
        log.info("Loaded summary for relay_count=%d from %s", count, key)
    return summaries


def load_summaries_from_dir(local_dir: str) -> dict[int, dict]:
    summaries = {}
    for count in RELAY_COUNTS:
        p = Path(local_dir) / f"relays-{count}"
        jsons = sorted(p.glob("*_summary.json"))
        if not jsons:
            log.warning("No summary JSON found in %s", p)
            continue
        with open(jsons[-1]) as f:
            data = json.load(f)
        summaries[count] = data.get("ceiling") or {}
    return summaries


def load_cpu_from_s3(s3, bucket: str) -> pd.DataFrame:
    """
    Read the combined CloudWatch metrics CSV from metrics/all_services_*.csv
    and return a DataFrame filtered to CPUUtilization.
    """
    keys = _s3_list_keys(s3, bucket, "metrics")
    csv_keys = [k for k in keys if "all_services_" in k and k.endswith(".csv")]
    if not csv_keys:
        log.warning("No combined CloudWatch CSV found under metrics/")
        return pd.DataFrame()
    frames = []
    for key in csv_keys:
        try:
            frames.append(_s3_read_csv(s3, bucket, key))
        except (BotoCoreError, ClientError) as exc:
            log.warning("Could not read %s: %s", key, exc)
    if not frames:
        return pd.DataFrame()
    df = pd.concat(frames, ignore_index=True)
    return df[df["metric"] == "CPUUtilization"] if "metric" in df.columns else df


def load_cpu_from_dir(local_dir: str) -> pd.DataFrame:
    p = Path(local_dir)
    frames = [pd.read_csv(f) for f in p.glob("all_services_*.csv")]
    if not frames:
        return pd.DataFrame()
    df = pd.concat(frames, ignore_index=True)
    return df[df["metric"] == "CPUUtilization"] if "metric" in df.columns else df


# ── Chart helpers ─────────────────────────────────────────────────────────────

def _style_axes(ax, title: str, xlabel: str, ylabel: str):
    ax.set_title(title, fontsize=14, fontweight="bold", pad=12)
    ax.set_xlabel(xlabel, fontsize=12)
    ax.set_ylabel(ylabel, fontsize=12)
    ax.grid(axis="y", linestyle="--", alpha=0.5)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    ax.set_xticks(RELAY_COUNTS)


def _save(fig, out_dir: Path, filename: str, s3_args) -> Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / filename
    fig.savefig(path, dpi=150, bbox_inches="tight")
    log.info("Saved %s", path)

    if s3_args and getattr(s3_args, "bucket", None):
        try:
            s3 = boto3.client("s3", region_name=s3_args.region)
            with open(path, "rb") as f:
                s3.put_object(
                    Bucket=s3_args.bucket,
                    Key=f"charts/experiment-2/{filename}",
                    Body=f.read(),
                    ContentType="image/png",
                )
            log.info("Uploaded to s3://%s/charts/experiment-2/%s", s3_args.bucket, filename)
        except (BotoCoreError, ClientError) as exc:
            log.error("S3 upload failed for %s: %s", filename, exc)

    plt.close(fig)
    return path


# ── Chart 1: max throughput vs relay count ────────────────────────────────────

def chart_max_throughput(summaries: dict, out_dir: Path, s3_args) -> Path:
    counts = sorted(summaries)
    rps    = [summaries[c].get("rps", 0) for c in counts]

    fig, ax = plt.subplots(figsize=(8, 5))
    ax.plot(counts, rps, marker="o", linewidth=2.5, color="#2196F3",
            label="Max sustainable throughput")
    for x, y in zip(counts, rps):
        ax.annotate(f"{y:.0f}", (x, y), textcoords="offset points",
                    xytext=(0, 8), ha="center", fontsize=9)

    _style_axes(
        ax,
        title="Experiment 2 — Max Sustainable Throughput vs Relay Count",
        xlabel="Number of relay nodes",
        ylabel="Max throughput (req/s)",
    )
    ax.legend(fontsize=10)
    fig.tight_layout()
    return _save(fig, out_dir, "exp2_max_throughput_vs_relays.png", s3_args)


# ── Chart 2: p95 latency at max throughput ────────────────────────────────────

def chart_p95_at_ceiling(summaries: dict, out_dir: Path, s3_args) -> Path:
    counts = sorted(summaries)
    p95    = [summaries[c].get("p95_ms", 0) for c in counts]

    fig, ax = plt.subplots(figsize=(8, 5))
    ax.plot(counts, p95, marker="s", linewidth=2.5, color="#F44336",
            label="p95 latency at max throughput")
    ax.axhline(500, linestyle="--", color="grey", linewidth=1,
               label="Threshold (500 ms)")
    for x, y in zip(counts, p95):
        ax.annotate(f"{y:.0f} ms", (x, y), textcoords="offset points",
                    xytext=(0, 8), ha="center", fontsize=9)

    _style_axes(
        ax,
        title="Experiment 2 — p95 Latency at Max Throughput vs Relay Count",
        xlabel="Number of relay nodes",
        ylabel="p95 latency (ms)",
    )
    ax.legend(fontsize=10)
    fig.tight_layout()
    return _save(fig, out_dir, "exp2_p95_at_ceiling_vs_relays.png", s3_args)


# ── Chart 3: CPU per service type ─────────────────────────────────────────────

def chart_cpu_by_service(cpu_df: pd.DataFrame, summaries: dict,
                          out_dir: Path, s3_args) -> Path:
    """
    Grouped bar chart — avg CPU utilisation per service at each relay count.
    If no CloudWatch data is available, produce a placeholder chart.
    """
    services = list(SERVICE_COLORS)
    counts   = sorted(summaries)
    x        = np.arange(len(counts))
    width    = 0.18

    fig, ax = plt.subplots(figsize=(10, 5))

    if cpu_df.empty:
        log.warning("No CPU data available — generating placeholder chart")
        ax.text(0.5, 0.5, "CloudWatch data not yet available",
                transform=ax.transAxes, ha="center", va="center",
                fontsize=13, color="grey")
    else:
        for i, service in enumerate(services):
            sub = cpu_df[cpu_df["service"] == service]
            avgs = []
            for count in counts:
                # Average CPU over the experiment window for this relay count
                # (all data present in the combined CSV is used as-is)
                val = sub["value"].mean() if not sub.empty else 0
                avgs.append(round(val, 1))

            bars = ax.bar(
                x + i * width, avgs, width,
                label=SERVICE_LABELS[service],
                color=SERVICE_COLORS[service],
                alpha=0.85,
            )
            ax.bar_label(bars, fmt="%.0f%%", padding=2, fontsize=8)

    ax.set_title("Experiment 2 — CPU Utilisation per Service vs Relay Count",
                 fontsize=14, fontweight="bold", pad=12)
    ax.set_xlabel("Number of relay nodes", fontsize=12)
    ax.set_ylabel("Average CPU utilisation (%)", fontsize=12)
    ax.set_xticks(x + width * (len(services) - 1) / 2)
    ax.set_xticklabels([str(c) for c in counts])
    ax.set_ylim(0, 105)
    ax.grid(axis="y", linestyle="--", alpha=0.5)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    ax.legend(fontsize=10, framealpha=0.8)
    fig.tight_layout()
    return _save(fig, out_dir, "exp2_cpu_by_service_vs_relays.png", s3_args)


# ── Chart 4: throughput per relay node ───────────────────────────────────────

def chart_throughput_per_relay(summaries: dict, out_dir: Path, s3_args) -> Path:
    """
    Line chart — total throughput ÷ relay count vs relay count.
    A perfectly linear scale-out would be a flat horizontal line.
    Annotations mark where scaling deviates from linear.
    """
    counts = sorted(summaries)
    total  = [summaries[c].get("rps", 0) for c in counts]
    per_relay = [t / c if c else 0 for t, c in zip(total, counts)]

    # Linear reference: throughput-per-relay at the smallest count
    baseline = per_relay[0] if per_relay else 0

    fig, ax = plt.subplots(figsize=(8, 5))
    ax.plot(counts, per_relay, marker="o", linewidth=2.5, color="#2196F3",
            label="Throughput per relay node")
    ax.axhline(baseline, linestyle="--", color="grey", linewidth=1.2,
               label=f"Linear reference ({baseline:.0f} req/s per relay)")

    # Annotate deviation from linear
    for x, y in zip(counts, per_relay):
        pct = ((y - baseline) / baseline * 100) if baseline else 0
        label = f"{y:.0f}\n({pct:+.0f}%)"
        ax.annotate(label, (x, y), textcoords="offset points",
                    xytext=(0, 10), ha="center", fontsize=8.5)

    _style_axes(
        ax,
        title="Experiment 2 — Throughput per Relay Node vs Relay Count",
        xlabel="Number of relay nodes",
        ylabel="Throughput per relay node (req/s)",
    )
    ax.legend(fontsize=10)
    fig.tight_layout()
    return _save(fig, out_dir, "exp2_throughput_per_relay.png", s3_args)


# ── CLI ───────────────────────────────────────────────────────────────────────

def parse_args(argv=None):
    p = argparse.ArgumentParser(description="Generate Experiment 2 charts.")
    src = p.add_mutually_exclusive_group()
    src.add_argument("--bucket",    default=os.getenv("EXPERIMENT_RESULTS_BUCKET", ""),
                     help="S3 bucket (env: EXPERIMENT_RESULTS_BUCKET)")
    src.add_argument("--local-dir", dest="local_dir",
                     help="Local directory with exp2 results (alternative to S3)")
    p.add_argument("--region",  default=os.getenv("AWS_DEFAULT_REGION", "us-east-1"))
    p.add_argument("--out-dir", dest="out_dir", default="docs/charts",
                   help="Local directory to write PNGs (default: docs/charts)")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    if not args.bucket and not args.local_dir:
        log.error("Provide --bucket or --local-dir")
        sys.exit(1)

    out_dir = Path(args.out_dir)
    s3_args = args if args.bucket else None

    if args.local_dir:
        summaries = load_summaries_from_dir(args.local_dir)
        cpu_df    = load_cpu_from_dir(args.local_dir)
    else:
        s3        = boto3.client("s3", region_name=args.region)
        summaries = load_summaries_from_s3(s3, args.bucket)
        cpu_df    = load_cpu_from_s3(s3, args.bucket)

    if not summaries:
        log.error("No Experiment 2 summary data found — run throughput_scaling.py first")
        sys.exit(1)

    log.info("Generating Experiment 2 charts for relay counts: %s", sorted(summaries))
    chart_max_throughput(summaries,        out_dir, s3_args)
    chart_p95_at_ceiling(summaries,        out_dir, s3_args)
    chart_cpu_by_service(cpu_df, summaries, out_dir, s3_args)
    chart_throughput_per_relay(summaries,  out_dir, s3_args)

    log.info("All Experiment 2 charts written to %s", out_dir)


if __name__ == "__main__":
    main()
