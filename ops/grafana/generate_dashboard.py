#!/usr/bin/env python3
"""Genera ops/grafana/dashboards/orderbook.json (el dashboard provisionado de Grafana).

    python3 ops/grafana/generate_dashboard.py

El JSON es el entregable; este script existe para poder revisar las consultas PromQL y la organización como código y no como un JSON de
miles de líneas. Solo biblioteca estándar. Toda métrica usada existe en internal/metrics y cmd/server/main.go (o es de Go/proceso).
"""
import json
from pathlib import Path

OUT = Path(__file__).resolve().parent / "dashboards" / "orderbook.json"
DS = {"type": "prometheus", "uid": "prometheus"}
R = "$rate"  # ventana de cálculo (variable del dashboard)

_id = 0
_panels = []
_y = 0
_x = 0
_rowh = 0


def _next_id():
    global _id
    _id += 1
    return _id


def row(title, description=""):
    global _y, _x, _rowh
    _y += _rowh
    _x = _rowh = 0
    _panels.append({"id": _next_id(), "type": "row", "title": title, "collapsed": False, "panels": [],
                    "gridPos": {"x": 0, "y": _y, "w": 24, "h": 1}, "description": description})
    _y += 1


def place(w, h):
    global _x, _y, _rowh
    if _x + w > 24:
        _y += _rowh
        _x = _rowh = 0
    pos = {"x": _x, "y": _y, "w": w, "h": h}
    _x += w
    _rowh = max(_rowh, h)
    return pos


def tgt(expr, legend="", ref="A", fmt=None, instant=False):
    t = {"refId": ref, "expr": expr, "datasource": DS, "legendFormat": legend, "instant": instant, "range": not instant}
    if fmt:
        t["format"] = fmt
    return t


def steps(*pairs):
    """steps((None,'green'),(0.1,'yellow'),(0.25,'red'))"""
    return {"mode": "absolute", "steps": [{"color": c, "value": v} for v, c in pairs]}


def stat(title, expr, unit, th=None, desc="", w=4, h=4, decimals=None, mappings=None, graph=True, color_mode="background", legend=""):
    fc = {"unit": unit, "thresholds": th or steps((None, "blue")), "mappings": mappings or []}
    if decimals is not None:
        fc["decimals"] = decimals
    _panels.append({"id": _next_id(), "type": "stat", "title": title, "description": desc, "datasource": DS, "gridPos": place(w, h),
                    "targets": [tgt(expr, legend)], "fieldConfig": {"defaults": fc, "overrides": []},
                    "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}, "colorMode": color_mode,
                                "graphMode": "area" if graph else "none", "textMode": "value", "justifyMode": "center"}})


def ts(title, targets, unit, desc="", w=12, h=8, stack=False, fill=15, overrides=None, minimum=None, maximum=None, log=False, calcs=None,
       legend_mode="table", thresholds=None, draw="line"):
    custom = {"lineWidth": 2, "fillOpacity": fill, "showPoints": "never", "spanNulls": True, "drawStyle": draw,
              "stacking": {"mode": "normal" if stack else "none", "group": "A"}, "axisSoftMin": 0 if minimum is None else minimum}
    if log:
        custom["scaleDistribution"] = {"type": "log", "log": 10}
    fc = {"unit": unit, "custom": custom}
    if minimum is not None and not log:
        fc["min"] = minimum
    if maximum is not None:
        fc["max"] = maximum
    if thresholds:
        fc["thresholds"] = thresholds
        custom["thresholdsStyle"] = {"mode": "line"}
    _panels.append({"id": _next_id(), "type": "timeseries", "title": title, "description": desc, "datasource": DS, "gridPos": place(w, h),
                    "targets": targets, "fieldConfig": {"defaults": fc, "overrides": overrides or []},
                    "options": {"legend": {"displayMode": legend_mode, "placement": "bottom", "showLegend": True,
                                           "calcs": calcs or ["lastNotNull", "mean", "max"]},
                                "tooltip": {"mode": "multi", "sort": "desc"}}})


