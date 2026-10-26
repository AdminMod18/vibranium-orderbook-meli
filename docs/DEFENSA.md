# Guía de defensa — Order Book de Vibranium

Documento vivo: se completa fase a fase. Objetivo: poder explicar **qué**, **por qué** y **qué pasa si falla**.

## Fase 1 — Motor de matching (`internal/engine`)

### Cómo funciona un libro de ofertas
- **Bids** (compras) y **asks** (ventas) viven en lados separados. Una orden *límite* dice "hasta Qty a Price o mejor".
- Al llegar una orden (**taker**) se cruza contra el lado opuesto (**makers**) mientras los precios sean compatibles:
  compra cruza si `ask <= precio_compra`; venta cruza si `bid >= precio_venta`.
- **Prioridad precio-tiempo**: primero el mejor precio; a igual precio, la orden más antigua (FIFO).
- **Precio de ejecución = precio del maker** (el que ya estaba en el libro). El taker puede salir mejor que su límite.
- **Fills parciales**: lo no ejecutado queda "en negociación" (resting) en el libro. Una orden pasa por
  `OPEN → PARTIALLY_FILLED → FILLED` o `CANCELLED`.
- Un libro sano **nunca está cruzado**: mejor compra < mejor venta (el test aleatorio lo verifica).

### Decisiones de diseño
| Decisión | Motivo |
|---|---|
| Precio y cantidad como `int64` (centavos / unidades) | Sin `float`: los saldos cuadran exacto. Límites `MaxPrice`/`MaxQty` = 1e9 garantizan que `Price*Qty` no desborda. |
| Estructura: mapa precio→cola FIFO + heap de precios | Mejor precio en O(1), insertar nivel nuevo O(log n), cancelar O(1) vía índice `resting`. |
| `Book` **no es thread-safe** a propósito | El orden de llegada define el resultado, así que el matching es secuencial. Se escala haciéndolo rápido (memoria, sin I/O) y con un único goroutine; la concurrencia se maneja antes (API + cola). Evita locks y contención. |
| `Seq` por contador, no por reloj | Determinista: mismo log de entrada ⇒ mismo libro y mismos trades (clave para replay/recuperación). |

### Números
`BenchmarkSubmit` ≈ 0,9 µs/orden en un hilo ⇒ ~1M órdenes/s de capacidad del motor vs. 5.000/s exigidos.
El cuello de botella real será el I/O (log, base de datos), no el matching.

### Preguntas probables
- *¿Por qué un solo hilo y no concurrencia?* Porque dos órdenes simultáneas necesitan un orden total; paralelizar obliga a locks y no gana nada cuando el matching tarda ~1 µs.
- *¿Cómo escalarías más allá?* Particionando por instrumento (un motor por mercado). Dentro de un mismo mercado el orden sigue siendo secuencial.
- *¿Por qué precio del maker?* Es la convención estándar: respeta la oferta publicada y da mejora de precio al taker.
- *¿Qué pasa si compra y venta son del mismo usuario (self-trade)?* **Pendiente de decidir**: hoy se permite; en producción se previene (wash trading).

## Fase 2 — Billeteras, reservas y liquidación (`internal/wallet`, `internal/exchange`)

### El problema que resuelve
Un bot manda 1.000 compras de R$ 50 con R$ 100 de saldo. Si el saldo solo se mira al ejecutar, gasta el mismo dinero varias veces.
**Solución: reservar al ingresar la orden.**

| Evento | Efecto en la billetera |
|---|---|
| Orden de compra entra | `BRLFree -> BRLHeld` por `Price*Qty` |
| Orden de venta entra | `VIBFree -> VIBHeld` por `Qty` |
| Sin saldo libre | La orden se rechaza y **ni toca el libro** (`ErrInsufficientFunds`) |
| Trade a precio P (cantidad q) | Comprador: sale `L*q` de lo reservado, `P*q` se paga y `(L-P)*q` vuelve a libre; recibe `q` VIB. Vendedor: sale `q` VIB reservado y recibe `P*q` BRL libre. (`L` = límite de compra) |
| Cancelación | Se libera lo reservado de la cantidad no ejecutada |

