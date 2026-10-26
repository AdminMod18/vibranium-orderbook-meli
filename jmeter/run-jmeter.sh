#!/usr/bin/env bash
# Ejecuta los planes JMeter de forma reproducible y deja los resultados en jmeter/results/<fecha>-<escenario>[-<etiqueta>]/
#
#   ./jmeter/run-jmeter.sh integridad                 # validación financiera determinista (1 hilo, segundos)
#   ./jmeter/run-jmeter.sh humo                       # 30 s a 500 órdenes/s: ¿el plan y el servidor responden?
#   ./jmeter/run-jmeter.sh gradual                    # rampa de 0 a 128 hilos en 5 min con tope de 5.000 órdenes/s
#   ./jmeter/run-jmeter.sh sostenida                  # 10 min a 5.000 órdenes/s   (DURATION=1800 para 30 min)
#   ./jmeter/run-jmeter.sh estres                     # 5 min con 256 hilos y SIN tope de tasa (busca el máximo)
#
# Variables (todas opcionales):  HOST PORT PROTOCOL  THREADS RAMPUP DURATION TPS USERS  WITH_HISTORY=true
#   LABEL=etiqueta (se añade al nombre de la carpeta)   OUT_DIR   JMETER_HOME   HEAP="-Xms1g -Xmx2g"
#   PROM=localhost:9090 (si responde, se exportan las métricas del motor del mismo periodo)
#   ALLOW_REMOTE=1 (necesario para apuntar a un host que no sea localhost)   KEEP_JTL=0 (borra el .jtl crudo tras resumirlo)
#   DASHBOARD=1 (genera además el dashboard HTML estándar de JMeter)   MIN_FREE_GB=2 (se aborta si el disco baja de ese valor)
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && { set -a; . ./.env; set +a; }

ESC=${1:-}
[ -n "$ESC" ] || { sed -n 2,16p "$0" | sed 's/^# \{0,1\}//'; exit 2; }
HOST=${HOST:-localhost}; PORT=${PORT:-${API_PORT:-8080}}; PROTOCOL=${PROTOCOL:-http}
PROM=${PROM:-localhost:${PROMETHEUS_PORT:-9090}}

# Seguridad: estas pruebas CREAN datos (usuarios jm<run>-uN, depósitos y órdenes). No se apuntan a otro host sin pedirlo.
case "$HOST" in localhost|127.0.0.1|::1) ;; *) [ "${ALLOW_REMOTE:-0}" = 1 ] || { echo "HOST=$HOST no es local. Estas pruebas crean datos y la auditoría de conservación supone que nadie más escribe: no las apunte a un entorno compartido o productivo. Si es un entorno de pruebas exclusivo, use ALLOW_REMOTE=1."; exit 2; } ;; esac

# JMeter
if [ -n "${JMETER_HOME:-}" ]; then JM="$JMETER_HOME/bin/jmeter"
elif command -v jmeter >/dev/null 2>&1; then JM=$(command -v jmeter)
elif [ -x /opt/apache-jmeter-5.6.3/bin/jmeter ]; then JM=/opt/apache-jmeter-5.6.3/bin/jmeter
else echo "No encuentro JMeter (>= 5.6). Descárguelo de https://jmeter.apache.org/download_jmeter.cgi y defina JMETER_HOME."; exit 1; fi
command -v java >/dev/null 2>&1 || { echo "Falta Java (JDK/JRE 17+)."; exit 1; }
curl -sf -o /dev/null "$PROTOCOL://$HOST:$PORT/healthz" || { echo "La API no responde en $PROTOCOL://$HOST:$PORT (¿docker compose up -d?)."; exit 1; }

case "$ESC" in
  integridad)  PLAN=jmeter/vibranium-integrity.jmx; P_THREADS=1;   P_RAMP=0;   P_DUR=0;    P_TPS=0 ;;
  humo)        PLAN=jmeter/vibranium-load.jmx;      P_THREADS=32;  P_RAMP=5;   P_DUR=30;   P_TPS=500 ;;
  gradual)     PLAN=jmeter/vibranium-load.jmx;      P_THREADS=128; P_RAMP=300; P_DUR=420;  P_TPS=5000 ;;
  sostenida)   PLAN=jmeter/vibranium-load.jmx;      P_THREADS=128; P_RAMP=30;  P_DUR=600;  P_TPS=5000 ;;
  estres)      PLAN=jmeter/vibranium-load.jmx;      P_THREADS=256; P_RAMP=60;  P_DUR=300;  P_TPS=0 ;;
  *) echo "Escenario desconocido: $ESC"; exit 2 ;;
esac
THREADS=${THREADS:-$P_THREADS}; RAMPUP=${RAMPUP:-$P_RAMP}; DURATION=${DURATION:-$P_DUR}; TPS=${TPS:-$P_TPS}; USERS=${USERS:-100}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%d%H%M%S)$(printf '%04x' $((RANDOM)))}
STAMP=$(date -u +%Y%m%d-%H%M%S)
OUT=${OUT_DIR:-jmeter/results/${STAMP}-${ESC}${LABEL:+-$LABEL}}
mkdir -p "$OUT"

cat > "$OUT/params.txt" <<EOT
escenario=$ESC
plan=$PLAN
host=$HOST
port=$PORT
protocol=$PROTOCOL
threads=$THREADS
rampup_s=$RAMPUP
duration_s=$DURATION
tps_objetivo=$TPS
users=$USERS
run_id=$RUN_ID
with_history=${WITH_HISTORY:-false}
EOT