def bargauge(title, expr, unit, legend, desc="", w=8, h=7, th=None, maximum=None):
    fc = {"unit": unit, "thresholds": th or steps((None, "blue")), "min": 0}
    if maximum is not None:
        fc["max"] = maximum
    _panels.append({"id": _next_id(), "type": "bargauge", "title": title, "description": desc, "datasource": DS, "gridPos": place(w, h),
                    "targets": [tgt(expr, legend, instant=True)], "fieldConfig": {"defaults": fc, "overrides": []},
                    "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}, "orientation": "horizontal",
                                "displayMode": "gradient", "showUnfilled": True, "valueMode": "color"}})


def heatmap(title, expr, desc="", w=12, h=8):
    _panels.append({"id": _next_id(), "type": "heatmap", "title": title, "description": desc, "datasource": DS, "gridPos": place(w, h),
                    "targets": [tgt(expr, "{{le}}", fmt="heatmap")],
                    "options": {"calculate": False, "yAxis": {"axisPlacement": "left", "unit": "s"},
                                "color": {"mode": "scheme", "scheme": "Turbo", "fill": "dark-orange", "scale": "exponential", "exponent": 0.5,
                                          "steps": 64, "reverse": False},
                                "cellGap": 1, "tooltip": {"show": True, "yHistogram": True}, "legend": {"show": True},
                                "rowsFrame": {"layout": "auto"}},
                    "fieldConfig": {"defaults": {"custom": {"scaleDistribution": {"type": "linear"}}}, "overrides": []}})


def text(content, w=24, h=3):
    _panels.append({"id": _next_id(), "type": "text", "title": "", "gridPos": place(w, h),
                    "options": {"mode": "markdown", "content": content}, "transparent": True})


def color_override(name, color):
    return {"matcher": {"id": "byName", "options": name}, "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": color}}]}


def q(quantile, metric, extra="", by="le"):
    return f"histogram_quantile({quantile}, sum by ({by}) (rate({metric}_bucket{extra}[{R}])))"


HTTP = "orderbook_http_request_duration_seconds"
POST = '{route="POST /orders"}'

# ====================================================================================== 1. ESTADO GENERAL
text("**Cómo leer este panel.** Arriba: el estado del sistema de un vistazo (verde = bien, amarillo = atención, rojo = problema). "
     "Cada sección siguiente baja un nivel: tráfico → latencia → cola del secuenciador → persistencia → memoria → historial → idempotencia. "
     "Pase el cursor por el ⓘ de cada panel para ver qué mide y cómo interpretarlo. La **ventana de cálculo** de las tasas se elige arriba a la izquierda.", h=3)
row("1 · Estado general (de un vistazo)", "Indicadores clave: si todo está en verde, el sistema está sano.")
stat("Órdenes aceptadas / s", f'sum(rate(orderbook_orders_total{{result="accepted"}}[{R}]))', "reqps", steps((None, "blue")),
     "Órdenes que entraron al sistema y se confirmaron (201). Es el throughput útil.", decimals=0)
stat("Trades / s", f"rate(orderbook_trades_total[{R}])", "reqps", steps((None, "blue")), "Ejecuciones (cruces) por segundo.", decimals=0)
stat("p99 POST /orders", q(0.99, HTTP, POST), "s", steps((None, "green"), (0.1, "yellow"), (0.25, "red")),
     "El 99 % de las órdenes se responde en menos de este tiempo (visto por el servidor). Verde < 100 ms, rojo ≥ 250 ms.", decimals=1)
stat("Rechazos 503 / s", f'sum(rate(orderbook_http_requests_total{{code="503"}}[{R}])) or vector(0)', "reqps",
     steps((None, "green"), (0.01, "orange")), "Backpressure: la cola del secuenciador se llenó y se rechazó rápido. Distinto de cero = el sistema está saturado.", decimals=1)
stat("Errores 5xx / s", f'sum(rate(orderbook_http_requests_total{{code=~"5..",code!="503"}}[{R}])) or vector(0)', "reqps",
     steps((None, "green"), (0.001, "red")), "Errores del servidor distintos de 503 (incluye 504 por timeout). Debe ser siempre 0.", decimals=2)
stat("Ocupación de cola", "orderbook_queue_depth / orderbook_queue_capacity", "percentunit", steps((None, "green"), (0.5, "yellow"), (0.8, "red")),
     "Comandos esperando al secuenciador / capacidad. Cerca de 100 % empiezan los 503.", decimals=2)
