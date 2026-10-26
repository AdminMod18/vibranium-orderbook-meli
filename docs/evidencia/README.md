# Evidencia de la fase de optimización

Registros crudos de las mediciones citadas en `docs/OPTIMIZACION.md`. Se conservan **antes** y **después** de los cambios.

| Archivo | Qué es |
|---|---|
| `antes/soak_run2.log`, `antes/soak_run3.log` | Soak original (12 min con estado grande; 14 min desde cero con Postgres activo) |
| `antes/e1_soak.log`, `antes/e1_gctrace.log`, `antes/e1_memoria.log` | 8 min con `GODEBUG=gctrace=1` y muestreo de memoria cada 5 s, código anterior |
| `antes/e2a.*` | Reinicio en caliente bajo carga (5.000/s) con el re-escaneo del journal por el proyector: tiempos, carga y CPU |
| `despues/e2b.*` | Mismo experimento tras reanudar el proyector por offset |
| `despues/e3_*` | 7 min equivalentes a e1 con el código nuevo (gctrace y memoria). Cortado por falta de disco, ver el informe |
| `despues/e5_soak10.log`, `latency_breakdown_e5.txt` | 10 min desde cero con Postgres activo y la descomposición de latencia por etapa |
| `despues/ab_snapshots.log` | A/B de snapshots: cada 200.000 / sin snapshots / cada 1.000.000 |
| `despues/verify_phase3b.log` | `verify-all` completo: 14/14 PASS |

Entorno de todas las corridas: sandbox de 4 núcleos compartidos con generador, Postgres, Prometheus y Grafana; disco virtual (fsync casi instantáneo).
