# Fase de optimización: diagnóstico, cambios y resultados

> Regla de trabajo: **primero se midió, después se cambió, y cada cambio tiene una prueba que falla si se revierte.**
> Los registros crudos están en [`evidencia/`](evidencia/README.md). Entorno de todas las mediciones: sandbox de **4 núcleos compartidos**
> con el generador de carga, Postgres, Prometheus y Grafana; **disco virtual** (fsync de ≈ 0,4 ms). No es hardware de producción.

## 1. Resumen

| Pregunta | Respuesta con evidencia |
|---|---|
| ¿Por qué p90 = 24,6 s y máx. = 33 s en el soak? | Por **un solo minuto**: tras un reinicio, el proyector del historial **releía y decodificaba todo el journal** (7,5 M registros, ≈ 60 s, 2-3 núcleos, 9 GB de basura) y el sistema bajó a 2.005 órd/s. Se acumuló un atraso de ≈ 33 s que el generador (que mide desde la hora en que la orden *debía* salir) tardó ≈ 3 min en drenar. **Reproducido** (58 errores 504, p99 2,45 s) y **corregido** (0 errores, p99 0,58 s). |
| ¿Qué domina la latencia de cola con el sistema ya caliente? | **La pausa del snapshot** (copia del estado dentro del secuenciador): p99 = **12 ms sin snapshots** vs **101-342 ms** con uno cada 200.000 registros y **47 ms** con uno cada 1.000.000. No son fsync, ni matching, ni la ventana de idempotencia. |
| ¿Por qué RSS ≫ heap vivo? | RSS ≈ `heap_sys − memoria devuelta al SO + 25-50 MiB`. El heap en uso llega a ≈ 2× lo vivo (objetivo del GC con `GOGC=100`) y, en cada snapshot, además aparecen ≈ 230 MiB de copia del estado y (antes) ≈ 930 MiB de basura de serialización. La cifra «0,7 GiB vivos» era el perfil **justo tras restaurar**; en carga lo vivo es 0,6-1,6 GiB. |
| ¿Se sostienen 5.000 órd/s? | **Sí en lo medido**: todas las corridas completaron 5.000 órd/s efectivas sin un solo error (hasta 14 min antes de los cambios, 10 min después, con Postgres activo). **No demostrado:** ≥ 30 min, disco real, y un objetivo de latencia concreto (no lo hay en el enunciado). Ver §7. |

## 2. Cómo se midió (reproducible)

| Herramienta | Para qué | Cómo |
|---|---|---|
| `scripts/soak.sh` | tabla por minuto: throughput, cola, heap/RSS, pausas de GC y de snapshot, lag del historial, disco; reinicio final | `./scripts/soak.sh 10 5000` |
| `GODEBUG=gctrace=1` | ciclos de GC, fase concurrente, pausas *stop-the-world* (STW), objetivo del heap | variable de entorno del contenedor |
| `pprof` (`/debug/pprof/allocs`, puerto de admin) | **quién genera la basura** (diferencia entre dos capturas) | `go tool pprof -sample_index=alloc_space -base a.pb b.pb` |
| `scripts/latency-breakdown.sh` | **descompone la latencia** del servidor: cola · matching · fsync · HTTP (p50/p99/p99.9 por ventana de 1 min) | `./scripts/latency-breakdown.sh 10` |
| `/proc/stat` | descartar *steal* / saturación de CPU del entorno | muestreo cada 30 s |
| `SNAPSHOT_SCALE=1 go test -run ScaleReport ./internal/snapshot` | costo de capturar/escribir un snapshot con 5 M ids + 1 M órdenes | prueba de medición, no de aserción |
| Experimento E2 | reinicio en caliente con carga a 5.000/s | reiniciar el contenedor y lanzar `loadgen` |

## 3. Diagnóstico

