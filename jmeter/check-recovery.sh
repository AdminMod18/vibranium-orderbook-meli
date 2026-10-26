#!/usr/bin/env bash
# Recuperación tras una caída DURA, con JMeter como testigo (separada de las pruebas de carga):
#   1) genera carga con JMeter (escenario humo)   2) toma una foto: /audit y los saldos de 5 usuarios
#   3) SIGKILL al contenedor de la app y la vuelve a levantar   4) exige: mismos totales, invariantes ok y mismos saldos
#   5) ejecuta el plan de integridad financiera sobre el sistema ya recuperado
#
#   ./jmeter/check-recovery.sh            (necesita: docker compose up -d, JMeter y python3)
#   COMPOSE_ARGS="-f docker-compose.yml -f otro.yml" ./jmeter/check-recovery.sh
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && { set -a; . ./.env; set +a; }
COMPOSE="docker compose ${COMPOSE_ARGS:-}"
H=${HOST:-localhost}; P=${PORT:-${API_PORT:-8080}}; A="http://$H:$P"
fail=0; ok() { echo "  [OK]    $*"; }; bad() { echo "  [FALLO] $*"; fail=1; }

echo ">> 1) carga con JMeter (humo)"
LABEL=recuperacion-carga ./jmeter/run-jmeter.sh humo 2>&1 | grep -E "resumen|^>> terminado" || { echo "la carga falló"; exit 1; }
RUN=$(ls -d jmeter/results/*recuperacion-carga | tail -1); RID=$(grep '^run_id=' "$RUN/params.txt" | cut -d= -f2)

echo ">> 2) foto antes de la caída"
snap() { echo "$(curl -s "$A/audit")"; for u in 0 1 2 3 4; do curl -s "$A/balances/jm${RID}-u$u"; echo; done; }
BEFORE=$(snap); echo "$BEFORE" | head -1
echo "$BEFORE" | head -1 | grep -q '"ok":true' && ok "auditoría ok antes de la caída" || bad "auditoría no ok antes de la caída"

echo ">> 3) SIGKILL a la app y reinicio"
$COMPOSE kill -s SIGKILL orderbook >/dev/null 2>&1 || { echo "no pude matar la app (¿docker compose up -d?)"; exit 1; }
sleep 1; $COMPOSE up -d orderbook >/dev/null 2>&1
t0=$(date +%s); until curl -sf "$A/readyz" >/dev/null; do sleep 0.5; [ $(( $(date +%s) - t0 )) -gt 120 ] && { echo "la app no volvió en 120 s"; exit 1; }; done
echo "   volvió en $(( $(date +%s) - t0 )) s"

echo ">> 4) comparación"
AFTER=$(snap); echo "$AFTER" | head -1
[ "$BEFORE" = "$AFTER" ] && ok "totales y saldos de 5 usuarios IDÉNTICOS tras la caída" || { bad "el estado cambió tras la caída"; diff <(echo "$BEFORE") <(echo "$AFTER") | head -6; }

echo ">> 5) integridad financiera sobre el sistema recuperado"
LABEL=recuperacion-integridad ./jmeter/run-jmeter.sh integridad 2>&1 | grep -E "resumen|!!" ; [ "${PIPESTATUS[0]}" = 0 ] && ok "plan de integridad sin errores" || bad "el plan de integridad falló"
[ $fail = 0 ] && echo "RECUPERACIÓN VERIFICADA" || { echo "RECUPERACIÓN CON FALLOS"; exit 1; }
