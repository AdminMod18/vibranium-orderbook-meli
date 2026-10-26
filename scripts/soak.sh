#!/usr/bin/env bash
# Prueba de RESISTENCIA (soak): carga sostenida con muestreo por minuto de memoria, GC, cola, snapshots, ventana de
# idempotencia, historial y espacio en disco; y al final mide cuánto tarda un reinicio con el estado ya grande.
#
#   ./scripts/soak.sh [minutos=20] [ordenes_por_segundo=5000]
#
# OJO: escribe MUCHOS datos (≈ 360 bytes por orden en el journal + el historial en Postgres: 20 min a 5.000/s ≈ 2-3 GB).
# Se detiene sola si el espacio libre del journal baja de MIN_FREE_GB (por defecto 3).
# Requiere el stack levantado (docker compose up -d). Pide confirmación (ASSUME_YES=1 para saltarla).
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && { set -a; . ./.env; set +a; }
MIN=${1:-20}; RATE=${2:-5000}; MIN_FREE_GB=${MIN_FREE_GB:-3}
COMPOSE="docker compose ${COMPOSE_ARGS:-}"
API=localhost:${API_PORT:-8080}; MET=localhost:${METRICS_PORT:-9102}
OUT=$(mktemp -d); CSV=$OUT/muestras.csv; LOAD=$OUT/loadgen.log

if [ "${ASSUME_YES:-0}" != 1 ]; then
  echo "ATENCIÓN: soak.sh genera ≈ $((MIN * 60 * RATE)) órdenes (varios GB en disco: journal, snapshots y Postgres)."
  echo "Corte con Ctrl+C si hace falta. No borra nada, pero sí LLENA datos del stack actual."
  [ -t 0 ] || { echo "Sin terminal interactiva: defina ASSUME_YES=1."; exit 2; }
  read -r -p "Escriba SI para continuar: " a; [ "$a" = SI ] || { echo "Cancelado."; exit 1; }
fi
curl -s -o /dev/null "http://$API/healthz" || { echo "La API no responde en $API: docker compose up -d"; exit 1; }

m()    { curl -s "http://$MET/metrics" | awk -v n="$1" '$1==n {print $2; exit}'; }
msum() { curl -s "http://$MET/metrics" | awk -v p="$1" 'index($1,p)==1 {s+=$2} END {print s+0}'; }

echo "Soak: $MIN min a $RATE órdenes/s. Salida en $OUT"
$COMPOSE run --rm loadgen -rate "$RATE" -duration "${MIN}m" -workers 128 >"$LOAD" 2>&1 &
LPID=$!
printf '%-4s %-7s %-5s %-5s %-6s %-8s %-8s %-6s %-7s %-9s %-9s %-9s %-9s %-9s %-7s %-8s\n' min ord/s cola 503/s expir heapMiB rssMiB gorut gcMaxms jrnlMiB ventana snapAge snapPausms snapWr_s lag libreGiB
echo "min,ords,cola,rej,exp,heap,rss,gor,gc,jrnl,win,age,pause,write,lag,free" >"$CSV"