### 3.1 La latencia extrema (p90 24,6 s)
- El generador es *open-loop*: la latencia se mide desde la hora **planificada** de envío. Si el servidor se atrasa, el atraso queda en la medición (correcto: evita la «omisión coordinada»).
- Minuto 1 de aquella corrida: **2.005 órd/s** de 5.000 ⇒ déficit de 3.000 × 60 = 180.000 órdenes ≈ **36 s de atraso**; el máximo medido fue 33 s. Luego una ráfaga de recuperación a 7.066 órd/s (min 3) drenó el atraso. Los minutos 1-4 son ≈ 33 % de todas las peticiones y llevaron atrasos de decenas de segundos ⇒ un p90 de 24,6 s es coherente con eso. **Era un transitorio, no el régimen estable** (de hecho, desde el minuto 5 el sistema sostuvo 5.0xx órd/s).
- **Causa (verificada en E2):** al arrancar, el proyector abría el journal en el offset 0 y, para llegar a `last_seq`, **decodificaba cada registro** (`json.Unmarshal`) solo para descartarlo. Con 7,5 M registros: ≈ 60 s a ≈ 125.000 reg/s, con el proceso usando 1,7-2,9 núcleos (4 en total, compartidos) y generando 9 GB de basura que disparan el GC. **El costo crece linealmente con el journal** (a 5.000/s: ≈ 1 h de re-lectura por cada día de operación). Es un defecto del código, no del entorno.
- **Un detalle de medición:** la columna «cola» de `soak.sh` nunca pasó de ≈ 127 porque el generador usa 128 conexiones: es un tope del generador, no del servidor. La cola real hay que leerla en `queue_wait_seconds`.

### 3.2 Espera en cola vs procesamiento vs persistencia vs HTTP
Corrida e5 (10 min a 5.000/s desde cero, Postgres activo). Valores en ms, por ventanas de 1 min:

| Etapa | p50 | p99 | p99.9 (típico / peor ventana) |
|---|---|---|---|
| **Cola** (espera antes de ejecutarse) | 0,31 | 9,2 (peor 20,9) | **95 / 235** |
| Matching + liquidación del lote | 0,03 | 0,45 | 2,0 / 2,4 |
| **fsync** del journal (por lote) | 0,38 | 3,3 | 8,8 / 14,3 |
| HTTP completo `POST /orders` (visto por el servidor) | 1,03 | 21,6 | 158 / 394 |
| *Cliente (loadgen), toda la corrida* | 1,78 | 202 | 687 (máx. 887) |

**Lectura:** el matching cuesta microsegundos y el fsync ≈ 0,4 ms; **la cola de la distribución está en la espera**, es decir, en momentos en que el secuenciador **está ocupado en algo que no es ejecutar órdenes** (copiar el estado para un snapshot) o el proceso está parado (GC). El cliente ve más que el servidor (p99 202 vs 22 ms) porque las peticiones que llegan durante una pausa no empiezan a medirse hasta que el servidor vuelve a aceptarlas.

### 3.3 fsync, group commit, snapshots, GC, secuenciador, ventana de idempotencia

| Componente | Medición | Veredicto |
|---|---|---|
| **fsync** | p50 0,38 ms, p99 3,3 ms (disco virtual) | No es cuello de botella *aquí*. En disco real costará milisegundos; **no se pudo medir**. |
| **Group commit** | mediana de 1,6 comandos por fsync a 5.000/s | Agrupa poco porque el fsync es casi gratis; en un disco real agrupará más. |
| **Secuenciador / matching** | `batch_exec` p99 0,45 ms; 2,3 % de la CPU (perfil previo) | No es cuello de botella. |
| **Snapshot — pausa** | 150-230 ms por millón de órdenes en el libro (prueba de escala); 163-799 ms observados en carga | **Domina el p99.** Ver A/B (§3.4). |
| **Snapshot — escritura** (fuera del secuenciador) | 2,1-2,8 s para 210 MiB; hasta 9,8 s bajo carga | Pasaba 932 MiB de basura por 210 MiB de archivo (4,4×): **corregido** (§5). |
| **GC** | antes: 16,9 ciclos/min, fase concurrente mediana 1,1 s; 2 pausas STW atípicas (867 y 447 ms) de 129 ciclos | La frecuencia dependía de la tasa de basura (§3.5). Las 2 pausas atípicas **no se reprodujeron** (§3.6). |
| **Ventana de idempotencia** | 305 MiB vivos para 5 M ids; `add()` reserva 42-79 B por orden en régimen estacionario | El costo es **memoria**, no CPU ni basura. El 20 % de `idWindow.add` en un perfil venía de **reconstruirla al arrancar**, no del régimen estable (hipótesis rechazada). |

