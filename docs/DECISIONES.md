# Registro de decisiones (ADR ligero)

Cada decisión: **qué**, **por qué**, **alternativas descartadas** y **estado**.
Es la materia prima para la sustentación: si te preguntan "¿por qué así?", la respuesta está aquí.
Estados: ✅ tomada · 🟡 tomada para el MVP, se revisaría en producción · ❓ pendiente.

Regla del desafío: *"Cuidado con el overengineering"*. Varias decisiones son simples **a propósito**; la justificación está escrita.

---

## Dominio y matching

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D1 | Solo órdenes **límite** (+ cancelación) | Es el núcleo de un order book y alcanza para el MVP | *Market*, *IOC/FOK*, *stop*: se añaden sobre el mismo `match()` | 🟡 |
| D2 | Precio y cantidad como **`int64`** (centavos / unidades) | `float` produce errores de redondeo y los saldos dejarían de cuadrar | `decimal` lib: más pesado, innecesario con enteros | ✅ |
| D3 | `MaxPrice` y `MaxQty` = 1e9 | Garantiza que `Price*Qty` ≤ 1e18 < 9.2e18 (no desborda `int64`) | Usar `big.Int`: lento e innecesario | ✅ |
| D4 | **Prioridad precio-tiempo** (mejor precio, luego FIFO) | Es la regla estándar de un order book; es justa y determinista | Pro-rata: otro tipo de mercado | ✅ |
| D5 | **Precio de ejecución = precio del maker** | Convención estándar: respeta la oferta publicada; el taker recibe mejora de precio | Precio medio o del taker: injusto con quien publicó primero | ✅ |
| D6 | **Fills parciales**: el remanente queda en el libro | Es cómo funciona un libro real | — | ✅ |
| D7 | Estructura: mapa precio→cola FIFO + heap de precios + índice por id | Mejor precio O(1), insertar nivel O(log n), cancelar O(1) | Árbol rojo-negro / skip list: más código sin ganancia real a este volumen | ✅ |
| D8 | `Seq` por **contador**, no por reloj | Determinismo: mismo log de entrada ⇒ mismo libro y trades (base de la recuperación) | Timestamp: no es único ni determinista | ✅ |
| D9 | **Self-trade permitido** (el mismo usuario puede cruzarse consigo) | Mantiene los saldos consistentes (hay test) y no complica el MVP | Prevenirlo (cancelar taker / maker / ambos): evita *wash trading*; es política de producto | ❓ **Pendiente** — comentarlo con el equipo |

## Concurrencia y escala

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D10 | `Book` **no es thread-safe** a propósito; **un solo goroutine** ejecuta el matching | El orden de llegada decide quién gana ⇒ el matching es secuencial por naturaleza. Medido: ~0,9 µs/orden ⇒ ~1M órdenes/s vs 5.000/s exigidos. Sin locks, sin contención | Locks / sharding por precio: más complejo, más lento, y rompe la prioridad de tiempo | ✅ |
| D11 | La concurrencia vive **antes** del motor: API stateless (N réplicas) + cola | El cuello de botella real es el I/O (red, log, DB), no el matching | — | ✅ |
| D12 | Escalado horizontal futuro: **un secuenciador por instrumento** (partición) | Mercados distintos son independientes; dentro de uno el orden es total | Un libro global: no escala con más activos | 🟡 (MVP = 1 instrumento) |
| D13 | Cola de entrada **acotada**; si se llena ⇒ `503 + Retry-After` | *Backpressure* explícito: mejor rechazar rápido que acumular latencia sin límite y quedarse sin memoria | Cola infinita: oculta la sobrecarga y puede tumbar el proceso | ✅ |
| D14 | Respuesta **síncrona**: la API espera el resultado del matching | Simple de probar y de demostrar (el evaluador manda una orden y ve el resultado al instante) | Asíncrono (`202 Accepted` + consulta): escala más y desacopla, pero complica la demo | 🟡 |

