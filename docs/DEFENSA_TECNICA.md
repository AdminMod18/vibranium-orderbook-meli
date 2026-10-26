# Defensa técnica — Vibranium Order Book

> Documento para una revisión técnica. **Convención:** cada afirmación importante lleva una etiqueta.
> **[HECHO]** = verificado con una prueba, una medición o el código (se cita dónde) · **[HIPÓTESIS]** = explicación plausible sin demostrar ·
> **[LIMITACIÓN]** = algo que el sistema no hace o no se probó · **[RECOMENDACIÓN]** = qué haría a continuación.
> Nada de lo que aquí figura como «hecho» se afirma sin evidencia en el repositorio; lo que falta está en §7 y §8.
> Rama `claude/focused-babbage-2ppmql`. Esta entrega **está lista para revisión, no para producción** (§8).

## 1. Resumen ejecutivo y objetivo

**Objetivo.** Un *order book* de límite para el activo **Vibranium** cotizado en **BRL**: órdenes de compra y venta, cruce con prioridad precio-tiempo, liquidación correcta de saldos
(créditos y débitos), consulta de libro/saldos/historial, y los atributos que pide una plataforma: concurrencia correcta, **5.000 órdenes/s**, trazabilidad, resiliencia y observabilidad.
Debe poder levantarse y probarse fácilmente, y poder explicarse qué ocurre cuando falla cada componente.

**Qué es.** Un servicio Go (≈ 6.900 líneas, tests incluidos) con un **secuenciador de un solo escritor**, un **journal** append-only que es la fuente de verdad, **snapshots** como acelerador de arranque,
y **PostgreSQL como modelo de lectura** (historial y trazabilidad) alimentado desde el journal. Se opera con Docker Compose (app + Postgres + Prometheus + Grafana).

**Estado honesto en una tabla.**

| | |
|---|---|
| Funciona y está verificado | matching, liquidación, reservas, cancelación, idempotencia, recuperación tras `kill -9`, Postgres caído, candado de instancia única, backpressure, alertas, `verify-all` 14/14 |
| Mejoró con evidencia | arranque en caliente (p99 2,45 s → 0,58 s, 58 errores → 0), basura generada −68 %, ciclos de GC 16,9 → 4,6 por minuto, y la degradación en 30 min por `Book.Depth` (throughput 4.140 → 5.512 órd/s al final, p99 del matching 29,6 → 0,8 ms) (§6) |
| Medido, con reservas | **5.000 órd/s:** con el generador *open-loop* hasta 14 min; con JMeter, **30 min sin PostgreSQL** a 5.334 órd/s de media (82 % de las ventanas de 5 s ≥ 4.900, 0 errores). **Con PostgreSQL, JMeter no pasa de ≈ 4.300** (techo de JMeter + CPU compartida). Todo en disco virtual (§6) |
| **No** hecho | prueba de ≥ 30 min **con** Postgres y en disco real, réplica/HA, autenticación, rotación del journal, corrección de fondo de la pausa del snapshot (§7, §8) |

## 2. Arquitectura y flujo de una orden

```
cliente ──HTTP──> API (net/http, JSON, límites, request-id, métricas)
                   │  POST /orders
                   ▼
            cola acotada (QUEUE_SIZE=10.000)  ── llena ──> 503 overloaded (inmediato)
                   │
                   ▼
   ┌────────── SECUENCIADOR (1 goroutine) ──────────────────────────────────────┐
   │ toma hasta 1.024 comandos (group commit) y, EN ORDEN, para cada uno:       │
   │   descarta si el cliente ya no espera (ctx vencido)                        │
   │   validar → ¿id repetido? → Hold (reservar fondos) → Book.Submit (matching)│
   │   → Settle (liquidar cada trade) → registro de journal                     │
   │ luego: UN fsync del lote → recién entonces responde a todos → ¿snapshot?   │
   └────────────────────────────────────────────────────────────────────────────┘
          │ append (CRC)                              │ lectura (solo lo ya sincronizado)
          ▼                                           ▼
   JOURNAL (fuente de verdad) ───────────────>  PROYECTOR ──tx por lote──> PostgreSQL
   genesis + [len][crc32c][JSON]                (offset y last_seq en la misma tx)   (historial, auditoría)
          │
          └── SNAPSHOT (copia del estado cada N registros; escritura atómica fuera del secuenciador)
```