### 3.4 A/B de snapshots (misma carga, 4 min a 5.000/s, sin Postgres, estado de 3-8 M registros)

| Configuración | p50 | p99 | p99.9 | máx. | pausa del último snapshot |
|---|---|---|---|---|---|
| A1: cada 200.000 | 1,27 ms | **101 ms** | 350 ms | 513 ms | 163 ms |
| **B: sin snapshots** | 1,24 ms | **12 ms** | 245 ms | 333 ms | – |
| A2: cada 200.000 | 1,35 ms | **257 ms** | 504 ms | 553 ms | 295 ms |
| **C: cada 1.000.000** | 1,18 ms | **47 ms** | 275 ms | 441 ms | 435 ms |
| A3: cada 200.000 | 1,22 ms | **342 ms** | 1.017 ms | 1.200 ms | 799 ms |

El estado crece de una corrida a otra (también la pausa: 163 → 799 ms), por eso A sube; **B y C se intercalan en el mismo tramo** y son claramente mejores. Quitar los snapshots baja el p99 de 100-340 ms a 12 ms, **pero el p99.9 sigue en ≈ 250 ms**: queda una causa que **no es** el snapshot (GC y otras pausas). El costo de espaciarlos: tras un *crash* hay hasta ≈ 1 M de registros de cola que reproducir (≈ 9 s a ≈ 116.000 reg/s). Un reinicio **ordenado** no reproduce nada (snapshot final).

### 3.5 Por qué RSS = 3,7 GiB con 0,7 GiB «vivos»
Muestreo cada 5 s (`antes/e1_memoria.log`), en el instante del RSS máximo (3.082 MiB):

| Componente | MiB |
|---|---|
| `heap_inuse` (objetivo del GC `next_gc` = 2.836) | 2.751 |
| `heap_idle` aún **no devuelto al SO** (1.437 idle − 1.182 devueltos) | 255 |
| Pilas, binario, mapeos | ≈ 50 |
| **RSS** | **3.082** |

- `RSS ≈ heap_sys − heap_released + 25-50 MiB` (mediana del error, ambas corridas).
- **Lo vivo no es 0,7 GiB:** el perfil de 712 MiB se tomó justo tras restaurar. En carga, `gctrace` muestra **606-1.634 MiB vivos tras cada GC**: 305 MiB de ventana + el libro (≈ 232 MiB en el perfil de 712 MiB) + lo que crece con la carga (el generador deja millones de órdenes en espera).
- `GOGC=100` permite que el heap llegue a **≈ 2× lo vivo** antes del siguiente ciclo, y los 3,7 GiB originales eran **sin** `GOMEMLIMIT`. Cada snapshot sumaba además ≈ 230 MiB de copia y ≈ 930 MiB de basura de `json.Marshal`.
- Con `GOMEMLIMIT=3GiB` y `mem_limit=4g` (compose) el pico pasó de 3,7 GiB (sin límite) a 3.082 MiB y, tras el cambio C3 de §5, a 2.552 MiB **con un estado mayor**.

### 3.6 Entorno compartido vs código

