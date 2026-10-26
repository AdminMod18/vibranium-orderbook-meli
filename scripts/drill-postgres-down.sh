#!/usr/bin/env bash
# Drill: Postgres se cae EN PLENO USO. El matching debe seguir; el historial debe ponerse al día al volver.
# Requiere el Postgres del compose arriba (docker compose up -d postgres) y la app del compose DETENIDA
# (docker compose stop orderbook): dos proyectores sobre la misma BD se pisarían.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && { set -a; . ./.env; set +a; }
DATABASE_URL=${DATABASE_URL:-postgres://postgres:orderbook@localhost:${POSTGRES_PORT:-5432}/orderbook}
# Por defecto usa docker compose; cada comando puede sustituirse (p. ej. PG_STOP="pg_ctlcluster 16 main stop").
pg_stop()  { if [ -n "${PG_STOP:-}" ];  then eval "$PG_STOP";  else docker compose stop postgres;  fi; }
pg_start() { if [ -n "${PG_START:-}" ]; then eval "$PG_START"; else docker compose start postgres; fi; }
# El historial queda atado a un journal; como este drill usa un journal nuevo, se parte de una BD limpia.
pg_reset() {
  if [ -n "${PG_RESET:-}" ]; then eval "$PG_RESET"
  else docker compose exec -T postgres psql -q -U postgres -d orderbook -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public"; fi
}
if [ "${ASSUME_YES:-0}" != 1 ]; then
  echo "ATENCIÓN: este drill VACÍA el esquema 'public' de la base de Postgres del compose (borra el historial) y"
  echo "detiene/arranca el contenedor de Postgres. La app del compose debe estar detenida (docker compose stop orderbook)."
  if [ ! -t 0 ]; then echo "Sin terminal interactiva: defina ASSUME_YES=1 para confirmar."; exit 2; fi
  read -r -p "Escriba SI para continuar: " ans; [ "$ans" = SI ] || { echo "Cancelado."; exit 1; }
fi
pg_reset >/dev/null 2>&1

WORK=$(mktemp -d)
cleanup() { # espera a que el servidor termine (escribe su snapshot final) ANTES de borrar; conserva el código de salida real
  local rc=$? pid; pid=$(cat "$WORK/pid" 2>/dev/null || true)
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; for _ in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done; fi
  rm -rf "$WORK" 2>/dev/null || true
  exit $rc
}
trap cleanup EXIT
export PORT=${PORT:-18082} JOURNAL_PATH="$WORK/journal.log" DATABASE_URL
EXE=""; [ "$(go env GOOS)" = windows ] && EXE=".exe"   # Go no añade .exe solo
go build -o "$WORK/server$EXE" ./cmd/server
wait_ready() { for _ in $(seq 1 100); do curl -s -o /dev/null "localhost:$PORT/healthz" && return 0; sleep 0.2; done; echo "el servidor no arrancó a tiempo"; return 1; }
"$WORK/server$EXE" >"$WORK/server.log" 2>&1 & echo $! >"$WORK/pid"; wait_ready
api() { curl -s "localhost:$PORT$1" "${@:2}"; }
order() { api /orders -XPOST -d "{\"id\":\"$1\",\"user_id\":\"$2\",\"side\":\"$3\",\"price\":100,\"qty\":$4}"; }

api /deposits -XPOST -d '{"user_id":"alice","brl":1000000}'
api /deposits -XPOST -d '{"user_id":"bob","vib":500}'
order s-1 bob SELL 10 >/dev/null; order b-1 alice BUY 10 >/dev/null
sleep 0.5
echo ">> con Postgres arriba, GET /orders/b-1:"; api /orders/b-1 | head -c 220; echo

echo ">> CAE POSTGRES"; pg_stop >/dev/null 2>&1; sleep 1
code=$(order s-2 bob SELL 5 -o /dev/null -w '%{http_code}' | tail -c 3 || true)
for i in $(seq 1 20); do order s-x$i bob SELL 1 >/dev/null; order b-x$i alice BUY 1 >/dev/null; done
echo ">> matching sigue durante la caída: alice =" "$(api /balances/alice)"
echo ">> consulta de historial durante la caída: HTTP $(api /orders/b-1 -o /dev/null -w '%{http_code}')  (503 esperado)"

echo ">> VUELVE POSTGRES"; pg_start >/dev/null 2>&1
for i in $(seq 1 60); do
  r=$(api /audit/history/alice); [[ "$r" == *'"lag":0'* ]] && break; sleep 0.5
done
echo ">> auditoría tras recuperarse: $r"
[[ "$r" == *'"consistent":true'* && "$r" == *'"conclusive":true'* ]] \
  || { echo "FALLO: el historial no se puso al día de forma consistente"; exit 1; }
echo "OK: el matching no se detuvo y el historial se reconcilió solo con el journal"
