#!/usr/bin/env bash
# Drill: Postgres se cae EN PLENO USO. El matching debe seguir; el historial debe ponerse al día al volver.
# Con docker compose (por defecto) o con comandos propios:
#   PG_STOP="docker compose stop postgres" PG_START="docker compose start postgres"
set -euo pipefail
cd "$(dirname "$0")/.."
DATABASE_URL=${DATABASE_URL:-postgres://postgres:orderbook@localhost:5432/orderbook}
PG_STOP=${PG_STOP:-docker compose stop postgres}
PG_START=${PG_START:-docker compose start postgres}
# El historial queda atado a un journal; como este drill usa un journal nuevo, se parte de una BD limpia.
PG_RESET=${PG_RESET:-docker compose exec -T postgres psql -U postgres -d orderbook -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public"}
$PG_RESET >/dev/null

WORK=$(mktemp -d); trap 'kill "$(cat "$WORK/pid" 2>/dev/null)" 2>/dev/null || true; rm -rf "$WORK"' EXIT
export PORT=${PORT:-18082} JOURNAL_PATH="$WORK/journal.log" DATABASE_URL
go build -o "$WORK/server" ./cmd/server
"$WORK/server" >"$WORK/server.log" 2>&1 & echo $! >"$WORK/pid"; sleep 1.5
api() { curl -s "localhost:$PORT$1" "${@:2}"; }
order() { api /orders -XPOST -d "{\"id\":\"$1\",\"user_id\":\"$2\",\"side\":\"$3\",\"price\":100,\"qty\":$4}"; }

api /deposits -XPOST -d '{"user_id":"alice","brl":1000000}'
api /deposits -XPOST -d '{"user_id":"bob","vib":500}'
order s-1 bob SELL 10 >/dev/null; order b-1 alice BUY 10 >/dev/null
sleep 0.5
echo ">> con Postgres arriba, GET /orders/b-1:"; api /orders/b-1 | head -c 220; echo

echo ">> CAE POSTGRES"; $PG_STOP >/dev/null 2>&1; sleep 1
code=$(order s-2 bob SELL 5 -o /dev/null -w '%{http_code}' | tail -c 3 || true)
for i in $(seq 1 20); do order s-x$i bob SELL 1 >/dev/null; order b-x$i alice BUY 1 >/dev/null; done
echo ">> matching sigue durante la caída: alice =" "$(api /balances/alice)"
echo ">> consulta de historial durante la caída: HTTP $(api /orders/b-1 -o /dev/null -w '%{http_code}')  (503 esperado)"

echo ">> VUELVE POSTGRES"; $PG_START >/dev/null 2>&1
for i in $(seq 1 60); do
  r=$(api /audit/history/alice); [[ "$r" == *'"lag":0'* ]] && break; sleep 0.5
done
echo ">> auditoría tras recuperarse: $r"
[[ "$r" == *'"consistent":true'* && "$r" == *'"conclusive":true'* ]] \
  || { echo "FALLO: el historial no se puso al día de forma consistente"; exit 1; }
echo "OK: el matching no se detuvo y el historial se reconcilió solo con el journal"
