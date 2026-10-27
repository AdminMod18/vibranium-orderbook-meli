#!/usr/bin/env python3
"""Genera docs/img/arquitectura-app.svg: el camino de una orden dentro del proceso (un solo escritor).

    python3 docs/img/src/make_arquitectura_app.py
    node docs/img/src/render.js docs/img/arquitectura-app.svg      # opcional: PNG (Playwright)

Solo biblioteca estándar. El SVG es el entregable; el script permite revisar el diagrama como código.
"""
from pathlib import Path

OUT = Path(__file__).resolve().parent.parent / "arquitectura-app.svg"
W, H = 1900, 800
BLUE, GREEN, AMBER, GRAY, INK, RED = "#2563eb", "#059669", "#d97706", "#64748b", "#0f172a", "#dc2626"
F = "Inter, 'Segoe UI', Roboto, Arial, sans-serif"
o = []


def add(s):
    o.append(s)


def rect(x, y, w, h, fill="#fff", stroke=GRAY, sw=1.5, rx=12, dash=None, extra=""):
    d = f' stroke-dasharray="{dash}"' if dash else ""
    add(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{rx}" fill="{fill}" stroke="{stroke}" stroke-width="{sw}"{d} {extra}/>')


def text(x, y, s, size=15, weight=400, fill=INK, anchor="start"):
    s = s.replace("&", "&amp;").replace("<", "&lt;")
    add(f'<text x="{x}" y="{y}" font-size="{size}" font-weight="{weight}" fill="{fill}" text-anchor="{anchor}">{s}</text>')


def box(x, y, w, h, title, lines=(), color=GRAY, fill="#fff", tsize=16):
    rect(x, y, w, h, fill=fill, stroke=color, sw=2)
    text(x + 14, y + 26, title, tsize, 700, color if color != GRAY else INK)
    for i, ln in enumerate(lines):
        text(x + 14, y + 50 + i * 21, ln, 14, 400, "#334155")


def pvc(x, y, w, h, title, sub):
    rect(x, y, w, h, fill="#ecfdf5", stroke=GREEN, sw=2, rx=10)
    add(f'<ellipse cx="{x+24}" cy="{y+h/2}" rx="11" ry="14" fill="#fff" stroke="{GREEN}" stroke-width="2"/>')
    text(x + 46, y + h / 2 - 3, title, 14.5, 700, GREEN)
    text(x + 46, y + h / 2 + 16, sub, 12.5, 400, "#065f46")


def arrow(pts, color, dash=None, label=None, lx=None, ly=None, sw=2.6, np=False):
    d = "M " + " L ".join(f"{x},{y}" for x, y in pts)
    da = f' stroke-dasharray="{dash}"' if dash else ""
    add(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="{sw}"{da} marker-end="url(#a-{color[1:]})"/>')
    if label:
        w = len(label) * 7.0 + 18
        rect(lx - 4, ly - 17, w, 24, fill="#ffffff", stroke="none", rx=6, extra='opacity="0.92"')
        text(lx + 5, ly, label, 13, 600, color)
    if np:
        mx, my = lx, ly + 20
        rect(mx - 4, my - 13, 30, 19, fill="#eef2ff", stroke="#6366f1", sw=1.2, rx=9)
        text(mx + 11, my + 1, "NP", 11, 700, "#4338ca", "middle")


def num(x, y, n, color=BLUE):
    add(f'<circle cx="{x}" cy="{y}" r="14" fill="{color}"/>')
    text(x, y + 5, str(n), 15, 800, "#fff", "middle")



add(f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" font-family="{F}">')
add("<defs>" + "".join(
    f'<marker id="a-{c[1:]}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="9" markerHeight="9" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>'
    for c in (BLUE, GREEN, AMBER, GRAY, RED)) + "</defs>")
rect(0, 0, W, H, fill="#ffffff", stroke="none", rx=0)
text(40, 54, "Vibranium Order Book · Camino de una orden", 30, 800)
text(40, 84, "Dentro del proceso: un solo escritor ordena todo, y solo confirma lo que ya está en disco. Es lo que corre en el pod orderbook-0 (o en docker compose).", 15.5, 400, "#475569")

box(40, 290, 220, 130, "Clientes", ["JMeter · robots · curl", "miles de órdenes a la vez"], BLUE, "#eff6ff")

rect(290, 120, 1330, 640, fill="#f8fafc", stroke="#94a3b8", sw=2.5, rx=18)
text(312, 152, "Proceso orderbook · un solo binario Go · 1 réplica (un solo escritor por diseño)", 17, 700, "#334155")

box(320, 220, 230, 230, "API HTTP :8080", ["valida JSON y ids", "ventana de idempotencia", "(5 M ids; repetido → 409)", "sin saldo → 422", "cancelar · consultar libro"], BLUE, "#eff6ff")
box(600, 255, 170, 160, "Cola acotada", ["10 000 comandos", "", "si se llena:", "503 (backpressure)"], BLUE, "#eff6ff")
rect(830, 200, 470, 270, fill="#eff6ff", stroke=BLUE, sw=2)
text(846, 228, "Secuenciador · UN solo escritor", 17, 700, BLUE)
text(846, 252, "toma de a un comando: orden total, sin concurrencia", 13, 400, "#1d4ed8")
box(848, 268, 205, 140, "Libro de ofertas", ["heap de precios + FIFO", "ejecuta al precio del", "maker (el que esperaba)", "máx. 10 000 trades/orden"], BLUE, "#fff", 15)
box(1073, 268, 205, 140, "Billeteras", ["reserva al ingresar:", "saldo libre vs reservado", "conservación verificable", "(/audit)"], BLUE, "#fff", 15)
text(846, 438, "group commit: hasta 1 024 comandos por lote", 13.5, 600, "#1d4ed8")
box(1360, 230, 220, 200, "Journal (disco)", ["append [largo|crc32c|JSON]", "UN fsync por lote", "recién entonces responde", "si falla: fail-stop", "(no confirma lo no persistido)"], GREEN, "#f0fdf4")
pvc(1360, 462, 220, 62, "Volumen persistente", "fuente de verdad")
box(1360, 570, 220, 120, "Proyector asíncrono", ["lee solo lo ya durable", "reanuda por offset"], GREEN, "#f0fdf4")
box(1650, 570, 220, 120, "PostgreSQL", ["historial y trazabilidad", "(órdenes, trades, mayor)"], GREEN, "#f0fdf4")

box(830, 520, 270, 120, "Snapshots", ["cada 1 M de registros", "aceleran el arranque", "pausan al escritor ≈ 1-2 s"], GRAY, "#f1f5f9")
box(320, 520, 270, 120, "Administración :9102", ["/metrics → Prometheus", "pprof · /healthz · /readyz"], AMBER, "#fffbeb")

# flujo de la orden (azul)
arrow([(260, 320), (320, 320)], BLUE); num(290, 296, 1)
arrow([(550, 335), (600, 335)], BLUE); num(575, 311, 2)
arrow([(770, 335), (830, 335)], BLUE); num(800, 311, 3)
arrow([(1300, 335), (1360, 335)], GREEN); num(1330, 311, 4)
arrow([(320, 395), (260, 395)], BLUE, label="201", lx=272, ly=384); num(290, 421, 5)
# persistencia (verde)
arrow([(1470, 430), (1470, 462)], GREEN)
arrow([(1470, 524), (1470, 570)], GREEN, label="lee", lx=1480, ly=552)
arrow([(1580, 630), (1650, 630)], GREEN); num(1615, 606, 6)
# snapshot / admin
arrow([(960, 470), (960, 520)], GRAY, dash="7 5")
add(f'<path d="M 590 580 L 320 580" stroke="none"/>')

text(312, 722, "Arranque: snapshot (si sirve) + cola del journal; cada trade reproducido se verifica contra el registrado. El journal manda: el snapshot es solo una optimización.", 13.5, 500, "#334155")
text(312, 744, "① entra · ② se valida y se encola · ③ se ordena y empareja · ④ se persiste (UN fsync) · ⑤ recién entonces se responde · ⑥ el historial se pone al día sin bloquear al escritor.", 13.5, 600, "#334155")

add("</svg>")
OUT.write_text("\n".join(o), encoding="utf-8")
print("escrito", OUT)
