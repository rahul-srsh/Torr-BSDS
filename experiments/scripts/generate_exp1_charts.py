"""
generate_exp1_charts.py — Generate Experiment 1 visualisations.

Reads Locust stats_history CSV results from a local directory, produces four
publication-quality charts, and saves PNGs to docs/charts/.

Charts generated:
  1. p50 latency vs concurrency   (line — direct / 1-hop / 3-hop)
  2. p95 latency vs concurrency   (line — direct / 1-hop / 3-hop)
  3. Per-hop latency cost          (bar  — 1-hop−direct, 3-hop−1-hop per level)
  4. Throughput (req/s) vs concurrency (line — all three modes)

Usage:
  python generate_exp1_charts.py \\
    --results-dir experiments/results \\
    --out-dir docs/charts

  # optionally also upload to S3:
  python generate_exp1_charts.py \\
    --results-dir experiments/results \\
    --bucket hopvault-experiment-results-... \\
    --region us-west-2 \\
    --out-dir docs/charts
"""

import argparse
import logging
import os
import sys
from pathlib import Path

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.ticker as mticker
import pandas as pd

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger(__name__)

CONCURRENCY_LEVELS = [10, 50, 100, 200]

COLORS = {
    "direct": "#2196F3",   # blue
    "1-hop":  "#FF9800",   # orange
    "3-hop":  "#F44336",   # red
}

SCENARIO_LABELS = {
    "direct": "Direct (no circuit)",
    "1-hop":  "1-hop circuit",
    "3-hop":  "3-hop circuit",
}

# Map scenario name → stats_history file stem in results dir
HISTORY_FILES = {
    "direct": "direct_stats_history.csv",
    "1-hop":  "1hop_stats_history.csv",
    "3-hop":  "3hop_stats_history.csv",
}


# ── Data loading ──────────────────────────────────────────────────────────────

def load_history(results_dir: Path, scenario: str) -> pd.DataFrame:
    """Load a stats_history CSV for the given scenario."""
    fname = HISTORY_FILES[scenario]
    path = results_dir / fname
    if not path.exists():
        log.warning("File not found: %s", path)
        return pd.DataFrame()
    df = pd.read_csv(path)
    # Drop rows with no data (User Count == 0 or N/A percentiles)
    df = df[df["User Count"] > 0]
    df = df[df["50%"].notna() & (df["50%"] != "N/A")]
    df["50%"] = pd.to_numeric(df["50%"], errors="coerce")
    df["95%"] = pd.to_numeric(df["95%"], errors="coerce")
    df["Requests/s"] = pd.to_numeric(df["Requests/s"], errors="coerce")
    # Keep only Aggregated rows
    agg_mask = df["Name"].isna() | (df["Name"] == "Aggregated") | (df["Name"] == "")
    df = df[agg_mask]
    return df