| Factor | Evidencia | Atribución |
|---|---|---|
| Proyector re-lee el journal al arrancar | E2: 58 × 504, p99 2,45 s | **Código** (corregido) |
| `ReadNew` reservaba 1 MiB por llamada | 5,4 GB de 11 GB de basura en 80 s | **Código** (corregido) |
| `snapshot.Write` reservaba 4,4× el archivo | 932 MiB por 210 MiB | **Código** (corregido) |
| Pausa del snapshot ∝ órdenes en el libro | 150-230 ms por millón; A/B | **Código/diseño** (mitigado con el intervalo; sin corrección de fondo) |
| CPU del entorno | durante el soak: usuario 45-49 %, sistema 17-19 %, **idle ≈ 20 %, iowait 7-8 %, steal 1,5-2 %** | El entorno **no** estaba saturado ni con *steal* relevante; sí es compartido (4 núcleos para todo el stack). |
| Pausas STW de 867 y 447 ms | en una prueba controlada (GC marcando + `Capture` de 5 M órdenes) el máximo tiempo en detener el mundo fue **66 µs** | **Sin causa establecida.** No se atribuyen a `Capture`. Con 68 % menos de basura no volvieron a aparecer (e3: 31 ciclos, pausa máx. 2,2 ms; e5: 4,8 ms), pero la muestra es chica: **no es una conclusión**. Posible efecto del entorno virtualizado. |
| Disco virtual | fsync 0,4 ms | **Entorno**: optimista. |
| Generador de carga | acumula 1-1,7 M de órdenes en el libro por cada 3 M (precio al azar, 3 % de cancelaciones) | **Artefacto de la carga**: infla la pausa del snapshot; un libro real podría ser más (o menos) profundo. |

## 4. Hipótesis y su estado

| # | Hipótesis | Estado | Cómo se decidió |
|---|---|---|---|
| H1 | El arranque del proyector (re-lectura del journal) causa el transitorio | ✅ Verificada | E2 antes/después |
| H2 | `ReadNew` genera basura por llamada | ✅ Verificada | pprof + test (1.084 B vs 1.049.979 B con la mutación) |
| H3 | `snapshot.Write` genera basura ∝ al estado | ✅ Verificada | prueba de escala (932 → 46 MiB) |
| H4 | La pausa del snapshot domina el p99 | ✅ Verificada | A/B (12 ms sin snapshots) |
| H5 | fsync / group commit limitan | ❌ Rechazada | fsync 0,4 ms; la cola está en la espera, no en el fsync |
| H6 | El matching limita | ❌ Rechazada | 0,03 ms por lote |
| H7 | La ventana de idempotencia genera basura en régimen estable | ❌ Rechazada | 42-79 B por orden |
| H8 | `Capture` retrasa el *stop-the-world* del GC | ❌ Rechazada | 66 µs en prueba controlada |
| H9 | Recorrer los niveles de precio acelera `Capture` | ❌ No confirmada | 200-650 ms vs 220-250 ms (ruidoso); no se adoptó |
| H10 | Las pausas STW de 450-870 ms son del código | ❓ Abierta | no reproducidas |
| H11 | El entorno (CPU *steal*) explica el p99 | ❌ Descartada como causa principal | steal 1,5-2 %, idle 20 % |
| H12 | En 30 min, JMeter degrada el sistema por el **GC** (heap creciente) | ❌ Rechazada | `gctrace`: el GC consumió 2 % de la CPU acumulada en 15 min |
| H13 | En 30 min, la degradación (5.500 → 4.100 órd/s; p99 23 → 87 ms) viene de **`Book.Depth`**: recorría todas las órdenes de los niveles consultados dentro del secuenciador | ✅ Verificada | perfil de CPU a los 12 min: `Depth` = 18,4 % de la CPU total y ≈ 50 % del tiempo del secuenciador; test de escala (×62 con 100× más órdenes por nivel); desaparece tras el cambio (§8) |

