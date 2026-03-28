#!/usr/bin/env python3

import csv
import os
import sys


def main() -> int:
    if len(sys.argv) != 5:
        print(
            "usage: extract_locust_metrics.py <stats_csv> <scenario> <concurrency> <summary_csv>",
            file=sys.stderr,
        )
        return 1

    stats_csv, scenario, concurrency, summary_csv = sys.argv[1:]

    with open(stats_csv, newline="", encoding="utf-8") as file:
        reader = csv.DictReader(file)
        aggregated = None
        for row in reader:
            if row.get("Name") == "Aggregated":
                aggregated = row
                break

    if aggregated is None:
        raise SystemExit(f"could not find Aggregated row in {stats_csv}")

    output_exists = os.path.exists(summary_csv)
    os.makedirs(os.path.dirname(summary_csv), exist_ok=True)

    with open(summary_csv, "a", newline="", encoding="utf-8") as file:
        writer = csv.DictWriter(
            file,
            fieldnames=[
                "scenario",
                "concurrency",
                "requests",
                "failures",
                "rps",
                "avg_ms",
                "p50_ms",
                "p95_ms",
                "p99_ms",
            ],
        )

        if not output_exists:
            writer.writeheader()

        writer.writerow(
            {
                "scenario": scenario,
                "concurrency": concurrency,
                "requests": aggregated.get("Request Count", "0"),
                "failures": aggregated.get("Failure Count", "0"),
                "rps": aggregated.get("Requests/s", "0"),
                "avg_ms": aggregated.get("Average Response Time", "0"),
                "p50_ms": aggregated.get("50%", "0"),
                "p95_ms": aggregated.get("95%", "0"),
                "p99_ms": aggregated.get("99%", "0"),
            }
        )

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
