# Informe de pruebas

> Todo lo de este informe se reproduce con **un solo comando**: `./scripts/verify-all.sh` (≈ 2-4 min; reinicia el stack desde cero).
> Termina con una tabla PASS/FAIL y no deja fallar nada en silencio.

## 1. Qué se prueba y cómo

| Capa | Qué demuestra | Cómo se ejecuta |
|---|---|---|
| **45 tests de Go** con el detector de carreras (`-race`) | Reglas del libro (precio-tiempo, parciales, precio del maker), reservas, saldos, idempotencia, concurrencia (100 goroutines), journal (cola truncada, CRC, corrupción real), recuperación, snapshots, API y métricas | `go test -race ./...` |
| **Pruebas aleatorias de invariantes** | 20.000 órdenes con cancelaciones: el libro nunca queda cruzado, se conserva el valor, cada trade respeta los límites de precio | dentro de `go test` |
| **Integración con Postgres real** | El historial reproduce al motor; el saldo recalculado desde depósitos+trades == billetera; reinicio del proyector sin duplicados; Postgres caído; journal ajeno rechazado | `TEST_DATABASE_URL=… go test ./internal/history` |
| **Matriz de recuperación** | Caída sin apagado ordenado, apagado ordenado, reinicios encadenados y 6 snapshots inservibles (corrupto, truncado, de otro journal, offset inválido, offset fuera de rango, estado inconsistente): **todos llegan al mismo estado** | `go test ./internal/recovery` |
| **Smoke end-to-end** (22 comprobaciones) | El ciclo completo contra el sistema levantado: cruces, parciales, cancelación, idempotencia, saldo insuficiente, auditoría, historial, métricas, Prometheus y Grafana | `./scripts/smoke.sh` |
| **Carga** | 5.000 órdenes/s con generador *open-loop* y auditoría de saldos al final | `docker compose run --rm loadgen -rate 5000` |
| **4 drills de fallos** | Ver §3 | `./scripts/drill-*.sh` |

Cobertura de sentencias por paquete (cada uno con sus propios tests): exchange 87,5 %, engine 85,6 %, recovery 84,6 %, sequencer 76,1 %, api 61,1 %, journal 53,1 %, snapshot 43,2 %. *(La restauración del libro vive en `engine` pero se prueba desde `exchange` y `recovery`, y Go no cuenta la cobertura entre paquetes; `history` marca 16 % porque sus tests exigen Postgres y se omiten sin `TEST_DATABASE_URL`.)*

## 2. Capacidad

Entorno: **máquina de 4 núcleos compartida** entre generador, app, Postgres, Prometheus, Grafana y Docker. Disco virtual (fsync muy rápido). Todas las órdenes aceptadas y los saldos auditados en cada escalón.

| Objetivo | Efectivas | p50 | p99 | p99,9 | Cola máx. | CPU app / Postgres / generador |
|---|---|---|---|---|---|---|
| **5.000/s** (el requisito) | 5.000 | 1,2 ms | **16 ms** | 39 ms | – | 1,3 / 0,7 / 0,65 núcleos |
| 8.000/s | 8.000 | 1,5 ms | 146-200 ms | 218-288 ms | 16 | 1,3 / 1,0 / 0,8 |
| 12.000/s | 11.991 | 2,8 ms | 199 ms | 246 ms | 16 | 1,5 / 0,9 / 1,0 |
| 16.000/s | 15.995 | 9,1 ms | 140 ms | 163 ms | 22 | 1,5 / 0,8 / 1,0 |

**Perfil de CPU de la app a 8.000/s** (13,3 s de CPU en 12 s): matching+billeteras **2,3 %** · journal (escritura/fsync) 14 % · JSON 13 % · HTTP/red 33 % · GC 14 %.

**Lectura honesta**
- El sistema sostuvo **3,2× el requisito** sin rechazar ni perder una orden. Con 16.000/s la cola del secuenciador llegó a 22 de 50.000: **el límite fue el CPU de la máquina, no el diseño**; en producción API, Postgres y generador irían en equipos distintos.
- La latencia de cola (p99) sube desde ~8.000/s por saturación de CPU. Con 5.000/s en condiciones limpias fue de 16-29 ms.
- **Las colas largas aparecen con otra actividad pesada simultánea** (navegador para capturas, parada/arranque de contenedores): p99 252 ms y máximo 825 ms. Es compatible con contención del entorno pero **no está demostrado**; hay que repetirlo en la máquina de la presentación.
- Con 5.000/s el *group commit* agrupó pocos comandos por fsync (≈1-4) porque este disco virtual hace fsync casi instantáneo; en un SSD/EBS real el fsync cuesta milisegundos y el agrupamiento rinde mucho más. **No está medido en disco real.**

