#!/usr/bin/env bash
#
# smoke-test.sh — is this deployment actually working?
#
# Usage:
#   scripts/smoke-test.sh
#   QUANTOS_API=http://localhost:8080 scripts/smoke-test.sh
#   QUANTOS_TOKEN=<jwt> scripts/smoke-test.sh      # skip the login step
#
# Environment:
#   QUANTOS_API       base URL (default http://localhost:8080)
#   QUANTOS_TOKEN     bearer token; obtained by logging in if unset
#   QUANTOS_SUBJECT   login subject (default demo)
#   QUANTOS_PASSWORD  login password (default demo)
#
# Checks the two probes and four real endpoints. Exits non-zero and prints the
# offending response on the first failure, because a smoke test that reports
# "3 of 6 passed" and exits 0 is a smoke test nobody reads.
#
# The dev credentials below only exist in embedded and compose mode
# (config auth.dev_users). In cluster mode, pass QUANTOS_TOKEN.
set -euo pipefail

API="${QUANTOS_API:-http://localhost:8080}"
SUBJECT="${QUANTOS_SUBJECT:-demo}"
PASSWORD="${QUANTOS_PASSWORD:-demo}"
TOKEN="${QUANTOS_TOKEN:-}"

pass=0
fail=0

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m  ok\033[0m %s\n' "$*"; pass=$((pass + 1)); }
bad()  { printf '\033[31mfail\033[0m %s\n' "$*" >&2; fail=$((fail + 1)); }
die()  { printf '\033[31mfail\033[0m %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null || die "curl is not installed"

# get PATH EXPECTED_STATUS [DESCRIPTION]
#
# Prints the body on an unexpected status. The body matters here: the API's
# uniform envelope (internal/httpx) carries a `degraded` list, and a 200 with a
# non-empty degraded list is a different situation from a clean 200.
get() {
  local path="$1" want="$2" desc="${3:-$1}" auth=()
  [ -n "$TOKEN" ] && auth=(-H "Authorization: Bearer ${TOKEN}")

  local body status
  body=$(curl -sS -o /tmp/qs.body -w '%{http_code}' "${auth[@]}" "${API}${path}" 2>/tmp/qs.err) || {
    bad "${desc}: request failed — $(head -1 /tmp/qs.err)"
    return 1
  }
  status="$body"
  if [ "$status" != "$want" ]; then
    bad "${desc}: expected ${want}, got ${status}"
    head -c 400 /tmp/qs.body >&2; echo >&2
    return 1
  fi
  ok "${desc} (${status})"
  return 0
}

log "target ${API}"

# ------------------------------------------------------------- probes --------

# /healthz returns 200 even when degraded, and lists what is degraded in the
# body. A 200 here says the process is up, not that it is doing its job.
get /healthz 200 "healthz"

degraded=$(sed -n 's/.*"degraded":\[\([^]]*\)\].*/\1/p' /tmp/qs.body || true)
if [ -n "$degraded" ]; then
  printf '\033[33m warn\033[0m degraded subsystems: %s\n' "$degraded" >&2
fi

# /readyz is the one that matters. It returns 503 when the universe is empty,
# when no strategies are loaded, or when App.CanEmitSignals is false - the last
# meaning the provenance store is unwritable and signal emission is suspended
# (ADR-003). A deployment that is healthy but not ready is emitting nothing.
if ! get /readyz 200 "readyz"; then
  echo "  -> the reason is in the body above; docs/runbooks/postgres-unavailable.md" >&2
fi

# ------------------------------------------------------------- auth ----------

if [ -z "$TOKEN" ]; then
  log "logging in as ${SUBJECT}"
  if ! curl -sS -o /tmp/qs.login -w '%{http_code}' \
        -H 'Content-Type: application/json' \
        -d "{\"subject\":\"${SUBJECT}\",\"password\":\"${PASSWORD}\"}" \
        "${API}/api/v1/auth/login" | grep -q '^200$'; then
    bad "login failed"
    head -c 300 /tmp/qs.login >&2; echo >&2
    echo "  -> dev_users exist only in embedded and compose mode; set QUANTOS_TOKEN for a cluster" >&2
  else
    TOKEN=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' /tmp/qs.login)
    if [ -n "$TOKEN" ]; then
      ok "login"
    else
      bad "login returned 200 with no token"
    fi
  fi
fi

[ -n "$TOKEN" ] || die "no token; cannot exercise the authenticated endpoints"

# ------------------------------------------------------------ endpoints ------

# Four real reads, one per scope group in internal/api.Mount, so a broken
# authorisation middleware shows up as a 403 on one of them rather than as a
# uniformly green run.
get /api/v1/market/regime 200 "market regime (read:market)"
get /api/v1/stocks        200 "universe (read:market)"
get /api/v1/signals       200 "signals (read:signals)"
get /api/v1/model-health  200 "model health (read:models)"

# The paper book. Reading it is the check that the portfolio path is wired; the
# platform is paper-trading only and this endpoint is the whole of "trading".
get /api/v1/paper/portfolio 200 "paper portfolio (read:portfolio)"

# Metrics. Not authenticated, and served on the same port as the API
# (internal/api.Mount registers /metrics on the main router).
if curl -fsS "${API}/metrics" | grep -q '^quantos_'; then
  ok "metrics exposed"
else
  bad "metrics: no quantos_ series found"
fi

# -------------------------------------------------------------- result -------

echo
if [ "$fail" -gt 0 ]; then
  printf '\033[31m%d checks failed\033[0m (%d passed)\n' "$fail" "$pass" >&2
  exit 1
fi
printf '\033[32mall %d checks passed\033[0m\n' "$pass"