stat("p50 POST /orders", q(0.5, HTTP, POST), "s", steps((None, "green"), (0.025, "yellow"), (0.1, "red")), "La mitad de las órdenes se responde en menos de esto.", decimals=1)
stat("p95 POST /orders", q(0.95, HTTP, POST), "s", steps((None, "green"), (0.05, "yellow"), (0.2, "red")), "El 95 % de las órdenes se responde en menos de esto.", decimals=1)
stat("Cancelaciones / s", f"sum(rate(orderbook_cancels_total[{R}])) or vector(0)", "reqps", steps((None, "blue")), "Cancelaciones de órdenes (exitosas o no).", decimals=1)
stat("Lag del historial", "orderbook_projector_lag", "short", steps((None, "green"), (10000, "yellow"), (100000, "red")),
     "Registros del journal que Postgres aún no refleja. Alto = el historial va atrasado (el matching NO se ve afectado).")
stat("Historial (Postgres)", "orderbook_projector_up", "none", steps((None, "red"), (1, "green")), "Si el proyector hacia Postgres está sano.",
     mappings=[{"type": "value", "options": {"0": {"text": "CAÍDO", "color": "red"}, "1": {"text": "OK", "color": "green"}}}], graph=False)
stat("Disco libre", "orderbook_journal_disk_free_bytes", "bytes", steps((None, "red"), (2 * 1024**3, "yellow"), (5 * 1024**3, "green")),
     "Espacio libre donde vive el journal. Con el disco lleno el servicio se detiene a propósito (fail-stop). Rojo < 2 GiB.", decimals=1)

# ====================================================================================== 2. TRÁFICO
row("2 · Tráfico de órdenes", "Qué entra, qué se acepta, qué se rechaza y cuánto se negocia.")
ts("Órdenes por segundo según resultado", [tgt(f"sum by (result) (rate(orderbook_orders_total[{R}]))", "{{result}}")], "reqps",
   "Cada línea es un destino posible de la orden: accepted (aceptada), duplicate (id repetido, 409), insufficient_funds (sin saldo, 422), invalid (datos inválidos, 400), "
   "timeout (504), overloaded (503).", stack=True, fill=35,
   overrides=[color_override("accepted", "green"), color_override("duplicate", "yellow"), color_override("insufficient_funds", "blue"),
              color_override("invalid", "orange"), color_override("timeout", "red"), color_override("overloaded", "dark-red")])
ts("Órdenes aceptadas: compra vs venta", [tgt(f'sum by (side) (rate(orderbook_orders_total{{result="accepted"}}[{R}]))', "{{side}}")], "reqps",
   "Equilibrio entre compras (BUY) y ventas (SELL) aceptadas.", stack=True, fill=35,
   overrides=[color_override("BUY", "green"), color_override("SELL", "red")])
ts("Trades y volumen negociado", [tgt(f"rate(orderbook_trades_total[{R}])", "trades / s", "A"),
                                  tgt(f"rate(orderbook_traded_units_total[{R}])", "unidades de VIB / s", "B"),
                                  tgt(f"rate(orderbook_traded_value_cents_total[{R}]) / 100", "valor negociado (R$ / s)", "C")], "short",
   "Ejecuciones por segundo, unidades de Vibranium negociadas por segundo y su valor en reales por segundo.", w=12)
ts("Respuestas HTTP por código", [tgt(f"sum by (code) (rate(orderbook_http_requests_total[{R}]))", "{{code}}")], "reqps",
   "Códigos de respuesta de la API pública: 201 orden creada, 200 consulta, 204 depósito, 4xx error del cliente, 503 sobrecarga, 504 timeout.", stack=True, fill=35,
   overrides=[color_override("201", "green"), color_override("200", "light-green"), color_override("204", "super-light-green"),
              color_override("400", "orange"), color_override("409", "yellow"), color_override("422", "blue"),
              color_override("503", "dark-red"), color_override("504", "red")])
bargauge("Peticiones por segundo, por ruta", f"sum by (route) (rate(orderbook_http_requests_total[{R}]))", "reqps", "{{route}}",
         "Qué endpoints se están usando y cuánto.", w=12, h=7)
