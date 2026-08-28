#!/usr/bin/env bash
#
# load-test.sh — put load on the API and check it against the SLO.
#
# Usage:
#   scripts/load-test.sh [--duration 30] [--concurrency 20] [--path /api/v1/signals]
#
# Environment:
#   QUANTOS_API       base URL (default http://localhost:8080)
#   QUANTOS_TOKEN     bearer token; obtained by logging in if unset
#
# Uses k6 if present, then hey, then falls back to a pure-curl generator that
# needs nothing but the shell. The fallback exists because a load test you
# cannot run without installing something is a load test nobody runs.
#
# The target is the API read SLO from docs/operations/slo.md: P99 < 300 ms.
# This script measures P99 of total request time, which includes connection
# setup and is therefore slightly pessimistic against that number - a pass here
# is a real pass, a marginal fail is worth re-measuring server-side against
# quantos_http_server_duration_seconds.
#
# This generates read traffic only. There is deliberately no write mode: the
# platform is paper-trading, POST /api/v1/paper/orders moves a simulated book,
# and filling it with load-test noise makes the evaluation records meaningless.
set -euo pipefail

API="${QUANTOS_API:-http://localhost:8080}"
TOKEN="${QUANTOS_TOKEN:-}"
DURATION=30
CONCURRENCY=20
TARGET_PATH=/api/v1/signals
# The SLO, in milliseconds.
P99_BUDGET_MS=300

while [ $# -gt 0 ]; do
  case "$1" in
    --duration)    DURATION="${2:?}"; shift 2 ;;
    --concurrency) CONCURRENCY="${2:?}"; shift 2 ;;
    --path)        TARGET_PATH="${2:?}"; shift 2 ;;
    -h|--help)     sed -n '2,26p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31mfail\033[0m %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null || die "curl is not installed"

if [ -z "$TOKEN" ]; then
  log "logging in"
  TOKEN=$(curl -sS -H 'Content-Type: application/json' \
    -d '{"subject":"demo","password":"demo"}' \
    "${API}/api/v1/auth/login" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [ -n "$TOKEN" ] || die "could not log in; set QUANTOS_TOKEN"
fi

URL="${API}${TARGET_PATH}"
log "${CONCURRENCY} workers against ${URL} for ${DURATION}s"

# ------------------------------------------------------------------ k6 -------

if command -v k6 >/dev/null 2>&1; then
  log "using k6"
  # The threshold is the SLO. k6 exits non-zero when a threshold is breached,
  # which is what makes this usable in CI rather than only by eye.
  K6_TOKEN="$TOKEN" K6_URL="$URL" k6 run \
    --vus "$CONCURRENCY" --duration "${DURATION}s" \
    --summary-trend-stats "avg,p(95),p(99),max" - <<'K6'
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  thresholds: {
    // docs/operations/slo.md: API read P99 < 300 ms, availability 99.9%.
    'http_req_duration': ['p(99)<300'],
    'http_req_failed': ['rate<0.001'],
  },
};

export default function () {
  const res = http.get(__ENV.K6_URL, {
    headers: { Authorization: `Bearer ${__ENV.K6_TOKEN}` },
  });
  check(res, { 'status 200': (r) => r.status === 200 });
}
K6
  exit $?
fi

# ----------------------------------------------------------------- hey -------

if command -v hey >/dev/null 2>&1; then
  log "using hey"
  hey -z "${DURATION}s" -c "$CONCURRENCY" \
      -H "Authorization: Bearer ${TOKEN}" "$URL" | tee /tmp/ql.hey

  # hey prints the 0.990 quantile in seconds under "Latency distribution".
  p99=$(awk '/99% in/ {print $3}' /tmp/ql.hey)
  if [ -n "$p99" ]; then
    ms=$(awk -v s="$p99" 'BEGIN {printf "%.0f", s * 1000}')
    log "p99 ${ms}ms (budget ${P99_BUDGET_MS}ms)"
    [ "$ms" -le "$P99_BUDGET_MS" ] || die "p99 ${ms}ms exceeds the ${P99_BUDGET_MS}ms SLO"
  fi
  exit 0
fi

# ---------------------------------------------------------- curl fallback ----

log "neither k6 nor hey found; using the curl fallback"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# One background worker per concurrency slot, each appending "status time_total"
# per request to its own file. Separate files rather than one shared file
# because concurrent appends over the pipe buffer size interleave, and a
# corrupted sample is worse than a missing one.
worker() {
  local id="$1" deadline=$((SECONDS + DURATION))
  while [ "$SECONDS" -lt "$deadline" ]; do
    curl -sS -o /dev/null -w '%{http_code} %{time_total}\n' \
      -H "Authorization: Bearer ${TOKEN}" "$URL" >> "${TMP}/w${id}" 2>/dev/null || \
      echo "000 0" >> "${TMP}/w${id}"
  done
}

for i in $(seq 1 "$CONCURRENCY"); do
  worker "$i" &
done
wait

cat "${TMP}"/w* > "${TMP}/all"

total=$(wc -l < "${TMP}/all")
[ "$total" -gt 0 ] || die "no samples collected"
ok_count=$(awk '$1 == "200"' "${TMP}/all" | wc -l)

# Sorting with sort(1) rather than awk's asort: asort is a gawk extension and
# ubuntu-latest ships mawk, so the script would work on a laptop and fail in CI.
awk '{printf "%.3f\n", $2 * 1000}' "${TMP}/all" | sort -n > "${TMP}/sorted"

quantile() {
  local q="$1" idx
  idx=$(awk -v n="$total" -v q="$q" 'BEGIN {i = int(n * q); print (i < 1 ? 1 : i)}')
  sed -n "${idx}p" "${TMP}/sorted"
}

p50=$(quantile 0.50)
p95=$(quantile 0.95)
p99=$(quantile 0.99)
success=$(awk -v ok="$ok_count" -v n="$total" 'BEGIN {printf "%.3f", ok / n * 100}')

printf 'requests   %d\n' "$total"
printf 'success    %s%%\n' "$success"
printf 'p50        %s ms\n' "$p50"
printf 'p95        %s ms\n' "$p95"
printf 'p99        %s ms  (budget %d ms)\n' "$p99" "$P99_BUDGET_MS"

# docs/operations/slo.md: API availability 99.9%, API read P99 < 300 ms.
awk -v s="$success" 'BEGIN {exit (s >= 99.9) ? 0 : 1}' \
  || die "availability ${success}% is below the 99.9% SLO"
awk -v p="$p99" -v b="$P99_BUDGET_MS" 'BEGIN {exit (p <= b) ? 0 : 1}' \
  || die "p99 ${p99}ms exceeds the ${P99_BUDGET_MS}ms SLO"

printf '\033[32mwithin SLO\033[0m\n'