**Flujo de una orden (código).**
1. `internal/api/api.go` · `placeOrder`: cuerpo ≤ 1 MiB y sin campos desconocidos; `side` ∈ {BUY, SELL}; si falta `id` se genera uno; plazo de 2 s (`requestTimeout`); middleware `observe` (X-Request-ID, métricas, log).
2. `internal/sequencer/sequencer.go` · `Do`: encola el comando **sin bloquear** (cola llena ⇒ `ErrOverloaded` ⇒ 503) y espera la respuesta o el vencimiento del contexto (⇒ 504).
3. `sequencer.run/processBatch`: un solo goroutine. Ejecuta cada comando en orden de llegada, hace `Append` al journal de cada uno que cambie el estado y **un único `Sync`** por lote; **solo después del fsync** responde. Si un comando ya venció, **no se ejecuta** (evita órdenes «fantasma» tras un 504; métrica `queue_expired_total`).
4. `internal/exchange/exchange.go` · `placeOrder`: valida (`engine.Validate`, ids ≤ 64 caracteres), consulta la ventana de idempotencia, `ledger.Hold` (sin saldo ⇒ 422 y la orden no entra), `book.Submit` (matching), `seen.add`, y por cada trade `ledger.Settle` (con devolución de la mejora de precio al comprador). Una orden truncada por el tope de trades libera su remanente.
5. `internal/engine/book.go` · `Submit/match`: prioridad **precio-tiempo**, el trade se hace **al precio del maker**, parciales, remanente en el libro; tope de **10.000 trades por orden** (el remanente se cancela).
6. `internal/journal/journal.go`: registro `[len u32][crc32c u32][JSON]`; primer registro `genesis` con un UUID que identifica al journal; `Sync` expone el offset durable.
7. `internal/history/projector.go`: lee el journal **solo hasta el offset ya sincronizado** y aplica lotes de hasta 5.000 registros a Postgres en **una transacción** junto con `last_seq` y `journal_offset`; reanuda por offset al arrancar (D72).
8. Lecturas: `GET /balances`, `/book`, `/audit` pasan por el secuenciador (vista **consistente**); `GET /orders`, `/trades`, `/audit/history/{user}` salen de Postgres (consistencia eventual, unos ms).

**Endpoints** (contrato completo en el README): `POST /deposits`, `POST /orders`, `DELETE /orders/{id}`, `GET /balances/{user}`, `GET /book?depth=N`, `GET /audit`, `GET /audit/history/{user}`, `GET /orders/{id}`, `GET /orders`, `GET /trades`, `/healthz`, `/readyz`. Puerto de administración aparte (`:9102`: `/metrics` y `pprof`).

## 3. Decisiones de diseño y su justificación

| Decisión | Por qué | Costo / alternativa descartada |
|---|---|---|
| **Un solo escritor (secuenciador)** [HECHO: `sequencer.go`] | El orden de llegada decide quién gana un cruce: el matching debe ser secuencial y **determinista**. Sin locks en el libro ni en las billeteras, los invariantes son fáciles de razonar. | Techo de un hilo: el perfil mostró que el matching usa ≈ 2,3 % de la CPU y que lo caro es red, JSON y disco. Escalar = particionar por instrumento. Alternativa descartada: locks por cuenta/nivel (carreras y orden no determinista). |
| **Cola acotada + backpressure** | Rechazar rápido y explícito (503 + `Retry-After`) es mejor que acumular latencia sin límite. Se dimensionó ≈ ritmo × plazo (10.000). | Con la cola grande las órdenes se ejecutaban *después* del 504 (medido: 50 de 50); por eso además se descartan los vencidos (D69). |
| **Group commit** | Un fsync por lote amortiza el costo de durabilidad: ninguna orden se confirma antes de estar en disco, sin pagar un fsync por orden. | En el disco virtual del entorno el fsync es casi gratis (0,4 ms) y agrupa poco (≈ 1,6 comandos por fsync): el beneficio real **no se pudo medir** (L22). |
| **Libro: heap de niveles + lista FIFO por nivel + mapa id→elemento** | Mejor precio en O(1), inserción/borrado de nivel en O(log L), **cancelación en O(1)**, prioridad de tiempo por FIFO. | `Depth()` recorre y ordena (O(L log L)): es de consulta, no del camino caliente. |
| **Enteros (centavos y unidades), nunca float** [HECHO: `domain/order.go`] | `Price×Qty` exacto y saldos que cuadran al centavo. Límites 10⁹ para que el producto no desborde int64. | La API exige enteros (un decimal da 400). |
| **Reserva al entrar, liquidación al cruzar** [HECHO: `wallet/ledger.go`] | No se puede gastar dos veces el mismo saldo; la cancelación solo libera lo reservado. | Una orden sin saldo ni entra al libro (422). |
| **Journal = fuente de verdad; estado en memoria derivado** | El motor es determinista: reproducir el log reconstruye el mismo estado. Cada comando se persiste antes de responder. | JSON es legible y auditable pero ocupa ≈ 380 B/registro (≈ 150 GB/día a 5.000/s sin rotación, L13). |
| **CRC por registro; cola truncada tolerada, corrupción en el medio = error** | Distingue «corte de energía a mitad de escritura» (esperable) de «el disco devolvió basura» (hay que parar). | – |
| **Snapshot = optimización, nunca la verdad** [HECHO: `recovery.go`, 6 casos probados] | Acota el arranque (7,06 s → 1,10 s para 817.000 registros). Si no sirve (corrupto, de otro journal, offset inválido…) se ignora y se reproduce todo. | Cada snapshot **pausa al secuenciador** mientras copia el estado (≈ 0,2 s por millón de órdenes en el libro): domina el p99 (§6). Por eso se espaciaron a 1 M de registros (D75). |
| **Idempotencia por ventana de ids (5 M)** [HECHO: `idwindow.go`] | Reintentar con el mismo `id` da 409 y no duplica. Por **cantidad** de órdenes y no por tiempo: reproducir el journal reconstruye exactamente la misma ventana. | ≈ 305 MiB para 5 M ids. Un id fuera de la ventana se acepta como orden nueva *salvo* que la orden siga en el libro (entonces 409 igual). |
| **Postgres como modelo de lectura, fuera del camino caliente** | Si cae, el matching sigue; al volver se reconcilia solo. Permite trazabilidad (`balances_from_history` recalcula saldos desde depósitos+trades y se compara con el motor). | Consistencia eventual en `/orders` y `/trades`. El historial ocupa más disco que el journal (≈ 585 B/orden, L19). |
| **Candado de instancia única sobre el journal** (`flock`/`LockFileEx`) | Dos procesos escribiendo el mismo journal lo corromperían. El SO libera el candado al morir el dueño. | No es alta disponibilidad: es protección, no réplica (L3). |
| **Fail-stop ante errores de escritura/fsync o de liquidación** | Seguir con un journal que no se puede escribir, o con un saldo inconsistente, sería peor que caer. Al reiniciar se reconstruye desde lo que sí quedó. | La política `restart: unless-stopped` de Docker reinicia sola. |