ts("Cancelaciones por resultado", [tgt(f"sum by (result) (rate(orderbook_cancels_total[{R}]))", "{{result}}")], "reqps",
   "ok = cancelada; not_found = ya estaba ejecutada o no existía (404).", w=12, h=7, stack=True, fill=35)

# ====================================================================================== 3. LATENCIA
row("3 · Latencia (medida por el servidor)", "Cuánto tarda en responder la API. No incluye el tiempo previo a que el servidor reciba la petición.")
ts("Latencia de POST /orders por percentil", [tgt(q(0.5, HTTP, POST), "p50 (mediana)", "A"), tgt(q(0.9, HTTP, POST), "p90", "B"),
                                              tgt(q(0.95, HTTP, POST), "p95", "C"), tgt(q(0.99, HTTP, POST), "p99", "D"),
                                              tgt(q(0.999, HTTP, POST), "p99.9", "E")], "s",
   "p99 = el 99 % de las órdenes tarda menos que esto. La leyenda muestra el último valor, el promedio y el máximo del periodo.")
heatmap("Distribución de los tiempos de respuesta de POST /orders", f'sum by (le) (increase({HTTP}_bucket{POST}[{R}]))',
        "Cada columna es un instante y cada fila un rango de tiempo; el color es cuántas órdenes cayeron ahí. Una franja horizontal estrecha = latencia estable; "
        "manchas arriba = picos (GC, snapshot).")
ts("Latencia p95 por ruta", [tgt(q(0.95, HTTP, by="le, route"), "{{route}}")], "s", "Qué endpoint es el más lento.", w=12, h=9, fill=0)
ts("Desglose de la latencia (p99): cola · matching · fsync · HTTP", [
    tgt(q(0.99, "orderbook_queue_wait_seconds"), "espera en cola", "A"), tgt(q(0.99, "orderbook_batch_exec_seconds"), "matching + liquidación (por lote)", "B"),
    tgt(q(0.99, "orderbook_journal_fsync_seconds"), "fsync del journal (por lote)", "C"), tgt(q(0.99, HTTP, POST), "HTTP completo POST /orders", "D")], "s",
   "Dónde se va el tiempo. Si domina la espera en cola, el secuenciador está ocupado (snapshots, lecturas pesadas); si domina el fsync, el disco es lento.", w=12, h=9, log=True, minimum=None)

# ====================================================================================== 4. SECUENCIADOR
row("4 · Secuenciador y cola (camino caliente)", "Un solo hilo ejecuta las órdenes en orden; esta sección muestra qué tan cargado está.")
ts("Cola del secuenciador: profundidad", [tgt("orderbook_queue_depth", "comandos en cola", "A")], "short",
   "Comandos esperando al secuenciador. La capacidad es de 10.000 (QUEUE_SIZE): si se acerca, la API empieza a responder 503. Ver también «Ocupación de cola» arriba.", w=8, calcs=["lastNotNull", "mean", "max"])
ts("Rechazos y descartes por segundo", [tgt(f"rate(orderbook_queue_rejected_total[{R}])", "rechazadas por cola llena (503)", "A"),
                                        tgt(f"rate(orderbook_queue_expired_total[{R}])", "descartadas: el cliente ya no esperaba", "B")], "reqps",
   "Debe estar en 0 en operación normal.", w=8, calcs=["lastNotNull", "max"],
   overrides=[color_override("rechazadas por cola llena (503)", "red"), color_override("descartadas: el cliente ya no esperaba", "orange")])
ts("Tiempo de espera en cola", [tgt(q(0.5, "orderbook_queue_wait_seconds"), "p50", "A"), tgt(q(0.95, "orderbook_queue_wait_seconds"), "p95", "B"),
                                tgt(q(0.99, "orderbook_queue_wait_seconds"), "p99", "C")], "s",
   "Cuánto espera un comando antes de ejecutarse. Los picos suelen coincidir con la pausa de un snapshot.", w=8)
ts("Group commit: comandos agrupados por cada fsync", [tgt(f"rate(orderbook_commands_processed_total[{R}]) / rate(orderbook_journal_syncs_total[{R}])", "promedio", "A"),
                                                       tgt(q(0.99, "orderbook_batch_size"), "p99 del tamaño de lote", "B")], "short",
   "Cuántas órdenes comparten un mismo fsync. Con un disco real, más carga = lotes más grandes y menos fsync por orden.", w=8)
