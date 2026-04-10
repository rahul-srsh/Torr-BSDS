# Torr-BSDS experiment Makefile
#
# Required env vars (set in your shell or pass on the command line):
#   DIRECTORY_URL      — directory server base URL
#   ECHO_SERVER_URL    — echo server URL (e.g. http://<ip>:8080/echo)
#   AWS_REGION         — AWS region (default: us-east-1)
#   CLUSTER            — ECS cluster name (default: hopvault-cluster)
#   RESULTS_BUCKET     — S3 bucket for experiment results
#
# Usage examples:
#   make scale-relays COUNT=5
#   make run-direct
#   make run-1hop STAGE_DURATION=90
#   make run-3hop CONCURRENCY_LEVELS=10,50,100,200
#   make run-exp2 RELAY_COUNT=10

# ── Defaults ──────────────────────────────────────────────────────────────────

AWS_REGION         ?= us-east-1
CLUSTER            ?= hopvault-cluster
RELAY_SERVICE      ?= relay-node
RESULTS_BUCKET     ?= $(shell aws ecs describe-services \
                          --cluster $(CLUSTER) \
                          --services directory-server \
                          --query 'services[0].serviceArn' \
                          --output text 2>/dev/null || echo "")

DIRECTORY_URL      ?=
ECHO_SERVER_URL    ?=

# Locust common settings
CONCURRENCY_LEVELS ?= 10,50,100,200
STAGE_DURATION     ?= 60
LOCUST_DIR         := experiments/locust

# Experiment 2
RELAY_COUNT        ?= 2
MIN_USERS          ?= 10
MAX_USERS          ?= 500
STEP_SIZE          ?= 10
STEP_DURATION      ?= 30
P95_THRESHOLD_MS   ?= 500
ERROR_RATE_THRESHOLD ?= 0.01

# scale-relays timeouts
ECS_WAIT_SECS      ?= 300   # 5 minutes for Fargate provisioning
DIR_WAIT_SECS      ?= 120   # 2 minutes for registration

.PHONY: help scale-relays \
        run-direct run-1hop run-3hop \
        run-exp2 \
        charts-exp1 charts-exp2 \
        export-metrics

# ── Help ──────────────────────────────────────────────────────────────────────

help:
	@echo ""
	@echo "Torr-BSDS experiment targets"
	@echo "────────────────────────────────────────────────────────"
	@echo "  scale-relays COUNT=N       Scale relay ECS service to N tasks"
	@echo "                             and wait until all are registered"
	@echo ""
	@echo "  run-direct                 Experiment 1 — direct baseline"
	@echo "  run-1hop                   Experiment 1 — 1-hop circuit"
	@echo "  run-3hop                   Experiment 1 — 3-hop circuit"
	@echo ""
	@echo "  run-exp2 RELAY_COUNT=N     Experiment 2 — throughput scaling"
	@echo ""
	@echo "  charts-exp1                Generate Experiment 1 charts"
	@echo "  charts-exp2                Generate Experiment 2 charts"
	@echo ""
	@echo "  export-metrics START=... END=...   Pull CloudWatch metrics to S3"
	@echo ""
	@echo "Required env vars: DIRECTORY_URL, ECHO_SERVER_URL, RESULTS_BUCKET"
	@echo ""

# ── scale-relays ──────────────────────────────────────────────────────────────

scale-relays:
ifndef COUNT
	$(error COUNT is required: make scale-relays COUNT=5)
endif
	@echo "→ Scaling $(RELAY_SERVICE) to $(COUNT) tasks on cluster $(CLUSTER)..."
	@aws ecs update-service \
	    --cluster $(CLUSTER) \
	    --service $(RELAY_SERVICE) \
	    --desired-count $(COUNT) \
	    --region $(AWS_REGION) \
	    --output text --query 'service.serviceName' > /dev/null
	@echo "→ Waiting for $(COUNT) ECS tasks to reach RUNNING state (max $(ECS_WAIT_SECS)s)..."
	@$(MAKE) --no-print-directory _wait-ecs-running COUNT=$(COUNT)
	@echo "→ Waiting for $(COUNT) relays to register with directory server..."
	@$(MAKE) --no-print-directory _wait-dir-registered COUNT=$(COUNT)
	@echo "✓ $(COUNT) relay nodes are running and registered."

_wait-ecs-running:
	@elapsed=0; \
	while [ $$elapsed -lt $(ECS_WAIT_SECS) ]; do \
	    running=$$(aws ecs describe-services \
	        --cluster $(CLUSTER) \
	        --services $(RELAY_SERVICE) \
	        --region $(AWS_REGION) \
	        --query 'services[0].runningCount' \
	        --output text 2>/dev/null); \
	    if [ "$$running" = "$(COUNT)" ]; then \
	        echo "  ECS: $(COUNT)/$(COUNT) tasks running ($$elapsed s)"; \
	        exit 0; \
	    fi; \
	    echo "  ECS: $$running/$(COUNT) running... ($$elapsed s)"; \
	    sleep 10; \
	    elapsed=$$((elapsed + 10)); \
	done; \
	echo "✗ Timed out waiting for ECS tasks after $(ECS_WAIT_SECS)s"; \
	exit 1

_wait-dir-registered:
ifndef DIRECTORY_URL
	$(error DIRECTORY_URL is required for relay registration check)