## 4. Invariantes contables y cómo se verifican

| Invariante | Dónde se comprueba | Cómo |
|---|---|---|
| **Conservación de valor**: suma de saldos (libre + reservado) == total depositado, en BRL y en VIB | `exchange.CheckInvariants` (expuesto en `GET /audit`, ejecutado dentro del secuenciador ⇒ vista consistente) | tests de propiedades (20.000 órdenes con cancelaciones), `scripts/smoke.sh`, drills, **JMeter** (`tearDown` y plan de integridad: total == base + depositado) |
| **Sin saldos negativos** | `CheckInvariants`; en tiempo de ejecución `Hold/Settle` rechazan o hacen *panic* | tests, JMeter (cada `GET /balances`) |
| **Reservas == órdenes abiertas** (`held` de cada usuario == lo que comprometen sus órdenes en el libro) | `CheckInvariants` recorre `book.RestingOrders()` | tests de recuperación comparan el estado tras reiniciar |
| **El libro nunca queda cruzado** (mejor compra < mejor venta) | tests de propiedades | JMeter (`GET /book?depth=5` en la carga: ordenado y no cruzado) |
| **El trade se hace al precio del maker y respeta los límites** | `engine.match` | tests; JMeter valida cada trade de cada respuesta (compra ≤ límite, venta ≥ límite, `total == precio × cantidad`, `Σ trades == filled`) |
| **Reproducir el journal da el mismo estado** | `journal.Apply` **verifica cada trade** reproducido contra el registrado | matriz de recuperación (crash, apagado ordenado, reinicios encadenados, 6 snapshots inservibles) |
| **El historial coincide con el motor** | `GET /audit/history/{user}` compara el saldo del motor con el recalculado desde Postgres (`conclusive`/`consistent`) | tests de integración con Postgres real, drill `postgres-down`, JMeter con `WITH_HISTORY=true` |

Una **violación de liquidación en ejecución es un bug, no un error de usuario**: `placeOrder` hace *panic* (fail-stop) en vez de seguir con un saldo inconsistente. [HECHO: `exchange.go`]

## 5. Recuperación y comportamiento ante fallos

| Fallo | Qué ocurre (hecho verificado) | Evidencia |
|---|---|---|
| **`kill -9` del proceso** (incluso bajo carga) | Al reiniciar: snapshot (si sirve) + cola del journal; invariantes `ok` y **totales idénticos**. Lo que no se confirmó (no llegó al fsync) no se prometió al cliente. | `drill-kill9.sh` (10 repeticiones), `drill-docker-crash.sh` |
| **Corte a mitad de un registro** (cola truncada / CRC malo en el último) | Se descarta esa cola y se sigue escribiendo | tests `TestTornTail…`, `TestBadCRCOnLastRecord…` |
| **Corrupción en el medio del journal** | **El servidor se niega a arrancar** (`ErrCorrupt`): preferible a servir un estado dudoso | `TestCorruptionInTheMiddleIsAnError` |
| **Error de escritura o de `fsync`** | `panic` (fail-stop) y reinicio por Docker; nada se confirmó sin fsync | `sequencer.processBatch` |
| **Disco lleno** | Ocurrió de verdad durante las pruebas: `panic: fsync del journal falló: no space left on device`, Docker reinició el contenedor, lo confirmado quedó intacto. Hay alertas previas (`JournalDiskLow < 2 GiB`, `JournalDiskFillingUp`) | `docs/OPTIMIZACION.md` §7, `alerts.yml` |
| **PostgreSQL no disponible** | El matching **sigue** (201). Los endpoints de historial dan **503**. El proyector reintenta con backoff (≤ 5 s) y, al volver, **se pone al día solo** hasta `lag=0` y coincide con el motor. | `drill-postgres-down.sh`, test de integración |
| **Historial por delante del journal** (journal retrocedido o restaurado de una copia vieja) | El proyector **se detiene** y reporta `ErrHistoryAhead`; no mezcla historias | `TestProjectorStopsWhenPostgresIsAheadOfTheJournal` |
| **Journal ajeno** (otro UUID de genesis) | `ErrJournalMismatch`: no proyecta | `TestProjectorRefusesForeignJournal` |
| **Dos instancias sobre el mismo journal** | La segunda se niega a arrancar (código 1); la primera no se afecta | `drill-single-instance.sh` (procesos y contenedores) |
| **Snapshot corrupto/ajeno/inconsistente** | Se ignora y se reproduce el journal completo | 6 casos en `recovery_test.go` |
| **Sobrecarga** | 503 rápido y explícito; nada se pierde ni se descuadra; tras reiniciar se recupera todo lo aceptado | `drill-overload.sh` (307.282 rechazos, 50.601 aceptadas, saldos `ok`) |
| **Cliente corta la conexión (timeout)** | La orden **no se ejecuta** si ya había vencido en la cola; si ya estaba en marcha puede ejecutarse y el reintento con el mismo `id` da 409 (y `GET /orders/{id}` informa el estado) | D69, test de «ejecuciones fantasma» (0 de 50) |