ts("Duración del matching y del fsync por lote", [tgt(q(0.5, "orderbook_batch_exec_seconds"), "matching p50", "A"), tgt(q(0.99, "orderbook_batch_exec_seconds"), "matching p99", "B"),
                                                   tgt(q(0.5, "orderbook_journal_fsync_seconds"), "fsync p50", "C"), tgt(q(0.99, "orderbook_journal_fsync_seconds"), "fsync p99", "D")], "s",
   "El matching cuesta microsegundos; el fsync depende del disco.", w=8)
ts("Peticiones HTTP en curso", [tgt("orderbook_http_in_flight_requests", "en curso", "A")], "short", "Conexiones siendo atendidas ahora mismo.", w=8, calcs=["lastNotNull", "mean", "max"])

# ====================================================================================== 5. PERSISTENCIA
row("5 · Persistencia: journal y snapshots", "El journal es la fuente de verdad; los snapshots aceleran el arranque.")
ts("Crecimiento del journal", [tgt(f"rate(orderbook_journal_last_seq[{R}])", "registros por segundo", "A"),
                               tgt(f"rate(orderbook_journal_bytes[{R}]) / 1024 / 1024", "MiB por segundo", "B")], "short",
   "Ritmo al que se escribe el journal.", w=8)
ts("Tamaño del journal y espacio libre", [tgt("orderbook_journal_bytes", "tamaño del journal", "A"), tgt("orderbook_journal_disk_free_bytes", "espacio libre en disco", "B")], "bytes",
   "Si el espacio libre llega a 0, el servidor se detiene a propósito (fail-stop). Alertas: < 2 GiB o se llena en menos de 6 h.", w=8,
   thresholds=steps((None, "red"), (2 * 1024**3, "transparent")), calcs=["lastNotNull", "min"])
ts("fsync por segundo", [tgt(f"rate(orderbook_journal_syncs_total[{R}])", "fsync / s", "A")], "ops", "Cuántas veces por segundo se sincroniza el journal a disco.", w=8)
ts("Snapshots: antigüedad (registros por reproducir tras un crash)", [tgt("orderbook_snapshot_age_records", "registros desde el último snapshot", "A")], "short",
   "Cuánto habría que reproducir si el proceso muriera ahora (≈ 9 s por millón). Se reinicia a 0 con cada snapshot (cada 1.000.000 de registros).", w=8,
   thresholds=steps((None, "transparent"), (1000000, "orange")))
ts("Snapshots: pausa del secuenciador y tiempo de escritura", [tgt("orderbook_snapshot_pause_seconds", "pausa del secuenciador (copia del estado)", "A"),
                                                              tgt("orderbook_snapshot_write_seconds", "escritura a disco (en segundo plano)", "B")], "s",
   "La pausa detiene el matching mientras se copia el estado: es la principal causa de picos de latencia con estado grande.", w=8,
   overrides=[color_override("pausa del secuenciador (copia del estado)", "red")])
stat("Último reinicio: tiempo de recuperación", "orderbook_recovery_seconds", "s", steps((None, "green"), (5, "yellow"), (30, "red")),
     "Cuánto tardó en recuperarse el estado al arrancar (snapshot + cola del journal).", w=4, h=8, decimals=1, graph=False)
stat("Último reinicio: registros reproducidos", "orderbook_recovery_replayed_records", "short", steps((None, "blue")),
     "Registros del journal reproducidos al arrancar (0 si el snapshot estaba al día).", w=4, h=8, graph=False)

# ====================================================================================== 6. RUNTIME
row("6 · Memoria y recolector de basura (Go)", "Qué tan sano está el proceso por dentro.")
ts("Memoria: heap en uso y RSS", [tgt("go_memstats_heap_inuse_bytes", "heap en uso (lo que usa el programa)", "A"), tgt("process_resident_memory_bytes", "RSS (lo que el SO le asigna)", "B"),
                                  tgt("go_memstats_heap_idle_bytes - go_memstats_heap_released_bytes", "heap libre aún no devuelto al SO", "C")], "bytes",
   "RSS ≈ heap reservado − lo devuelto al SO. Crece con el libro de órdenes y la ventana de idempotencia; el contenedor tiene un tope (mem_limit).", w=8)