## 5. Cambios realizados (solo los respaldados por evidencia)

**No se tocó:** el orden determinista de ejecución, el `fsync` por lote ni «responder solo tras el fsync», la validación contable, `/audit`, las comprobaciones de recuperación, la ventana de idempotencia (5 M), ni la semántica de las órdenes. El formato del journal y del snapshot **no cambia** (los snapshots viejos se leen igual).

| # | Cambio | Prueba de regresión (y mutación que la hace fallar) |
|---|---|---|
| **C1** | **El proyector reanuda por offset.** `projector_state.journal_offset` guarda dónde empieza el último registro proyectado (misma transacción que `last_seq`). Al arrancar relee **solo ese registro** para comprobar que el journal contiene lo proyectado y sigue desde ahí. Si el offset no sirve (historial anterior a la columna, mitad de un registro, de otro registro, más allá del final), **se lee desde el inicio**, como siempre. | `TestProjectorMirrorsEngineAndSurvivesRestart` (re-lee exactamente 1 registro) · 4 casos de offset inservible · 2 casos de offset **adelantado** (saltaría registros). Mutaciones: sin continuidad de secuencia → falla *«orden b-g9-o0: no encontrado»*; sin exigir releer el registro `last` → falla *«no alcanzó el journal: 645 de 1275»*. |
| **C2** | **`journal.Reader` reutiliza** su `bufio` de 1 MiB y su buffer de payload. | `TestReadNewDoesNotAllocateAMebibytePerCall` (1.084 B por llamada; con la mutación 1.049.979 B) · `TestReaderBufferReuseDoesNotCorruptRecords` (registros de tamaños muy distintos, lecturas de 1, 3, 50…). |
| **C3** | **`snapshot.Write` en *streaming*** (elemento a elemento, mismo formato `[len][crc][JSON]`, CRC y longitud al vuelo, cabecera al final). | `TestStreamedJSONEqualsMarshal` (5 estados, incl. vacíos y >4096 elementos: el JSON equivale a `json.Marshal` y `Read` devuelve el mismo estado) · `TestWriteAllocationDoesNotScaleWithTheState` (0,27× el archivo; con `json.Marshal` 4,7×). |
| **C4** | **`SNAPSHOT_EVERY` por defecto 1.000.000** (antes 200.000). Es configuración: `SNAPSHOT_EVERY=200000` lo revierte. | A/B de §3.4. Costo: cola de ≤ ~1 M de registros tras un crash (≈ 9 s). |
| **C6** | **`Book.Depth` en O(niveles)**: cada nivel mantiene su cantidad pendiente (`level.qty`) de forma incremental (alta, ejecución parcial, baja). Hallazgo de la prueba de 30 min con JMeter (§8). | `TestDepthCostDoesNotGrowWithOrdersPerLevel` (×0,8 con 100× más órdenes por nivel; con el recorrido antiguo ×62) · `TestLevelQtyStaysConsistentAcrossFillsCancelsAndRestore` (30.000 órdenes con cancelaciones y restauración: la cantidad incremental == la recalculada; sin descontar lo ejecutado al maker falla con *«qty incremental=138, recalculada=54»*). |
| **C5** | Observabilidad: métrica `projector_rescanned_records_total`, `scripts/latency-breakdown.sh`, pruebas de medición de escala. | – |
| (previo) | `GOMEMLIMIT=3GiB`, `mem_limit=4g`, `scripts/seed.sh` | – |

## 6. Resultados antes / después

