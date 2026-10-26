#!/usr/bin/env bash
# Guion interactivo de la demo para la presentación.
#   ./scripts/demo.sh            -> avanza con Enter en cada paso (para ir explicando)
#   DEMO_AUTO=1 ./scripts/demo.sh -> sin pausas (para ensayar)
# Requiere el stack arriba:  docker compose up -d
set -uo pipefail
cd "$(dirname "$0")/.."
A=${API:-localhost:8080}
COMPOSE="docker compose ${COMPOSE_ARGS:-}"
R=$(date +%s)$RANDOM; AL=alice-$R; BO=bob-$R
B=$'\033[1m'; D=$'\033[2m'; C=$'\033[36m'; G=$'\033[32m'; N=$'\033[0m'
PP=cat; command -v jq >/dev/null 2>&1 && PP="jq -c ."

pause() { [ "${DEMO_AUTO:-0}" = 1 ] || read -r -p "${D}   [Enter para continuar]${N} " _; echo; }
say()   { printf '\n%s━━ %s%s\n%s%s%s\n' "$B" "$1" "$N" "$D" "$2" "$N"; }
post()  { printf '%s$ curl -XPOST %s%s -d %s%s\n' "$C" "$A" "$1" "'$2'" "$N"; curl -s -XPOST "$A$1" -d "$2" | $PP; echo; }
del()   { printf '%s$ curl -XDELETE %s%s%s\n' "$C" "$A" "$1" "$N"; curl -s -XDELETE "$A$1" | $PP; echo; }
get()   { printf '%s$ curl %s%s%s\n' "$C" "$A" "$1" "$N"; curl -s "$A$1" | $PP; echo; }
run()   { printf '%s$ %s%s\n' "$C" "$*" "$N"; eval "$*"; echo; }

curl -s -o /dev/null "$A/healthz" || { echo "La API no responde en $A. Levante el stack:  docker compose up -d"; exit 1; }

if [[ "$(curl -s "$A/book?depth=1")" != *'"asks":[],"bids":[]'* ]]; then
  printf '%sAVISO:%s el libro NO está vacío (quedaron órdenes de pruebas de carga o drills). Los cruces de la demo no serán los esperados.\n' "$B" "$N"
  echo "Para una demo limpia:  docker compose down -v && docker compose up -d"
  [ "${DEMO_AUTO:-0}" = 1 ] || { read -r -p "¿Continuar igual? [s/N] " yn; [[ "$yn" =~ ^[sSyY]$ ]] || exit 1; }
fi

say "1. Las billeteras" "Alice tiene R\$ 10.000,00 (en centavos) y Bob tiene 100 unidades de Vibranium. Precios en centavos."
post /deposits "{\"user_id\":\"$AL\",\"brl\":1000000}"
post /deposits "{\"user_id\":\"$BO\",\"vib\":100}"
pause

say "2. Una VENTA sin contraparte queda 'en negociación'" "Bob ofrece 10 unidades a R\$ 10,00. Nadie compra todavía: la orden espera en el libro y sus 10 unidades quedan RESERVADAS."
post /orders "{\"id\":\"s1-$R\",\"user_id\":\"$BO\",\"side\":\"SELL\",\"price\":1000,\"qty\":10}"
get "/balances/$BO"
get "/book?depth=5"
pause

say "3. Una COMPRA que cruza: ejecuta al precio del vendedor" "Alice ofrece pagar hasta R\$ 11,00 por 4 unidades. Se ejecuta a R\$ 10,00 (el precio que ya estaba en el libro): mejora de precio. Se acreditan y debitan ambas billeteras."
post /orders "{\"id\":\"b1-$R\",\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":1100,\"qty\":4}"
get "/balances/$AL"
get "/balances/$BO"
pause

say "4. Ejecución PARCIAL" "Alice pide 20 unidades a R\$ 10,00. Solo quedan 6 de Bob: se ejecutan 6 y las 14 restantes quedan esperando en el libro."
post /orders "{\"id\":\"b2-$R\",\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":1000,\"qty\":20}"
get "/book?depth=5"
pause

say "5. Cancelar libera lo reservado" "Alice cancela lo que quedó pendiente: su dinero reservado vuelve a estar disponible."
del "/orders/b2-$R"
get "/balances/$AL"
pause

say "6. Protecciones" "Sin saldo la orden ni entra al libro (422). Un reintento con el mismo id no duplica nada (409). Un bot que manda órdenes sin parar no puede gastar dos veces el mismo saldo."
post /orders "{\"id\":\"x-$R\",\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":1000000,\"qty\":1000000}"
post /orders "{\"id\":\"b1-$R\",\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":1000,\"qty\":1}"
pause

say "7. Trazabilidad: el historial explica cada centavo" "El saldo recalculado SOLO con depósitos + trades (desde Postgres) coincide con el del motor. 'lag':0 = el historial está al día."
for i in $(seq 1 40); do r=$(curl -s "$A/audit/history/$AL"); [[ "$r" == *'"lag":0'* ]] && break; sleep 0.5; done
get "/audit/history/$AL"
get "/orders/b1-$R"
get "/audit"
pause

say "8. Escala y observabilidad" "5.000 órdenes/s durante 20 s. Abra el dashboard en http://localhost:3000 mientras corre: órdenes/s, latencia, cola, group commit, fsync. Al terminar audita los saldos."
echo "   (abra ahora http://localhost:3000)"; pause
run "$COMPOSE run --rm loadgen -rate 5000 -duration 20s -workers 128 2>&1 | grep -E 'órdenes completadas|latencia|respuestas|auditor|OK|FALLO'"
pause

say "9. ¿Qué pasa si se cae la aplicación?" "kill -9 al proceso en plena carga. Docker la reinicia y recupera TODO desde el journal (log de eventos con fsync). Los saldos deben seguir cuadrando."
pause
run "./scripts/drill-docker-crash.sh 2>&1 | tail -6"
pause

say "10. ¿Y si se cae Postgres?" "El mercado NO depende del historial: las órdenes siguen entrando. Solo las consultas históricas degradan (503). Al volver, el historial se pone al día solo."
run "$COMPOSE stop postgres >/dev/null 2>&1; echo 'Postgres detenido'"
post /orders "{\"id\":\"p1-$R\",\"user_id\":\"$AL\",\"side\":\"BUY\",\"price\":900,\"qty\":1}"
printf '%s$ curl -o /dev/null -w "%%{http_code}" %s/orders/p1-%s   (historial)%s\n' "$C" "$A" "$R" "$N"; echo "HTTP $(curl -s -o /dev/null -w '%{http_code}' "$A/orders/p1-$R")  <- 503 esperado"; echo
run "$COMPOSE start postgres >/dev/null 2>&1; echo 'Postgres iniciado'"
for i in $(seq 1 90); do r=$(curl -s "$A/audit/history/$AL"); [[ "$r" == *'"lag":0'* ]] && break; sleep 0.5; done
get "/audit/history/$AL"
pause

curl -s -XDELETE "$A/orders/p1-$R" >/dev/null 2>&1   # deja el libro como estaba (la demo se puede repetir)
printf '%s%sDemo completa.%s Todo lo mostrado queda auditado: "ok":true en /audit.\n' "$B" "$G" "$N"