**[LIMITACIÓN] No cubierto:** pérdida del disco o del volumen (no hay réplica ni copia, L6/L3); un disco que miente sobre `fsync`; fallo de memoria del host; *split-brain* (no hay clúster).
**Tiempos medidos:** recuperación de 816.904 registros: **7,06 s sin snapshot**, **1,10 s con snapshot tras crash**, 1,02 s tras apagado ordenado; con un journal de 2,7 GiB y snapshot de 268 MiB el reinicio ordenado tardó 11,8 s (carga del snapshot).

## 6. Resultados de rendimiento (antes y después) y condiciones de ejecución

**Condiciones de TODAS las mediciones [HECHO].** Sandbox de **4 núcleos compartidos** (servidor, Postgres, Prometheus, el generador y, en §6.2, JMeter compiten por ellos), 16 GB de RAM, **disco virtual** (fsync ≈ 0,4 ms: no es almacenamiento real), Go 1.24, JMeter 5.6.3 / Java 21. JMeter se conectó a la IP del contenedor para evitar el `docker-proxy` (≈ 0,4 núcleos). Cada corrida arranca con volúmenes nuevos. Son una **referencia comparativa, no una garantía de capacidad** (L10).

### 6.1 Con el generador *open-loop* del proyecto (`cmd/loadgen`, dentro de la red de Docker)
Mide la latencia desde la hora **planificada** de envío, así que incluye cualquier atraso del servidor. Detalle en `docs/OPTIMIZACION.md` y `docs/INFORME_PRUEBAS.md`.

| Medida | Antes | Después | Condiciones |
|---|---|---|---|
| Capacidad con estado vacío (`INFORME_PRUEBAS` §2) | 5.000/s: p99 16 ms · 16.000/s efectivas sin rechazar | – | corridas cortas, sin cambios de código relevantes para esto |
| Reinicio en caliente bajo 5.000/s (journal de 7,5–8,3 M registros) | **58 errores 504**, p99 **2,45 s**, máx. 2,73 s; el proyector releía 7.508.336 registros | **0 errores**, p99 **0,58 s**, máx. 0,95 s; releyó **1** registro | mismo journal y misma carga |
| Basura generada (80 s a 5.000/s) | 11,0 GB | 3,6 GB (**−68 %**) | mismo procedimiento (pprof) |
| Ciclos de GC por minuto | 16,9 | 4,6 | el «después» con un estado mayor |
| Memoria reservada por snapshot de 210 MiB | 932 MiB | 46 MiB | prueba de escala |
| p99 de 4 min, estado grande (sin Postgres) | 101–342 ms (snapshot cada 200.000) | 47 ms (cada 1.000.000); 12 ms sin snapshots | A/B intercalado |
| Soak de 14 min desde cero (con Postgres) | p50 1,97 · p99 337 · p99.9 1.339 ms | (10 min) p50 1,78 · p99 202 · p99.9 687 ms | **no comparable estrictamente**: duraciones distintas, variación 3× entre corridas |

### 6.2 Con JMeter (`jmeter/`): vista del cliente + métricas del motor
Plan de carga con validación de negocio de **cada** respuesta. «ANTES» = binario del commit `78bdefc`; «DESPUÉS» = el de la fase de optimización; «DESPUÉS C1–C5» no incluye la corrección C6 de `Book.Depth`; «DESPUÉS + C6» y «30 MIN · con C6» sí (§6.3).

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

**Lectura [HECHO + límites].**
1. **Ninguna corrida de JMeter con Postgres alcanza 5.000 órd/s sostenidas** (solo el 3–4 % de las ventanas de 5 s llega a 4.900; con la corrección C6 también ≈ 4.300): JMeter consume ≈ 2,2 núcleos y satura en ≈ 4,3 mil órd/s en esta máquina; el servidor estaba al ≈ 75 % de un núcleo y la cola casi vacía. **El techo medido es el de JMeter+CPU compartida, no el del servidor** (el generador open-loop sostuvo 5.000/s hasta 14 min y 16.000/s en pruebas cortas).
2. **Antes vs. después a igual carga ofrecida (10 min, Postgres):** el throughput es el mismo (JMeter limita), el p99 baja de 57 a 48 ms y el p99.9 de 207 a 109 ms, pero esas diferencias de latencia **no son concluyentes** (una repetición por corrida, variación de hasta 3× en este entorno). **Sí son grandes y coherentes con las mediciones previas** las del motor: RSS máx. 1.880 → 774 MiB, heap 1.616 → 660 MiB, pausa de GC máx. 675 → 13 ms.
3. **Reinicio en caliente (1,7 M registros):** con el binario antiguo el lag del historial llegó a 20.587 registros durante el re-escaneo (≈ 14 s) y a 1.001 con el nuevo; el efecto en JMeter es moderado (p99 62 vs 49 ms) porque el journal es chico. Con 7,5 M registros (generador open-loop) fue 58 errores y p99 de 2,45 s.
4. **30 minutos:** ver §6.3. Sin Postgres y en disco virtual (§7).
5. **Estrés (256 hilos, sin tope):** el máximo (≈ 4,5 mil órd/s) es otra vez el techo de JMeter; no hubo ni un 503 porque con 256 conexiones la cola de 10.000 no puede llenarse (el backpressure se prueba con `drill-overload.sh`: 307.282 rechazos 503 y 0 inconsistencias).
6. **Errores:** 0 en todas las corridas, incluidas las validaciones de negocio (coherencia de trades, límites de precio, libro no cruzado, saldos no negativos y **conservación de valor al final**).

