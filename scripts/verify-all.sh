#!/usr/bin/env bash
# Verificación completa: tests + smoke + carga + drills de resiliencia, con una tabla PASS/FAIL al final.
# Nada puede fallar en silencio: cada paso se ejecuta aislado y se informa su resultado y duración.
#
#   ./scripts/verify-all.sh            (~4 min; reinicia el stack desde cero: BORRA los datos locales)
#   SKIP_GO=1 ./scripts/verify-all.sh  (sin los pasos que requieren Go instalado)
#
# Variables: COMPOSE_ARGS (p. ej. "-f docker-compose.yml -f override.yml"), API (por defecto localhost:8080).
cd "$(dirname "$0")/.."
COMPOSE="docker compose ${COMPOSE_ARGS:-}"
A=${API:-localhost:8080}
NAMES=(); RES=(); DUR=(); FAIL=0
HAVE_GO=0; command -v go >/dev/null 2>&1 && [ "${SKIP_GO:-0}" != 1 ] && HAVE_GO=1

step() { # step "nombre" comando...
  local name=$1; shift
  printf '\n\033[1m▶ %s\033[0m\n' "$name"
  local t0=$SECONDS out rc
  out=$("$@" 2>&1); rc=$?
  printf '%s\n' "$out" | tail -${TAIL:-6} | cut -c1-200
  NAMES+=("$name"); DUR+=("$((SECONDS - t0))s")
  if [ $rc -eq 0 ]; then RES+=("PASS"); else RES+=("FAIL"); FAIL=1; fi
}
skip() { NAMES+=("$1"); RES+=("SKIP"); DUR+=("-"); printf '\n\033[2m▶ %s: omitido (%s)\033[0m\n' "$1" "$2"; }
wait_up() { for _ in $(seq 1 90); do [ "$($COMPOSE ps --format '{{.Health}}' 2>/dev/null | grep -c healthy)" -ge 2 ] && curl -s -o /dev/null "$A/healthz" && sleep 4 && return 0; sleep 1; done; return 1; }
reset_stack() { $COMPOSE down -v >/dev/null 2>&1; $COMPOSE up -d >/dev/null 2>&1 && wait_up; }

command -v docker >/dev/null 2>&1 || { echo "Falta Docker. Ejecute ./scripts/preflight.sh"; exit 2; }

if [ $HAVE_GO = 1 ]; then
  TAIL=10 step "Tests unitarios y de concurrencia (go test -race)" bash -c 'go vet ./... && go test -race -count=1 ./...'
else skip "Tests (go test)" "Go no instalado o SKIP_GO=1"; fi

step "Levantar el stack desde cero" reset_stack
TAIL=8 step "Smoke test end-to-end (22 comprobaciones)" ./scripts/smoke.sh
TAIL=6 step "Carga: 5.000 órdenes/s durante 15 s + auditoría de saldos" $COMPOSE run --rm loadgen -rate 5000 -duration 15s -workers 128
TAIL=4 step "Drill: crash real de la app (kill -9) y recuperación" ./scripts/drill-docker-crash.sh
step "Detener la app (para el drill de Postgres)" $COMPOSE stop orderbook
TAIL=4 step "Drill: Postgres cae en pleno uso" ./scripts/drill-postgres-down.sh
if [ $HAVE_GO = 1 ]; then
  TAIL=4 step "Drill: kill -9 en plena carga (servidor local)" ./scripts/drill-kill9.sh
  TAIL=4 step "Drill: sobrecarga y backpressure (503)" ./scripts/drill-overload.sh
  TAIL=6 step "Drill: idempotencia con ventana y cambio de ventana entre reinicios" ./scripts/drill-idempotency.sh
else skip "Drills kill-9, sobrecarga e idempotencia" "requieren Go"; fi
step "Dejar el stack limpio y operativo" reset_stack

printf '\n\033[1m════════ RESUMEN ════════\033[0m\n'
for i in "${!NAMES[@]}"; do
  c=32; [ "${RES[$i]}" = FAIL ] && c=31; [ "${RES[$i]}" = SKIP ] && c=33
  printf '  \033[%sm%-4s\033[0m  %-62s %s\n' "$c" "${RES[$i]}" "${NAMES[$i]}" "${DUR[$i]}"
done
if [ $FAIL = 0 ]; then printf '\n\033[32mTODO VERIFICADO.\033[0m\n'; else printf '\n\033[31mHAY FALLOS (ver arriba).\033[0m\n'; exit 1; fi
