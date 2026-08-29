### QuantOS — developer entry points
#
# The two that matter:
#   make demo   runs the ten scenarios in one process, no infrastructure
#   make dev    brings up the full local stack and the dashboard
#
# Everything else is a target you can read before you run.

SHELL       := /usr/bin/env bash
GO          ?= go
GOFLAGS     ?=
PKG         := ./...
BIN         := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS     := -s -w \
	-X github.com/udaykishoreresu/quantos/internal/cli.buildVersion=$(VERSION) \
	-X github.com/udaykishoreresu/quantos/internal/cli.buildCommit=$(COMMIT)
SERVICES    := market-service signal-service risk-service portfolio-service \
               news-service evaluation-service alert-service backtest-service api-gateway
COMPOSE     := docker compose -f deploy/docker-compose.yml
PYTHON      ?= python3
VENV        := ml/.venv

.DEFAULT_GOAL := help

## ---------------------------------------------------------------- help ----

.PHONY: help
help: ## show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nQuantOS targets\n\n"} \
	/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo ""

##@ Run

.PHONY: demo
demo: ## run the ten demo scenarios in one process (no infrastructure needed)
	$(GO) run ./cmd/quantos demo

.PHONY: run
run: ## run every role in one process with the in-memory stack
	$(GO) run ./cmd/quantos run

.PHONY: dev
# The default profile is the backing stores plus one all-roles container. The
# nine role services live behind `--profile split` and need a broker driver
# that does not exist yet; the note at the top of deploy/docker-compose.yml
# explains why.
dev: compose-up wait-infra migrate ## bring up the full local stack (Postgres, ClickHouse, Redis, Kafka, observability)
	@echo ""
	@echo "  QuantOS local stack is up."
	@echo "    API          http://localhost:8080/healthz"
	@echo "    Dashboard    http://localhost:3000   (run 'make frontend' in another shell)"
	@echo "    Grafana      http://localhost:3001   (admin / admin)"
	@echo "    Prometheus   http://localhost:9091"
	@echo "    Jaeger       http://localhost:16686"
	@echo ""
	@echo "  Tail the platform:  $(COMPOSE) logs -f quantos"
	@echo "  Stop everything:    make dev-down"

.PHONY: dev-down
dev-down: ## stop the local stack and remove volumes
	$(COMPOSE) down -v

.PHONY: compose-up
compose-up:
	$(COMPOSE) up -d --build

.PHONY: wait-infra
wait-infra:
	@echo "waiting for infrastructure to become healthy..."
	@for i in $$(seq 1 60); do \
		if $(COMPOSE) ps --format json 2>/dev/null | grep -q '"Health":"starting"'; then sleep 2; else break; fi; \
	done
	@echo "infrastructure ready"

.PHONY: migrate
migrate: ## apply the PostgreSQL and ClickHouse schemas
	@echo "applying PostgreSQL schema..."
	@$(COMPOSE) exec -T postgres psql -v ON_ERROR_STOP=1 -U quantos -d quantos \
		< deploy/sql/postgres/001_schema.sql
	@echo "applying ClickHouse schema..."
	@$(COMPOSE) exec -T clickhouse clickhouse-client --multiquery \
		< deploy/sql/clickhouse/001_schema.sql
	@echo "schemas applied"

.PHONY: frontend
frontend: ## run the dashboard in development mode
	cd frontend && npm install && npm run dev

##@ Build

.PHONY: build
build: $(addprefix build-,$(SERVICES)) build-quantos ## build every binary into ./bin

build-quantos:
	@mkdir -p $(BIN)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/quantos ./cmd/quantos

build-%:
	@mkdir -p $(BIN)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$* ./services/$*

.PHONY: docker
docker: ## build container images for every service
	@for s in $(SERVICES); do \
		echo "building quantos/$$s:$(VERSION)"; \
		docker build -f deploy/Dockerfile --build-arg SERVICE=$$s \
			-t quantos/$$s:$(VERSION) -t quantos/$$s:latest . || exit 1; \
	done

##@ Test

.PHONY: test
test: ## run unit and integration tests with the race detector
	$(GO) test -race -count=1 -timeout 10m $(PKG)