Una orden = 3 pasos en un mismo goroutine: **1) reservar, 2) cruzar, 3) liquidar**. Nadie ve estados intermedios.

### Invariantes (la "prueba de que los saldos cuadran")
1. **Conservación**: la suma de BRL y de VIB de todo el sistema nunca cambia (sin depósitos/retiros). `Ledger.Totals()`.
2. **Sin negativos** en ninguna billetera.
3. **Reservas == órdenes abiertas**: lo `Held` de cada usuario es exactamente `sum(Price*Remaining)` de sus compras abiertas y `sum(Remaining)` de sus ventas abiertas. `Exchange.CheckInvariants()`.

`TestRandomizedConservation` los verifica tras **cada** operación de una simulación caótica (cancelaciones, parciales, rechazos).
Esto es lo que se mostrará al evaluador al terminar su prueba de carga.

### Decisiones
- **Idempotencia por `order_id`**: un id ya procesado (aunque haya quedado FILLED y salido del libro) se rechaza (`ErrDuplicateOrder`). Un reintento del cliente no duplica una orden. En Fase 4 este set pasa a persistirse.
- **Liquidación que falla = `panic`**: si `Settle` detecta una inconsistencia es un bug, no un error de usuario. Es preferible detener el proceso (y recuperarlo desde el log) a seguir con saldos corruptos. `Settle` comprueba precondiciones antes de mutar, nunca deja una liquidación a medias.
- **Self-trade**: permitido y consistente (el patrimonio no cambia, hay test). Prevenirlo es una mejora de producto (anti wash-trading).
- **Sin fees** en el MVP. Se añadirían como una tercera pata en `Settle` hacia una cuenta de la plataforma.

### Preguntas probables
- *¿Y si el servicio muere entre reservar y cruzar?* Hoy todo es memoria, no hay estado a medias. En Fase 4 el log de órdenes es la fuente de verdad: al reiniciar se reproduce y se reconstruye libro y saldos de forma determinista.
- *¿Por qué reservar y no validar al ejecutar?* Porque una orden puede esperar en el libro minutos; sin reserva, otras órdenes gastarían ese saldo y el trade fallaría en la liquidación.
- *¿Por qué devolver la diferencia al comprador?* Reservó a su límite pero pagó el precio del maker.

## Fase 3 — API y secuenciador (`internal/sequencer`, `internal/api`, `cmd/server`)

### Cómo se maneja "miles de órdenes en el mismo milisegundo"
```
  N requests HTTP concurrentes (goroutines)
          │  cada una: valida JSON -> crea un comando -> intenta ENCOLAR (no bloqueante)
          ▼
   [ cola acotada (chan) ]  ── llena? ──► 503 + Retry-After  (backpressure)
          │
          ▼
   UN goroutine "secuenciador": toma comandos de a uno, en orden, y ejecuta Exchange
          │  (reservar -> cruzar -> liquidar), sin locks
          ▼
   responde por un canal propio de cada comando -> la request HTTP devuelve el resultado
```
- **El orden de la cola ES el orden total del libro**: define quién llegó primero (prioridad de tiempo).
- Las goroutines HTTP son baratas y paralelas; lo único serializado es lo que *debe* serlo.
- **Backpressure**: encolar nunca bloquea; con la cola llena se responde `503` al instante. Una cola infinita escondería la sobrecarga y acabaría en OOM.
- **Responder sin bloquear**: el canal de respuesta tiene buffer 1, así el secuenciador jamás se frena por un cliente lento.

