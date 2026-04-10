"""
export_cloudwatch_metrics.py — Pull CloudWatch metrics for all HopVault ECS
services and export as CSVs to S3.

Metrics pulled per service (1-minute resolution):
  CPUUtilization, MemoryUtilization, NetworkBytesIn, NetworkBytesOut

Covers both Experiment 1 and Experiment 2 runs. Parameterized by time range
so it can be rerun for any window without touching the script.

Usage:
  python export_cloudwatch_metrics.py \\
    --start "2024-01-15T10:00:00Z" \\
    --end   "2024-01-15T12:00:00Z" \\
    --bucket hopvault-experiment-results-123456789-us-east-1 \\
    --cluster hopvault-cluster \\
    --region us-east-1 \\
    --prefix metrics

  All flags except --start and --end have defaults or can be set via env vars:
    EXPERIMENT_RESULTS_BUCKET, AWS_DEFAULT_REGION, ECS_CLUSTER_NAME
"""

import argparse
import csv
import io
import logging
import os
import sys
from datetime import datetime, timezone

import boto3
from botocore.exceptions import BotoCoreError, ClientError

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
log = logging.getLogger(__name__)

SERVICES = [
    "directory-server",
    "guard-node",
    "relay-node",
    "exit-node",
    "echo-server",
]

METRICS = [
    ("CPUUtilization",    "Percent"),
    ("MemoryUtilization", "Percent"),
    ("NetworkBytesIn",    "Bytes"),
    ("NetworkBytesOut",   "Bytes"),
]


# ── CloudWatch helpers ────────────────────────────────────────────────────────

def _parse_utc(ts: str) -> datetime:
    """Parse an ISO-8601 UTC string into a timezone-aware datetime."""
    ts = ts.rstrip("Z")
    dt = datetime.fromisoformat(ts)
    return dt.replace(tzinfo=timezone.utc)


def fetch_metric(cw, cluster: str, service: str, metric: str, unit: str,
                 start: datetime, end: datetime) -> list[dict]:
    """
    Pull one CloudWatch metric for an ECS service at 1-minute resolution.
    Returns a list of {timestamp, service, metric, value} dicts.
    """
    response = cw.get_metric_data(
        MetricDataQueries=[
            {
                "Id": "m1",
                "MetricStat": {
                    "Metric": {
                        "Namespace": "AWS/ECS",
                        "MetricName": metric,
                        "Dimensions": [
                            {"Name": "ServiceName", "Value": service},
                            {"Name": "ClusterName", "Value": cluster},
                        ],
                    },
                    "Period": 60,
                    "Stat": "Average",
                },
                "ReturnData": True,
            }
        ],
        StartTime=start,
        EndTime=end,
        ScanBy="TimestampAscending",
    )

    rows = []
    result = response["MetricDataResults"][0]
    for ts, val in zip(result["Timestamps"], result["Values"]):
        rows.append({
            "timestamp": ts.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "service":   service,
            "metric":    metric,
            "unit":      unit,
            "value":     round(val, 4),
        })
    return rows


def fetch_service_metrics(cw, cluster: str, service: str,
                          start: datetime, end: datetime) -> list[dict]:
    """Fetch all metrics for one service and return combined rows."""
    all_rows = []
    for metric, unit in METRICS:
        try:
            rows = fetch_metric(cw, cluster, service, metric, unit, start, end)
            all_rows.extend(rows)
            log.info("  %s / %s — %d data points", service, metric, len(rows))
        except (BotoCoreError, ClientError) as exc:
            log.warning("  %s / %s — fetch failed: %s", service, metric, exc)
    return all_rows


# ── S3 helpers ────────────────────────────────────────────────────────────────

def rows_to_csv(rows: list[dict]) -> bytes:
    if not rows:
        return b""
    buf = io.StringIO()
    writer = csv.DictWriter(buf, fieldnames=rows[0].keys())
    writer.writeheader()
    writer.writerows(rows)
    return buf.getvalue().encode()


def upload_to_s3(s3, bucket: str, key: str, data: bytes):
    s3.put_object(Bucket=bucket, Key=key, Body=data)
    log.info("uploaded s3://%s/%s (%d bytes)", bucket, key, len(data))


# ── Main ──────────────────────────────────────────────────────────────────────

def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description="Export CloudWatch ECS metrics to S3 as CSV."
    )
    parser.add_argument(
        "--start", required=True,
        help="Start time in ISO-8601 UTC, e.g. 2024-01-15T10:00:00Z",
    )
    parser.add_argument(
        "--end", required=True,
        help="End time in ISO-8601 UTC, e.g. 2024-01-15T12:00:00Z",
    )
    parser.add_argument(
        "--bucket",
        default=os.getenv("EXPERIMENT_RESULTS_BUCKET", ""),
        help="S3 bucket name (env: EXPERIMENT_RESULTS_BUCKET)",
    )
    parser.add_argument(
        "--cluster",
        default=os.getenv("ECS_CLUSTER_NAME", "hopvault-cluster"),
        help="ECS cluster name (env: ECS_CLUSTER_NAME, default: hopvault-cluster)",
    )
    parser.add_argument(
        "--region",
        default=os.getenv("AWS_DEFAULT_REGION", "us-east-1"),
        help="AWS region (env: AWS_DEFAULT_REGION, default: us-east-1)",
    )
    parser.add_argument(
        "--prefix",
        default="metrics",
        help="S3 key prefix (default: metrics)",
    )
    parser.add_argument(
        "--services",
        default=",".join(SERVICES),
        help="Comma-separated list of ECS service names to pull",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)

    if not args.bucket:
        log.error("--bucket or EXPERIMENT_RESULTS_BUCKET is required")
        sys.exit(1)

    start = _parse_utc(args.start)
    end   = _parse_utc(args.end)
    if end <= start:
        log.error("--end must be after --start")
        sys.exit(1)

    services = [s.strip() for s in args.services.split(",") if s.strip()]
    window   = f"{start.strftime('%Y%m%dT%H%M%S')}_{end.strftime('%Y%m%dT%H%M%S')}"

    log.info("Pulling metrics for window %s → %s", args.start, args.end)
    log.info("Cluster: %s | Services: %s", args.cluster, services)

    cw = boto3.client("cloudwatch", region_name=args.region)
    s3 = boto3.client("s3",         region_name=args.region)

    all_rows: list[dict] = []

    for service in services:
        log.info("Fetching metrics for service: %s", service)
        rows = fetch_service_metrics(cw, args.cluster, service, start, end)

        if rows:
            key  = f"{args.prefix}/{service}_{window}.csv"
            data = rows_to_csv(rows)
            try:
                upload_to_s3(s3, args.bucket, key, data)
            except (BotoCoreError, ClientError) as exc:
                log.error("Failed to upload %s: %s", key, exc)
            all_rows.extend(rows)
        else:
            log.warning("No data returned for %s", service)

    # Combined CSV with all services
    if all_rows:
        key  = f"{args.prefix}/all_services_{window}.csv"
        data = rows_to_csv(all_rows)
        try:
            upload_to_s3(s3, args.bucket, key, data)
        except (BotoCoreError, ClientError) as exc:
            log.error("Failed to upload combined CSV: %s", exc)

    log.info("Done. %d total data points across %d services.", len(all_rows), len(services))


if __name__ == "__main__":
    main()
