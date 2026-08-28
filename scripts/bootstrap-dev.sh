#!/usr/bin/env bash
#
# bootstrap-dev.sh — bring the local compose stack up and make it useful.
#
# Usage:
#   scripts/bootstrap-dev.sh [--no-build] [--timeout SECONDS]
#
#   --no-build        reuse existing images instead of rebuilding
#   --timeout N       how long to wait for readiness (default 180)
#
# What it does, in order: starts deploy/docker-compose.yml, waits for the four
# backing stores to report healthy, applies the PostgreSQL and ClickHouse
# schemas, creates the Kafka topic set, then waits for every service to report
# ready. It is `make dev` plus the two steps that make the stack answer
# questions rather than just be up.
#
# Idempotent: running it twice is a no-op on a healthy stack.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f deploy/docker-compose.yml)
BUILD=1
TIMEOUT=180

while [ $# -gt 0 ]; do
  case "$1" in
    --no-build) BUILD=0; shift ;;
    --timeout)  TIMEOUT="${2:?--timeout needs a value}"; shift 2 ;;
    -h|--help)  sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31mfail\033[0m %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null || die "docker is not installed"
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"

# ---------------------------------------------------------------- start ------

log "starting the stack"
if [ "$BUILD" -eq 1 ]; then
  "${COMPOSE[@]}" up -d --build
else
  "${COMPOSE[@]}" up -d
fi

# ---------------------------------------------------------------- wait -------

# The compose file declares healthchecks for postgres, clickhouse, redis and
# kafka. Waiting on those rather than on a fixed sleep is the difference between
# a script that works on a cold laptop and one that works on yours.
wait_healthy() {
  local svc="$1" deadline=$((SECONDS + TIMEOUT)) state
  log "waiting for ${svc}"
  while [ "$SECONDS" -lt "$deadline" ]; do
    state=$("${COMPOSE[@]}" ps --format json "$svc" 2>/dev/null \
      | sed -n 's/.*"Health":"\([a-z]*\)".*/\1/p' | head -1)
    case "$state" in
      healthy) return 0 ;;
      unhealthy) die "${svc} reported unhealthy; ${COMPOSE[*]} logs ${svc}" ;;
    esac
    sleep 2
  done
  die "${svc} did not become healthy within ${TIMEOUT}s"
}

for svc in postgres clickhouse redis kafka; do
  wait_healthy "$svc"
done

# ------------------------------------------------------------- schema --------

# The platform suppresses signal emission when it cannot persist provenance
# (App.CanEmitSignals, ADR-003). Without these schemas every service starts,
# reports healthy, and emits nothing - which looks like a bug and is not one.
log "applying the PostgreSQL schema"
"${COMPOSE[@]}" exec -T postgres \
  psql -v ON_ERROR_STOP=1 -U quantos -d quantos < deploy/sql/postgres/001_schema.sql

log "applying the ClickHouse schema"
"${COMPOSE[@]}" exec -T clickhouse \
  clickhouse-client --user quantos --password quantos --multiquery \
  < deploy/sql/clickhouse/001_schema.sql

# ------------------------------------------------------------- topics --------

# The kafka-init service in the compose file already does this on first start.
# Running it again is harmless (--if-not-exists) and covers the case where the
# stack was brought up before this script existed.
log "creating the Kafka topic set"
"${COMPOSE[@]}" run --rm --no-deps \
  -v "$PWD/deploy/kafka/create-topics.sh:/scripts/create-topics.sh:ro" \
  --entrypoint /bin/bash kafka-init /scripts/create-topics.sh \
  || warn "topic creation reported an error; check with: ${COMPOSE[*]} logs kafka-init"

# -------------------------------------------------------------- ready --------

# Every service serves /readyz (internal/api.Mount). Readiness here means more
# than "the process is up": it means the universe is loaded, strategies are
# loaded, and provenance can be persisted.
declare -A PORTS=(
  [api-gateway]=8080
  [market-service]=8081
  [signal-service]=8082
  [risk-service]=8083
  [portfolio-service]=8084
  [news-service]=8085
  [evaluation-service]=8086
  [alert-service]=8087
  [backtest-service]=8088
)

wait_ready() {
  local svc="$1" port="$2" deadline=$((SECONDS + TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl -fsS "http://127.0.0.1:${port}/readyz" >/dev/null 2>&1; then
      log "${svc} ready on :${port}"
      return 0
    fi
    sleep 2
  done
  warn "${svc} did not become ready within ${TIMEOUT}s"
  curl -sS "http://127.0.0.1:${port}/readyz" 2>&1 | head -3 >&2 || true
  return 1
}

failed=0
for svc in "${!PORTS[@]}"; do
  wait_ready "$svc" "${PORTS[$svc]}" || failed=1
done

[ "$failed" -eq 0 ] || die "one or more services did not become ready"

# --------------------------------------------------------------- done --------

cat <<'BANNER'

  QuantOS local stack is up.

    API          http://localhost:8080/healthz
    Grafana      http://localhost:3001   (admin / admin)
    Prometheus   http://localhost:9091
    Jaeger       http://localhost:16686

  Log in:   curl -s localhost:8080/api/v1/auth/login \
              -d '{"subject":"demo","password":"demo"}'
  Smoke:    scripts/smoke-test.sh
  Load:     scripts/load-test.sh
  Stop:     docker compose -f deploy/docker-compose.yml down -v

BANNER