## 3. Resiliencia

| Drill | Qué se rompe | Qué se verifica | Resultado |
|---|---|---|---|
| `drill-kill9.sh` | `kill -9` al servidor en plena carga | Tras reiniciar: invariantes `ok` y **totales idénticos** | ✅ (10 repeticiones bajo CPU cargada) |
| `drill-docker-crash.sh` | `kill -9` al proceso de la app dentro del compose | Docker la reinicia solo; recupera del volumen; saldos intactos | ✅ (snapshot + cola: 110 ms) |
| `drill-postgres-down.sh` | Se detiene Postgres en pleno uso | El matching sigue (201), el historial responde 503, y al volver se reconcilia hasta `lag=0` y coincide con el motor | ✅ |
| `drill-overload.sh` | Cola de 8 posiciones + 512 clientes a máxima velocidad | **307.282 rechazos 503** y 50.601 aceptadas; saldos `ok`; el reinicio recupera todo lo aceptado | ✅ (4 repeticiones) |

**Tiempo de recuperación** (mismo journal de 816.904 registros): sin snapshot **7,06 s** · con snapshot tras crash **1,10 s** · tras apagado ordenado **1,02 s**.

## 4. Hallazgos durante las pruebas (lo que las pruebas encontraron de verdad)

Cada ítem es un defecto o una creencia errónea que **una prueba descubrió** y que quedó corregida y cubierta.

1. **El proyector se saltaba registros en silencio.** Un Postgres con `last_seq=10012` de otra corrida + un journal nuevo ⇒ ignoraba los primeros 10.012 registros. *Corregido:* registro `genesis` con UUID que ata journal e historial; ante un desajuste el proyector se detiene y avisa (D38).
2. **`docker kill` no activa la política de reinicio.** Docker lo trata como parada manual; parecía que la app "no se recuperaba". Para simular un fallo real se mata el *proceso*; el drill lo hace donde es seguro y avisa donde no (Docker Desktop).
3. **El drill de Postgres traía valores por defecto rotos** (SQL entre comillas partido por el shell). Solo se vio al probarlo con Docker real.
4. **Medí mal el tiempo de recuperación.** Había documentado "1-3 s por millón"; medido fue **7-9 s por millón**: el reinicio crecía sin límite. *Corregido:* snapshots (D51) y la cifra errónea reemplazada en toda la documentación.
5. **Falta de una invariante de conservación.** Un snapshot con +777 reales de la nada **no se detectaba**. *Corregido:* suma de saldos == total depositado, verificada en `/audit` y al restaurar (D56).
6. **Los drills fallaban en silencio** tras renombrar un mensaje de log (`grep` + `set -e`). Motivó `verify-all.sh`, que informa cada paso.
7. **Un drill intermitente** (3 de 6 fallos con la máquina cargada): el servidor escribía su snapshot final al apagarse mientras el script borraba el directorio. *Corregido* esperando al proceso antes de limpiar; **10/10 y 4/4** después.
8. **La demo y el smoke exigen un libro vacío** (si no, los cruces no son los esperados tras una prueba de carga): ahora lo comprueban, avisan cómo reiniciar y se limpian solos.

## 5. Limitaciones (lo que NO se probó o no es válido)

- **Windows/Git Bash y Docker Desktop no se pudieron probar** (el desarrollo se hizo en Linux). Los scripts contemplan `.exe`, CRLF y PIDs ajenos, pero requieren que se ejecute `./scripts/preflight.sh` allí.
- **El `Dockerfile` completo no se ejecutó de punta a punta** aquí (los contenedores de build no tenían salida a internet). Se verificó la compilación dentro de `golang:1.24` y el resto de la imagen por separado. Correr `docker compose up --build` en una máquina con internet.
- **Una sola instancia y un solo disco**: no hay réplica en espera (L3, L6); un fallo del disco pierde el journal.
- **Estado sin límite** (ids vistos, journal): L12, L13 en `DECISIONES.md`.
- Cifras de rendimiento medidas en entorno compartido y con disco virtual (L10).

## 6. Reproducir

```bash
./scripts/preflight.sh        # ¿tengo lo necesario?
./scripts/verify-all.sh       # todo: tests + stack + smoke + carga + 4 drills (tabla PASS/FAIL)
./scripts/demo.sh             # guion interactivo para la presentación
```
