#!/usr/bin/env bash
# Drill de resiliencia: carga concurrente -> kill -9 -> reinicio -> auditoría de saldos.
# Demuestra que el journal + recuperación no pierden ni inventan dinero tras una caída brusca.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
cleanup() { # espera a que el servidor termine (escribe su snapshot final) ANTES de borrar; conserva el código de salida real
  local rc=$? pid; pid=$(cat "$WORK/pid" 2>/dev/null || true)
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; for _ in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done; fi
  rm -rf "$WORK" 2>/dev/null || true
  exit $rc
}
trap cleanup EXIT
export PORT=${PORT:-18081} JOURNAL_PATH="$WORK/journal.log"
EXE=""; [ "$(go env GOOS)" = windows ] && EXE=".exe"   # Go no añade .exe solo
go build -o "$WORK/server$EXE" ./cmd/server

wait_ready() { for _ in $(seq 1 100); do curl -s -o /dev/null "localhost:$PORT/healthz" && return 0; sleep 0.2; done; echo "el servidor no arrancó a tiempo"; return 1; }
start() { "$WORK/server$EXE" >>"$WORK/server.log" 2>&1 & echo $! >"$WORK/pid"; wait_ready; }
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
{ grep "recuperación completa" "$WORK/server.log" || true; } | tail -1
after=$(api /audit)
echo "auditoría antes : $before"
echo "auditoría después: $after"
[[ "$after" == *'"ok":true'* ]] || { echo "FALLO: invariantes rotos"; exit 1; }
[[ "$(echo "$before" | sed 's/.*"total_brl"/x/')" == "$(echo "$after" | sed 's/.*"total_brl"/x/')" ]] \
  || { echo "FALLO: los totales cambiaron"; exit 1; }
echo "OK: totales conservados e invariantes intactos tras kill -9"