def steady_state_by_level(df: pd.DataFrame, metric: str,
                           levels: list = None) -> dict:
    """
    For each concurrency level, take the median of the metric during the
    last 30 seconds of that stage (steady-state approximation).
    Returns {level: value}.
    """
    if df.empty or metric not in df.columns:
        return {}

    levels = levels or CONCURRENCY_LEVELS
    result = {}
    for level in levels:
        sub = df[df["User Count"] == level][metric].dropna()
        if sub.empty:
            continue
        # Take the latter half of samples at this level as steady state
        n = max(1, len(sub) // 2)
        result[level] = sub.iloc[-n:].median()
    return result


def load_all_scenarios(results_dir: Path) -> dict:
    """Return {scenario: DataFrame} for direct, 1-hop, 3-hop."""
    return {s: load_history(results_dir, s) for s in ("direct", "1-hop", "3-hop")}


# ── Chart helpers ─────────────────────────────────────────────────────────────

def _style_axes(ax, title: str, xlabel: str, ylabel: str):
    ax.set_title(title, fontsize=14, fontweight="bold", pad=12)
    ax.set_xlabel(xlabel, fontsize=12)
    ax.set_ylabel(ylabel, fontsize=12)
    ax.grid(axis="y", linestyle="--", alpha=0.5)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    ax.legend(fontsize=10, framealpha=0.8)
    ax.set_xticks(CONCURRENCY_LEVELS)


def _save(fig, out_dir: Path, filename: str, args) -> Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / filename
    fig.savefig(path, dpi=150, bbox_inches="tight")
    log.info("Saved %s", path)

    if args and getattr(args, "bucket", None):
        try:
            import boto3
            from botocore.exceptions import BotoCoreError, ClientError
            s3 = boto3.client("s3", region_name=args.region)
            with open(path, "rb") as f:
                s3.put_object(
                    Bucket=args.bucket,
                    Key=f"charts/experiment-1/{filename}",
                    Body=f.read(),
                    ContentType="image/png",
                )
            log.info("Uploaded to s3://%s/charts/experiment-1/%s", args.bucket, filename)
        except Exception as exc:
            log.error("S3 upload failed for %s: %s", filename, exc)

    plt.close(fig)
    return path


# ── Chart 1 & 2: latency vs concurrency ──────────────────────────────────────

def chart_latency(data: dict, percentile: str, out_dir: Path, args) -> Path:
    col = f"{percentile}%"
    fig, ax = plt.subplots(figsize=(9, 5))

    for scenario in ("direct", "1-hop", "3-hop"):
        vals = steady_state_by_level(data[scenario], col)
        if not vals:
            log.warning("No data for %s %s", scenario, col)
            continue
        levels = sorted(vals)
        ax.plot(
            levels,
            [vals[l] for l in levels],
            marker="o",
            linewidth=2,
            color=COLORS[scenario],
            label=SCENARIO_LABELS[scenario],
        )

    _style_axes(
        ax,
        title=f"Experiment 1 — p{percentile} Latency vs Concurrency",
        xlabel="Concurrent users",
        ylabel=f"p{percentile} latency (ms)",
    )
    ax.yaxis.set_minor_locator(mticker.AutoMinorLocator())
    fig.tight_layout()
    return _save(fig, out_dir, f"exp1_p{percentile}_latency_vs_concurrency.png", args)


# ── Chart 3: per-hop latency cost ─────────────────────────────────────────────

def chart_hop_cost(data: dict, out_dir: Path, args) -> Path:
    direct_vals = steady_state_by_level(data["direct"], "50%")
    hop1_vals   = steady_state_by_level(data["1-hop"],  "50%")
    hop3_vals   = steady_state_by_level(data["3-hop"],  "50%")

    levels = CONCURRENCY_LEVELS
    cost_1_minus_d = [
        max(0, hop1_vals.get(l, 0) - direct_vals.get(l, 0)) for l in levels
    ]
    cost_3_minus_1 = [
        max(0, hop3_vals.get(l, 0) - hop1_vals.get(l, 0))   for l in levels
    ]

    x     = list(range(len(levels)))
    width = 0.35

    fig, ax = plt.subplots(figsize=(9, 5))
    bars1 = ax.bar(
        [i - width / 2 for i in x], cost_1_minus_d, width,
        label="Guard encryption cost (1-hop − direct)",
        color="#FF9800", alpha=0.85,
    )
    bars2 = ax.bar(
        [i + width / 2 for i in x], cost_3_minus_1, width,
        label="Relay + exit cost (3-hop − 1-hop)",
        color="#F44336", alpha=0.85,
    )

    ax.bar_label(bars1, fmt="%.0f ms", padding=3, fontsize=9)
    ax.bar_label(bars2, fmt="%.0f ms", padding=3, fontsize=9)

    ax.set_title(
        "Experiment 1 — Per-hop Latency Cost (p50)",
        fontsize=14, fontweight="bold", pad=12,
    )
    ax.set_xlabel("Concurrent users", fontsize=12)
    ax.set_ylabel("Added latency (ms)", fontsize=12)
    ax.set_xticks(x)
    ax.set_xticklabels([str(l) for l in levels])
    ax.grid(axis="y", linestyle="--", alpha=0.5)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    ax.legend(fontsize=10, framealpha=0.8)
    fig.tight_layout()
    return _save(fig, out_dir, "exp1_hop_latency_cost.png", args)


# ── Chart 4: throughput vs concurrency ───────────────────────────────────────

def chart_throughput(data: dict, out_dir: Path, args) -> Path:
    fig, ax = plt.subplots(figsize=(9, 5))

    for scenario in ("direct", "1-hop", "3-hop"):
        vals = steady_state_by_level(data[scenario], "Requests/s")
        if not vals:
            continue
        levels = sorted(vals)
        ax.plot(
            levels,
            [vals[l] for l in levels],
            marker="s",
            linewidth=2,
            color=COLORS[scenario],
            label=SCENARIO_LABELS[scenario],
        )

    _style_axes(
        ax,
        title="Experiment 1 — Throughput vs Concurrency",
        xlabel="Concurrent users",
        ylabel="Requests per second",
    )
    fig.tight_layout()
    return _save(fig, out_dir, "exp1_throughput_vs_concurrency.png", args)


# ── CLI ───────────────────────────────────────────────────────────────────────

def parse_args(argv=None):
    p = argparse.ArgumentParser(description="Generate Experiment 1 charts.")
    p.add_argument("--results-dir", dest="results_dir",
                   default="experiments/results",
                   help="Local directory containing *_stats_history.csv files")
    p.add_argument("--bucket",  default="",
                   help="S3 bucket to upload charts (optional)")
    p.add_argument("--region",  default=os.getenv("AWS_DEFAULT_REGION", "us-west-2"))
    p.add_argument("--out-dir", dest="out_dir", default="docs/charts",
                   help="Local directory to write PNGs (default: docs/charts)")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    results_dir = Path(args.results_dir)
    out_dir     = Path(args.out_dir)

    log.info("Loading Experiment 1 data from %s...", results_dir)
    data = load_all_scenarios(results_dir)

    for scenario, df in data.items():
        log.info("  %s: %d rows loaded", scenario, len(df))

    log.info("Generating charts...")
    chart_latency(data, "50", out_dir, args)
    chart_latency(data, "95", out_dir, args)
    chart_hop_cost(data,      out_dir, args)
    chart_throughput(data,    out_dir, args)

    log.info("All Experiment 1 charts written to %s", out_dir)


if __name__ == "__main__":
    main()
