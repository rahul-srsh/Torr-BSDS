#!/usr/bin/env python3
"""
kill_relay.py — Failure injection script for Experiment 3.

Identifies a running relay ECS task and kills it after an optional delay.
Records the task ID, relay nodeId, and exact kill timestamp for correlation
with Locust experiment data.

Usage:
  python scripts/kill_relay.py --cluster hopvault --service relay-service
  python scripts/kill_relay.py --cluster hopvault --service relay-service --delay 30
  python scripts/kill_relay.py --cluster hopvault --delay 60 --output kill_event.json

Flags:
  --cluster    ECS cluster name (required)
  --service    ECS service name (default: relay-service)
  --delay      Seconds to wait before killing the task (default: 0)
  --output     Path to write the kill event JSON (default: stdout only)
  --dry-run    Print what would be killed without actually killing
"""

import argparse
import json
import sys
import time
from datetime import datetime, timezone

import boto3
from botocore.exceptions import BotoCoreError, ClientError


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description="Kill a relay ECS task for failure injection")
    parser.add_argument("--cluster", required=True, help="ECS cluster name")
    parser.add_argument("--service", default="relay-service", help="ECS service name (default: relay-service)")
    parser.add_argument("--delay", type=int, default=0, help="Seconds to wait before kill (default: 0)")
    parser.add_argument("--output", default="", help="Path to write kill event JSON")
    parser.add_argument("--dry-run", action="store_true", help="Print what would be killed without killing")
    return parser.parse_args(argv)


def find_running_task(ecs, cluster, service):
    """Find a running task in the given ECS service."""
    task_arns = ecs.list_tasks(
        cluster=cluster,
        serviceName=service,
        desiredStatus="RUNNING",
    ).get("taskArns", [])

    if not task_arns:
        return None, None

    tasks = ecs.describe_tasks(cluster=cluster, tasks=[task_arns[0]]).get("tasks", [])
    if not tasks:
        return None, None

    task = tasks[0]
    task_arn = task["taskArn"]
    task_id = task_arn.split("/")[-1]

    # Try to extract the node ID from environment variables or tags.
    node_id = ""
    for container in task.get("containers", []):
        for env in container.get("environment", []):
            if env.get("name") == "NODE_ID":
                node_id = env.get("value", "")
                break
        if node_id:
            break

    return task_id, node_id


def kill_task(ecs, cluster, task_id, dry_run=False):
    """Stop an ECS task. Returns True on success."""
    if dry_run:
        print(f"[dry-run] would stop task {task_id} in cluster {cluster}")
        return True

    try:
        ecs.stop_task(
            cluster=cluster,
            task=task_id,
            reason="Experiment 3: failure injection",
        )
        return True
    except (BotoCoreError, ClientError) as exc:
        print(f"[error] failed to stop task {task_id}: {exc}", file=sys.stderr)
        return False


def main(argv=None):
    args = parse_args(argv)
    ecs = boto3.client("ecs")

    # Find a running relay task.
    print(f"[kill_relay] looking for running tasks in {args.cluster}/{args.service}...")
    task_id, node_id = find_running_task(ecs, args.cluster, args.service)

    if not task_id:
        print(f"[kill_relay] no running tasks found in {args.cluster}/{args.service}", file=sys.stderr)
        sys.exit(1)

    print(f"[kill_relay] found task={task_id} nodeId={node_id or 'unknown'}")

    # Wait for the configured delay.
    if args.delay > 0:
        print(f"[kill_relay] waiting {args.delay}s before kill...")
        time.sleep(args.delay)

    # Record the kill timestamp at millisecond precision.
    kill_time = datetime.now(timezone.utc)
    kill_timestamp_ms = int(kill_time.timestamp() * 1000)
    kill_iso = kill_time.isoformat()

    print(f"[kill_relay] killing task {task_id} at {kill_iso} ({kill_timestamp_ms}ms)")

    success = kill_task(ecs, args.cluster, task_id, dry_run=args.dry_run)

    # Build event record.
    event = {
        "cluster": args.cluster,
        "service": args.service,
        "taskId": task_id,
        "nodeId": node_id or "unknown",
        "killTimestamp": kill_iso,
        "killTimestampMs": kill_timestamp_ms,
        "success": success,
        "dryRun": args.dry_run,
    }

    event_json = json.dumps(event, indent=2)
    print(f"[kill_relay] event:\n{event_json}")

    # Optionally write to file.
    if args.output:
        with open(args.output, "w") as f:
            f.write(event_json + "\n")
        print(f"[kill_relay] event written to {args.output}")

    if not success:
        sys.exit(1)


if __name__ == "__main__":
    main()
