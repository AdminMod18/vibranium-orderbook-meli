# Resumen de las corridas de JMeter (generado por `python3 jmeter/make_report.py md --config jmeter/report/runs.json`)

| Corrida | Carga | Órdenes OK | órd/s media | ventanas 5 s ≥ 4.900 órd/s | p50 | p90 | p95 | p99 | p99.9 | máx (ms) | Errores |
|---|---|---|---|---|---|---|---|---|---|---|---|
| ANTES · 10 min · PG | 600 s | 2,554,132 | **4,256** | 4 % | 8 | 25 | 33 | 57 | 207 | 718 | 0 |
| DESPUÉS C1–C5 · 10 min · PG | 600 s | 2,573,775 | **4,288** | 3 % | 8 | 23 | 29 | 48 | 109 | 710 | 0 |
| DESPUÉS + C6 · 10 min · PG | 600 s | 2,579,966 | **4,300** | 5 % | 6 | 20 | 27 | 45 | 108 | 544 | 0 |
| GRADUAL · 7 min · PG | 420 s | 1,715,567 | **4,083** | 3 % | 6 | 17 | 23 | 39 | 85 | 207 | 0 |
| ESTRÉS · 5 min · PG | 301 s | 1,345,820 | **4,478** | 19 % | 7 | 24 | 34 | 58 | 131 | 267 | 0 |
| 30 MIN · sin PG · antes de C6 | 1800 s | 8,693,172 | **4,829** | 42 % | 5 | 28 | 39 | 66 | 110 | 1388 | 0 |
| REINICIO · ANTES | 101 s | 416,650 | **4,141** | 12 % | 8 | 24 | 31 | 62 | 297 | 925 | 0 |
| REINICIO · DESPUÉS | 100 s | 419,664 | **4,186** | 0 % | 8 | 23 | 30 | 49 | 100 | 194 | 0 |
| 15 MIN · diag · antes de C6 | 900 s | 4,523,109 | **5,025** | 63 % | 4 | 18 | 25 | 44 | 77 | 1344 | 0 |
| 30 MIN · sin PG · con C6 | 1800 s | 9,603,174 | **5,334** | 82 % | 3 | 10 | 15 | 30 | 65 | 1556 | 0 |

| Corrida | RSS máx (MiB) | heap máx (MiB) | pausa GC máx (ms) | pausa snapshot máx (ms) | cola máx | cola p99 mediana / máx (ms) | matching p99 mediana / máx (ms) | fsync p99 mediana / máx (ms) |
|---|---|---|---|---|---|---|---|---|
| ANTES · 10 min · PG | 1,880 | 1,616 | 675 | 309 | 111 | 24 / 48 | 5 / 17 | 9 / 14 |
| DESPUÉS C1–C5 · 10 min · PG | 774 | 660 | 13 | 685 | 128 | 22 / 43 | 5 / 13 | 8 / 10 |
| DESPUÉS + C6 · 10 min · PG | 776 | 742 | 15 | 520 | 121 | 21 / 34 | 1 / 2 | 8 / 9 |
| GRADUAL · 7 min · PG | 476 | 419 | 9 | 64 | 61 | 18 / 25 | 3 / 8 | 7 / 9 |
| ESTRÉS · 5 min · PG | 417 | 346 | 23 | 44 | 220 | 23 / 41 | 2 / 5 | 8 / 9 |
| 30 MIN · sin PG · antes de C6 | 1,955 | 1,772 | 8 | 1,333 | 127 | 43 / 91 | 16 / 37 | 4 / 8 |
| REINICIO · ANTES | 1,219 | 874 | 3 | 629 | 87 | 24 / 44 | 7 / 7 | 9 / 17 |
| REINICIO · DESPUÉS | 753 | 679 | 7 | 0 | 95 | 24 / 27 | 8 / 8 | 8 / 9 |
| 15 MIN · diag · antes de C6 | 1,324 | 1,231 | 10 | 1,309 | 128 | 22 / 48 | 7 / 20 | 3 / 8 |
| 30 MIN · sin PG · con C6 | 2,503 | 2,308 | 8 | 1,524 | 128 | 9 / 23 | 1 / 1 | 3 / 7 |