.PHONY: test-short
test-short: ## run fast tests only
	$(GO) test -short -count=1 $(PKG)

.PHONY: cover
cover: ## produce a coverage report over the packages that carry logic
	$(GO) test -count=1 -coverprofile=coverage.out \
		-coverpkg=./internal/... ./internal/... ./tests/...
	@$(GO) tool cover -func=coverage.out | tail -1
	@$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: test-failure
test-failure: ## run the failure-injection suite
	$(GO) test -count=1 -tags failure ./tests/failure/...

.PHONY: test-load
test-load: ## run the load harness against the in-process pipeline
	$(GO) test -count=1 -run TestLoad -bench=. -benchtime=10s ./tests/load/...

.PHONY: bench
bench: ## run micro-benchmarks
	$(GO) test -run '^$$' -bench=. -benchmem ./internal/features/... ./internal/mlinfer/...

##@ Quality

.PHONY: lint
lint: fmt-check vet arch ## run every static check

.PHONY: fmt
fmt: ## format the Go sources
	gofmt -w $(shell find . -name '*.go' -not -path './frontend/*')

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './frontend/*')); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet:
	$(GO) vet $(PKG)

.PHONY: arch
arch: ## assert the architectural invariants (ADR-005, ADR-011)
	$(GO) test -count=1 -run 'TestArchitecture' ./tests/integration/...

.PHONY: validate
validate: ## validate configuration and strategies
	$(GO) run ./cmd/quantos validate

.PHONY: tidy
tidy:
	$(GO) mod tidy

##@ Research

.PHONY: export-features
export-features: ## export labelled feature snapshots for Python research
	$(GO) run ./cmd/quantos export-features -days 45 -out ml/data/features.csv

.PHONY: venv
venv: ## create the Python research environment
	$(PYTHON) -m venv $(VENV)
	$(VENV)/bin/pip install --upgrade pip
	$(VENV)/bin/pip install -r ml/requirements.txt

.PHONY: train
train: export-features ## train the direction model and write a versioned artifact
	$(PYTHON) ml/training/train.py \
		--data ml/data/features.csv \
		--out ml/artifacts \
		--model-id direction-3class \
		--horizon 60m

.PHONY: evaluate
evaluate: ## run the walk-forward evaluation and write the report
	$(PYTHON) ml/evaluation/walk_forward.py --data ml/data/features.csv --out ml/reports

.PHONY: backtest
backtest: ## run a deterministic backtest over simulated history
	$(GO) run ./cmd/quantos backtest -strategy momentum -days 45 -seed 20260827

##@ Kubernetes (local)

# The local cluster is one kind node running the full backing stack and QuantOS
# as a single all-roles process. The reason it is not the nine-service split in
# infra/kubernetes/base is in infra/kubernetes/local/quantos.yaml: that split
# needs a broker, and drivers/kafka does not exist yet.
KIND_CLUSTER ?= quantos
KUBECTX      := kind-$(KIND_CLUSTER)
K8S_LOCAL    := infra/kubernetes/local
# kustomize refuses by default to read files outside the kustomization root,
# and the local overlay generates ConfigMaps from deploy/sql and deploy/grafana
# so the cluster applies exactly the files the compose stack does. Copying them
# in would guarantee they drift.
KUSTOMIZE_FLAGS := --load-restrictor LoadRestrictionsNone

.PHONY: k8s-preflight
k8s-preflight: ## check that docker, kind and kubectl are present
	@command -v docker  >/dev/null || { echo "docker not found: install Docker Desktop or colima"; exit 1; }
	@docker info >/dev/null 2>&1 || { echo "the docker daemon is not running"; exit 1; }
	@command -v kind    >/dev/null || { echo "kind not found: brew install kind"; exit 1; }
	@command -v kubectl >/dev/null || { echo "kubectl not found: brew install kubectl"; exit 1; }
	@command -v kustomize >/dev/null || { echo "kustomize not found: brew install kustomize"; exit 1; }
	@echo "docker, kind, kubectl and kustomize are all present"

