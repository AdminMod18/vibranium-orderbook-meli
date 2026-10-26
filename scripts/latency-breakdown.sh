#!/usr/bin/env bash
# Descompone la latencia del servidor por etapa usando los histogramas que ya expone la app y guarda Prometheus:
#   cola (queue_wait) · matching+liquidación del lote (batch_exec) · fsync del journal · HTTP completo (por ruta)
# Para cada etapa muestra el p50, p99 y el MÁXIMO observado entre las ventanas de 1 min del periodo pedido.
#   ./scripts/latency-breakdown.sh [minutos=10]        (el stack debe estar arriba y haber recibido carga)
# Requiere Prometheus (docker compose lo levanta) y curl + python3 (o jq no hace falta).
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f .env ] && { set -a; . ./.env; set +a; }
MIN=${1:-10}
P=${PROM:-localhost:${PROMETHEUS_PORT:-9090}}
curl -sf "http://$P/-/ready" >/dev/null || { echo "Prometheus no responde en $P (¿docker compose up -d?)"; exit 1; }
command -v python3 >/dev/null || { echo "Se necesita python3 para procesar la respuesta"; exit 1; }
python3 - "$P" "$MIN" <<'PY'
import json, sys, time, urllib.parse, urllib.request
prom, minutes = sys.argv[1], int(sys.argv[2])
end = time.time(); start = end - minutes * 60
def q(expr):
    url = f"http://{prom}/api/v1/query_range?" + urllib.parse.urlencode({"query": expr, "start": start, "end": end, "step": 15})
    r = json.load(urllib.request.urlopen(url, timeout=30))
    out = []
    for s in r["data"]["result"]:
        out += [float(v) for _, v in s["values"] if v not in ("NaN", "+Inf", "-Inf")]
    return out
def stage(metric, extra=""):
    row = []
    for qt in (0.5, 0.99, 0.999):
        v = q(f'histogram_quantile({qt}, sum(rate(orderbook_{metric}_bucket{extra}[1m])) by (le))')
        row.append((sorted(v)[len(v)//2] if v else float("nan"), max(v) if v else float("nan")))
    return row
stages = [
    ("cola (espera antes de ejecutarse)", "queue_wait_seconds", ""),
    ("matching + liquidación del lote",   "batch_exec_seconds", ""),
    ("fsync del journal (por lote)",       "journal_fsync_seconds", ""),
    ("HTTP completo POST /orders",         "http_request_duration_seconds", '{route="POST /orders"}'),
]
print(f"Latencia por etapa, últimos {minutes} min (ventanas de 1 min, muestreo cada 15 s). Valores en ms.")
print(f"{'etapa':40s} {'p50 típico':>11s} {'p50 máx':>9s} {'p99 típico':>11s} {'p99 máx':>9s} {'p99.9 típico':>13s} {'p99.9 máx':>10s}")
for name, metric, extra in stages:
    (p50, p50m), (p99, p99m), (p999, p999m) = stage(metric, extra)
    f = lambda x: f"{x*1000:9.2f}"
    print(f"{name:40s} {f(p50):>11s} {f(p50m):>9s} {f(p99):>11s} {f(p99m):>9s} {f(p999):>13s} {f(p999m):>10s}")
bs = q('histogram_quantile(0.5, sum(rate(orderbook_batch_size_bucket[1m])) by (le))')
if bs: print(f"\nComandos por fsync (group commit), mediana de p50: {sorted(bs)[len(bs)//2]:.1f}")
print("\nNota: 'típico' = mediana de las ventanas; 'máx' = la peor ventana. Los histogramas de cola/batch/fsync tienen cubo máximo de 1 s:")
print("un valor de ≈1000 ms significa 'al menos 1 s'. Compárelo con la latencia que mide el cliente (loadgen) para ver cuánto es red/HTTP.")
PY