| Medida | Antes | Después | Comparable porque… |
|---|---|---|---|
| **Reinicio en caliente bajo 5.000/s** (E2, journal de 7,5-8,3 M) | 58 errores 504 · p90 335 ms · **p99 2,45 s** · máx. 2,73 s · proyector 1,7-2,9 núcleos durante ≈ 60 s | **0 errores** · p90 9 ms · **p99 584 ms** · máx. 949 ms · ≈ 1,1 núcleos | mismo journal y misma carga |
| Registros re-leídos por el proyector al arrancar | 7.508.336 (todo) | **1** | mismo journal |
| Basura generada en 80 s (20 s ociosos + 60 s a 5.000/s) | 11.009 MB | **3.560 MB** (−68 %) · proyector 7.252 → 1.156 MB | mismo procedimiento (pprof) |
| Ciclos de GC por minuto | 16,9 (129 ciclos) | **4,6** (31 ciclos) | 8 y 7 min a 5.000/s; el «después» con un estado **mayor** (8,3 M vs 4,3 M registros) |
| Pausa STW máxima | 867 ms (2 atípicas ≥ 447 ms) | 2,2 ms (e3) · 4,8 ms (e5) | muestra chica; **no es una conclusión** (H10) |
| Pico de RSS / `heap_inuse` | 3.082 / 2.751 MiB | 2.552 / 2.360 MiB | el «después» con estado mayor |
| Memoria reservada por snapshot de 210 MiB | 932 MiB | **46 MiB** | mismo estado (prueba de escala) |
| p99 de 4 min, estado grande | 101-342 ms (cada 200.000) | **47 ms** (cada 1 M) | A/B intercalado (§3.4) |
| Desde cero, Postgres activo (14 min antes / 10 min después) | p50 1,97 · p90 10,0 · p99 337 · p99.9 1.339 · máx. 1.712 ms | p50 1,78 · p90 6,9 · p99 202 · p99.9 687 · máx. 887 ms | **no estrictamente**: duraciones distintas y variación entre corridas (A1…A3 varían 3×). No se afirma mejora. |

`verify-all`: **14/14 PASS** (ver `evidencia/despues/verify_phase3b.log`), `go test -race ./...` verde (77 tests; los de Postgres con `TEST_DATABASE_URL`), pruebas de alertas verdes.

## 7. Lo que no se pudo completar o no está demostrado

1. **Prueba de ≥ 30 min.** No se hizo. Necesita ≈ 10 GB libres (journal ≈ 100 MiB/min + Postgres ≈ 100 MiB/min + WAL) y el sandbox tiene ≈ 4-5 GB libres **después** de borrar cachés y la imagen `golang:1.24`; los 11 GB restantes son los volúmenes de pruebas anteriores, que **se conservaron** por indicación expresa. Lo más largo tras los cambios: 10 min desde cero con Postgres (e5), 7 min con estado grande (e3, cortada por la guarda de disco de `soak.sh`), un segundo intento de 17 min que la misma guarda cortó a los 10 min (`evidencia/despues/e6_soak_cortado_por_disco.log`: 5.015 órd/s, 0 errores, auditoría `ok`) y 5 × 4 min sin Postgres (A/B). **Nunca se llegó a los 16,7 min que hacen falta para llenar la ventana de 5 M desde cero con Postgres activo**; la ventana llena sí se ejercitó antes de los cambios (soak original) y en e1/e3 con estado heredado.
2. **«Almacenamiento real».** El entorno solo ofrece disco virtual: el costo real del fsync y el beneficio real del group commit **no se midieron**.
3. **Un incidente que sí validó un diseño:** al quedarse sin disco, el servidor hizo *fail-stop* (`panic: fsync del journal falló: no space left on device`), Docker lo reinició y no se perdió nada de lo confirmado. Está documentado como hallazgo.
4. **Sin corrección de fondo para la pausa del snapshot.** Solo se espació. Si hace falta un p99 < 50 ms con estados de millones de órdenes, la opción respaldada por los datos es tomar los snapshots desde una **réplica de seguimiento** (un segundo `Exchange` que reproduce el journal) para que el secuenciador nunca se pause; cuesta **duplicar la memoria** y es un cambio de arquitectura (de paso, el primer paso hacia una réplica en espera, L3/L6). No se implementó sin una decisión del equipo.
5. **Windows / Docker Desktop:** no se ejecutaron estos cambios allí.

