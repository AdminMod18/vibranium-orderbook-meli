#!/usr/bin/env bash
# Fondea billeteras para poder probar sin depositar a mano (OPCIONAL: el sistema funciona igual sin esto;
# quien prefiera hacerlo manual usa POST /deposits, ver README).
#
#   ./scripts/seed.sh                         -> alice bob carol dave erin, con los montos por defecto
#   ./scripts/seed.sh ana luis                -> solo esos usuarios
#   ./scripts/seed.sh --count 100             -> user-1 ... user-100
#   ./scripts/seed.sh --brl 50000000 --vib 500 ana   -> montos propios (BRL en CENTAVOS, VIB en unidades)
#
# Por defecto cada usuario recibe R$ 1.000.000,00 (100.000.000 centavos) y 1.000 unidades de Vibranium,
# así puede comprar Y vender. ATENCIÓN: un depósito SUMA; correrlo dos veces duplica los saldos.
# Cada depósito queda en el journal y en la auditoría como cualquier otro (el total depositado cuadra con /audit).
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && { set -a; . ./.env; set +a; }
A=${API:-localhost:${API_PORT:-8080}}
BRL=${BRL:-100000000}
VIB=${VIB:-1000}
users=()

while [ $# -gt 0 ]; do
  case "$1" in
    --brl)   BRL=$2; shift 2 ;;
    --vib)   VIB=$2; shift 2 ;;
    --count) n=$2; shift 2; for i in $(seq 1 "$n"); do users+=("user-$i"); done ;;
    -h|--help) sed -n 2,13p "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "Opción desconocida: $1 (use --help)"; exit 2 ;;
    *)  users+=("$1"); shift ;;
  esac
done
[ ${#users[@]} -gt 0 ] || users=(alice bob carol dave erin)

[[ "$BRL" =~ ^[0-9]+$ && "$VIB" =~ ^[0-9]+$ ]] || { echo "--brl y --vib deben ser enteros no negativos"; exit 2; }
curl -s -o /dev/null "http://$A/healthz" 2>/dev/null || curl -s -o /dev/null "$A/healthz" || { echo "La API no responde en $A. Levante el stack:  docker compose up -d"; exit 1; }
case "$A" in http*) base=$A ;; *) base="http://$A" ;; esac

ok=0; fail=0
for u in "${users[@]}"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -XPOST "$base/deposits" -d "{\"user_id\":\"$u\",\"brl\":$BRL,\"vib\":$VIB}")
  if [ "$code" = 204 ]; then ok=$((ok+1)); else fail=$((fail+1)); echo "  [FALLÓ] $u -> HTTP $code (ids válidos: 1-64 caracteres [A-Za-z0-9._:@+-])"; fi
done
echo "Fondeados: $ok usuario(s) con R\$ $((BRL/100)),$(printf '%02d' $((BRL%100))) y $VIB VIB cada uno.$([ "$fail" -gt 0 ] && echo " Fallidos: $fail.")"
[ ${#users[@]} -le 10 ] && for u in "${users[@]}"; do echo "  $u: $(curl -s "$base/balances/$u")"; done
echo "Auditoría: $(curl -s "$base/audit")"
[ "$fail" = 0 ]
