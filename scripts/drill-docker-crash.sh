#!/usr/bin/env bash
# Drill con contenedores: crash REAL de la app (kill -9 al proceso, como un OOM) en pleno uso.
# Requiere:  docker compose up -d   (con la app sana)
# Nota: `docker kill` cuenta como parada MANUAL y Docker no aplica la política de reinicio;
#       por eso, donde se puede, se mata el PROCESO desde el host (lo que haría un OOM real).
#       En Docker Desktop (Windows/macOS) el proceso vive dentro de una VM y su PID no es visible
#       desde aquí (matar ese número podría afectar a OTRO proceso): se usa `docker kill` y se
#       reinicia a mano, que igualmente prueba la recuperación del journal desde el volumen.
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
HOST_KILL=0
if [ "$(uname -s)" = Linux ] && ! docker info --format '{{.OperatingSystem}}' 2>/dev/null | grep -qi desktop; then HOST_KILL=1; fi
if [ "$HOST_KILL" = 1 ]; then
  PID=$(docker inspect -f '{{.State.Pid}}' "$CNAME")
  echo ">> kill -9 al proceso de la app (pid $PID) con carga en curso"
  kill -9 "$PID"
else
  echo ">> docker kill (SIGKILL) a la app con carga en curso  [Docker Desktop: sin kill de host]"
  docker kill "$CNAME" >/dev/null
fi
kill $LOAD 2>/dev/null || true; wait 2>/dev/null || true

for i in $(seq 1 30); do curl -s -o /dev/null "$A/healthz" && break; sleep 0.5; done
if ! curl -s -o /dev/null "$A/healthz"; then
  echo ">> (docker kill es parada manual: no hay reinicio automático) reiniciando a mano"
  docker compose start orderbook >/dev/null
  for i in $(seq 1 60); do curl -s -o /dev/null "$A/healthz" && break; sleep 0.5; done
else
  echo ">> Docker la reinició sola: RestartCount=$(docker inspect -f '{{.RestartCount}}' "$CNAME")"
fi
docker compose logs orderbook --no-log-prefix 2>&1 | grep "journal recuperado" | tail -1 | cut -c1-220
after=$(api /audit); echo "auditoría: $after"
[[ "$after" == *'"ok":true'* ]] || { echo "FALLO: invariantes rotos"; exit 1; }
[[ "$(echo "$after" | sed 's/.*"total_brl"/x/')" == "$before" ]] || { echo "FALLO: los totales cambiaron"; exit 1; }
echo "OK: crash real recuperado desde el volumen; saldos intactos"