endif
	@elapsed=0; \
	while [ $$elapsed -lt $(DIR_WAIT_SECS) ]; do \
	    count=$$(curl -sf $(DIRECTORY_URL)/nodes \
	        | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('relay',[])))" \
	        2>/dev/null || echo 0); \
	    if [ "$$count" = "$(COUNT)" ]; then \
	        echo "  Directory: $(COUNT)/$(COUNT) relays registered ($$elapsed s)"; \
	        exit 0; \
	    fi; \
	    echo "  Directory: $$count/$(COUNT) relays registered... ($$elapsed s)"; \
	    sleep 5; \
	    elapsed=$$((elapsed + 5)); \
	done; \
	echo "✗ Timed out waiting for directory registration after $(DIR_WAIT_SECS)s"; \
	exit 1

# ── Experiment 1 ──────────────────────────────────────────────────────────────

run-direct:
ifndef ECHO_SERVER_URL
	$(error ECHO_SERVER_URL is required)
endif
	@echo "→ Running Experiment 1: direct baseline"
	cd $(LOCUST_DIR) && \
	EXPERIMENT_RESULTS_BUCKET=$(RESULTS_BUCKET) \
	CONCURRENCY_LEVELS=$(CONCURRENCY_LEVELS) \
	STAGE_DURATION=$(STAGE_DURATION) \
	TARGET_PATH=/echo \
	locust -f echo_baseline.py \
	    --headless \
	    --host $(ECHO_SERVER_URL) \
	    --csv=../../experiments/results/direct \
	    --logfile=../../experiments/results/direct_locust.log

run-1hop:
ifndef DIRECTORY_URL
	$(error DIRECTORY_URL is required)
endif
ifndef ECHO_SERVER_URL
	$(error ECHO_SERVER_URL is required)
endif
	@echo "→ Running Experiment 1: 1-hop circuit"
	cd $(LOCUST_DIR) && \
	DIRECTORY_URL=$(DIRECTORY_URL) \
	ECHO_SERVER_URL=$(ECHO_SERVER_URL) \
	EXPERIMENT_RESULTS_BUCKET=$(RESULTS_BUCKET) \
	CONCURRENCY_LEVELS=$(CONCURRENCY_LEVELS) \
	STAGE_DURATION=$(STAGE_DURATION) \
	locust -f circuit_1hop.py \
	    --headless \
	    --host $(DIRECTORY_URL) \
	    --csv=../../experiments/results/1hop \
	    --logfile=../../experiments/results/1hop_locust.log

run-3hop:
ifndef DIRECTORY_URL
	$(error DIRECTORY_URL is required)
endif
ifndef ECHO_SERVER_URL
	$(error ECHO_SERVER_URL is required)
endif
	@echo "→ Running Experiment 1: 3-hop circuit"
	cd $(LOCUST_DIR) && \
	DIRECTORY_URL=$(DIRECTORY_URL) \
	ECHO_SERVER_URL=$(ECHO_SERVER_URL) \
	EXPERIMENT_RESULTS_BUCKET=$(RESULTS_BUCKET) \
	CONCURRENCY_LEVELS=$(CONCURRENCY_LEVELS) \
	STAGE_DURATION=$(STAGE_DURATION) \
	locust -f circuit_3hop.py \
	    --headless \
	    --host $(DIRECTORY_URL) \
	    --csv=../../experiments/results/3hop \
	    --logfile=../../experiments/results/3hop_locust.log

# ── Experiment 2 ──────────────────────────────────────────────────────────────

run-exp2:
ifndef DIRECTORY_URL
	$(error DIRECTORY_URL is required)
endif
ifndef ECHO_SERVER_URL
	$(error ECHO_SERVER_URL is required)
endif
	@echo "→ Running Experiment 2: throughput scaling (relay_count=$(RELAY_COUNT))"
	cd $(LOCUST_DIR) && \
	DIRECTORY_URL=$(DIRECTORY_URL) \
	ECHO_SERVER_URL=$(ECHO_SERVER_URL) \
	RELAY_COUNT=$(RELAY_COUNT) \
	MIN_USERS=$(MIN_USERS) \
	MAX_USERS=$(MAX_USERS) \
	STEP_SIZE=$(STEP_SIZE) \
	STEP_DURATION=$(STEP_DURATION) \
	P95_THRESHOLD_MS=$(P95_THRESHOLD_MS) \
	ERROR_RATE_THRESHOLD=$(ERROR_RATE_THRESHOLD) \
	EXPERIMENT_RESULTS_BUCKET=$(RESULTS_BUCKET) \
	locust -f throughput_scaling.py \
	    --headless \
	    --host $(DIRECTORY_URL) \
	    --csv=../../experiments/results/exp2_relays$(RELAY_COUNT) \
	    --logfile=../../experiments/results/exp2_relays$(RELAY_COUNT)_locust.log

# ── Charts ────────────────────────────────────────────────────────────────────

charts-exp1:
	@echo "→ Generating Experiment 1 charts"
	python3 experiments/scripts/generate_exp1_charts.py \
	    --bucket $(RESULTS_BUCKET) \
	    --region $(AWS_REGION) \
	    --out-dir docs/charts

charts-exp2:
	@echo "→ Generating Experiment 2 charts"
	python3 experiments/scripts/generate_exp2_charts.py \
	    --bucket $(RESULTS_BUCKET) \
	    --region $(AWS_REGION) \
	    --out-dir docs/charts

# ── CloudWatch export ─────────────────────────────────────────────────────────

export-metrics:
ifndef START
	$(error START is required: make export-metrics START=2024-01-15T10:00:00Z END=...)
endif
ifndef END
	$(error END is required: make export-metrics START=... END=2024-01-15T12:00:00Z)
endif
	@echo "→ Exporting CloudWatch metrics ($(START) → $(END))"
	python3 experiments/scripts/export_cloudwatch_metrics.py \
	    --start $(START) \
	    --end   $(END) \
	    --bucket $(RESULTS_BUCKET) \
	    --region $(AWS_REGION) \
	    --cluster $(CLUSTER)