## Billeteras y consistencia

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D15 | **Reservar fondos al ingresar la orden** (BRL en compras, VIB en ventas) | Impide gastar dos veces el mismo saldo; una orden puede esperar minutos en el libro | Validar al ejecutar: los trades fallarían en la liquidación | ✅ |
| D16 | Orden = **reservar → cruzar → liquidar** en un mismo goroutine | Nadie ve estados intermedios; no hacen falta transacciones distribuidas | Saga entre servicios: necesaria si billetera y libro fueran servicios separados | ✅ |
| D17 | Mejora de precio: se **devuelve al comprador** `(límite − precio_trade) × qty` | Reservó a su límite pero pagó el precio del maker | — | ✅ |
| D18 | **Idempotencia por `order_id`**; id repetido ⇒ `409 Conflict` | Un reintento del cliente no puede duplicar la orden (los bots reintentan) | Devolver el resultado original en el reintento (más correcto, requiere guardar resultados) | 🟡 limitación conocida, ver L2 |
| D19 | Inconsistencia al liquidar ⇒ **`panic`** (se detiene el proceso) | Es un bug, no un error de usuario; mejor parar y recuperar desde el log que operar con saldos corruptos | Seguir y registrar el error: arriesga dinero | ✅ |
| D20 | **Sin comisiones (fees)** | MVP | Tercera pata en `Settle` hacia una cuenta de la plataforma | 🟡 |
| D21 | Billeteras se crean por **depósito** (`POST /deposits`), sin registro/login | El desafío indica que no hace falta; sirve para sembrar saldos en la demo | — | ✅ |
| D22 | **Invariantes auditables** (`CheckInvariants` + `Totals`): conservación, sin negativos, reservas == órdenes abiertas | Es la forma de demostrar "los saldos cuadran" tras la prueba de carga | — | ✅ |

## Durabilidad y recuperación (Fase 4a)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D26 | **Journal (log append-only) como fuente de verdad**; el estado en memoria es una *vista* reconstruible | El motor es determinista ⇒ reproducir el log da exactamente el mismo estado. Es el patrón *event sourcing / write-ahead log* | Guardar el estado en una DB por orden: latencia de red por cada orden, no llega a 5.000/s con consistencia fuerte | ✅ |
| D27 | **Group commit**: un lote de comandos comparte un único `fsync`; se responde al cliente SOLO después | `fsync` es lo caro (ms). Medido: ~22 comandos por fsync bajo carga concurrente. Cero órdenes confirmadas y perdidas | `fsync` por orden: ~100-1000 órdenes/s en disco típico. Sin `fsync`: rápido pero pierde datos confirmados | ✅ |
| D28 | Formato `[len][crc32c][JSON]` | El CRC distingue una escritura truncada por la caída (se descarta) de corrupción real (se aborta). JSON = legible/auditable | Binario (protobuf): más compacto/rápido, pero opaco para auditar. Revisable si el volumen lo exige | 🟡 |
| D29 | **Fail-stop**: si falla escribir o `fsync` el journal ⇒ `panic`; si la recuperación ve corrupción real ⇒ el servidor **no arranca** | Operar sin poder garantizar durabilidad, o con estado dudoso, es peor que estar caído (es dinero) | Degradar a modo memoria: arriesga confirmar órdenes que se perderían | ✅ |
| D30 | Solo se registran los comandos que **cambian el estado** (lo rechazado y las lecturas no) | Reproducir solo lo exitoso reconstruye el mismo estado; log más pequeño | Registrar todo: más auditoría de rechazos, a costa de espacio (rechazos irían a logs/métricas) | ✅ |
| D31 | La marca de tiempo del registro es **informativa**, nunca entra en la lógica | Si el matching usara el reloj, reproducir el log no sería determinista | — | ✅ |
| D32 | Al reproducir se **verifica que cada orden dé los mismos trades** que registró (`ErrDiverged`) | Detecta no-determinismo o un bug de versión al instante, en vez de corromper saldos | — | ✅ |
| D33 | Recuperación **completa desde el inicio** del log (sin snapshots) en el MVP | Medido: 700 registros en ~6 ms (≈1-3 s por millón). Snapshots son una optimización con complejidad propia | Snapshot + rotación de segmentos: ver `MEJORAS.md` | 🟡 |

