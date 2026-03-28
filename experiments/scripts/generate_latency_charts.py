#!/usr/bin/env python3

import csv
import os
import sys

import matplotlib.pyplot as plt


def load_rows(path: str):
    with open(path, newline="", encoding="utf-8") as file:
      return list(csv.DictReader(file))


def rows_for(rows, scenario):
    filtered = [row for row in rows if row["scenario"] == scenario]
    filtered.sort(key=lambda row: int(row["concurrency"]))
    return filtered


def plot_latency(rows, scenario, output_path):
    x = [int(row["concurrency"]) for row in rows]
    p50 = [float(row["p50_ms"]) for row in rows]
    p95 = [float(row["p95_ms"]) for row in rows]
    p99 = [float(row["p99_ms"]) for row in rows]

    plt.figure(figsize=(10, 6))
    plt.plot(x, p50, marker="o", label="p50")
    plt.plot(x, p95, marker="o", label="p95")
    plt.plot(x, p99, marker="o", label="p99")
    plt.title(f"{scenario} latency vs concurrency")
    plt.xlabel("Concurrent users")
    plt.ylabel("Latency (ms)")
    plt.grid(True, alpha=0.3)
    plt.legend()
    plt.tight_layout()
    plt.savefig(output_path)
    plt.close()


def plot_p95_comparison(direct_rows, one_hop_rows, output_path):
    x = [int(row["concurrency"]) for row in direct_rows]
    direct = [float(row["p95_ms"]) for row in direct_rows]
    one_hop = [float(row["p95_ms"]) for row in one_hop_rows]

    plt.figure(figsize=(10, 6))
    plt.plot(x, direct, marker="o", label="direct p95")
    plt.plot(x, one_hop, marker="o", label="one-hop p95")
    plt.title("Direct vs one-hop p95 latency")
    plt.xlabel("Concurrent users")
    plt.ylabel("Latency (ms)")
    plt.grid(True, alpha=0.3)
    plt.legend()
    plt.tight_layout()
    plt.savefig(output_path)
    plt.close()


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: generate_latency_charts.py <summary_csv> <output_dir>", file=sys.stderr)
        return 1

    summary_csv, output_dir = sys.argv[1:]
    rows = load_rows(summary_csv)
    direct_rows = rows_for(rows, "direct")
    one_hop_rows = rows_for(rows, "one-hop")

    os.makedirs(output_dir, exist_ok=True)

    if direct_rows:
        plot_latency(direct_rows, "direct", os.path.join(output_dir, "direct-latency-vs-concurrency.png"))
    if one_hop_rows:
        plot_latency(one_hop_rows, "one-hop", os.path.join(output_dir, "one-hop-latency-vs-concurrency.png"))
    if direct_rows and one_hop_rows:
        plot_p95_comparison(direct_rows, one_hop_rows, os.path.join(output_dir, "direct-vs-one-hop-p95.png"))

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
