#!/usr/bin/env python3
"""Resume resultados de JMeter y genera el informe HTML de rendimiento. Solo biblioteca estándar (sin dependencias).

    python3 jmeter/make_report.py summarize jmeter/results/<corrida> [--fail-on-error]
        results.jtl (+ prometheus.csv, meta.json)  ->  summary.json, timeseries.csv, histogram.csv, errors.csv, labels.csv

    python3 jmeter/make_report.py html --config jmeter/report/runs.json --out jmeter/report/index.html
        lee los summary.json / timeseries.csv / prometheus.csv de cada corrida y arma UN archivo HTML autocontenido (SVG inline).

Qué es de JMeter y qué es del motor: la latencia, el throughput, los errores y los hilos salen del .jtl (lo que vio el CLIENTE).
Heap, RSS, GC, cola y snapshots salen de Prometheus (lo que reporta el MOTOR) y se alinean por reloj de pared; el informe
las rotula por separado y no se inventa ninguna serie que no exista.
"""
import argparse
import csv
import html
import json
import math
import re
import sys
from array import array
from collections import Counter, defaultdict
from pathlib import Path

BIN_S = 5                                  # ancho de las ventanas de las series temporales
MAIN = "POST /orders"                      # la operación de negocio que se cuenta como "órdenes"
PCTS = (50, 90, 95, 99, 99.9)


def pct(sorted_vals, p):
    if not len(sorted_vals):
        return float("nan")
    k = max(0, min(len(sorted_vals) - 1, math.ceil(p / 100.0 * len(sorted_vals)) - 1))
    return float(sorted_vals[k])


def norm_msg(m):
    m = re.sub(r"\d+", "N", (m or "").strip())
    return m[:110]


