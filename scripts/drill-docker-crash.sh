#!/usr/bin/env bash
# Drill con contenedores: crash REAL de la app (kill -9 al proceso, como un OOM) en pleno uso.
# Requiere:  docker compose up -d   (con la app sana)
# Nota: `docker kill` cuenta como parada MANUAL y Docker no aplica la política de reinicio;
#       por eso se mata el proceso desde el host, que es lo que ocurre en un fallo real.
set -euo pipefail
cd "$(dirname "$0")/.."
A=${API:-localhost:8080}
CNAME=$(docker compose ps -q orderbook)
[ -n "$CNAME" ] || { echo "la app no está corriendo: docker compose up -d"; exit 1; }
api() { curl -s "$A$1" "${@:2}"; }

api /deposits -XPOST -d '{"user_id":"drill-a","brl":1000000}' >/dev/null
api /deposits -XPOST -d '{"user_id":"drill-b","vib":1000}'    >/dev/null
before=$(api /audit | sed 's/.*"total_brl"/x/')
( for i in $(seq 1 200); do
    api /orders -o /dev/null -XPOST -d "{\"id\":\"dk-$RANDOM-$i\",\"user_id\":\"drill-a\",\"side\":\"BUY\",\"price\":$((90 + RANDOM % 15)),\"qty\":$((1 + RANDOM % 4))}" &
    api /orders -o /dev/null -XPOST -d "{\"id\":\"ds-$RANDOM-$i\",\"user_id\":\"drill-b\",\"side\":\"SELL\",\"price\":$((90 + RANDOM % 15)),\"qty\":$((1 + RANDOM % 4))}" &
    [ $((i % 20)) = 0 ] && wait
  done; wait ) & LOAD=$!
sleep 0.4
PID=$(docker inspect -f '{{.State.Pid}}' "$CNAME")
echo ">> kill -9 al proceso de la app (pid $PID) con carga en curso"
kill -9 "$PID"; kill $LOAD 2>/dev/null || true; wait 2>/dev/null || true

for i in $(seq 1 60); do curl -s -o /dev/null "$A/healthz" && break; sleep 0.5; done
echo ">> Docker la reinició: RestartCount=$(docker inspect -f '{{.RestartCount}}' "$CNAME")"
docker compose logs orderbook --no-log-prefix 2>&1 | grep "journal recuperado" | tail -1 | cut -c1-220
after=$(api /audit); echo "auditoría: $after"
[[ "$after" == *'"ok":true'* ]] || { echo "FALLO: invariantes rotos"; exit 1; }
[[ "$(echo "$after" | sed 's/.*"total_brl"/x/')" == "$before" ]] || { echo "FALLO: los totales cambiaron"; exit 1; }
echo "OK: crash real recuperado desde el volumen; saldos intactos"
