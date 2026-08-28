#!/usr/bin/env bash
#
# db-migrate.sh — apply the PostgreSQL and ClickHouse schemas.
#
# Usage:
#   scripts/db-migrate.sh                      # compose stack, both engines
#   scripts/db-migrate.sh --target cluster     # against RDS + ClickHouse via env
#   scripts/db-migrate.sh --engine postgres    # one engine only
#   scripts/db-migrate.sh --dry-run            # print what would run
#
# Environment (cluster target):
#   QUANTOS_POSTGRES_DSN     required; same variable internal/config reads
#   QUANTOS_CLICKHOUSE_URL   required unless --engine postgres
#   QUANTOS_CLICKHOUSE_USER  default quantos
#   QUANTOS_CLICKHOUSE_PASSWORD
#
# The schemas in deploy/sql/ are additive and idempotent (CREATE TABLE IF NOT
# EXISTS), so re-running this is safe. It is not a migration *framework*: there
# is one numbered file per engine and no down-migration, because a down-migration
# on the provenance store is a way to lose the record of a decision, and ADR-003
# makes that record a product requirement rather than a nice-to-have.
#
# Postgres first, always. App.CanEmitSignals gates every signal on the
# provenance store being writable; a platform with a ClickHouse schema and no
# Postgres schema starts, reports healthy, and emits nothing.
set -euo pipefail

cd "$(dirname "$0")/.."

TARGET=compose
ENGINE=both
DRY_RUN=0

PG_SCHEMA=deploy/sql/postgres/001_schema.sql
CH_SCHEMA=deploy/sql/clickhouse/001_schema.sql

while [ $# -gt 0 ]; do
  case "$1" in
    --target)  TARGET="${2:?}"; shift 2 ;;
    --engine)  ENGINE="${2:?}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$TARGET" in compose|cluster) ;; *) echo "--target must be compose or cluster" >&2; exit 2 ;; esac
case "$ENGINE" in both|postgres|clickhouse) ;; *) echo "--engine must be both, postgres or clickhouse" >&2; exit 2 ;; esac

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31mfail\033[0m %s\n' "$*" >&2; exit 1; }

[ -f "$PG_SCHEMA" ] || die "missing ${PG_SCHEMA}"
[ -f "$CH_SCHEMA" ] || die "missing ${CH_SCHEMA}"

COMPOSE=(docker compose -f deploy/docker-compose.yml)

# ----------------------------------------------------------- postgres --------

migrate_postgres() {
  log "applying ${PG_SCHEMA}"
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "--- would apply to ${TARGET}:"
    grep -cE '^\s*CREATE' "$PG_SCHEMA" | xargs printf '    %s CREATE statements\n'
    return 0
  fi

  if [ "$TARGET" = compose ]; then
    "${COMPOSE[@]}" exec -T postgres \
      psql -v ON_ERROR_STOP=1 -U quantos -d quantos < "$PG_SCHEMA"
  else
    command -v psql >/dev/null || die "psql is not installed"
    [ -n "${QUANTOS_POSTGRES_DSN:-}" ] || die "QUANTOS_POSTGRES_DSN is not set"
    # ON_ERROR_STOP so a failed statement halts rather than leaving a partial
    # schema that the next run treats as already applied.
    psql -v ON_ERROR_STOP=1 -d "$QUANTOS_POSTGRES_DSN" -f "$PG_SCHEMA"
  fi
  log "postgres schema applied"
}

# ---------------------------------------------------------- clickhouse -------

migrate_clickhouse() {
  log "applying ${CH_SCHEMA}"
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "--- would apply to ${TARGET}:"
    grep -cE '^\s*CREATE' "$CH_SCHEMA" | xargs printf '    %s CREATE statements\n'
    return 0
  fi

  if [ "$TARGET" = compose ]; then
    "${COMPOSE[@]}" exec -T clickhouse \
      clickhouse-client --user quantos --password quantos --multiquery < "$CH_SCHEMA"
  else
    [ -n "${QUANTOS_CLICKHOUSE_URL:-}" ] || die "QUANTOS_CLICKHOUSE_URL is not set"
    local user="${QUANTOS_CLICKHOUSE_USER:-quantos}"
    # Over the HTTP interface so this needs curl rather than the ClickHouse
    # client binary, which is not present on most runners.
    #
    # The credentials go in headers, not in the URL: a URL ends up in shell
    # history, in `ps` output and in the server's query log.
    curl -sSf \
      -H "X-ClickHouse-User: ${user}" \
      -H "X-ClickHouse-Key: ${QUANTOS_CLICKHOUSE_PASSWORD:-}" \
      --data-binary "@${CH_SCHEMA}" \
      "${QUANTOS_CLICKHOUSE_URL}/?multiquery=1" > /dev/null
  fi
  log "clickhouse schema applied"
}

# ------------------------------------------------------------ verify ---------

verify() {
  [ "$DRY_RUN" -eq 1 ] && return 0
  [ "$TARGET" = compose ] || return 0

  # The one table that must exist for the platform to emit anything. If it is
  # missing, every service will start, report healthy, and produce no signals -
  # a failure mode that looks like a bug in the engine and is not one.
  log "verifying the provenance tables"
  "${COMPOSE[@]}" exec -T postgres \
    psql -qtA -U quantos -d quantos \
    -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" \
    | while read -r n; do
        [ "${n:-0}" -gt 0 ] || die "no tables in the public schema after migration"
        log "${n} tables present"
      done
}

case "$ENGINE" in
  both)       migrate_postgres; migrate_clickhouse ;;
  postgres)   migrate_postgres ;;
  clickhouse) migrate_clickhouse ;;
esac

verify
log "done"
