# Pruebas de rendimiento e integridad con Apache JMeter

Este directorio contiene todo lo necesario para **reproducir** las pruebas de carga e integridad con JMeter y para generar el informe
visual. Está pensado para ejecutarse **solo contra un entorno de pruebas exclusivo** (típicamente `docker compose up -d` en su máquina).

| Archivo | Qué es |
|---|---|
| `vibranium-integrity.jmx` | **Integridad financiera**: 1 hilo, determinista, 25 pasos con importes exactos (27 con el historial de Postgres), con elección automática de una banda de precios aislada (no es una prueba de carga). |
| `vibranium-load.jmx` | **Carga**: línea base → fondeo → carga parametrizable → auditoría de conservación de valor. |
| `generate_plans.py` | Genera los dos `.jmx` (el XML no se edita a mano; la lógica de validación está en Groovy legible dentro de este archivo). |
| `run-jmeter.sh` | Ejecuta un escenario, guarda los resultados en `jmeter/results/<fecha>-<escenario>/` y los resume. |
| `export_prometheus.py` | Exporta a CSV las métricas **del motor** (Prometheus) del periodo de una corrida. |
| `make_report.py` | `summarize` (resume un `.jtl`) y `html` (genera el informe `jmeter/report/index.html`). |
| `report/` | Informe HTML y su configuración (`runs.json`). |
| `results/` | Un directorio por corrida: `params.txt`, `meta.json`, `summary.json`, `timeseries.csv`, `histogram.csv`, `errors.csv`, `labels.csv`, `prometheus.csv`. |

## Requisitos
- **Java 17+** y **Apache JMeter 5.6.x** (probado con 5.6.3). Sin plugins: solo componentes estándar.
  Descarga: <https://jmeter.apache.org/download_jmeter.cgi> (verifique el SHA-512). Defina `JMETER_HOME` o deje `jmeter` en el `PATH`.
- **Python 3.9+** (solo biblioteca estándar) para resumir resultados y generar el informe.
- La API en marcha: `docker compose up -d` (por defecto `http://localhost:8080`). Para correlacionar con el motor hace falta Prometheus (lo levanta el compose en `:9090`).

## Ejecutar desde la interfaz gráfica de JMeter (recomendado para presentar)
1. Abra JMeter (`bin/jmeter.bat` en Windows, `bin/jmeter` en Linux/macOS) y cargue **`jmeter/vibranium-load.jmx`** (File → Open).
2. Haga clic en el nodo raíz **«Vibranium Order Book - CARGA»**: ahí está la tabla **User Defined Variables** con todos los parámetros. Edite el valor y listo:
   - **`duration`**: duración de la carga en **segundos** (`600` = 10 min, `1800` = 30 min, `3600` = 1 h…).
   - `threads` (usuarios concurrentes), `rampup` (segundos en subir de 0 a `threads`), `tps` (órdenes/s objetivo; `0` = sin tope), `users` (usuarios ficticios que se fondean).
   - `host` / `port` / `protocol` (servidor bajo prueba; por defecto `localhost:8080`), `mid_price` / `spread` / `cancel_pct`, `with_history` (`true` si Postgres está activo).
3. Para ver números en vivo, active los receptores **Summary Report** y **Aggregate Report** (clic derecho → *Enable*; vienen deshabilitados porque en corridas largas gastan memoria).
4. Pulse **Start** (▶). Para detener antes de tiempo: **Stop** (■). Al terminar, el paso final comprueba la auditoría (`GET /audit`) y la conservación de valor; si falla, aparece en rojo.
Para la integridad financiera abra `jmeter/vibranium-integrity.jmx` y pulse Start (es de un solo hilo y dura segundos).
Cualquiera de esos valores también se puede pasar por consola: `-Jduration=1800` tiene prioridad sobre lo que diga la tabla.

> La **interfaz gráfica es para presentar y para pruebas cortas**: JMeter recomienda correr las cargas largas en modo consola (más abajo), porque la GUI consume CPU y puede distorsionar la medición.

