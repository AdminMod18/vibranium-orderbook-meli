#!/usr/bin/env python3
"""Exporta a CSV las métricas del MOTOR (las que expone la app y guarda Prometheus) para el periodo de una corrida de JMeter.

    python3 jmeter/export_prometheus.py --prom localhost:9090 --start <epoch> --end <epoch> --out prometheus.csv

Solo usa la biblioteca estándar. Son métricas del motor, NO de JMeter: el informe las muestra aparte y las alinea por tiempo.
Si una serie no existe (p. ej. Prometheus no estaba levantado) simplemente no aparece: no se inventa nada.
"""
import argparse
import csv
import json
import sys
import urllib.parse
import urllib.request

# nombre de columna -> expresión PromQL (ventanas de 30 s para suavizar el muestreo de 5 s)
SERIES = {
    "queue_depth": "orderbook_queue_depth",
    "heap_inuse_bytes": "go_memstats_heap_inuse_bytes",
    "heap_idle_bytes": "go_memstats_heap_idle_bytes",
    "rss_bytes": "process_resident_memory_bytes",
    "goroutines": "go_goroutines",
    "gc_pause_max_s": 'go_gc_duration_seconds{quantile="1"}',
    "gc_cycles_per_s": "rate(go_gc_duration_seconds_count[30s])",
    "snapshot_pause_s": "orderbook_snapshot_pause_seconds",
    "snapshot_write_s": "orderbook_snapshot_write_seconds",
    "projector_lag": "orderbook_projector_lag",
    "journal_bytes": "orderbook_journal_bytes",
    "disk_free_bytes": "orderbook_journal_disk_free_bytes",
    "engine_orders_per_s": "sum(rate(orderbook_orders_total[30s]))",
    "queue_wait_p99_s": "histogram_quantile(0.99, sum(rate(orderbook_queue_wait_seconds_bucket[30s])) by (le))",
    "batch_exec_p99_s": "histogram_quantile(0.99, sum(rate(orderbook_batch_exec_seconds_bucket[30s])) by (le))",
    "fsync_p99_s": "histogram_quantile(0.99, sum(rate(orderbook_journal_fsync_seconds_bucket[30s])) by (le))",
    "http_post_orders_p99_s": 'histogram_quantile(0.99, sum(rate(orderbook_http_request_duration_seconds_bucket{route="POST /orders"}[30s])) by (le))',
    "rejected_503_per_s": "rate(orderbook_queue_rejected_total[30s])",
}


def query_range(prom, expr, start, end, step):
    url = f"http://{prom}/api/v1/query_range?" + urllib.parse.urlencode({"query": expr, "start": start, "end": end, "step": step})
    with urllib.request.urlopen(url, timeout=60) as r:
        data = json.load(r)
    out = {}
    for s in data["data"]["result"]:
        for ts, v in s["values"]:
            if v not in ("NaN", "+Inf", "-Inf"):
                out[int(float(ts))] = float(v)   # si hubiera varias series se queda con la última: aquí hay una sola instancia
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--prom", default="localhost:9090")
    ap.add_argument("--start", type=int, required=True)
    ap.add_argument("--end", type=int, required=True)
    ap.add_argument("--step", type=int, default=5)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    cols, total = {}, 0
    for name, expr in SERIES.items():
        try:
            cols[name] = query_range(a.prom, expr, a.start, a.end, a.step)
        except Exception as e:  # noqa: BLE001
            print(f"  (sin serie {name}: {e})", file=sys.stderr)
            cols[name] = {}
        total += len(cols[name])
    stamps = sorted({t for c in cols.values() for t in c})
    with open(a.out, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["epoch_s", "t_rel_s"] + list(SERIES))
        for t in stamps:
            w.writerow([t, t - a.start] + [("%.6g" % cols[n][t]) if t in cols[n] else "" for n in SERIES])
    print(f"  Prometheus: {len(stamps)} instantes, {sum(1 for c in cols.values() if c)}/{len(SERIES)} series -> {a.out}")
    return 0 if total else 1


if __name__ == "__main__":
    sys.exit(main())
