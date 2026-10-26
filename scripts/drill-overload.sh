#!/usr/bin/env bash
# Drill de sobrecarga: cola diminuta + muchísimos clientes => el sistema debe RECHAZAR con 503
# (backpressure), nunca perder una orden aceptada ni descuadrar un saldo, y recuperarse limpio.
# Necesita Go y curl (corre el servidor localmente, sin Docker).
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
EXE=""; [ "$(go env GOOS)" = windows ] && EXE=".exe"
go build -o "$WORK/server$EXE" ./cmd/server
go build -o "$WORK/loadgen$EXE" ./cmd/loadgen
export PORT=${PORT:-18083} ADMIN_PORT=${ADMIN_PORT:-19103} JOURNAL_PATH="$WORK/journal.log" QUEUE_SIZE=8

wait_ready() { for _ in $(seq 1 100); do curl -s -o /dev/null "localhost:$PORT/healthz" && return 0; sleep 0.2; done; echo "el servidor no arrancó a tiempo"; return 1; }
start() { "$WORK/server$EXE" >>"$WORK/server.log" 2>&1 & echo $! >"$WORK/pid"; wait_ready; }
api() { curl -s "localhost:$PORT$1" "${@:2}"; }

start
echo ">> cola de 8 posiciones + 512 clientes a máxima velocidad durante 10 s"
out=$("$WORK/loadgen$EXE" -url "http://localhost:$PORT" -rate 0 -workers 512 -users 50 -duration 10s 2>&1 || true)
echo "$out" | grep -E "órdenes completadas|respuestas|auditor"
rej=$(curl -s "localhost:$ADMIN_PORT/metrics" | awk '/^orderbook_queue_rejected_total/ {print $2}')
acc=$(curl -s "localhost:$ADMIN_PORT/metrics" | awk '/^orderbook_orders_total\{result="accepted"/ {s+=$2} END {print s+0}')
echo ">> rechazadas por backpressure (503): ${rej:-0}   aceptadas: $acc"
[ "${rej:-0}" -gt 0 ] || { echo "FALLO: se esperaba backpressure (503) y no ocurrió"; exit 1; }
before=$(api /audit | sed 's/.*"total_brl"/x/')
echo "$(api /audit)"; [[ "$before" == x* && "$(api /audit)" == *'"ok":true'* ]] || { echo "FALLO: invariantes rotos tras la sobrecarga"; exit 1; }

echo ">> parada ordenada (SIGTERM) y reinicio: todo lo aceptado debe recuperarse"
kill "$(cat "$WORK/pid")"; sleep 2; start
after=$(api /audit); echo "$after"
[[ "$after" == *'"ok":true'* ]] || { echo "FALLO: invariantes rotos tras recuperar"; exit 1; }
[[ "$(echo "$after" | sed 's/.*"total_brl"/x/')" == "$before" ]] || { echo "FALLO: los totales cambiaron al recuperar"; exit 1; }
{ grep "recuperación completa" "$WORK/server.log" || true; } | tail -1 | cut -c1-230
echo "OK: con sobrecarga se rechazó con 503, no se perdió ni descuadró nada y la recuperación es exacta"