### 6.3 La prueba de 30 minutos y lo que reveló
**Primera corrida (antes de C6) — se degradó [HECHO].** 8,69 M órdenes, 0 errores, 4.829 órd/s de media, pero el throughput bajó por bloques de 5 min de 5.456 a **4.140 órd/s**, el p99 mediano de 23 a **87 ms** y el p99 del matching por lote de 1,9 a **29,6 ms**. Con una repetición de 15 min con `gctrace` y perfiles de CPU se descartó el GC (2 % de la CPU) y el fsync, y se verificó la causa: **`Book.Depth` consumía el 18,4 % de toda la CPU dentro del secuenciador** (recorría todas las órdenes de los niveles consultados; el plan pide `GET /book` en el 1 % de las iteraciones). Ver `docs/OPTIMIZACION.md` §8.

**Segunda corrida (con C6) [HECHO]** — `jmeter/results/20261026-151602-sostenida-depthfix-30min-sinpg`, inicio 15:16 UTC, commit `1659909`, 128 hilos, objetivo 5.000 órd/s, **sin PostgreSQL**, disco virtual, terminó con `rc=0` sin abortar: **9.603.174 órdenes, 0 errores, 5.334 órd/s de media, p50 3 · p90 10 · p95 15 · p99 30 · p99.9 65 ms, máx. 1,56 s**. Por bloques de 5 min el throughput fue 5.144 · 5.112 · 5.341 · 5.319 · 5.580 · 5.512 órd/s y el p99 del matching por lote 0,6–0,8 ms: **sin degradación**. El 82 % de las ventanas de 5 s alcanzó ≥ 4.900 órd/s (mínimo 3.341).

**Lo que esto SÍ y NO demuestra.**
- Sí: con carga ofrecida de 5.000 órd/s, el sistema (sin Postgres) aceptó **más de 5.000 órd/s de media durante 30 minutos** con 0 errores y con la validación de negocio de cada respuesta, y la degradación con el crecimiento del libro desapareció.
- No: **no demuestra 5.000 órd/s sostenidas con PostgreSQL** (con Postgres JMeter satura en ≈ 4.300 y no se pudo hacer la corrida larga por disco), **ni en almacenamiento real**, ni con el generador en otra máquina. El 18 % de las ventanas de 5 s por debajo de 4.900 se debe sobre todo a las **pausas del snapshot (hasta 1,5 s)**, que siguen sin corregirse de fondo (L21). Al terminar, `GET /audit` con ≈ 3 M de órdenes en el libro tardó **1,95 s** (L17: corre dentro del secuenciador).
- El disco libre llegó a 0,47 GiB según la métrica del servidor: esa corrida estuvo cerca de llenar el disco del entorno.

Informe visual con todas las series (throughput, p50/p90/p95/p99, distribución, errores, hilos, cola, GC, heap/RSS, snapshots): **[`jmeter/report/index.html`](../jmeter/report/index.html)**; CSV fuente en `jmeter/results/*/`.

## 7. Pruebas aprobadas, fallidas, pendientes y limitaciones

### 7.1 Validación final ejecutada sobre el commit entregado [HECHO]
Commit: `1659909` (+ cambios sin commitear del informe) · rama `claude/focused-babbage-2ppmql` · fecha 2026-10-26T16:04Z

| # | Comando | Duración | Resultado | Log |
|---|---|---|---|---|
| 1 | gofmt -l . (debe estar vacío) y go vet ./... | 0 s | **PASA** | `docs/evidencia/validacion/01.log` |
| 2 | TEST_DATABASE_URL=…/orderbook_test go test -race -count=1 ./...  (incluye integración con PostgreSQL real) | 34 s | **PASA** | `docs/evidencia/validacion/02.log` |
| 3 | ./scripts/test-alerts.sh (promtool: 13 reglas, 26 casos) | 3 s | **PASA** | `docs/evidencia/validacion/03.log` |
| 4 | ./scripts/verify-all.sh (tests, alertas, stack, smoke, carga, drills de crash/Postgres/sobrecarga/idempotencia/instancia única) | 99 s | **PASA** | `docs/evidencia/validacion/04.log` |
| 5 | ./jmeter/run-jmeter.sh integridad  (WITH_HISTORY=true: 25 pasos, incluye el historial en Postgres) | 7 s | **PASA** | `docs/evidencia/validacion/05.log` |
| 6 | ./jmeter/check-recovery.sh  (carga JMeter, SIGKILL, reinicio, mismos saldos, integridad) | 62 s | **PASA** | `docs/evidencia/validacion/06.log` |
| 7 | ./scripts/drill-idempotency.sh  (ventana, reintentos y reinicios con otra ventana) | 2 s | **PASA** | `docs/evidencia/validacion/07.log` |