### Endpoints
| Método y ruta | Uso | Códigos |
|---|---|---|
| `POST /orders` | `{id?, user_id, side: BUY\|SELL, price, qty}` → orden + trades | 201, 400, 409 (id repetido), 422 (sin saldo), 503 (cola llena) |
| `DELETE /orders/{id}` | Cancela y libera la reserva | 200, 404 |
| `GET /balances/{user}` | Saldos libre/reservado en BRL y VIB | 200 |
| `POST /deposits` | `{user_id, brl, vib}` siembra saldos (solo para pruebas) | 204, 400 |
| `GET /book?depth=N` | Niveles agregados de compras y ventas | 200 |
| `GET /audit` | Verifica invariantes y totales | 200 / 500 |
| `GET /healthz` | Liveness | 200 |

Precios en **centavos**, cantidades en **unidades**. El `id` de la orden es la clave de idempotencia.

### Apagado ordenado (`cmd/server`)
SIGTERM → 1) `srv.Shutdown` deja de aceptar y espera las requests en vuelo → 2) `seq.Stop()` vacía la cola → salir.
Ninguna orden aceptada se pierde por un deploy.

### Preguntas probables
- *¿Por qué no un lock sobre el libro?* Con locks, miles de goroutines compiten y el orden de adquisición no es el de llegada; la cola da orden explícito y evita contención.
- *¿Qué pasa si hay más de 5.000 órdenes/s?* El secuenciador procesa ~1M/s; el límite real será red/HTTP. Si se satura, 503 con `Retry-After`, nunca pérdida silenciosa.
- *¿Y si la API tiene varias réplicas?* Son stateless y todas hablan con el mismo secuenciador. Hoy comparten proceso; en la Fase 4 el secuenciador se alimenta de un log y la API puede separarse.
- *¿El cliente puede reintentar sin riesgo?* Sí si envía `id`: un reintento da `409` en vez de duplicar. Limitación L2: tras un timeout no ve el resultado original (ver `DECISIONES.md`).
- *¿Qué hace el `panic` por inconsistencia dentro del secuenciador?* Detiene el proceso; el orquestador lo reinicia y la Fase 4 reconstruye el estado desde el log.

## Fase 4a — Durabilidad y recuperación (`internal/journal`)

### Cómo funciona
```
 comando ──► secuenciador ejecuta en memoria ──► Append al buffer del journal
                  (lote de hasta 1024 comandos)          │
                                                         ▼
                                     UN fsync para todo el lote  (group commit)
                                                         │
                                          recién ahora se responde a cada cliente
```
- **Regla de oro: nunca se confirma algo que no esté en disco.** Si el proceso muere antes del fsync, el cliente no recibió confirmación y el log no tiene la orden: coherente.
- **Recuperación**: al arrancar se lee el journal y se re-ejecuta cada registro sobre un Exchange vacío. Como el motor es determinista, el resultado es idéntico (el test compara cuentas y libro completos). Además se **verifica cada trade** contra lo registrado.
- **Escritura truncada** (caída a mitad de un registro): el CRC la detecta; se descarta SOLO la cola incompleta y se sigue escribiendo. **Corrupción en medio del log**: error y no arranca (nunca se descartan datos confirmados en silencio).

### Qué pasa si falla...
| Falla | Efecto | Cómo se recupera |
|---|---|---|
| Se cae el proceso (`kill -9`, OOM) | Se pierde la memoria, no el journal | Reinicio: snapshot + cola (~1 s) o replay completo (~7-9 s por millón de registros). Probado en `scripts/drill-kill9.sh` |
| Corte de energía a mitad de escritura | Registro final incompleto | CRC lo detecta, se descarta esa cola; esas órdenes nunca se confirmaron |
| Falla `write`/`fsync` (disco lleno/dañado) | No se puede garantizar durabilidad | `panic` (fail-stop): mejor caído que mentir; se reinicia desde lo durable |
| Corrupción real del journal | Estado dudoso | El servidor se niega a arrancar y avisa; requiere intervención humana |
| Se pierde el disco | Se pierde el log | **Limitación L6**: en producción, volumen replicado/standby (ver `MEJORAS.md`) |
| La API cae pero el secuenciador no | Sin entrada de requests | Réplicas stateless detrás de un balanceador |

