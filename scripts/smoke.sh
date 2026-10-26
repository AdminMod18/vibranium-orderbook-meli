#!/usr/bin/env bash
# Prueba de humo end-to-end contra el sistema YA levantado (docker compose up -d).
# Recorre el ciclo completo y falla con un mensaje claro si algo no se comporta como se espera.
set -uo pipefail
A=${API:-localhost:8080}
RUN=$(date +%s)$RANDOM            # permite repetir el smoke sin chocar con ids/usuarios anteriores
FAILS=0
pass() { printf '  [OK]    %s\n' "$1"; }
bad()  { printf '  [FALLO] %s\n          obtenido: %s\n' "$1" "$2"; FAILS=$((FAILS+1)); }
has()  { [[ "$2" == *"$3"* ]] && pass "$1" || bad "$1 (se esperaba «$3»)" "$2"; }
post() { curl -s -XPOST "$A$1" -d "$2"; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

echo "1) El servicio responde"
for i in $(seq 1 60); do [ "$(code "$A/healthz")" = 200 ] && break; sleep 1; done
[ "$(code "$A/healthz")" = 200 ] && pass "GET /healthz = 200" || { bad "el servicio no responde en $A" "¿corrió 'docker compose up -d'?"; exit 1; }

# El smoke asume un libro vacío (si hay órdenes de pruebas anteriores, los cruces no serían los esperados).
if [[ "$(curl -s "$A/book?depth=1")" != *'"asks":[],"bids":[]'* ]]; then
  echo "El libro NO está vacío (quedaron órdenes de pruebas de carga o drills anteriores)."
  echo "Para una prueba limpia:  docker compose down -v && docker compose up -d   y repita."
  exit 2
fi

echo "2) Billeteras y un cruce de órdenes"
AL=alice-$RUN; BO=bob-$RUN
post /deposits "{\"user_id\":\"$AL\",\"brl\":1000000}" >/dev/null
post /deposits "{\"user_id\":\"$BO\",\"vib\":100}" >/dev/null
r=$(post /orders "{\"id\":\"s-$RUN\",\"user_id\":\"$BO\",\"side\":\"SELL\",\"price\":900,\"qty\":4}")
has "una venta sin contraparte queda OPEN en el libro" "$r" '"status":"OPEN"'
r=$(post /orders "{\"id\":\"b-$RUN\",\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":1000,\"qty\":10}")
has "la compra se ejecuta al precio del vendedor (900), no al suyo (1000)" "$r" '"price":900,"qty":4'
has "la compra queda PARTIALLY_FILLED con 6 pendientes" "$r" '"remaining":6,"status":"PARTIALLY_FILLED"'
r=$(curl -s "$A/balances/$AL")
has "alice: pagó 3600, reservó 6000 y recibió 4 VIB" "$r" '"brl_free":990400,"brl_held":6000,"vib_free":4'
r=$(curl -s "$A/balances/$BO")
has "bob: cobró 3600 y entregó 4 VIB" "$r" '"brl_free":3600,"brl_held":0,"vib_free":96'

echo "3) Reglas de protección"
has "mismo id dos veces => 409 (idempotencia)" "$(code -XPOST "$A/orders" -d "{\"id\":\"b-$RUN\",\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":1,\"qty\":1}")" 409
has "sin saldo => 422" "$(code -XPOST "$A/orders" -d "{\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":1000000,\"qty\":1000000}")" 422
has "cantidad inválida => 400" "$(code -XPOST "$A/orders" -d "{\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":10,\"qty\":0}")" 400
r=$(curl -s -XDELETE "$A/orders/b-$RUN")
has "cancelar libera lo reservado" "$r" '"status":"CANCELLED"'
has "tras cancelar, alice recupera su reserva" "$(curl -s "$A/balances/$AL")" '"brl_held":0'

echo "4) Auditoría: los saldos cuadran"
has "invariantes del motor" "$(curl -s "$A/audit")" '"ok":true'

echo "5) Historial en Postgres (eventualmente consistente)"
r=""
for i in $(seq 1 40); do r=$(curl -s "$A/audit/history/$AL"); [[ "$r" == *'"lag":0'* ]] && break; sleep 0.5; done
has "el historial alcanzó al journal (lag 0)" "$r" '"lag":0'
has "saldo recalculado desde el historial == saldo del motor" "$r" '"consistent":true'
has "la orden y sus trades son consultables" "$(curl -s "$A/orders/s-$RUN")" '"status":"FILLED"'

echo "6) Observabilidad"
M=${METRICS:-localhost:9102}
has "GET /readyz = 200" "$(code "$A/readyz")" 200
r=$(curl -s "$M/metrics")
has "/metrics (puerto de administración) expone las órdenes" "$r" 'orderbook_orders_total{result="accepted"'
has "/metrics expone la cola del secuenciador" "$r" 'orderbook_queue_capacity'
has "/metrics NO está en el puerto público de la API" "$(code "$A/metrics")" 404
PR=${PROMETHEUS:-localhost:9090}
if [ "$(code "$PR/-/ready")" = 200 ]; then
  t=""; for i in $(seq 1 20); do t=$(curl -s "$PR/api/v1/targets"); [[ "$t" == *'"health":"up"'* ]] && break; sleep 1; done
  has "Prometheus está leyendo las métricas de la app" "$t" '"health":"up"'
else printf '  [AVISO] Prometheus no responde en %s (omitido)\n' "$PR"; fi
GR=${GRAFANA:-localhost:3000}
if [ "$(code "$GR/api/health")" = 200 ]; then
  has "Grafana tiene el dashboard provisionado" "$(curl -s -u admin:admin "$GR/api/search?query=Order")" 'orderbook-overview'
else printf '  [AVISO] Grafana no responde en %s (omitido)\n' "$GR"; fi

echo
if [ "$FAILS" = 0 ]; then echo "SMOKE OK: todo funciona."; else echo "SMOKE FALLÓ: $FAILS comprobación(es)."; exit 1; fi