`verify-all` (14 pasos) cubre: tests con `-race`, alertas, arranque desde cero, *smoke* de 22 comprobaciones, carga a 5.000/s con auditoría, **crash real de la app (`kill -9`)**, segundo contenedor sobre el mismo volumen, **caída de Postgres**, `kill -9` en plena carga, **sobrecarga/backpressure**, **idempotencia con ventana** y **instancia única**. Los *drills* y sus resultados están en `docs/INFORME_PRUEBAS.md` §3.

### 7.2 Fallidas o interrumpidas durante el trabajo (no se ocultan) [HECHO]
| Qué | Qué pasó | Estado |
|---|---|---|
| Soak original de 22 min | El contenedor del entorno se reinició a los 10 min (infraestructura del sandbox, no la app). Se repitió una corrida de 12 min sobre el estado grande. | Parcial: 10 + 12 min |
| `verify-all` con el disco lleno | `panic: fsync del journal falló: no space left on device` (fail-stop **correcto**); el resultado se descartó y se repitió con espacio. | Repetido y aprobado |
| Soak de 17 min con Postgres | Cortado a los 10 min por la guarda de disco de `soak.sh`. | **Interrumpido**: no cuenta como 17 min |
| Primeras corridas JMeter de la batería | Abortadas a los ≈ 7 min por la guarda de disco (Grafana ocupaba espacio con el driver `vfs`); se repitieron sin Grafana. | Descartadas |
| `check-recovery.sh` (primer intento) | Los totales y saldos coincidieron tras `SIGKILL`, pero el **plan de integridad falló** porque asumía un libro aislado y la carga previa dejó ofertas que se cruzaban con su banda de precios. Se corrigió el plan (elige su banda de precios en el hueco del libro y falla con «PRECONDICIÓN NO CUMPLIDA» si no lo hay). | Corregido, ver §7.1 |
| JMeter *dashboard* estándar | No se generó: exigía columnas que se omitieron para ahorrar disco. El informe propio (`jmeter/report/`) cubre las gráficas. | No entregado |

### 7.3 Pendientes (no se marcan como aprobadas) [LIMITACIÓN]
- **Prueba de ≥ 30 min con PostgreSQL activo** y **en almacenamiento real**: no se pudo (el sandbox ofrece disco virtual y ≈ 5 GB libres; con Postgres hacen falta ≈ 10 GB). La de 30 min se hizo **sin Postgres y en disco virtual**.
- **Demostrar 5.000 órd/s sostenidas con JMeter:** no demostrado (techo de JMeter en este hardware). Con el generador open-loop, hasta 14 min.
- Medir el costo real del `fsync` y del *group commit* (disco virtual: 0,4 ms).
- Windows / Docker Desktop: los scripts y planes no se ejecutaron allí (solo el *build* y Grafana, por el usuario).
- Generador de carga en una máquina distinta de la del servidor.
- Los tests de integración con Postgres requieren `TEST_DATABASE_URL` (apuntando a una base cuyo nombre contenga `test`); se ejecutaron en §7.1.

### 7.4 Limitaciones conocidas del sistema
Ver `docs/DECISIONES.md` (L2–L22) y §8. Las más importantes: sin autenticación (L16), un solo nodo/disco (L3/L6), el journal y el historial no rotan (L13/L19), pausa del snapshot ∝ órdenes en el libro (L21), lecturas pesadas dentro del secuenciador (L17).


## 8. Riesgos restantes y decisiones que impedirían aprobar producción

| # | Riesgo | Gravedad para producción | Decisión pendiente / recomendación |
|---|---|---|---|
| 1 | **Sin autenticación ni autorización:** cualquiera puede depositar y cancelar órdenes ajenas (`DELETE` no comprueba dueño) — L16 | **Bloqueante** | [RECOMENDACIÓN] identidad en el borde (mTLS/JWT) y comprobar propietario; depósitos solo por un servicio interno |
| 2 | **Un solo disco, un solo nodo:** perder el volumen = perder el journal — L3/L6 | **Bloqueante** | [RECOMENDACIÓN] réplica en espera que consuma el mismo journal, o log replicado (Kafka/Raft); copias periódicas |
| 3 | **El journal no rota** (≈ 150 GB/día a 5.000/s) y el historial en Postgres tampoco tiene retención — L13/L19 | **Bloqueante** a medio plazo | [RECOMENDACIÓN] segmentos rotados + truncado tras el snapshot; particionado/retención en Postgres |
| 4 | **Pausa del snapshot O(órdenes en el libro)** y lecturas pesadas (`/audit`, `Depth`) dentro del secuenciador — L17/L21 | Alta (cola de latencia) | [RECOMENDACIÓN] generar los snapshots desde una réplica de seguimiento (duplica memoria) y sacar las lecturas del hilo único |
| 5 | **Rendimiento no demostrado en condiciones de producción:** sin prueba de ≥ 30 min con Postgres, sin disco real, generador en la misma máquina — L10/L22 | Alta | repetir en hardware dedicado, con Postgres y el generador en otra máquina (§6, §7) |
| 6 | Memoria: ≈ 3 GiB de pico con 5 M ids en la ventana; hay `GOMEMLIMIT`/`mem_limit` pero no se validó el comportamiento al tocar el límite — L20 | Media | dimensionar con datos reales de carga |
| 7 | Un solo instrumento, un solo mercado — L4 | Media | particionar por instrumento |
| 8 | Sin trazas distribuidas (hay métricas, logs con request-id y alertas) — L11 | Baja | OpenTelemetry |
| 9 | `JOURNAL_NOSYNC` existe (para tests): activarlo en producción rompe la garantía de durabilidad | Media (error operativo) | [RECOMENDACIÓN] eliminarlo del binario de producción o exigir una bandera explícita |
| 10 | Auto-operación entre pares: *self-trade* permitido (D9); sin límites por usuario ni *rate limiting* | Media | decisión de producto |