ts("Pausa del recolector de basura (GC)", [tgt('go_gc_duration_seconds{quantile="1"}', "pausa máxima reciente (stop-the-world)", "A")], "s",
   "Cuánto detuvo el GC al programa en su peor ciclo reciente. Debe ser de milisegundos.", w=8, calcs=["lastNotNull", "mean", "max"])
ts("Ciclos de GC y goroutines", [tgt(f"rate(go_gc_duration_seconds_count[{R}])", "ciclos de GC por segundo", "A"), tgt("go_goroutines", "goroutines", "B")], "short",
   "Frecuencia del recolector (más basura = más ciclos) y cantidad de goroutines.", w=8)

# ====================================================================================== 7. HISTORIAL
row("7 · Historial en PostgreSQL (proyector)", "Copia consultable del journal. Si Postgres cae, el matching sigue funcionando.")
ts("Lag del historial", [tgt("orderbook_projector_lag", "registros pendientes de reflejar en Postgres", "A")], "short",
   "Debe tender a 0 cuando baja la carga. Si sube sin parar, Postgres no da abasto o está caído.", w=8, thresholds=steps((None, "transparent"), (100000, "red")))
ts("Journal vs historial", [tgt("orderbook_journal_last_seq", "último registro del journal", "A"), tgt("orderbook_projector_seq", "último registro reflejado en Postgres", "B")], "short",
   "La distancia entre ambas líneas es el lag.", w=8, calcs=["lastNotNull"])
ts("Errores del proyector y relecturas", [tgt(f"rate(orderbook_projector_failures_total[{R}])", "errores por segundo", "A"),
                                          tgt("orderbook_projector_rescanned_records_total", "registros releídos al arrancar (acumulado)", "B")], "short",
   "Errores = Postgres caído o lento. Relecturas: 1 si reanudó por offset; ≈ todo el journal si tuvo que leerlo completo.", w=8, calcs=["lastNotNull", "max"])

# ====================================================================================== 8. IDEMPOTENCIA
row("8 · Idempotencia (ventana de ids recordados)", "Evita duplicar una orden si el cliente reintenta con el mismo id.")
ts("Ventana de idempotencia", [tgt("orderbook_idempotency_window_size", "ids recordados", "A"), tgt("orderbook_idempotency_window_limit", "límite configurado", "B")], "short",
   "Cuántos ids de órdenes recientes se recuerdan. Al llegar al límite, los más antiguos se olvidan.", w=12, calcs=["lastNotNull", "max"])
ts("Ids olvidados por segundo", [tgt(f"rate(orderbook_idempotency_evicted_total[{R}])", "ids expulsados / s", "A")], "ops",
   "0 mientras la ventana no está llena; luego igual al ritmo de órdenes.", w=12)

DASH = {
    "uid": "orderbook-overview",
    "title": "Vibranium Order Book — Panel de control",
    "tags": ["orderbook", "vibranium"],
    "timezone": "browser", "schemaVersion": 39, "version": 2, "refresh": "5s", "editable": True,
    "time": {"from": "now-15m", "to": "now"},
    "graphTooltip": 1,
    "annotations": {"list": []},
    "links": [],
    "templating": {"list": [
        {"name": "rate", "label": "Ventana de cálculo", "type": "custom", "query": "30s,1m,5m", "current": {"selected": True, "text": "1m", "value": "1m"},
         "options": [{"selected": False, "text": "30s", "value": "30s"}, {"selected": True, "text": "1m", "value": "1m"}, {"selected": False, "text": "5m", "value": "5m"}],
         "hide": 0, "description": "Ventana sobre la que se calculan las tasas por segundo y los percentiles"}]},
    "panels": _panels,
}

if __name__ == "__main__":
    OUT.write_text(json.dumps(DASH, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    print(f"escrito {OUT} con {sum(1 for p in _panels if p['type'] != 'row' and p['type'] != 'text')} paneles en {sum(1 for p in _panels if p['type'] == 'row')} secciones")
