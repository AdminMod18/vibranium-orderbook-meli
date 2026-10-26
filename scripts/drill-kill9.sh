#!/usr/bin/env bash
# Drill de resiliencia: carga concurrente -> kill -9 -> reinicio -> auditoría de saldos.
# Demuestra que el journal + recuperación no pierden ni inventan dinero tras una caída brusca.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d); trap 'kill "$(cat "$WORK/pid" 2>/dev/null)" 2>/dev/null || true; rm -rf "$WORK"' EXIT
export PORT=${PORT:-18081} JOURNAL_PATH="$WORK/journal.log"
EXE=""; [ "$(go env GOOS)" = windows ] && EXE=".exe"   # Go no añade .exe solo
go build -o "$WORK/server$EXE" ./cmd/server

start() { "$WORK/server$EXE" >>"$WORK/server.log" 2>&1 & echo $! >"$WORK/pid"; sleep 1; }
api() { curl -s "localhost:$PORT$1" "${@:2}"; }

start
for u in a b c d; do api /deposits -XPOST -d "{\"user_id\":\"$u\",\"brl\":1000000,\"vib\":1000}"; done
before=$(api /audit)

( for i in $(seq 1 400); do
    for u in a b c d; do
      side=BUY; [ $((RANDOM % 2)) = 0 ] && side=SELL
      api /orders -o /dev/null -XPOST -d "{\"id\":\"$u-$i\",\"user_id\":\"$u\",\"side\":\"$side\",\"price\":$((95 + RANDOM % 11)),\"qty\":$((1 + RANDOM % 5))}" &
    done; wait
  done ) & LOAD=$!
sleep 1.5
echo ">> kill -9 con la carga en curso"
kill -9 "$(cat "$WORK/pid")"; kill $LOAD 2>/dev/null || true; wait 2>/dev/null || true

echo ">> reinicio (recuperando desde el journal)"
start
grep "journal recuperado" "$WORK/server.log" | tail -1
after=$(api /audit)
echo "auditoría antes : $before"
echo "auditoría después: $after"
[[ "$after" == *'"ok":true'* ]] || { echo "FALLO: invariantes rotos"; exit 1; }
[[ "$(echo "$before" | sed 's/.*"total_brl"/x/')" == "$(echo "$after" | sed 's/.*"total_brl"/x/')" ]] \
  || { echo "FALLO: los totales cambiaron"; exit 1; }
echo "OK: totales conservados e invariantes intactos tras kill -9"