# ----------------------------------------------------------------------------- summarize
def summarize(d: Path, fail_on_error=False):
    jtl = d / "results.jtl"
    if not jtl.exists():
        print(f"no existe {jtl}", file=sys.stderr)
        return 1
    labels = defaultdict(lambda: {"n": 0, "ok": 0, "sum": 0, "max": 0, "vals": array("I")})
    errors = Counter()
    bins = defaultdict(lambda: {"ok": 0, "err": 0, "vals": array("I"), "all": 0, "thr": 0})
    allv = array("I")
    t_first = t_last = None
    t_load0 = t_load1 = None
    max_threads = 0
    with open(jtl, newline="") as f:
        for row in csv.DictReader(f):
            try:
                ts = int(row["timeStamp"]); el = int(row["elapsed"])
            except (KeyError, ValueError):
                continue
            lab = row["label"]; ok = row["success"] == "true"
            t_first = ts if t_first is None or ts < t_first else t_first
            t_last = ts + el if t_last is None or ts + el > t_last else t_last
            L = labels[lab]; L["n"] += 1; L["sum"] += el; L["max"] = max(L["max"], el)
            if ok:
                L["ok"] += 1; L["vals"].append(el)
            else:
                errors[(lab, row.get("responseCode", ""), norm_msg(row.get("failureMessage")))] += 1
            if lab == MAIN:
                t_load0 = ts if t_load0 is None or ts < t_load0 else t_load0
                t_load1 = ts if t_load1 is None or ts > t_load1 else t_load1
    if t_load0 is None:                       # plan de integridad (no tiene POST /orders con ese nombre) o corrida vacía
        t_load0, t_load1 = t_first or 0, t_last or 0
    # segunda pasada para las series (necesita t_load0)
    with open(jtl, newline="") as f:
        for row in csv.DictReader(f):
            try:
                ts = int(row["timeStamp"]); el = int(row["elapsed"])
            except (KeyError, ValueError):
                continue
            if ts < t_load0:
                continue
            b = bins[(ts - t_load0) // (BIN_S * 1000)]
            b["all"] += 1
            try:
                b["thr"] = max(b["thr"], int(row.get("allThreads") or 0))
            except ValueError:
                pass
            if row["label"] == MAIN:
                if row["success"] == "true":
                    b["ok"] += 1; b["vals"].append(el); allv.append(el)
                else:
                    b["err"] += 1
    if bins and t_load1 is not None:
        last = max(bins)
        if t_load1 < t_load0 + (last + 1) * BIN_S * 1000 - 2000:   # el último tramo de 5 s quedó a medias: no es una caída de throughput
            del bins[last]
    max_threads = max((b["thr"] for b in bins.values()), default=0)

    # --- series temporales
    nb = (max(bins) + 1) if bins else 0
    with open(d / "timeseries.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["epoch_s", "t_rel_s", "orders_ok_per_s", "orders_err_per_s", "all_requests_per_s", "p50_ms", "p90_ms", "p95_ms", "p99_ms", "max_ms", "active_threads"])
        for i in range(nb):
            b = bins.get(i)
            if not b:
                continue
            v = sorted(b["vals"])
            w.writerow([(t_load0 // 1000) + i * BIN_S, i * BIN_S, f"{b['ok']/BIN_S:.1f}", f"{b['err']/BIN_S:.1f}", f"{b['all']/BIN_S:.1f}",
                        *(f"{pct(v, p):.2f}" if v else "" for p in (50, 90, 95, 99)), f"{max(v):.0f}" if v else "", b["thr"]])
    # --- histograma (cubos logarítmicos, ms)
    edges = [0.5 * (2 ** (k / 2)) for k in range(0, 30)]
    allv_sorted = sorted(allv)
    counts = [0] * (len(edges) + 1)
    for e in allv_sorted:
        k = 0
        while k < len(edges) and e > edges[k]:
            k += 1
        counts[k] += 1
    with open(d / "histogram.csv", "w", newline="") as f:
        w = csv.writer(f); w.writerow(["hasta_ms", "ordenes"])
        for k, c in enumerate(counts):
            w.writerow([f"{edges[k]:.2f}" if k < len(edges) else "inf", c])
    # --- errores y etiquetas
    with open(d / "errors.csv", "w", newline="") as f:
        w = csv.writer(f); w.writerow(["etiqueta", "codigo_http", "mensaje", "cantidad"])
        for (lab, code, msg), n in errors.most_common():
            w.writerow([lab, code, msg, n])
    label_rows = []
    with open(d / "labels.csv", "w", newline="") as f:
        w = csv.writer(f); w.writerow(["etiqueta", "muestras", "ok", "errores", "media_ms", "p50_ms", "p95_ms", "p99_ms", "max_ms"])
        for lab, L in sorted(labels.items()):
            v = sorted(L["vals"])
            row = [lab, L["n"], L["ok"], L["n"] - L["ok"], f"{L['sum']/max(L['n'],1):.2f}", f"{pct(v,50):.1f}", f"{pct(v,95):.1f}", f"{pct(v,99):.1f}", L["max"]]
            w.writerow(row); label_rows.append(row)
    # --- resumen
    main = labels.get(MAIN)
    dur_load = max((t_load1 - t_load0) / 1000.0, 0.001)
    meta = {}
    if (d / "meta.json").exists():
        meta = json.loads((d / "meta.json").read_text())
    params = {}
    if (d / "params.txt").exists():
        params = dict(l.strip().split("=", 1) for l in (d / "params.txt").read_text().splitlines() if "=" in l)
    total_err = sum(L["n"] - L["ok"] for L in labels.values())
    expected_codes = sum(L["ok"] for lab, L in labels.items() if lab.startswith("ERR-ESPERADO"))
    s = {
        "meta": meta, "params": params,
        "duracion_total_s": round(((t_last or 0) - (t_first or 0)) / 1000.0, 1),
        "duracion_carga_s": round(dur_load, 1),
        "ordenes_ok": main["ok"] if main else 0,
        "ordenes_error": (main["n"] - main["ok"]) if main else 0,
        "ordenes_por_s_media": round((main["ok"] if main else 0) / dur_load, 1),
        "ordenes_por_s_max_bin": max((b["ok"] / BIN_S for b in bins.values()), default=0),
        "peticiones_totales": sum(L["n"] for L in labels.values()),
        "errores_totales": total_err,
        "errores_esperados_validados": expected_codes,
        "hilos_max": max_threads,
        "latencia_ms": {f"p{p}": round(pct(allv_sorted, p), 2) for p in PCTS} | {"max": float(allv_sorted[-1]) if len(allv_sorted) else None,
                                                                                    "media": round(sum(allv_sorted) / len(allv_sorted), 2) if len(allv_sorted) else None},
        "errores_por_tipo": [{"etiqueta": k[0], "codigo": k[1], "mensaje": k[2], "n": n} for k, n in errors.most_common(20)],
        "tiene_prometheus": (d / "prometheus.csv").exists(),
    }
    (d / "summary.json").write_text(json.dumps(s, indent=2, ensure_ascii=False))
    lat = s["latencia_ms"]
    print(f"  resumen: {s['ordenes_ok']} órdenes ok, {s['ordenes_error']} con error, {s['ordenes_por_s_media']}/s media; "
          f"p50={lat['p50']} p95={lat['p95']} p99={lat['p99']} ms; errores totales (todas las etiquetas)={total_err}")
    if total_err:
        for e in s["errores_por_tipo"][:6]:
            print(f"    ! {e['n']:>8}  {e['etiqueta']} [{e['codigo']}] {e['mensaje']}")
    return 3 if (fail_on_error and total_err) else 0


# ----------------------------------------------------------------------------- SVG
PALETTE = ["#2a6fdb", "#e8590c", "#2f9e44", "#9c36b5", "#c92a2a", "#0b7285", "#a67c00"]


def nice_ticks(lo, hi, n=5):
    if hi <= lo:
        hi = lo + 1
    raw = (hi - lo) / n
    mag = 10 ** math.floor(math.log10(raw))
    step = min((m * mag for m in (1, 2, 2.5, 5, 10) if m * mag >= raw), default=raw)
    start = math.floor(lo / step) * step
    ticks = []
    v = start
    while v <= hi + step * 0.5:
        ticks.append(v); v += step
    return ticks


def fmt(v):
    if abs(v) >= 1e6:
        return f"{v/1e6:g}M"
    if abs(v) >= 1e3:
        return f"{v/1e3:g}k"
    return f"{v:g}"


def line_chart(title, series, xlabel="segundos desde el inicio de la carga", ylabel="", yzero=True, ylog=False, w=900, h=250, note=""):
    """series: [(label, [(x, y), ...]) ...]. Devuelve un <svg>. Las series vacías se omiten."""
    series = [(l, [(x, y) for x, y in pts if y is not None and not (isinstance(y, float) and math.isnan(y))]) for l, pts in series]
    series = [(l, p) for l, p in series if p]
    if not series:
        return f'<div class="nodata"><b>{html.escape(title)}</b><br>sin datos para esta corrida</div>'
    ml, mr, mt, mb = 62, 14, 46, 42
    xs = [x for _, p in series for x, _ in p]; ys = [y for _, p in series for _, y in p]
    x0, x1 = min(xs), max(xs)
    ylo = 0 if yzero else min(ys)
    if ylog:
        ys_pos = [y for y in ys if y > 0] or [1]
        ylo = 10 ** math.floor(math.log10(min(ys_pos))); yhi = 10 ** math.ceil(math.log10(max(ys_pos)))
    else:
        yhi = max(ys) * 1.08 if max(ys) > ylo else ylo + 1
    X = lambda x: ml + (x - x0) / max(x1 - x0, 1e-9) * (w - ml - mr)
    if ylog:
        Y = lambda y: mt + (1 - (math.log10(max(y, ylo)) - math.log10(ylo)) / max(math.log10(yhi) - math.log10(ylo), 1e-9)) * (h - mt - mb)
        yticks = [10 ** k for k in range(int(math.log10(ylo)), int(math.log10(yhi)) + 1)]
    else:
        Y = lambda y: mt + (1 - (y - ylo) / max(yhi - ylo, 1e-9)) * (h - mt - mb)
        yticks = [t for t in nice_ticks(ylo, yhi) if ylo <= t <= yhi * 1.001]
    o = [f'<svg viewBox="0 0 {w} {h}" role="img" aria-label="{html.escape(title)}" class="chart">']
    o.append(f'<text x="{ml}" y="18" class="ct">{html.escape(title)}</text>')
    for t in yticks:
        y = Y(t)
        o.append(f'<line x1="{ml}" x2="{w-mr}" y1="{y:.1f}" y2="{y:.1f}" class="grid"/><text x="{ml-6}" y="{y+4:.1f}" class="tick" text-anchor="end">{fmt(t)}</text>')
    for t in nice_ticks(x0, x1, 6):
        if x0 <= t <= x1:
            x = X(t)
            o.append(f'<line x1="{x:.1f}" x2="{x:.1f}" y1="{h-mb}" y2="{h-mb+4}" class="axis"/><text x="{x:.1f}" y="{h-mb+16}" class="tick" text-anchor="middle">{fmt(t)}</text>')
    o.append(f'<line x1="{ml}" x2="{w-mr}" y1="{h-mb}" y2="{h-mb}" class="axis"/><line x1="{ml}" x2="{ml}" y1="{mt}" y2="{h-mb}" class="axis"/>')
    o.append(f'<text x="{(ml+w-mr)/2:.0f}" y="{h-6}" class="tick" text-anchor="middle">{html.escape(xlabel)}</text>')
    if ylabel:
        o.append(f'<text transform="translate(13 {(mt+h-mb)/2:.0f}) rotate(-90)" class="tick" text-anchor="middle">{html.escape(ylabel)}</text>')
    lx = ml + 6
    for i, (lab, pts) in enumerate(series):
        c = PALETTE[i % len(PALETTE)]
        d = " ".join(f"{'M' if j == 0 else 'L'}{X(x):.1f},{Y(y):.1f}" for j, (x, y) in enumerate(pts))
        o.append(f'<path d="{d}" fill="none" stroke="{c}" stroke-width="1.6" stroke-linejoin="round"/>')
        o.append(f'<rect x="{lx}" y="{h-mb-14-0}" width="0" height="0"/>')
    # leyenda arriba a la derecha
    lx = w - mr - 4
    for i, (lab, _) in reversed(list(enumerate(series))):
        c = PALETTE[i % len(PALETTE)]
        tw = 7 * len(lab) + 22
        lx -= tw
        o.append(f'<line x1="{lx}" x2="{lx+14}" y1="32" y2="32" stroke="{c}" stroke-width="2.5"/><text x="{lx+18}" y="36" class="tick">{html.escape(lab)}</text>')
    o.append("</svg>")
    return "".join(o) + (f'<p class="note">{note}</p>' if note else "")


def bar_chart(title, cats, series, unit="ms", w=900, h=260, log=False):
    """Barras agrupadas: cats = ['p50','p95',...]; series = [(label, [valores...])]."""
    vals = [v for _, vs in series for v in vs if v is not None and v > 0]
    if not vals:
        return f'<div class="nodata"><b>{html.escape(title)}</b><br>sin datos</div>'
    ml, mr, mt, mb = 62, 14, 46, 40
    if log:
        lo = 10 ** math.floor(math.log10(min(vals))); hi = 10 ** math.ceil(math.log10(max(vals)))
        Y = lambda v: mt + (1 - (math.log10(max(v, lo)) - math.log10(lo)) / max(math.log10(hi) - math.log10(lo), 1e-9)) * (h - mt - mb)
        ticks = [10 ** k for k in range(int(math.log10(lo)), int(math.log10(hi)) + 1)]
    else:
        lo, hi = 0, max(vals) * 1.1
        Y = lambda v: mt + (1 - v / hi) * (h - mt - mb)
        ticks = [t for t in nice_ticks(lo, hi) if t <= hi * 1.001]
    o = [f'<svg viewBox="0 0 {w} {h}" role="img" aria-label="{html.escape(title)}" class="chart">', f'<text x="{ml}" y="18" class="ct">{html.escape(title)}</text>']
    for t in ticks:
        o.append(f'<line x1="{ml}" x2="{w-mr}" y1="{Y(t):.1f}" y2="{Y(t):.1f}" class="grid"/><text x="{ml-6}" y="{Y(t)+4:.1f}" class="tick" text-anchor="end">{fmt(t)}</text>')
    gw = (w - ml - mr) / len(cats)
    bw = min(46, gw / (len(series) + 1))
    for ci, cat in enumerate(cats):
        gx = ml + ci * gw + gw / 2 - bw * len(series) / 2
        for si, (_, vs) in enumerate(series):
            v = vs[ci]
            if v is None or v <= 0:
                continue
            c = PALETTE[si % len(PALETTE)]
            y = Y(v)
            o.append(f'<rect x="{gx+si*bw:.1f}" y="{y:.1f}" width="{bw-3:.1f}" height="{h-mb-y:.1f}" fill="{c}"><title>{html.escape(series[si][0])} · {cat}: {v:g} {unit}</title></rect>')
            o.append(f'<text x="{gx+si*bw+(bw-3)/2:.1f}" y="{y-3:.1f}" class="tick" text-anchor="middle">{(f"{v:,.0f}" if v >= 100 else f"{v:.3g}")}</text>')
        o.append(f'<text x="{ml+ci*gw+gw/2:.1f}" y="{h-mb+16}" class="tick" text-anchor="middle">{html.escape(cat)}</text>')
    o.append(f'<line x1="{ml}" x2="{w-mr}" y1="{h-mb}" y2="{h-mb}" class="axis"/>')
    lx = w - mr - 4
    for i, (lab, _) in reversed(list(enumerate(series))):
        c = PALETTE[i % len(PALETTE)]
        lx -= 7 * len(lab) + 22
        o.append(f'<rect x="{lx}" y="26" width="12" height="9" fill="{c}"/><text x="{lx+16}" y="35" class="tick">{html.escape(lab)}</text>')
    o.append("</svg>")
    return "".join(o)


def hist_chart(title, rows, w=900, h=230):
    """rows = [(hasta_ms, n)]"""
    tot = sum(n for _, n in rows)
    if not tot:
        return f'<div class="nodata"><b>{html.escape(title)}</b><br>sin datos</div>'
    ml, mr, mt, mb = 62, 14, 30, 46
    mx = max(n for _, n in rows) * 1.08
    bw = (w - ml - mr) / len(rows)
    o = [f'<svg viewBox="0 0 {w} {h}" role="img" aria-label="{html.escape(title)}" class="chart"><text x="{ml}" y="18" class="ct">{html.escape(title)}</text>']
    for t in nice_ticks(0, mx, 4):
        if t <= mx:
            y = mt + (1 - t / mx) * (h - mt - mb)
            o.append(f'<line x1="{ml}" x2="{w-mr}" y1="{y:.1f}" y2="{y:.1f}" class="grid"/><text x="{ml-6}" y="{y+4:.1f}" class="tick" text-anchor="end">{fmt(t)}</text>')
    for i, (edge, n) in enumerate(rows):
        bh = n / mx * (h - mt - mb)
        x = ml + i * bw
        o.append(f'<rect x="{x+1:.1f}" y="{h-mb-bh:.1f}" width="{bw-2:.1f}" height="{bh:.1f}" fill="#2a6fdb"><title>≤ {edge} ms: {n} órdenes ({100*n/tot:.2f} %)</title></rect>')
        if i % 3 == 0:
            lab = edge if edge == "inf" else (f"{float(edge):.3g}")
            o.append(f'<text x="{x+bw/2:.1f}" y="{h-mb+14}" class="tick" text-anchor="middle">{"&gt;" if edge=="inf" else ""}{lab}</text>')
    o.append(f'<text x="{(ml+w-mr)/2:.0f}" y="{h-6}" class="tick" text-anchor="middle">tiempo de respuesta de POST /orders (ms, cubos logarítmicos: cada barra incluye hasta su valor)</text>')
    o.append(f'<line x1="{ml}" x2="{w-mr}" y1="{h-mb}" y2="{h-mb}" class="axis"/></svg>')
    return "".join(o)


# ----------------------------------------------------------------------------- HTML
CSS = """
:root{--bg:#f7f8fa;--fg:#1c2330;--mut:#5b6678;--card:#fff;--bd:#dfe3ea;--acc:#2a6fdb;--grid:#e6e9ef;--ok:#2f9e44;--bad:#c92a2a;--warn:#a67c00}
@media (prefers-color-scheme:dark){:root{--bg:#10141b;--fg:#e6e9ef;--mut:#93a0b4;--card:#171c25;--bd:#2a3240;--grid:#262e3b;--acc:#6ea0ff}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:1000px;margin:0 auto;padding:24px 16px 80px}h1{font-size:26px;margin:0 0 4px}h2{font-size:20px;margin:36px 0 8px;border-bottom:1px solid var(--bd);padding-bottom:6px}h3{font-size:16px;margin:22px 0 6px}
.sub{color:var(--mut);margin:0 0 16px}.card{background:var(--card);border:1px solid var(--bd);border-radius:10px;padding:14px 16px;margin:12px 0}
table{border-collapse:collapse;width:100%;font-size:13.5px;display:block;overflow-x:auto}th,td{border-bottom:1px solid var(--bd);padding:6px 10px;text-align:right;white-space:nowrap}th:first-child,td:first-child{text-align:left}th{color:var(--mut);font-weight:600}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin:10px 0}.kpi{background:var(--card);border:1px solid var(--bd);border-radius:10px;padding:10px 12px}.kpi b{display:block;font-size:22px}.kpi span{color:var(--mut);font-size:12.5px}
.chart{width:100%;height:auto;background:var(--card);border:1px solid var(--bd);border-radius:10px;margin:8px 0}.chart text{fill:var(--fg);font:11px system-ui,sans-serif}.chart .ct{font-weight:600;font-size:13px}.chart .tick{fill:var(--mut)}
.chart .grid{stroke:var(--grid);stroke-width:1}.chart .axis{stroke:var(--mut);stroke-width:1}.nodata{border:1px dashed var(--bd);border-radius:10px;padding:18px;color:var(--mut);margin:8px 0}
.badge{display:inline-block;font-size:11.5px;font-weight:600;border-radius:999px;padding:1px 9px;margin-left:6px;border:1px solid var(--bd);vertical-align:middle}.b-j{color:var(--acc)}.b-m{color:var(--warn)}
.note{color:var(--mut);font-size:12.5px;margin:0 0 8px}.warn{border-left:4px solid var(--warn);background:var(--card);padding:10px 14px;border-radius:6px;margin:12px 0}.bad{color:var(--bad)}.ok{color:var(--ok)}code{background:var(--grid);padding:1px 5px;border-radius:4px;font-size:12.5px}
"""


def read_csv(p):
    if not Path(p).exists():
        return []
    with open(p, newline="") as f:
        return list(csv.DictReader(f))


def num(x):
    try:
        return float(x)
    except (TypeError, ValueError):
        return None


ENGINE_ROWS = [("RSS (MiB)", "rss_bytes", 1 / 1048576), ("heap en uso (MiB)", "heap_inuse_bytes", 1 / 1048576), ("pausa GC máx. reciente (ms)", "gc_pause_max_s", 1000),
               ("pausa del snapshot (ms)", "snapshot_pause_s", 1000), ("profundidad de cola", "queue_depth", 1), ("cola p99 (ms)", "queue_wait_p99_s", 1000),
               ("matching+liquidación p99 (ms)", "batch_exec_p99_s", 1000), ("fsync p99 (ms)", "fsync_p99_s", 1000), ("HTTP POST /orders p99 (ms)", "http_post_orders_p99_s", 1000),
               ("órdenes/s según el motor", "engine_orders_per_s", 1), ("lag del historial (registros)", "projector_lag", 1)]


def engine_vals(pm):
    out = {}
    for name, col, k in ENGINE_ROWS:
        v = sorted(num(x[col]) * k for x in pm if num(x[col]) is not None)
        if v:
            out[col] = (v[len(v) // 2], v[-1])
    return out


def engine_table(pm):
    v = engine_vals(pm)
    return ('<div class="card"><table><tr><th>Métrica del motor (Prometheus, muestreo cada 5 s)</th><th>mediana</th><th>máximo</th></tr>' + "".join(
        f"<tr><td>{n}</td><td>{v[c][0]:,.1f}</td><td>{v[c][1]:,.1f}</td></tr>" for n, c, _ in ENGINE_ROWS if c in v) + "</table></div>")


def trim_partial(ts):
    """El último tramo de 5 s suele quedar a medias (la prueba termina a mitad de ventana) y se vería como una caída falsa de throughput.
    Las corridas nuevas ya lo descartan al resumir; esto cubre las que se resumieron antes de ese ajuste."""
    if len(ts) > 8:
        ref = sorted(num(x["orders_ok_per_s"]) or 0 for x in ts[-7:-1])[3]
        if (num(ts[-1]["orders_ok_per_s"]) or 0) < 0.5 * ref:
            return ts[:-1]
    return ts


def run_section(r, repo):
    d = repo / r["dir"]
    s = json.loads((d / "summary.json").read_text())
    ts = trim_partial(read_csv(d / "timeseries.csv"))
    pm = read_csv(d / "prometheus.csv")
    hist = [(row["hasta_ms"], int(row["ordenes"])) for row in read_csv(d / "histogram.csv")]
    errs = read_csv(d / "errors.csv")
    lab = read_csv(d / "labels.csv")
    m, p, lat = s.get("meta", {}), s.get("params", {}), s["latencia_ms"]
    h = [f'<h2 id="{html.escape(r["id"])}">{html.escape(r["titulo"])}</h2>']
    h.append(f'<p class="sub">{html.escape(r.get("descripcion",""))}</p>')
    h.append('<div class="kpis">' + "".join(f'<div class="kpi"><b>{v}</b><span>{k}</span></div>' for k, v in [
        ("órdenes/s (media de la carga)", f'{s["ordenes_por_s_media"]:,.0f}'), ("p50 (ms)", f'{lat["p50"]}'), ("p95 (ms)", f'{lat["p95"]}'),
        ("p99 (ms)", f'{lat["p99"]}'), ("p99.9 (ms)", f'{lat["p99.9"]}'), ("máx (ms)", f'{lat["max"]:.0f}' if lat["max"] is not None else "–"),
        ("órdenes OK", f'{s["ordenes_ok"]:,}'), ("errores (todas las etiquetas)", f'<span class="{"bad" if s["errores_totales"] else "ok"}">{s["errores_totales"]:,}</span>'),
        ("hilos máx.", s["hilos_max"]), ("duración de la carga", f'{s["duracion_carga_s"]:.0f} s')]) + "</div>")
    h.append('<div class="card"><table><tr><th>Parámetro</th><th>Valor</th></tr>' + "".join(
        f"<tr><td>{html.escape(k)}</td><td>{html.escape(str(v))}</td></tr>" for k, v in [
            ("binario probado", r.get("binario", "?")), ("commit / rama del arnés de pruebas (este repositorio al ejecutar)", f'{m.get("commit","?")} / {m.get("rama","?")}'), ("fecha (UTC)", m.get("fecha_utc", "?")), ("escenario", p.get("escenario", "?")),
            ("hilos · rampa · duración · tasa objetivo", f'{p.get("threads","?")} · {p.get("rampup_s","?")} s · {p.get("duration_s","?")} s · {p.get("tps_objetivo","?")}/s'),
            ("entorno", f'{m.get("cpu_nucleos","?")} núcleos, {m.get("memoria_mb","?")} MB RAM; JMeter {m.get("jmeter","")}; {m.get("java","")}'),
            ("condiciones", r.get("condiciones", "")), ("almacenamiento", r.get("almacenamiento", "")),
            ("disco libre al terminar", f'{m.get("disco_libre_gb_al_terminar","?")} GB')]) + "</table></div>")
    if r.get("aviso"):
        h.append(f'<div class="warn">{r["aviso"]}</div>')
    tsx = lambda col: [(num(x["t_rel_s"]), num(x[col])) for x in ts]
    h.append('<h3>Lo que vio el cliente <span class="badge b-j">JMeter</span></h3>')
    h.append(line_chart("Throughput de POST /orders (órdenes/s, ventanas de 5 s)", [("OK", tsx("orders_ok_per_s")), ("con error", tsx("orders_err_per_s"))], ylabel="órdenes/s",
                        note="Solo cuenta aceptaciones 201 con validación de negocio superada; los errores se muestran aparte."))
    h.append(line_chart("Latencia de POST /orders por percentil (ms, ventanas de 5 s)", [("p50", tsx("p50_ms")), ("p90", tsx("p90_ms")), ("p95", tsx("p95_ms")), ("p99", tsx("p99_ms"))],
                        ylabel="ms (escala log)", ylog=True, yzero=False,
                        note="JMeter es de lazo cerrado con un Constant Throughput Timer: si el servidor se frena, los hilos esperan y la latencia medida NO incluye ese atraso (omisión coordinada). Las cifras del <code>loadgen</code> open-loop del informe de optimización sí lo incluyen."))
    h.append(line_chart("Hilos activos (usuarios concurrentes)", [("hilos", tsx("active_threads"))], ylabel="hilos"))
    h.append(hist_chart("Distribución de tiempos de respuesta de POST /orders", hist))
    if errs:
        h.append('<h3>Errores por tipo</h3><div class="card"><table><tr><th>Etiqueta</th><th>HTTP</th><th>Mensaje (números normalizados)</th><th>Cantidad</th></tr>' + "".join(
            f'<tr><td>{html.escape(e["etiqueta"])}</td><td>{html.escape(e["codigo_http"])}</td><td style="text-align:left;white-space:normal">{html.escape(e["mensaje"])}</td><td>{int(e["cantidad"]):,}</td></tr>' for e in errs[:15]) + "</table></div>")
    else:
        h.append('<p class="ok"><b>0 errores</b> en todas las etiquetas (los errores esperados del contrato, p. ej. 409/422/400, se validan y cuentan como aciertos).</p>')
    h.append('<h3>Por operación</h3><div class="card"><table><tr><th>Etiqueta</th><th>Muestras</th><th>Errores</th><th>media ms</th><th>p50</th><th>p95</th><th>p99</th><th>máx</th></tr>' + "".join(
        f'<tr><td>{html.escape(x["etiqueta"])}</td><td>{int(x["muestras"]):,}</td><td>{int(x["errores"]):,}</td><td>{x["media_ms"]}</td><td>{x["p50_ms"]}</td><td>{x["p95_ms"]}</td><td>{x["p99_ms"]}</td><td>{x["max_ms"]}</td></tr>' for x in lab) + "</table></div>")
    # motor
    h.append('<h3>Lo que reportó el motor <span class="badge b-m">Prometheus</span></h3>')
    if pm:
        h.append(engine_table(pm))
    if not pm:
        h.append('<div class="nodata">No hay métricas del motor para esta corrida (Prometheus no estaba disponible al ejecutarla). No se muestran series.</div>')
    else:
        t0 = num(pm[0]["epoch_s"]) - num(pm[0]["t_rel_s"])
        # eje alineado con JMeter: segundos desde el inicio de la CARGA
        load0 = num(ts[0]["epoch_s"]) if ts else t0
        col = lambda c, k=1.0: [(num(x["epoch_s"]) - load0, (num(x[c]) * k) if num(x[c]) is not None else None) for x in pm]
        h.append('<p class="note">Series del motor muestreadas cada 5 s por Prometheus y alineadas por reloj de pared con el inicio de la carga de JMeter. Origen: <code>prometheus.csv</code> (consultas PromQL en <code>jmeter/export_prometheus.py</code>).</p>')
        h.append(line_chart("Profundidad de la cola del secuenciador", [("cola", col("queue_depth"))], ylabel="comandos", note="Con 128 conexiones de cliente la cola no puede superar ≈128 aunque el servidor se frene."))
        h.append(line_chart("Heap en uso y RSS (MiB)", [("RSS", col("rss_bytes", 1 / 1048576)), ("heap en uso", col("heap_inuse_bytes", 1 / 1048576))], ylabel="MiB"))
        h.append(line_chart("Pausa máxima reciente del GC (ms) y pausa del snapshot (ms)", [("GC (stop-the-world)", col("gc_pause_max_s", 1000)), ("snapshot (secuenciador detenido)", col("snapshot_pause_s", 1000))], ylabel="ms"))
        h.append(line_chart("Ciclos de GC por segundo", [("ciclos/s", col("gc_cycles_per_s"))], ylabel="ciclos/s"))
        h.append(line_chart("p99 por etapa en el motor (ms): cola · matching · fsync · HTTP", [("cola", col("queue_wait_p99_s", 1000)), ("matching+liquidación", col("batch_exec_p99_s", 1000)),
                                                                                                 ("fsync", col("fsync_p99_s", 1000)), ("HTTP POST /orders", col("http_post_orders_p99_s", 1000))], ylabel="ms (log)", ylog=True, yzero=False))
        h.append(line_chart("Órdenes/s según el motor (comparar con el throughput de JMeter)", [("motor", col("engine_orders_per_s"))], ylabel="órdenes/s"))
        h.append(line_chart("Lag del historial en Postgres (registros)", [("lag", col("projector_lag"))], ylabel="registros", note="Vacío o en 0 si el proyector no estaba activo."))
    return "\n".join(h), s


def build_html(cfg, repo, out):
    runs = cfg["runs"]
    secs, sums = [], {}
    for r in runs:
        sec, s = run_section(r, repo)
        secs.append(sec); sums[r["id"]] = s
    h = [f'<!doctype html><html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Informe de rendimiento</title><style>{CSS}</style></head><body><main>']
    h.append(f'<h1>{html.escape(cfg.get("titulo","Informe de rendimiento"))}</h1><p class="sub">{html.escape(cfg.get("subtitulo",""))}</p>')
    h.append(f'<div class="card"><b>Generado:</b> {html.escape(cfg.get("generado",""))} · <b>commit del informe:</b> {html.escape(cfg.get("commit",""))} · <b>rama:</b> {html.escape(cfg.get("rama",""))}<br>'
             f'<b>Entorno de TODAS las corridas:</b> {html.escape(cfg.get("entorno",""))}</div>')
    h.append(cfg.get("introduccion_html", ""))
    # comparación
    h.append('<h2 id="comparacion">Comparación entre corridas</h2>')
    ids = [r["id"] for r in runs if r["id"] in sums]
    names = {r["id"]: r.get("corto", r["id"]) for r in runs}
    if len(ids) > 1:
        h.append('<div class="card"><table><tr><th>Corrida</th><th>órd/s media</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th><th>p99.9</th><th>máx (ms)</th><th>errores</th><th>duración carga</th></tr>' + "".join(
            f'<tr><td>{html.escape(names[i])}</td><td>{sums[i]["ordenes_por_s_media"]:,.0f}</td>' + "".join(f'<td>{sums[i]["latencia_ms"][k]}</td>' for k in ("p50", "p90", "p95", "p99", "p99.9")) +
            f'<td>{sums[i]["latencia_ms"]["max"]:.0f}</td><td>{sums[i]["errores_totales"]:,}</td><td>{sums[i]["duracion_carga_s"]:.0f} s</td></tr>' for i in ids) + "</table></div>")
        cats = ["p50", "p90", "p95", "p99", "p99.9"]
        h.append(bar_chart("Latencia de POST /orders por percentil (ms, escala log) — vista del cliente (JMeter)", cats, [(names[i], [sums[i]["latencia_ms"][c] for c in cats]) for i in ids], log=True))
        h.append(bar_chart("Órdenes/s medias durante la carga (JMeter)", ["órdenes/s"], [(names[i], [sums[i]["ordenes_por_s_media"]]) for i in ids], unit="órd/s"))
    eng = {}
    for r in runs:
        pmrows = read_csv(Path(repo) / r["dir"] / "prometheus.csv")
        if pmrows:
            eng[r["id"]] = engine_vals(pmrows)
    if len(eng) > 1:
        h.append('<h3>Motor (Prometheus) <span class="badge b-m">no es JMeter</span></h3><div class="card"><table><tr><th>Métrica (mediana / máximo)</th>' +
                 "".join(f"<th>{html.escape(names[i])}</th>" for i in ids if i in eng) + "</tr>" + "".join(
            f"<tr><td>{n}</td>" + "".join((f"<td>{eng[i][c][0]:,.1f} / {eng[i][c][1]:,.1f}</td>" if c in eng[i] else "<td>–</td>") for i in ids if i in eng) + "</tr>"
            for n, c, _ in ENGINE_ROWS) + "</table></div>")
    h.append(cfg.get("comparacion_html", ""))
    h.extend(secs)
    h.append('<h2 id="limitaciones">Limitaciones y cómo leer este informe</h2>')
    h.append(cfg.get("limitaciones_html", ""))
    h.append("</main></body></html>")
    Path(out).write_text("\n".join(h), encoding="utf-8")
    print(f"escrito {out} ({Path(out).stat().st_size/1024:.0f} KiB)")


def markdown_tables(cfg, repo):
    """Tablas en Markdown (para pegar en documentos) con EXACTAMENTE los números de los summary.json y prometheus.csv."""
    rows, eng = [], []
    for r in cfg["runs"]:
        d = Path(repo) / r["dir"]
        s = json.loads((d / "summary.json").read_text()); lat = s["latencia_ms"]
        ts = trim_partial(read_csv(d / "timeseries.csv"))
        steady = sorted(num(x["orders_ok_per_s"]) for x in ts if (num(x["t_rel_s"]) or 0) >= 60)
        ge = (100.0 * sum(1 for v in steady if v >= 4900) / len(steady)) if steady else float("nan")
        rows.append(f'| {r["corto"]} | {s["duracion_carga_s"]:.0f} s | {s["ordenes_ok"]:,} | **{s["ordenes_por_s_media"]:,.0f}** | {ge:.0f} % | {lat["p50"]:g} | {lat["p90"]:g} | {lat["p95"]:g} | {lat["p99"]:g} | {lat["p99.9"]:g} | {lat["max"]:.0f} | {s["errores_totales"]} |')
        pm = read_csv(d / "prometheus.csv")
        if pm:
            v = engine_vals(pm)
            g = lambda c, i: f"{v[c][i]:,.0f}" if c in v else "–"
            eng.append(f'| {r["corto"]} | {g("rss_bytes",1)} | {g("heap_inuse_bytes",1)} | {g("gc_pause_max_s",1)} | {g("snapshot_pause_s",1)} | {g("queue_depth",1)} | {g("queue_wait_p99_s",0)} / {g("queue_wait_p99_s",1)} | {g("batch_exec_p99_s",0)} / {g("batch_exec_p99_s",1)} | {g("fsync_p99_s",0)} / {g("fsync_p99_s",1)} |')
    out = ["| Corrida | Carga | Órdenes OK | órd/s media | ventanas 5 s ≥ 4.900 órd/s | p50 | p90 | p95 | p99 | p99.9 | máx (ms) | Errores |", "|---|---|---|---|---|---|---|---|---|---|---|---|"] + rows
    out += ["", "| Corrida | RSS máx (MiB) | heap máx (MiB) | pausa GC máx (ms) | pausa snapshot máx (ms) | cola máx | cola p99 mediana / máx (ms) | matching p99 mediana / máx (ms) | fsync p99 mediana / máx (ms) |", "|---|---|---|---|---|---|---|---|---|"] + eng
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    a = sub.add_parser("summarize"); a.add_argument("dir"); a.add_argument("--fail-on-error", action="store_true")
    b = sub.add_parser("html"); b.add_argument("--config", required=True); b.add_argument("--out", required=True)
    c = sub.add_parser("md"); c.add_argument("--config", required=True)
    args = ap.parse_args()
    if args.cmd == "summarize":
        return summarize(Path(args.dir), args.fail_on_error)
    repo = Path(__file__).resolve().parent.parent
    cfg = json.loads(Path(args.config).read_text())
    if args.cmd == "md":
        print(markdown_tables(cfg, repo)); return 0
    build_html(cfg, repo, args.out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