HEAP=${HEAP:--Xms1g -Xmx2g}
export JVM_ARGS="$HEAP"
START=$(date +%s)
echo ">> $ESC: $PLAN  hilos=$THREADS rampa=${RAMPUP}s duración=${DURATION}s tasa=${TPS}/s run_id=$RUN_ID  ->  $OUT"
"$JM" -n -t "$PLAN" -l "$OUT/results.jtl" -j "$OUT/jmeter.log" \
  -Jhost="$HOST" -Jport="$PORT" -Jprotocol="$PROTOCOL" -Jthreads="$THREADS" -Jrampup="$RAMPUP" -Jduration="$DURATION" \
  -Jtps="$TPS" -Jusers="$USERS" -Jrun_id_fijo="$RUN_ID" -Jwith_history="${WITH_HISTORY:-false}" \
  ${MID_PRICE:+-Jmid_price=$MID_PRICE} ${SPREAD:+-Jspread=$SPREAD} ${CANCEL_PCT:+-Jcancel_pct=$CANCEL_PCT} ${BASE_PRICE:+-Jbase_price=$BASE_PRICE} \
  -Jjmeter.save.saveservice.output_format=csv -Jjmeter.save.saveservice.timestamp_format=ms \
  -Jjmeter.save.saveservice.print_field_names=true -Jjmeter.save.saveservice.thread_counts=true \
  -Jjmeter.save.saveservice.url=false -Jjmeter.save.saveservice.response_message=false -Jjmeter.save.saveservice.thread_name=false \
  -Jjmeter.save.saveservice.data_type=false -Jjmeter.save.saveservice.hostname=false -Jjmeter.save.saveservice.encoding=false \
  -Jjmeter.save.saveservice.subresults=false -Jjmeter.save.saveservice.assertion_results_failure_message=true \
  -Jsummariser.interval=30 \
  -Jjmeter.reportgenerator.overall_granularity=5000 &
JPID=$!
# Guarda de disco: el .jtl y el journal crecen durante la corrida; si queda menos de MIN_FREE_GB se detiene la prueba (shutdown limpio de JMeter).
MIN_FREE_GB=${MIN_FREE_GB:-2}
( while kill -0 $JPID 2>/dev/null; do
    free=$(df -BM --output=avail "$OUT" 2>/dev/null | tail -1 | tr -d ' M'); free=${free:-999999}
    if [ "$free" -lt $((MIN_FREE_GB * 1024)) ]; then echo "!! Quedan ${free} MB libres (< ${MIN_FREE_GB} GB): se detiene la prueba para no llenar el disco. La corrida queda INCOMPLETA." | tee "$OUT/ABORTADA_POR_DISCO.txt"; "$(dirname "$JM")/shutdown.sh" >/dev/null 2>&1; break; fi
    sleep 15
  done ) &
wait $JPID
RC=$?
END=$(date +%s)

{
  echo "{"
  echo "  \"escenario\": \"$ESC\", \"label\": \"${LABEL:-}\", \"run_id\": \"$RUN_ID\","
  echo "  \"inicio_epoch\": $START, \"fin_epoch\": $END, \"jmeter_rc\": $RC,"
  echo "  \"commit\": \"$(git rev-parse --short HEAD 2>/dev/null || echo desconocido)\", \"rama\": \"$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo desconocida)\","
  echo "  \"jmeter\": \"$("$JM" --version 2>/dev/null | grep -o '[0-9]\+\.[0-9]\+\.[0-9]\+' | head -1)\","
  echo "  \"java\": \"$(java -version 2>&1 | grep -m1 version | tr -d '\"')\","
  echo "  \"cpu_nucleos\": $(nproc 2>/dev/null || echo 0), \"memoria_mb\": $(free -m 2>/dev/null | awk '/Mem:/{print $2}' || echo 0),"
  echo "  \"disco_libre_gb_al_terminar\": \"$(df -BG --output=avail . 2>/dev/null | tail -1 | tr -d ' G')\","
  echo "  \"fecha_utc\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\""
  echo "}"
} > "$OUT/meta.json"

# Métricas del MOTOR (Prometheus) del mismo periodo, si está disponible. No son de JMeter: se guardan aparte.
if [ "$ESC" != integridad ] && curl -sf -o /dev/null "http://$PROM/-/ready"; then
  python3 jmeter/export_prometheus.py --prom "$PROM" --start "$START" --end "$END" --out "$OUT/prometheus.csv" || echo "(no se pudo exportar Prometheus)"
else
  [ "$ESC" = integridad ] || echo "(Prometheus no responde en $PROM: sin métricas del motor para esta corrida)"
fi

[ "${DASHBOARD:-0}" = 1 ] && [ -s "$OUT/results.jtl" ] && "$JM" -g "$OUT/results.jtl" -o "$OUT/dashboard" -Jjmeter.reportgenerator.overall_granularity=5000 >/dev/null 2>&1 && echo "  dashboard de JMeter en $OUT/dashboard/index.html"
python3 jmeter/make_report.py summarize "$OUT" $([ "$ESC" = integridad ] && echo --fail-on-error)
SRC=$?
[ "$SRC" = 0 ] || { [ "$ESC" = integridad ] && echo "!! LA INTEGRIDAD FINANCIERA FALLÓ (ver $OUT/errors.csv)" || echo "(no se pudo resumir el .jtl)"; }
[ "${KEEP_JTL:-1}" = 1 ] || { rm -f "$OUT/results.jtl"; echo "(results.jtl crudo borrado: KEEP_JTL=0)"; }
echo ">> terminado (rc JMeter=$RC) en $OUT"
[ "$RC" = 0 ] && [ "$ESC" = integridad ] && [ "$SRC" != 0 ] && exit 3
exit $RC