## 9. Preguntas difíciles de entrevista (y respuestas fundamentadas)

**1. «¿Por qué un solo hilo si piden 5.000 órdenes/s? ¿No es un cuello de botella?»**
El perfil de CPU a 8.000 órd/s mostró matching+billeteras en **2,3 %**; el 33 % es red, 14 % fsync, 13 % JSON, 14 % GC. Serializar lo barato es lo correcto, y mantiene el orden determinista. La capacidad medida con el generador *open-loop* fue **16.000 órd/s** en una máquina de 4 núcleos compartida. El techo real es el CPU de red/JSON, que escala con réplicas stateless; el secuenciador escala particionando por instrumento. [HECHO: `docs/INFORME_PRUEBAS.md` §2]

**2. «¿Cómo garantizas que no pierdes una orden confirmada?»**
`processBatch` solo responde **después** del `fsync` del lote. Si hay un error de escritura o de fsync, hace `panic`. Al reiniciar se reconstruye desde el journal. `kill -9` en plena carga: totales idénticos antes y después (10 repeticiones). **Límite:** asume que el disco cumple `fsync`; no hay réplica (L6).

**3. «¿Qué pasa si el cliente recibe un timeout?»**
Si la orden aún no había empezado, **no se ejecuta** (se descarta y se cuenta). Si ya estaba en marcha, puede ejecutarse; el reintento con el mismo `id` devuelve 409 y `GET /orders/{id}` dice su estado. Antes de la corrección (D69) 50 de 50 órdenes con timeout se ejecutaban igual.

**4. «¿Cómo evitas duplicados?»**
Con el `id` que envía el cliente: una ventana de los últimos 5 M ids aceptados (por cantidad, para que el replay sea idéntico) y, si el id salió de la ventana pero la orden **sigue en el libro**, el libro también responde 409. **Límite honesto:** un reintento *tras* 5 M órdenes de una orden ya ejecutada y fuera del libro se aceptaría como nueva (≈ 17 min a 5.000/s; un reintento real ocurre en segundos).

**5. «¿Cómo sabes que los saldos cuadran?»**
`CheckInvariants` (conservación + no negativos + reservas == órdenes abiertas) corre dentro del secuenciador en `/audit`; se usa en tests de propiedades, en cada drill y en el `tearDown` de JMeter. Además `/audit/history/{user}` recalcula el saldo desde el historial de Postgres y lo compara con el del motor.

**6. «¿Qué pasa si Postgres se cae en plena carga?»**
El matching no se entera (el journal es la fuente de verdad); `/orders` y `/trades` dan 503; el proyector reintenta con backoff y, al volver, reconcilia hasta `lag=0`. Verificado con `drill-postgres-down.sh` y con un test de integración contra Postgres real.

**7. «Tu soak original mostró p90 de 24 s. ¿Qué fue?»**
No era el régimen estable: fue **un minuto tras un reinicio**. El proyector releía y decodificaba el journal entero (7,5 M registros, ≈ 60 s, 2–3 núcleos, 9 GB de basura), el sistema bajó a 2.005 órd/s y se acumuló un atraso de ≈ 33 s que el generador (que mide desde la hora planificada) contó. Lo reproduje (58 errores 504, p99 2,45 s), lo corregí con reanudación por offset y verificación del último registro, y repetí: 0 errores, p99 0,58 s. Documentado en `docs/OPTIMIZACION.md`.

**8. «¿Por qué el RSS es 3,7 GiB si lo vivo es 0,7 GiB?»**
Lo vivo en carga era 0,6–1,6 GiB (el 0,7 era un perfil justo tras restaurar). RSS ≈ `heap_sys − devuelto al SO + ~40 MiB`; con `GOGC=100` el heap llega a ≈ 2× lo vivo, y cada snapshot generaba ≈ 930 MiB de basura de `json.Marshal` (ahora ≈ 46 MiB). Con `GOMEMLIMIT=3GiB` y `mem_limit=4g` bajó el pico. [HECHO: muestreo cada 5 s en `docs/evidencia/antes/e1_memoria.log`]

**9. «¿Qué domina la latencia de cola ahora?»**
La pausa del snapshot: p99 12 ms **sin** snapshots vs 101–342 ms con uno cada 200.000 registros y 47 ms cada 1.000.000 (A/B intercalado). La descomposición por etapas lo confirma: matching p99 0,45 ms, fsync 3,3 ms, **cola p99.9 ≈ 95–235 ms**. [HIPÓTESIS residual] el p99.9 que queda sin snapshots (~250 ms) es GC y entorno compartido; no se aisló.

