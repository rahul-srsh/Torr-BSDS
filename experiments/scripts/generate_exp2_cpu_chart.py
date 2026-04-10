"""
generate_exp2_cpu_chart.py — Pull Container Insights CPU data and regenerate
the exp2_cpu_by_service_vs_relays.png chart with real data.

Queries ECS/ContainerInsights namespace for CpuUtilized and CpuReserved
for guard-node, relay-node, and exit-node during the CPU-capture test windows.

Usage:
  python generate_exp2_cpu_chart.py \\
    --timestamps /tmp/cpu_timestamps.csv \\
    --out-dir docs/charts \\
    --region us-west-2

Timestamps CSV format (no header):
  relay_count,start_utc,end_utc
  e.g.: 2,2026-04-10T23:20:42Z,2026-04-10T23:24:03Z
"""

import argparse
import logging
import os
from datetime import datetime, timezone, timedelta
from pathlib import Path

import boto3
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger(__name__)

SERVICES = ["guard-node", "relay-node", "exit-node"]

SERVICE_LABELS = {
    "guard-node": "Guard node",
    "relay-node": "Relay node (per task)",
    "exit-node":  "Exit node",
}

COLORS = {
    "guard-node": "#2196F3",
    "relay-node": "#FF9800",
    "exit-node":  "#F44336",
}


def parse_utc(ts: str) -> datetime:
    ts = ts.rstrip("Z")
    dt = datetime.fromisoformat(ts)
    return dt.replace(tzinfo=timezone.utc)


def fetch_cpu_pct(cw, service: str, cluster: str,
                  start: datetime, end: datetime) -> float | None:
    """
    Fetch average CPU% = mean(CpuUtilized) / mean(CpuReserved) × 100
    for a service during [start, end]. Returns None if no data.
    """
    dims = [
        {"Name": "ServiceName", "Value": service},
        {"Name": "ClusterName", "Value": cluster},
    ]

    def _avg(metric):
        resp = cw.get_metric_statistics(
            Namespace="ECS/ContainerInsights",
            MetricName=metric,
            Dimensions=dims,
            StartTime=start,
            EndTime=end,
            Period=60,
            Statistics=["Average"],
        )
        pts = [p["Average"] for p in resp["Datapoints"]]
        return sum(pts) / len(pts) if pts else None

    utilized = _avg("CpuUtilized")
    reserved = _avg("CpuReserved")
    log.info("  %s: CpuUtilized=%.1f  CpuReserved=%.1f",
             service,
             utilized if utilized is not None else float("nan"),
             reserved if reserved is not None else float("nan"))

    if utilized is None or reserved is None or reserved == 0:
        return None
    return round((utilized / reserved) * 100, 2)


def load_timestamps(path: str) -> list[dict]:
    windows = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            parts = line.split(",")
            relay_count = int(parts[0])
            start = parse_utc(parts[1])
            end   = parse_utc(parts[2])
            windows.append({"relay_count": relay_count, "start": start, "end": end})
    windows.sort(key=lambda w: w["relay_count"])
    return windows


def main(argv=None):
    p = argparse.ArgumentParser(description="Generate exp2 CPU chart from Container Insights.")
    p.add_argument("--timestamps", default="/tmp/cpu_timestamps.csv",
                   help="CSV with relay_count,start,end rows")
    p.add_argument("--out-dir", dest="out_dir", default="docs/charts")
    p.add_argument("--cluster", default="hopvault-cluster")
    p.add_argument("--region", default=os.getenv("AWS_DEFAULT_REGION", "us-west-2"))
    args = p.parse_args(argv)

    windows = load_timestamps(args.timestamps)
    relay_counts = [w["relay_count"] for w in windows]
    log.info("Windows: %s", [(w["relay_count"], str(w["start"]), str(w["end"])) for w in windows])

    cw = boto3.client("cloudwatch", region_name=args.region)

    # {service: [cpu_pct_at_relay2, cpu_pct_at_relay5, ...]}
    data: dict[str, list] = {svc: [] for svc in SERVICES}

    for w in windows:
        log.info("=== relay_count=%d ===", w["relay_count"])
        # Skip the first minute (ramp-up) and last minute (wind-down)
        t_start = w["start"] + timedelta(minutes=1)
        t_end   = w["end"]   - timedelta(minutes=0, seconds=30)
        for svc in SERVICES:
            pct = fetch_cpu_pct(cw, svc, args.cluster, t_start, t_end)
            data[svc].append(pct if pct is not None else 0.0)
            log.info("  %s CPU%% = %s", svc, pct)

    # Plot
    fig, ax = plt.subplots(figsize=(9, 5))

    for svc in SERVICES:
        vals = data[svc]
        ax.plot(
            relay_counts, vals,
            marker="o", linewidth=2,
            color=COLORS[svc],
            label=SERVICE_LABELS[svc],
        )
        # Annotate each point
        for x, y in zip(relay_counts, vals):
            ax.annotate(f"{y:.0f}%", (x, y),
                        textcoords="offset points", xytext=(0, 6),
                        ha="center", fontsize=8, color=COLORS[svc])

    ax.set_title("Experiment 2 — CPU Utilisation per Task vs Relay Count",
                 fontsize=14, fontweight="bold", pad=12)
    ax.set_xlabel("Number of relay nodes", fontsize=12)
    ax.set_ylabel("Avg CPU utilisation per task (%)", fontsize=12)
    ax.set_xticks(relay_counts)
    ax.set_ylim(0, max(max(vals) for vals in data.values()) * 1.25 + 5)
    ax.grid(axis="y", linestyle="--", alpha=0.5)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    ax.legend(fontsize=10, framealpha=0.8)
    fig.tight_layout()

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    out_path = out_dir / "exp2_cpu_by_service_vs_relays.png"
    fig.savefig(out_path, dpi=150, bbox_inches="tight")
    plt.close(fig)
    log.info("Saved %s", out_path)


if __name__ == "__main__":
    main()