### Números para citar
- Group commit medido: ~22 comandos por fsync con 50 clientes concurrentes (sube con la carga).
- Recuperación: ~640-720 registros en 5-6 ms.
- Drill `kill -9` en plena carga: totales BRL/VIB idénticos antes y después, invariantes OK.

### Preguntas probables
- *¿Por qué no guardar cada orden en una base de datos?* Un round-trip por orden con consistencia fuerte no llega a 5.000/s con margen y obliga a transacciones sobre el libro. El log secuencial + memoria es órdenes de magnitud más rápido y da el mismo resultado.
- *¿Y por qué no Kafka?* Es el siguiente paso natural (mismo patrón, log replicado), pero para un MVP es overengineering. El journal está detrás de una interfaz pequeña (`Append/Sync/Reader`) para poder cambiarlo; ver `MEJORAS.md`.
- *¿Qué garantiza que el replay dé lo mismo?* Determinismo: sin reloj, sin aleatoriedad, un solo hilo, ids asignados por la API y guardados en el log; y la verificación trade a trade.
- *¿`fsync` por lote no retrasa?* Los comandos del lote esperan unos ms, pero el throughput sube órdenes de magnitud; la latencia es la de un fsync, no la de N.
- *¿Cuánto tarda recuperar con mucho histórico?* Sin snapshot es lineal: **medido 7 s para 816.904 registros** (≈110.000 registros/s). Con snapshot + cola: **~1 s**, y depende del tamaño del estado, no del historial (ver Fase 6).

## Fase 4b — Historial y trazabilidad (`internal/history`)

### Arquitectura
```
 journal (fuente de verdad, fsync) ──► Proyector (lee el archivo, aplica lotes en 1 transacción) ──► Postgres
                                                                                                   │
                         GET /orders/{id} · /orders · /trades · /audit/history/{user} ◄────────────┘
```
- **El mercado no depende de Postgres.** Probado: con Postgres caído el matching siguió y el historial respondió `503`; al volver se puso al día solo (`scripts/drill-postgres-down.sh`).
- **Exactly-once efectivo**: lote + `last_seq` en una transacción. Reiniciar el proyector (probado) no duplica ni pierde.
- **Trazabilidad demostrable**: `balances_from_history` (depósitos + trades) == billetera del motor, para todos los usuarios (test de integración con 10.000 registros).
- **Tablas**: `orders` (estado actual), `trades` (inmutable, append-only), `deposits`, `projector_state`.

### Verificado con Docker real
| Prueba | Resultado |
|---|---|
| `docker compose up`: Postgres + app, ambos `healthy` | ✅ la app arrancó **antes** que Postgres, reintentó y se conectó sola (D40) |
| Crash real: `kill -9` al proceso de la app con carga (`scripts/drill-docker-crash.sh`) | ✅ Docker la reinició (`RestartCount` sube), recuperó 189 registros en 1,5 ms, totales intactos |
| `docker compose stop postgres` en pleno uso (`scripts/drill-postgres-down.sh`) | ✅ matching `201`, historial `503`; al volver, `lag=0` y consistente con el motor |
| SIGTERM (apagado ordenado) | ✅ la app sale limpia (código 0) |

**Detalle útil:** `docker kill` cuenta como parada *manual* y Docker **no** aplica la política de reinicio; para simular un fallo real se mata el *proceso* desde el host (lo que haría el OOM killer). Quien pruebe con `docker kill` verá que la app no vuelve y pensará que está mal: no lo está.

### Una historia para contar (hallazgo real)
Un drill falló: el Postgres conservaba `last_seq=10012` de otra corrida, el journal era nuevo y el proyector **se saltó en silencio** los primeros 10.012 registros. Lección: *un read model debe estar atado a la identidad de su fuente*. Solución: registro `genesis` con UUID en el journal y vínculo en `projector_state`; si no coinciden, el proyector se detiene, avisa en logs y se reporta no saludable. Quedó cubierto con un test.