**10. «¿Por qué no eliminaste la pausa del snapshot?»**
Porque la solución de fondo (generar los snapshots desde una réplica de seguimiento) duplica la memoria y es un cambio de arquitectura sin una decisión del equipo. Solo la espacié (configurable) y documenté el costo: hasta ≈ 9 s más de replay tras un crash.

**11. «¿Qué mide JMeter y qué no?»**
JMeter valida **respuesta a respuesta** el negocio (coherencia de la orden, trades, límites de precio, libro no cruzado, conservación de valor al final). Es de lazo cerrado: la latencia que reporta excluye el atraso por pausas del servidor. Por eso las cifras del `loadgen` *open-loop* son más altas y más honestas respecto al cliente que espera. Y en este entorno JMeter consume más CPU que el servidor: su techo (§6) no es el del sistema.

**12. «¿Es seguro exponer esto?»**
No. No hay autenticación y `DELETE /orders/{id}` no comprueba propietario (L16). Aceptado por el alcance del desafío; es el bloqueante número 1 para producción (§8).

**13. «¿Se puede escalar horizontalmente?»**
La API sí es stateless respecto al motor *si* hubiera un secuenciador remoto; hoy API y secuenciador viven en el mismo proceso. Un instrumento = un secuenciador; más instrumentos = más secuenciadores (particionado). Más throughput por instrumento exige reducir el costo de JSON/red, no del matching.

**14. «¿Qué haces con el crecimiento del journal?»**
Hoy nada: no rota (≈ 150 GB/día a 5.000/s). Los snapshots ya permiten truncar lo anterior; falta implementar segmentos y la retención en Postgres (L13/L19). Lo digo como riesgo, no como característica.

**15. «¿Qué probaste que de verdad encontró bugs?»**
Una petición de 400 KB tumbaba el servicio (id sin límite → registro de journal > 1 MiB → `panic`); una orden legítima que cruzaba ≈ 6.100 órdenes también; `GET /book` con 50.000 niveles bloqueaba el motor 0,8 s; el proyector podía reflejar órdenes que el journal perdería tras un corte; dos instancias corrompían el journal; el proyector releía todo el journal al arrancar. Todos tienen test de regresión (con mutación que lo hace fallar). Ver `docs/INFORME_PRUEBAS.md` §4.

**16. «¿Qué descubrió la prueba de 30 minutos?»**
Que el sistema **se degradaba al crecer el libro**: 5.456 → 4.140 órd/s y p99 de 23 → 87 ms. No era el GC (2 % de la CPU, medido con `gctrace`) ni el fsync (2,4 → 4,1 ms): un perfil de CPU a los 12 min mostró `Book.Depth` con el **18,4 % de toda la CPU**, ejecutándose dentro del secuenciador y recorriendo todas las órdenes de los niveles consultados (el plan de JMeter pide el libro en el 1 % de las iteraciones). Mantuve la cantidad pendiente por nivel de forma incremental (O(niveles)), con un test de escala (×62 → ×0,8) y otro de consistencia con mutación; repetí los 30 minutos: 5.334 órd/s de media, p99 del matching 0,6–0,8 ms, sin degradación. **Lo que no puedo afirmar:** que sea así con PostgreSQL o en disco real (§7).

**17. «¿Por qué tu informe dice que JMeter no demuestra 5.000/s con Postgres?»**
Porque en esta máquina de 4 núcleos JMeter —validando cada respuesta con Groovy— consume ≈ 2,2 núcleos y satura en ≈ 4,3 mil órd/s mientras el servidor está al ≈ 75 % de un núcleo y la cola casi vacía. Es un límite del generador, no del servidor; lo digo en vez de presentar 4.300 como la capacidad del sistema. La única corrida larga que supera los 5.000 de media es la de 30 min sin Postgres.

## 10. Qué funciona, qué mejoró y qué falta

**Funciona (verificado).** Matching precio-tiempo con liquidación exacta (incluida la devolución de la mejora de precio), reservas y cancelación; idempotencia por `id`; durabilidad con group commit; recuperación tras `kill -9`, cola truncada y snapshots inservibles; Postgres como modelo de lectura que se reconcilia solo; candado de instancia única; backpressure; observabilidad (Prometheus, Grafana de 20 paneles, 13 alertas con 26 pruebas unitarias); `verify-all` de 14 pasos; plan JMeter de integridad (25 pasos con importes exactos; probado con una mutación del plan que lo hace fallar), de carga con validación de negocio por respuesta y de recuperación tras `SIGKILL` (`jmeter/check-recovery.sh`).

**Mejoró (con números y condiciones en §6).** Arranque en caliente bajo carga; basura generada (−68 %); ciclos de GC (−73 %); memoria por snapshot (932 → 46 MiB); la degradación en 30 minutos causada por `Book.Depth` (corregida y repetida: sin degradación); y se entiende por qué queda cola de latencia: la pausa del snapshot.

**Falta (explícito).** Autenticación; réplica/HA; rotación del journal y retención del historial; corrección de fondo de la pausa del snapshot; prueba de ≥ 30 min con Postgres y en disco real; medir el fsync real; validar en Windows; un generador de carga en una máquina distinta. **Esto no es una entrega lista para producción:** es una entrega lista para **revisión**, con sus límites a la vista.