## Ejecutar por consola
```bash
docker compose up -d
./jmeter/run-jmeter.sh integridad     # ~3 s. Debe terminar con "errores totales = 0"; si no, sale con código 3.
./jmeter/run-jmeter.sh humo           # 30 s a 500 órd/s: ¿responde todo?
./jmeter/run-jmeter.sh gradual        # carga gradual: rampa de 0 a 128 hilos en 5 min con tope de 5.000 órd/s (7 min en total)
./jmeter/run-jmeter.sh sostenida      # carga sostenida: 10 min a 5.000 órd/s con 128 hilos
DURATION=1800 ./jmeter/run-jmeter.sh sostenida   # 30 minutos (necesita ≈ 4 GB libres de disco)
./jmeter/run-jmeter.sh estres         # estrés: 256 hilos SIN tope de tasa durante 5 min (busca el máximo que aguanta el entorno)
```
Variables opcionales (todas por entorno, nada sensible en los planes): `HOST` `PORT` `PROTOCOL` · `THREADS` `RAMPUP` `DURATION` `TPS` `USERS` ·
`MID_PRICE` `SPREAD` `CANCEL_PCT` · `WITH_HISTORY=true` (añade consultas al historial y la verificación contra Postgres) ·
`LABEL=etiqueta` · `KEEP_JTL=0` (borra el `.jtl` crudo tras resumirlo: pesa ≈ 60 B por muestra) · `DASHBOARD=1` (genera además el dashboard estándar de JMeter) ·
`MIN_FREE_GB=2` (guarda de disco) · `PROM=host:puerto`. También puede pasar propiedades directamente: `jmeter -n -t jmeter/vibranium-load.jmx -Jhost=... -Jthreads=... -Jtps=...`.

| Propiedad `-J` | Defecto | Significado |
|---|---|---|
| `host` · `port` · `protocol` | `localhost` · `8080` · `http` | servidor bajo prueba |
| `threads` · `rampup` · `duration` | 128 · 30 s · 60 s | usuarios concurrentes, rampa y duración de la carga |
| `tps` | 0 | tasa objetivo de **órdenes por segundo** (0 = sin tope); se aplica con un *Constant Throughput Timer* solo a `POST /orders` |
| `users` | 100 | usuarios ficticios que se fondean (`jm<run>-uN`) |
| `fund_brl` · `fund_vib` | `100000000000` · `100000000` | fondeo por usuario (centavos de BRL · unidades de VIB) |
| `mid_price` · `spread` | 10000 · 50 | precio medio y semiancho (centavos): el precio sale de `[mid-spread, mid+spread]`, así hay cruces y parciales |
| `cancel_pct` | 3 | % de iteraciones que cancelan la orden si quedó en el libro |
| `with_history` | false | requiere Postgres: consulta `/orders/{id}` y, en integridad, `/audit/history` |
| `run_id` | automático | identificador único de la corrida (prefijo de todos los ids: `jm<run_id>-…`) |

## Qué valida cada plan (más allá del código HTTP)
**Carga** (`POST /orders`, cada respuesta): `201`; la orden devuelta coincide con la enviada (id, usuario, lado, precio, cantidad); `filled + remaining == qty`;
el estado es coherente con lo ejecutado; **la suma de los trades == filled**; cada trade es del taker, cumple `total == precio × cantidad` y **respeta el límite de precio**
(compra ≤ límite, venta ≥ límite). Además: cancelaciones (`200 CANCELLED` o `404 order_not_found` por carrera legítima), saldos **nunca negativos**,
**libro ordenado y nunca cruzado**, y los errores esperados del contrato (`400 invalid_side/invalid_id/invalid_request`, `409 duplicate_order`, `422 insufficient_funds`, `404 order_not_found`) con su `code`.
Al final (`tearDown`): `GET /audit` con `ok=true` y **conservación de valor**: `total_BRL/VIB == base + users × fondeo`.
**Integridad** (determinista): liquidación exacta (el trade se hace al **precio del maker**, se devuelve la mejora de precio al comprador), reservas, parciales, cancelación que libera lo reservado,
idempotencia (409), todos los errores del contrato, y conservación de valor; con `WITH_HISTORY=true`, que el saldo recalculado desde Postgres coincida con el del motor.