### Qué pasa si falla...
| Falla | Efecto | Recuperación |
|---|---|---|
| Postgres caído | Historial `503`; **matching intacto** | Proyector reintenta con backoff (0,2 s → 5 s) y se pone al día |
| Proyector reiniciado | Pausa del historial | Retoma desde `last_seq`, sin duplicados |
| Postgres perdido por completo | Se pierde el historial, no el dinero | Se reconstruye leyendo el journal desde cero |
| Postgres y journal de historias distintas | Riesgo de historial falso | `ErrJournalMismatch`: se niega a proyectar |
| Lag alto del proyector | Consultas desactualizadas | Métrica de lag + alerta (Fase 5) |

### Preguntas probables
- *¿Por qué no escribir en Postgres desde el secuenciador?* Acopla la latencia y la disponibilidad del mercado a la DB, y obliga a transacciones por orden. Así el matching solo paga un fsync por lote.
- *¿Y si el historial dice algo distinto al motor?* El motor manda (el journal es la verdad). `GET /audit/history/{user}` lo contrasta; con `lag=0` deben coincidir.
- *¿Cómo escalaría el historial?* Particionar tablas por tiempo, réplicas de lectura, y/o sustituir el proyector por un consumidor de Kafka.
- *¿Por qué eventualmente consistente?* Para desacoplar; la respuesta incluye `as_of_seq` y el lag es de milisegundos.

## Fase 5 — Observabilidad (`internal/metrics`, `ops/`)

Detalle completo, catálogo de métricas, alertas y mini-runbook en [OBSERVABILIDAD.md](OBSERVABILIDAD.md).

### Cómo contarlo
- **Tres pilares**: métricas (Prometheus), logs estructurados con `request_id`, y perfiles (`pprof`). Trazas distribuidas: siguiente paso.
- **Cada métrica responde una pregunta operativa**, no hay métricas "por si acaso".
- **Lo que más importa en este sistema**: profundidad y espera en cola (¿saturado?), duración del fsync (¿disco?), tamaño de lote (¿group commit funciona?), lag del proyector (¿historial al día?).
- **Backpressure visible**: 503 contados y alertados; nunca un error silencioso.
- **Una demo potente**: tumbar Postgres con carga y ver en Grafana cómo el lag del historial sube y vuelve a cero mientras las órdenes siguen entrando.

### Preguntas probables
- *¿Por qué Prometheus y Grafana?* Son capas distintas (almacenar/alertar vs. visualizar); estándar abierto; si usan otra plataforma las métricas siguen sirviendo.
- *¿Cómo evitas que las métricas exploten en cardinalidad?* Etiquetas acotadas: rutas por patrón, resultados en un conjunto fijo; hay un test.
- *¿Qué mirarías si p99 sube?* Espera en cola vs fsync: dice si es CPU del matching o el disco.
- *¿Mediste 5.000 órdenes/s?* Sí, con generador open-loop (sin coordinated omission): 150.000 órdenes, 100 % aceptadas, p99 16-29 ms en condiciones limpias, saldos auditados. Con otra actividad pesada en la misma máquina las colas empeoran; lo digo y lo documento en lugar de ocultarlo.
- *¿Y los logs?* JSON, con `request_id`; solo anomalías a `warn` para no ahogar el sistema a 5.000 req/s.

## Fase 6 — Escala, recuperación acotada y cierre

Informe completo y reproducible: [INFORME_PRUEBAS.md](INFORME_PRUEBAS.md). Guion de la presentación: [PRESENTACION.md](PRESENTACION.md).

### Hallazgo central: el motor secuencial NO es el cuello de botella
Perfil de CPU de la app a 8.000 órdenes/s (12 s, 13,3 s de CPU):

| Componente | CPU | % |
|---|---|---|
| **Matching + billeteras** (`PlaceOrder`) | 0,31 s | **2,3 %** |
| Escritura/fsync del journal | 1,88 s | 14 % |
| JSON | 1,76 s | 13 % |
| HTTP y red (`net/http`) | 4,40 s | 33 % |
| Recolector de basura | 1,83 s | 14 % |

