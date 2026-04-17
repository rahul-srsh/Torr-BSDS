# HopVault Week 4 — Experiment 3: Failure & Recovery

## Overview

Experiment 3 answers: "What happens when an onion routing node dies mid-traffic, and how fast can the client recover?"

We run the same experiment twice:
1. **Without pre-detection**: client only discovers the dead node when its request times out
2. **With pre-detection**: client polls the directory server for node health every 5s and proactively rebuilds before the next request fails

We compare detection time, rebuild time, and request loss between both approaches.

---

## Task Breakdown

### PHASE 1: Pure Code (no AWS needed, build and test locally)

#### Task 1: Client circuit rebuild on failure
**Type:** Code only
**File:** `client/client.go`

Implement failure detection and circuit rebuild logic in the client:

- When a request through the circuit fails (connection refused, timeout, unexpected disconnect), the client must:
  1. Detect which hop failed
  2. Discard the entire circuit (all 3 session keys)
  3. Query the directory server for a new circuit (which will exclude the dead node since it's failed its heartbeats)
  4. Perform key exchange with the new nodes
  5. Retry the original request on the new circuit
- Add a configurable retry limit (default 3 attempts) so the client doesn't loop forever
- Add a configurable circuit timeout (default 15s) — if no response within N seconds, treat circuit as broken
- Unit test: mock a failing relay, verify client rebuilds and retries
- Integration test: kill a relay mid-request, verify client recovers

#### Task 2: Client health-check pre-detection goroutine
**Type:** Code only
**File:** `client/client.go`

Add a background goroutine to the client that:

- Polls `GET /nodes` on the directory server every 5 seconds
- Compares the healthy node list against the current circuit's nodes
- If any node in the current circuit is no longer in the healthy list, proactively rebuild the circuit BEFORE the next request
- This should be togglable via a flag (e.g., `--health-check=true`) so we can run the experiment with and without it

#### Task 3: Failure injection script
**Type:** Code (will be run against AWS later)
**File:** `scripts/kill_relay.py` or `scripts/kill_relay.sh`

Write a script that:

- Identifies a running relay ECS task (by querying ECS or the directory server)
- Records the task ID, relay nodeId, and exact timestamp (millisecond precision)
- Calls `aws ecs stop-task` to kill it
- Has a configurable delay before kill (e.g., `--delay 30s` means "wait 30 seconds then kill")
- Optional: simulate network partition by modifying security group rules instead of killing the task
- Must be idempotent — running it twice doesn't cause errors
- Logs kill timestamp to stdout/file for correlation with Locust data

#### Task 4: Locust script for failure recovery testing
**Type:** Code only
**File:** `experiments/experiment3/locustfile.py`

Write a Locust script that:

- Runs at steady concurrency (configurable, default 50 users)
- Logs per-request: timestamp, success/failure, latency, circuit rebuild flag, rebuild duration
- Runs for 5+ minutes total
- Designed to coordinate with the failure injection script (kill happens at ~60s mark)
- Captures the exact window of request failures during recovery
- Differentiates between "request failed, circuit rebuilt successfully" and "request failed, rebuild also failed"
- Outputs results as CSV for chart generation

#### Task 5: Chart generation script
**Type:** Code only
**File:** `experiments/experiment3/generate_charts.py`

Python script using matplotlib that reads experiment CSV data and generates:

- Chart 1: Timeline — requests per second over time, vertical line at kill event. Overlay both runs (with and without pre-detection)
- Chart 2: Bar chart — detection time comparison (without vs. with pre-detection)
- Chart 3: Bar chart — total request loss comparison (without vs. with pre-detection)
- Chart 4: Scatter plot — per-request latency over time, showing the spike during recovery
- Chart 5: Timeline — when each client rebuilt its circuit relative to the kill event
- All charts clearly label the failure injection point
- Save PNGs to `docs/charts/` and S3 under `experiment-3/`

---

### PHASE 2: AWS Deployment & Experiments

#### Task 6: Run Experiment 3 — WITHOUT health-check pre-detection
**Type:** AWS experiment
**Requires:** Tasks 1, 3, 4 complete. Cluster deployed on ECS.

Process:
1. Ensure all services are running on ECS (directory, guard, relay, exit, echo server)
2. Start Locust at 50 concurrent users with `--health-check=false`
3. Let it stabilize for 60 seconds
4. Run the failure injection script to kill one relay
5. Let the experiment continue for another 4 minutes
6. Collect all data

Metrics to capture:
- **Detection time**: time between task kill and first client-side failure
- **Rebuild time**: time between first failure and first successful request on new circuit
- **Request loss**: total requests that failed during the recovery window
- CloudWatch metrics for all services during the run
- Save everything to S3 under `experiment-3/no-predetection/`

#### Task 7: Run Experiment 3 — WITH health-check pre-detection
**Type:** AWS experiment
**Requires:** Tasks 1, 2, 3, 4 complete. Cluster deployed on ECS.

Same process as Task 6 but with `--health-check=true`.

Expected improvements:
- Detection time should be faster (bounded by poll interval 5s + heartbeat timeout)
- Rebuild time should be similar (same rebuild logic)
- Request loss should be lower (some clients rebuild before their next request fails)
- Save everything to S3 under `experiment-3/with-predetection/`

#### Task 8: Generate charts and compare
**Type:** Code (local, after experiments)
**Requires:** Tasks 6, 7 complete with CSV data downloaded.

Run Task 5's chart generation script against both experiment datasets. Commit charts to `docs/charts/` in the repo.

---

## Execution Order

```
PHASE 1 — Build locally, no AWS needed:
  Task 1 (circuit rebuild)     ← do this first, everything depends on it
  Task 2 (health-check polling) ← depends on Task 1
  Task 3 (kill script)          ← independent, can parallel with Task 1
  Task 4 (Locust script)        ← depends on Task 1 for rebuild logging
  Task 5 (chart script)         ← write structure now, finalize after data exists

PHASE 2 — Deploy to AWS and run:
  Task 6 (experiment without pre-detection) ← needs Tasks 1, 3, 4
  Task 7 (experiment with pre-detection)    ← needs Tasks 1, 2, 3, 4
  Task 8 (generate charts)                  ← needs Tasks 6, 7
```

---

## File Structure

```
client/
  client.go              ← modify: add rebuild logic + health-check goroutine
  client_test.go         ← add: rebuild unit tests
  integration_test.go    ← add: kill-and-recover integration test

scripts/
  kill_relay.py          ← new: failure injection script

experiments/experiment3/
  locustfile.py          ← new: Locust script for failure recovery
  generate_charts.py     ← new: chart generation from CSV data
  README.md              ← new: how to run the experiment

docs/charts/
  *.png                  ← generated charts
```

---

## Claude Code Instructions

When implementing these tasks:

1. **Start with Task 1** — the circuit rebuild logic. This is the core feature. Make sure it compiles and the unit test passes before moving on.
2. **Task 2** is a small addition to client.go — a background goroutine with a ticker. Keep it behind a flag so it can be toggled.
3. **Task 3** — the kill script should be a standalone Python/bash script that takes `--cluster`, `--delay`, and `--service` flags. Use boto3 for AWS API calls.
4. **Task 4** — the Locust script should log CSV rows with columns: `timestamp,status,latency_ms,circuit_rebuilt,rebuild_duration_ms`. Make it importable by Locust CLI.
5. **Task 5** — write the chart script to accept a `--data-dir` flag pointing to the CSV directory. Hardcode the kill timestamp from the injection script's output.
6. **Do NOT attempt to run AWS experiments** — those are manual steps done by the team on the Learner Lab.