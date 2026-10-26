#!/usr/bin/env bash
# Drill de instancia única: dos procesos sobre el MISMO journal lo corromperían. El segundo debe negarse a
# arrancar SIN afectar al primero, y si el dueño muere (kill -9) el candado se libera solo.
#
#   ./scripts/drill-single-instance.sh            fase local (Go + curl; carpeta temporal, sin Docker)
#   ./scripts/drill-single-instance.sh --docker   fase Docker: un 2º contenedor sobre el volumen del compose en marcha
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && { set -a; . ./.env; set +a; }
ok()  { echo "  [OK]    $1"; }
bad() { echo "  [FALLO] $1"; exit 1; }

if [ "${1:-}" = "--docker" ]; then
  COMPOSE="docker compose ${COMPOSE_ARGS:-}"
  [ -n "$($COMPOSE ps --status running -q orderbook 2>/dev/null)" ] || bad "la app del compose no está corriendo (docker compose up -d)"
  out=$($COMPOSE run --rm --no-deps -T -e PORT=18999 -e ADMIN_PORT=19999 orderbook 2>&1); rc=$?
  [ $rc -ne 0 ] && ok "el 2º contenedor sobre el mismo volumen se negó a arrancar (código $rc)" || bad "el 2º contenedor arrancó: el journal podría corromperse"
  echo "$out" | grep -q "OTRA instancia" && ok "mensaje claro: $(echo "$out" | grep -o 'OTRA instancia[^"]*' | head -1 | cut -c1-90)" || bad "no se encontró el mensaje esperado"
  curl -s -o /dev/null "localhost:${API_PORT:-8080}/healthz" && ok "la instancia original sigue sirviendo" || bad "la instancia original dejó de responder"
  echo "OK: Docker impide dos instancias sobre el mismo volumen"; exit 0
fi

WORK=$(mktemp -d)
cleanup() {
  local rc=$?
  for f in pidA pidB; do p=$(cat "$WORK/$f" 2>/dev/null || true); [ -n "$p" ] && kill "$p" 2>/dev/null; done
  for f in pidA pidB; do p=$(cat "$WORK/$f" 2>/dev/null || true); [ -n "$p" ] && for _ in $(seq 1 100); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done; done
  rm -rf "$WORK" 2>/dev/null || true; exit $rc
}
trap cleanup EXIT
EXE=""; [ "$(go env GOOS)" = windows ] && EXE=".exe"
go build -o "$WORK/server$EXE" ./cmd/server || bad "no compila"
PA=${PORT_A:-18085}; PB=${PORT_B:-18086}; export JOURNAL_PATH="$WORK/journal.log"
ready() { for _ in $(seq 1 100); do curl -s -o /dev/null "localhost:$1/healthz" && return 0; sleep 0.2; done; return 1; }
startA() { PORT=$PA ADMIN_PORT=19105 "$WORK/server$EXE" >>"$WORK/a.log" 2>&1 & echo $! >"$WORK/pidA"; ready $PA || bad "la instancia A no arrancó"; }

echo ">> instancia A arranca y opera"
startA
curl -s -XPOST "localhost:$PA/deposits" -d '{"user_id":"alice","brl":100000}' -o /dev/null
code=$(curl -s -o /dev/null -w '%{http_code}' -XPOST "localhost:$PA/orders" -d '{"id":"a1","user_id":"alice","side":"BUY","price":10,"qty":3}')
[ "$code" = 201 ] && ok "A acepta órdenes (201)" || bad "A respondió $code"

echo ">> instancia B sobre el MISMO journal (debe negarse)"
PORT=$PB ADMIN_PORT=19106 timeout 15 "$WORK/server$EXE" >"$WORK/b.log" 2>&1; rcB=$?
[ $rcB -eq 1 ] && ok "B terminó con código 1 (no arrancó)" || bad "B terminó con código $rcB (se esperaba 1)"
grep -q "OTRA instancia" "$WORK/b.log" && ok "B explicó el motivo en su log" || bad "falta el mensaje en el log de B"
curl -s -o /dev/null "localhost:$PB/healthz" && bad "B quedó sirviendo" || ok "B no abrió ningún puerto"

echo ">> A no se vio afectada"
code=$(curl -s -o /dev/null -w '%{http_code}' -XPOST "localhost:$PA/orders" -d '{"id":"a2","user_id":"alice","side":"BUY","price":10,"qty":2}')
[ "$code" = 201 ] && ok "A sigue aceptando órdenes (201)" || bad "A respondió $code"
before=$(curl -s "localhost:$PA/balances/alice")
curl -s "localhost:$PA/audit" | grep -q '"ok":true' && ok "auditoría de A: ok" || bad "auditoría de A falló"

echo ">> kill -9 al dueño: el candado debe liberarse SOLO"
{ kill -9 "$(cat "$WORK/pidA")"; wait "$(cat "$WORK/pidA")"; } 2>/dev/null; sleep 0.5
PORT=$PB ADMIN_PORT=19106 "$WORK/server$EXE" >>"$WORK/b.log" 2>&1 & echo $! >"$WORK/pidB"
ready $PB && ok "B arrancó tras la muerte de A (sin candado huérfano)" || bad "B no pudo arrancar tras la muerte de A"
[ "$(curl -s "localhost:$PB/balances/alice")" = "$before" ] && ok "B recuperó exactamente el estado de A" || bad "los saldos difieren tras recuperar"
curl -s "localhost:$PB/audit" | grep -q '"ok":true' && ok "auditoría de B: ok" || bad "auditoría de B falló"
echo "OK: una sola instancia por journal; el candado se libera al morir el dueño"
