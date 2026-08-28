#!/usr/bin/env bash
# Creates the QuantOS topic set.
#
# Partition counts and retention come from internal/bus.TopicConfigs, which is
# the single source of truth; `quantos topics` prints the same table. Keeping
# them in one place is what stops a topic being created with the wrong
# partition count and silently breaking per-symbol ordering.
set -euo pipefail

BROKER="${KAFKA_BROKER:-kafka:9092}"
KT="kafka-topics.sh --bootstrap-server ${BROKER}"

echo "waiting for ${BROKER}..."
for _ in $(seq 1 60); do
  if ${KT} --list >/dev/null 2>&1; then break; fi
  sleep 2
done

create() {
  local name="$1" partitions="$2" retention_ms="$3" cleanup="${4:-delete}"
  echo "topic ${name}: partitions=${partitions} retention=${retention_ms}ms policy=${cleanup}"
  ${KT} --create --if-not-exists --topic "${name}" \
    --partitions "${partitions}" --replication-factor 1 \
    --config "retention.ms=${retention_ms}" \
    --config "cleanup.policy=${cleanup}" \
    --config "min.insync.replicas=1"
  # Every topic gets a dead-letter partner: a poison message must have
  # somewhere to go that is not "retry forever".
  ${KT} --create --if-not-exists --topic "${name}.dlq" \
    --partitions 1 --replication-factor 1 \
    --config "retention.ms=604800000"
}

DAY=86400000
create market.quotes            12 $((1 * DAY))
create market.trades            12 $((1 * DAY))
create market.bars              12 $((30 * DAY))
create market.news               6 $((30 * DAY))
create market.events             3 $((90 * DAY))
create market.stale              3 $((7 * DAY))
create market.rejected           3 $((7 * DAY))
create features.updated         12 $((7 * DAY))
create regime.updated            1 $((90 * DAY))
create prediction.generated     12 $((30 * DAY))
create signal.generated          6 $((365 * DAY)) compact
create signal.invalidated        6 $((365 * DAY))
create risk.updated              6 $((30 * DAY))
create alert.generated           6 $((90 * DAY))
create prediction.evaluated      6 $((365 * DAY))
create model.drift.detected      1 $((365 * DAY))

echo ""
echo "topics created:"
${KT} --list | sort