.PHONY: k8s-cluster
k8s-cluster: k8s-preflight ## create the kind cluster (idempotent)
	@if kind get clusters 2>/dev/null | grep -qx "$(KIND_CLUSTER)"; then \
		echo "cluster $(KIND_CLUSTER) already exists"; \
	else \
		kind create cluster --name $(KIND_CLUSTER) --config $(K8S_LOCAL)/kind-cluster.yaml; \
	fi

.PHONY: k8s-image
k8s-image: ## build the all-roles image and load it into the cluster
	@echo "building quantos/quantos:local ..."
	DOCKER_BUILDKIT=1 docker build \
		-f deploy/Dockerfile \
		--build-arg SERVICE=quantos \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t quantos/quantos:local .
	@echo "loading it into the node (there is no registry, so this is the only way in) ..."
	kind load docker-image quantos/quantos:local --name $(KIND_CLUSTER)

.PHONY: k8s-render
k8s-render: ## print the manifests without applying them
	kustomize build $(KUSTOMIZE_FLAGS) $(K8S_LOCAL)

.PHONY: k8s-up
k8s-up: k8s-cluster k8s-image ## create the cluster, build and load the image, apply everything
	kustomize build $(KUSTOMIZE_FLAGS) $(K8S_LOCAL) | kubectl --context $(KUBECTX) apply -f -
	@echo ""
	@echo "  waiting for the backing stores..."
	kubectl --context $(KUBECTX) -n quantos rollout status statefulset/postgres   --timeout=300s
	kubectl --context $(KUBECTX) -n quantos rollout status statefulset/redis      --timeout=180s
	kubectl --context $(KUBECTX) -n quantos rollout status statefulset/clickhouse --timeout=300s
	@echo "  waiting for QuantOS (warm-up replays 260 bars per symbol, so this is slow the first time)..."
	kubectl --context $(KUBECTX) -n quantos rollout status deployment/quantos --timeout=600s
	@$(MAKE) --no-print-directory k8s-status

.PHONY: k8s-status
k8s-status: ## what is running, and where to reach it
	@kubectl --context $(KUBECTX) -n quantos get pods -o wide
	@echo ""
	@echo "  API          http://localhost:8080/healthz"
	@echo "  Prometheus   http://localhost:9091"
	@echo "  Grafana      http://localhost:3001   (anonymous viewer, or admin/admin)"
	@echo ""
	@echo "  A token:  curl -s -XPOST localhost:8080/api/v1/auth/login \\"
	@echo "              -H 'content-type: application/json' \\"
	@echo "              -d '{\"subject\":\"demo\",\"password\":\"demo\"}'"
	@echo ""
	@echo "  The dashboard runs outside the cluster:"
	@echo "    cd frontend && QUANTOS_API_URL=http://localhost:8080 npm run dev"

.PHONY: k8s-logs
k8s-logs: ## follow the platform logs
	kubectl --context $(KUBECTX) -n quantos logs -f deployment/quantos

.PHONY: k8s-psql
k8s-psql: ## open a psql shell against the in-cluster database
	kubectl --context $(KUBECTX) -n quantos exec -it statefulset/postgres -- \
		psql -U quantos -d quantos

.PHONY: k8s-restart
k8s-restart: k8s-image ## rebuild the image and restart the platform pod
	kubectl --context $(KUBECTX) -n quantos rollout restart deployment/quantos
	kubectl --context $(KUBECTX) -n quantos rollout status deployment/quantos --timeout=600s

.PHONY: k8s-down
k8s-down: ## delete the QuantOS objects but keep the cluster
	kustomize build $(KUSTOMIZE_FLAGS) $(K8S_LOCAL) | kubectl --context $(KUBECTX) delete --ignore-not-found -f -

.PHONY: k8s-destroy
k8s-destroy: ## delete the whole kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

##@ Operations

.PHONY: topics
topics: ## print the event topic specification
	$(GO) run ./cmd/quantos topics

.PHONY: kafka-driver
kafka-driver: ## build the optional Kafka bus driver (separate module)
	cd drivers/kafka && $(GO) build ./...

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out coverage.html .quantos ml/data ml/reports

.PHONY: ci
ci: lint test cover ## what CI runs
