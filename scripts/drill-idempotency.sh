#!/usr/bin/env bash
# Drill de idempotencia con VENTANA: un reintento reciente se rechaza (409); un id muy antiguo (fuera de la
# ventana) se trata como orden nueva; una orden que SIGUE en el libro siempre se protege; y cambiar el tamaño
# de la ventana entre reinicios nunca impide arrancar. Necesita Go y curl (servidor local, sin Docker).
set -euo pipefail
cd "$(dirname "$0")/.."
WORK=$(mktemp -d)
cleanup() {
  local rc=$? pid; pid=$(cat "$WORK/pid" 2>/dev/null || true)
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; for _ in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done; fi
  rm -rf "$WORK" 2>/dev/null || true; exit $rc
}
trap cleanup EXIT
EXE=""; [ "$(go env GOOS)" = windows ] && EXE=".exe"
go build -o "$WORK/server$EXE" ./cmd/server
export PORT=${PORT:-18084} ADMIN_PORT=${ADMIN_PORT:-19104} JOURNAL_PATH="$WORK/journal.log"
wait_ready() { for _ in $(seq 1 100); do curl -s -o /dev/null "localhost:$PORT/healthz" && return 0; sleep 0.2; done; echo "el servidor no arrancó"; return 1; }
start() { IDEMPOTENCY_WINDOW=$1 "$WORK/server$EXE" >>"$WORK/server.log" 2>&1 & echo $! >"$WORK/pid"; wait_ready; }
stop()  { kill "$(cat "$WORK/pid")"; for _ in $(seq 1 100); do kill -0 "$(cat "$WORK/pid")" 2>/dev/null || break; sleep 0.1; done; }
code()  { curl -s -o /dev/null -w '%{http_code}' "$@"; }
order() { code -XPOST "localhost:$PORT/orders" -d "{\"id\":\"$1\",\"user_id\":\"alice\",\"side\":\"BUY\",\"price\":10,\"qty\":1}"; }
cancel(){ code -XDELETE "localhost:$PORT/orders/$1" >/dev/null; }
expect() { [ "$2" = "$3" ] && echo "  [OK]    $1 => $2" || { echo "  [FALLO] $1: esperaba $3 y fue $2"; exit 1; }; }
metric() { curl -s "localhost:$ADMIN_PORT/metrics" | awk -v m="orderbook_$1" '$1==m {print $2}'; }

echo ">> ventana de 50 ids"; start 50
curl -s -XPOST "localhost:$PORT/deposits" -d '{"user_id":"alice","brl":1000000}' >/dev/null
order first >/dev/null; cancel first                         # "first" entra y se cancela (ya no está en el libro)
order resting >/dev/null                                      # "resting" SIGUE en el libro
for i in $(seq 1 60); do order "n$i" >/dev/null; cancel "n$i"; done   # 60 órdenes empujan a los anteriores fuera de la ventana

expect "reintento RECIENTE (n60) se rechaza" "$(order n60)" 409
expect "id ANTIGUO fuera de la ventana (first) se acepta como nuevo" "$(order first)" 201
expect "orden que sigue en el libro (resting) se rechaza aunque su id ya salió de la ventana" "$(order resting)" 409
expect "tamaño de la ventana acotado a 50" "$(metric idempotency_window_size)" 50
[ "$(metric idempotency_evicted_total | cut -d. -f1)" -gt 0 ] && echo "  [OK]    ids expulsados: $(metric idempotency_evicted_total)"
expect "auditoría de saldos" "$(curl -s localhost:$PORT/audit | grep -o '"ok":true')" '"ok":true'
before=$(curl -s "localhost:$PORT/balances/alice")

echo ">> reinicios con OTRA ventana (el journal tiene el id 'first' aceptado dos veces)"
for w in 5 0 100000; do
  stop; start "$w"
  expect "arranca con ventana=$w y los saldos son idénticos" "$(curl -s "localhost:$PORT/balances/alice")" "$before"
  expect "  auditoría con ventana=$w" "$(curl -s localhost:$PORT/audit | grep -o '"ok":true')" '"ok":true'
done
echo "OK: idempotencia con ventana verificada (reciente=409, antiguo=nuevo, en libro=409, reinicio con otra ventana=ok)"