## Historial y trazabilidad (Fase 4b)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D34 | **Postgres como *read model*** alimentado por un **proyector** que lee el journal; fuera del camino del matching | Trazabilidad consultable (SQL, índices) sin frenar el motor. Si Postgres cae, el matching sigue (probado). Si se pierde, se reconstruye leyendo el journal | Escribir en Postgres desde el secuenciador: cada orden pagaría la latencia de la DB y un fallo de la DB tumbaría el mercado | ✅ |
| D35 | Cada lote se aplica en **una transacción junto con `last_seq`** | Efecto *exactly-once*: tras un fallo se reintenta el mismo lote sin duplicar ni perder | Idempotencia solo por `ON CONFLICT`: no cubre los UPDATE de los makers | ✅ |
| D36 | El proyector lee el **archivo** del journal (no un canal en memoria) | Desacoplado: puede correr en otro proceso o reemplazarse por un consumidor de Kafka sin tocar el motor. Retoma tras reinicios | Canal en memoria: más rápido, pero se pierde en cada caída y acopla ambos | ✅ |
| D37 | **Consistencia eventual** del historial; las respuestas incluyen `as_of_seq` | Es el costo de desacoplar. `lag = journal_seq − projected_seq` mide cuánto atrasa | Lecturas fuertes desde el motor: compiten con el matching | ✅ |
| D38 | **Identidad del journal** (registro `genesis` con UUID) y el historial se **ata** a ella | Descubierta con un drill real: un Postgres viejo + un journal nuevo hacía que el proyector se saltara registros en silencio. Ahora se niega a proyectar y avisa | Confiar en `last_seq` solo: no detecta que historial y journal son de historias distintas | ✅ |
| D39 | **Prueba de trazabilidad**: vista SQL `balances_from_history` recalcula el saldo de cada usuario solo con depósitos + trades; `GET /audit/history/{user}` lo contrasta con el motor | Demuestra que el historial explica *cada* centavo y *cada* unidad | — | ✅ |
| D40 | El servidor **arranca aunque Postgres no esté listo** (por eso `depends_on` sin `service_healthy`) | El mercado no debe depender del historial; el proyector reintenta con backoff exponencial | Esperar a Postgres: acopla disponibilidad del mercado a la del historial | ✅ |
| D41 | Un único proyector escribiendo por historial | Dos proyectores sobre el mismo Postgres se pisarían | *Leader election* / advisory lock si se escalara | 🟡 (L7) |

## Plataforma

| # | Decisión | Por qué | Estado |
|---|---|---|---|
| D23 | **Go 1.24** | Lenguaje habitual en plataforma/cloud, buena concurrencia, binario único y liviano para Docker | ✅ |
| D24 | Librería estándar (`net/http`) sin frameworks | Menos dependencias = más fácil de levantar y de defender (anti-overengineering) | ✅ |

---

## Limitaciones conocidas (honestidad ante el evaluador)

- ~~**L1 — Estado en memoria.**~~ Resuelta en Fase 4a: journal + recuperación (probado con `scripts/drill-kill9.sh`). Queda la limitación de disco único: ver L6.
- **L6 — El journal vive en UN disco.** Si se pierde el volumen se pierde el log. Mitigación de producción: volumen replicado / réplica en espera / Kafka con replicación (ver `MEJORAS.md`).
- **L7 — No hay candado de proceso único.** Dos instancias escribiendo el mismo journal lo corromperían. En Docker se corre una; en producción hay que usar *leader election* o un lock de archivo (ver `MEJORAS.md`).
- **L2 — Reintento tras timeout.** Si el cliente corta la conexión después de que la orden entró a la cola, la orden *sí* se procesa pero el cliente no ve el resultado; al reintentar recibe `409`. **Mitigado en 4b**: `GET /orders/{id}` consulta el estado (eventualmente consistente: una orden muy reciente puede tardar unos ms en aparecer). Mejora pendiente: devolver el resultado original en el reintento idempotente.
- **L3 — Un solo secuenciador = un solo punto de falla del mercado.** Mitigación: recuperación rápida desde snapshot+log; en producción, réplica en espera (*hot standby*) consumiendo el mismo log.
- **L4 — Un instrumento.** El diseño ya separa por mercado, pero el MVP solo expone Vibranium/BRL.
- **L5 — `Depth()` recorre el libro.** Es para consulta/observabilidad, no para el camino caliente.

## Preguntas para hacerle al equipo evaluador (el desafío invita a preguntar)

1. ¿Se desea prevenir el *self-trade*? ¿Cómo (cancelar taker, maker o ambos)?
2. ¿Hay que tratar órdenes *market* o solo *límite*?
3. ¿Las operaciones de prueba serán de un instrumento (Vibranium/BRL)? ¿Con cuántos usuarios y saldos iniciales?
4. ¿Cómo prefieren sembrar saldos para su prueba? (hoy: `POST /deposits`)
5. ¿La respuesta de la API debe ser síncrona (resultado inmediato) o aceptan `202`?
6. ¿Cobran comisiones en el MVP?

- **L8 — La etapa de *build* del `Dockerfile` no se pudo ejecutar en el entorno de desarrollo** (los contenedores de build no tenían salida a internet para `go mod download`). Sí se verificó con **Docker real**: la topología completa del compose (Postgres + app, red, volúmenes, healthchecks, política de reinicio) usando un binario compilado en el host con la misma etapa de runtime; el binario estático compila. Antes de la presentación, correr `docker compose up --build` en una máquina con internet.
- **L9 — El proyector relee el journal desde el inicio al arrancar** (salta lo ya reflejado). Es O(tamaño del log); con segmentos rotados + índice sería O(1). Ver `MEJORAS.md`.