## Escenarios y por qué son tres cosas distintas
| Qué | Dónde | Qué responde |
|---|---|---|
| **Carga** (gradual, sostenida, estrés) | `vibranium-load.jmx` | ¿cuánto aguanta y con qué latencia? Valida el negocio respuesta a respuesta, pero NO prueba cada importe. |
| **Integridad financiera** | `vibranium-integrity.jmx` | ¿cada importe es exacto? Un hilo, orden fijo, sin concurrencia. |
| **Recuperación ante caídas** | `scripts/drill-*.sh`, `scripts/verify-all.sh` | ¿qué pasa si el proceso muere, Postgres cae o el disco se llena? JMeter no puede matar procesos; se usa el mismo `integridad` antes y después si se quiere (`./jmeter/run-jmeter.sh integridad` tras reiniciar). |

## Requisito del plan de integridad: libro aislado
Un comprador se cruza con **toda** oferta ≤ su límite y un vendedor con toda demanda ≥ el suyo, así que el plan determinista necesita que sus órdenes no se crucen con las de nadie más. Por eso su primer paso (`01b`) lee el libro y **elige una banda de precios en el hueco** entre la mejor compra y la mejor venta (o `777000` si el libro está vacío; `-Jbase_price` lo cambia). Si no hay un hueco de al menos 3 centavos, **falla con «PRECONDICIÓN NO CUMPLIDA»** en lugar de dar un resultado engañoso: ejecútelo con el libro vacío (`docker compose down -v && docker compose up -d`) o sin carga simultánea.

## Seguridad y datos
- Las pruebas **crean** datos (usuarios `jm<run>-uN`, depósitos y órdenes) y no los borran: la API no tiene `DELETE` de usuarios. Por eso `run-jmeter.sh` **se niega** a apuntar a un host que no sea `localhost` salvo `ALLOW_REMOTE=1`.
- La comprobación de **conservación** supone que nadie más escribe en el sistema durante la prueba (compara contra la auditoría de antes). No la use en un sistema con otros clientes.
- No hay credenciales: la API no tiene autenticación (limitación conocida L16). Postgres/Grafana solo escuchan en `127.0.0.1`.
- Para empezar de cero: `docker compose down -v && docker compose up -d` (**borra** el journal y el historial: solo en un entorno de pruebas).

## Resultados y exportación
Cada corrida deja en `jmeter/results/<fecha>-<escenario>[-<etiqueta>]/`: `results.jtl` (CSV crudo; puede borrarse con `KEEP_JTL=0`), `timeseries.csv` (ventanas de 5 s),
`histogram.csv`, `errors.csv`, `labels.csv`, `summary.json`, `meta.json` (commit, rama, fecha, entorno), `params.txt` y, si Prometheus respondía, `prometheus.csv`.
Dashboard estándar de JMeter a partir de un `.jtl`: `jmeter -g results.jtl -o dashboard/` (el directorio de salida no debe existir).
Informe visual propio con varias corridas y la correlación con el motor:
```bash
# editar jmeter/report/runs.json (corridas y textos) y luego:
python3 jmeter/make_report.py html --config jmeter/report/runs.json --out jmeter/report/index.html
```

## Cómo leer los números (importante)
- **JMeter es de lazo cerrado:** si el servidor se frena, los hilos esperan y la latencia medida no incluye ese atraso («omisión coordinada»). El `cmd/loadgen` *open-loop* del proyecto sí lo incluye; por eso sus p99 son más altos para el mismo sistema.
- La columna «órdenes/s» cuenta solo `POST /orders` con `201` **y** validación de negocio superada.
- Las métricas del **motor** (heap, RSS, GC, cola, snapshots) son de Prometheus, no de JMeter; el informe las rotula aparte.
- **30 minutos:** necesitan ≈ 4 GB de disco (journal ≈ 110 MiB/min) y ≈ 10 GB si además corre Postgres. Si el entorno no los tiene, la corrida se aborta sola (guarda de disco) y queda marcada como **incompleta** (`ABORTADA_POR_DISCO.txt`).