prev_acc=$(msum 'orderbook_orders_total{result="accepted"'); prev_rej=$(m orderbook_queue_rejected_total); prev_exp=$(m orderbook_queue_expired_total); prev_t=$(date +%s); i=0; ABORT=0
while kill -0 $LPID 2>/dev/null; do
  sleep 60; i=$((i+1)); now=$(date +%s); dt=$((now-prev_t)); [ $dt -lt 1 ] && dt=1
  acc=$(msum 'orderbook_orders_total{result="accepted"'); rej=$(m orderbook_queue_rejected_total); exp=$(m orderbook_queue_expired_total)
  free=$(m orderbook_journal_disk_free_bytes)
  line=$(awk -v i=$i -v dt=$dt -v acc=$acc -v pa=$prev_acc -v rej=$rej -v pr=$prev_rej -v xp=$exp -v pe=$prev_exp -v free="$free" \
    -v q="$(m orderbook_queue_depth)" -v heap="$(m go_memstats_heap_alloc_bytes)" -v rss="$(m process_resident_memory_bytes)" \
    -v gor="$(m go_goroutines)" -v gc="$(curl -s "http://$MET/metrics" | awk '/^go_gc_duration_seconds\{quantile="1"\}/{print $2}')" \
    -v jr="$(m orderbook_journal_bytes)" -v win="$(m orderbook_idempotency_window_size)" -v age="$(m orderbook_snapshot_age_records)" \
    -v pau="$(m orderbook_snapshot_pause_seconds)" -v wr="$(m orderbook_snapshot_write_seconds)" -v lag="$(m orderbook_projector_lag)" 'BEGIN{
      printf "%d,%.0f,%.0f,%.1f,%d,%.0f,%.0f,%d,%.1f,%.0f,%d,%d,%.0f,%.2f,%d,%.2f\n", i,(acc-pa)/dt,q,(rej-pr)/dt,xp-pe,heap/1048576,rss/1048576,gor,gc*1000,jr/1048576,win,age,pau*1000,wr,lag,free/1073741824}')
  echo "$line" >>"$CSV"
  echo "$line" | awk -F, '{printf "%-4s %-7s %-5s %-5s %-6s %-8s %-8s %-6s %-7s %-9s %-9s %-9s %-9s %-9s %-7s %-8s\n",$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16}'
  prev_acc=$acc; prev_rej=$rej; prev_exp=$exp; prev_t=$now
  if awk -v f="$free" -v min="$MIN_FREE_GB" 'BEGIN{exit !(f/1073741824 < min)}'; then
    echo "!! Queda menos de ${MIN_FREE_GB} GiB libres: se detiene la carga para no llenar el disco."; ABORT=1; $COMPOSE kill loadgen >/dev/null 2>&1; pkill -P $LPID 2>/dev/null; break
  fi
done
wait $LPID 2>/dev/null

echo; echo "=== Resultado del generador (latencia medida por el cliente)"; grep -E "órdenes completadas|latencia|respuestas|cancelaciones|auditor|AVISO|OK|FALLO" "$LOAD" || cat "$LOAD" | tail -5
echo; echo "=== Extremos observados"
awk -F, 'NR>1 {if($3>qm)qm=$3; if($6>hm)hm=$6; if($7>rm)rm=$7; if($8>gm)gm=$8; if($9>cm)cm=$9; if($13>pm)pm=$13; if($14>wm)wm=$14; if($15>lm)lm=$15; if(min==""||$16<min)min=$16; if($11>vm)vm=$11; if($10>jm)jm=$10}
  END{printf "  cola máx: %d | heap máx: %d MiB | RSS máx: %d MiB | goroutines máx: %d | pausa GC máx: %.1f ms\n  pausa del snapshot máx: %d ms | escritura del snapshot máx: %.2f s | lag del historial máx: %d\n  ventana de idempotencia máx: %d ids | journal: %d MiB | libre mínimo en disco: %.2f GiB\n",qm,hm,rm,gm,cm,pm,wm,lm,vm,jm,min}' "$CSV"
echo "  auditoría final: $(curl -s "http://$API/audit")"

echo; echo "=== Reinicio con el estado ya grande"
restart_info() { $COMPOSE logs orderbook --no-log-prefix 2>&1 | grep "recuperación completa" | tail -1 | awk '{ for(i=1;i<=NF;i++){} print }' | sed -E 's/.*"desde_snapshot":([a-z]+).*"registros_reproducidos":([0-9]+).*"took":"([^"]+)".*/  desde_snapshot=\1  registros_reproducidos=\2  TIEMPO=\3/'; }
$COMPOSE restart orderbook >/dev/null 2>&1; for _ in $(seq 1 120); do curl -s -o /dev/null "http://$API/healthz" && break; sleep 1; done
echo "- apagado ordenado (SIGTERM, deja snapshot final) y arranque:"; restart_info
echo "  tamaño de archivos: $($COMPOSE exec -T orderbook sh -c 'ls -la /data | grep -E "journal.log$|snapshot.bin$" | awk "{printf \"%s=%.0fMiB \", \$9, \$5/1048576}"' 2>/dev/null)"
[ "$ABORT" = 1 ] && echo "(la corrida se cortó antes de tiempo por falta de disco)"
echo; echo "Muestras por minuto guardadas en $CSV"
