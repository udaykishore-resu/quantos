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
dev: compose-up wait-infra migrate ## bring up the full local stack (Kafka, Postgres, ClickHouse, Redis, observability)
	@echo ""
	@echo "  QuantOS local stack is up."
	@echo "    API          http://localhost:8080/healthz"
	@echo "    Dashboard    http://localhost:3000   (run 'make frontend' in another shell)"
	@echo "    Grafana      http://localhost:3001   (admin / admin)"
	@echo "    Prometheus   http://localhost:9091"
	@echo "    Jaeger       http://localhost:16686"
	@echo ""
	@echo "  Tail the platform:  $(COMPOSE) logs -f signal-service"
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