## 8. Hallazgo posterior: la degradación de la prueba de 30 min (JMeter) y su corrección

**Qué se vio (hecho).** En la corrida de 30 min con JMeter (sin Postgres, tasa objetivo 5.000 órd/s) el sistema **se degradó mientras el estado crecía**:
throughput 5.456 → 5.640 → 4.951 → 4.391 → 4.398 → **4.140 órd/s** (bloques de 5 min) y p99 mediano de 23 → **87 ms**. Desde Prometheus, por bloques de 5 min:
el p99 del **matching+liquidación** (por lote) subió de **1,9 a 29,6 ms**, el de la **cola** de 7,7 a 81,5 ms, mientras el fsync solo pasó de 2,4 a 4,1 ms. El motor (no JMeter) fue quien se frenó: `engine_orders_per_s` cayó igual que el throughput del cliente.

**Hipótesis y cómo se descartaron / verificaron (con una segunda corrida de 15 min con `gctrace` y perfiles de CPU, `evidencia/diagnostico_degradacion/`):**
- ❌ *GC*: el GC acumulado fue **2 %** de la CPU tras 15 min (3 ciclos/min, marcado de 0,5 s); no puede explicar una caída del 25 %.
- ❌ *fsync / disco*: p99 de 2,4 → 4,1 ms.
- ✅ **`engine.(*Book).Depth`**: el perfil de CPU de 30 s a los 12 min muestra `Depth` con **5,72 s de 31 s (18,4 % de TODA la CPU)**, ≈ la mitad de lo que hace el secuenciador (`processBatch` 11,5 s), y `Order.Remaining` (llamado desde `Depth`) con otro 13,6 % plano. A los 2 min, con el libro pequeño, `Depth` no aparece. Causa en el código: `Depth` sumaba `Remaining()` **recorriendo todas las órdenes de cada nivel consultado** (listas enlazadas con fallos de caché), y se ejecuta **dentro del secuenciador**, es decir, mientras se ejecuta no se procesa ninguna orden. El plan de JMeter pide `GET /book?depth=5` en el 1 % de las iteraciones (≈ 50 consultas/s); con millones de órdenes en espera repartidas en ≈ 100 niveles (decenas de miles por nivel) cada consulta bloqueaba el motor ≈ 4 ms de media.
- Esto ya figuraba como limitación (L5: «Depth recorre el libro; es para consulta») pero **subestimado**: se creía O(niveles) y era O(órdenes de los niveles consultados).

**Cambio (C6).** Cada nivel mantiene `qty` (cantidad pendiente) de forma incremental: se suma al insertar, se resta lo ejecutado en cada cruce y lo que queda al cancelar. `Depth` pasa a ser O(niveles) (se sigue ordenando una copia de los niveles). No cambia el orden de ejecución, ni lo que se persiste, ni la salida de `Depth`: el test restaura desde un snapshot y compara con la suma recalculada.
Medición aislada (`TestDepthCostDoesNotGrowWithOrdersPerLevel`, 200 consultas de `Depth(5)`): **con 100 niveles de 50 órdenes = 1,8 ms; con 100 niveles de 5.000 órdenes = 1,4 ms** (antes: 0,5 ms → 30,8 ms, ×62).

**Resultado de repetir los 30 minutos con la corrección** (misma configuración: JMeter 128 hilos, tasa objetivo 5.000, sin Postgres, estado nuevo):

PENDIENTE_30MIN

**Conclusión honesta.** El hallazgo y la corrección están respaldados por un perfil y por tests; el efecto sobre los 30 minutos se mide arriba. Esto **no** significa que el sistema esté listo para producción: sigue habiendo lecturas pesadas dentro del secuenciador (`/audit`, la copia del snapshot — L17/L21), y la prueba sigue siendo en disco virtual y sin Postgres.