**Cómo contarlo:** "serializar el matching en un hilo es barato porque el matching cuesta microsegundos; lo caro es red, JSON y disco, y eso sí escala: las APIs son stateless (N réplicas) y el fsync se amortiza con group commit."

### Capacidad (máquina de 4 núcleos compartida con generador, Postgres, Prometheus y Grafana)
Escalón de carga, todas las órdenes aceptadas y saldos auditados en cada uno:

| Objetivo | Efectivas | p50 | p99 | Cola (máx.) | CPU app / Postgres / generador |
|---|---|---|---|---|---|
| 5.000/s | 5.000 | 1,2 ms | **16 ms** | – | 1,3 / 0,7 / 0,65 núcleos |
| 8.000/s | 8.000 | 1,5 ms | 146-200 ms | 16 | 1,3 / 1,0 / 0,8 |
| 12.000/s | 11.991 | 2,8 ms | 199 ms | 16 | 1,5 / 0,9 / 1,0 |
| 16.000/s | 15.995 | 9,1 ms | 140 ms | 22 | 1,5 / 0,8 / 1,0 |

**Lectura:** el sistema sostuvo **3,2× el objetivo** sin rechazar nada. La cola del secuenciador casi no se usa (máx. 22 de 50.000): el límite es el CPU **total de la máquina** (las 4 CPU casi llenas), no el matching. En producción Postgres, la API y el generador estarían en máquinas distintas. El p99 sube a partir de ~8.000/s por saturación de CPU, no por diseño.

### Sobrecarga: backpressure en acción (`scripts/drill-overload.sh`)
Cola de 8 posiciones y 512 clientes a máxima velocidad (≈32.000 req/s de presión): **307.282 rechazadas con 503, 50.601 aceptadas**; saldos auditados `ok`; tras el reinicio se recuperó todo lo aceptado con totales idénticos. Se rechaza rápido y explícito, nunca se pierde ni se descuadra nada.

### Recuperación acotada con snapshots
- **Problema descubierto midiendo:** replay completo ≈ 110.000 registros/s ⇒ **7 s por 816.904 registros** (yo había escrito "1-3 s por millón" sin medir). A 5.000/s son ~3 min por millón: el reinicio crecía sin límite.
- **Solución:** snapshot + cola. Resultado sobre el mismo journal: **7,06 s → 1,10 s** (crash) y **1,02 s** (apagado ordenado).
- **Seguridad:** el snapshot es una optimización, nunca la verdad. Si no sirve (corrupto, truncado, de otro journal, offset inválido, estado inconsistente) se ignora y se reproduce todo: 6 casos probados, todos llegan al mismo estado.
- **Coste honesto:** una pausa del secuenciador de ~25-35 ms al copiar el estado (hasta ~120 ms con contención) con 800.000 ids; y el conjunto de ids vistos crece sin límite (L12). Ambos se acotarían con una ventana de idempotencia con TTL.

### Preguntas probables
- *¿Y si el reinicio tarda mucho?* Snapshots cada 200.000 registros + uno final al apagar: ~1 s. Sin snapshot, ~7-9 s por millón de registros.
- *¿Qué pasa si el snapshot está corrupto?* Se ignora y se reproduce el journal: el snapshot nunca es la verdad.
- *¿Cuánto escala?* 16.000 órdenes/s medidas (3,2× el objetivo) en una máquina de 4 núcleos compartida; el matching usa el 2,3 % del CPU. Más allá: particionar por instrumento y separar API/secuenciador.
- *¿Qué pasa con días de operación?* Honestamente: el conjunto de ids vistos y el journal crecen sin límite (L12/L13). Plan: ventana de idempotencia con TTL y rotación de segmentos aprovechando los snapshots.
- *¿Cómo sabes que funciona?* `./scripts/verify-all.sh`: tests con race detector, smoke de 22 comprobaciones, carga a 5.000/s y 4 drills, con tabla PASS/FAIL.

## Estado
Todas las fases completas. Pendiente solo lo operativo: añadir a los evaluadores como colaboradores del repositorio (ver PRESENTACION.md).
