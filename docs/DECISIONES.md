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
| D18 | **Idempotencia por `order_id`**; id repetido ⇒ `409 Conflict` (dentro de la ventana, ver D57) | Un reintento del cliente no puede duplicar la orden (los bots reintentan) | Devolver el resultado original en el reintento (más correcto, requiere guardar resultados) | 🟡 limitación conocida, ver L2 |
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
| D33 | ~~Recuperación completa desde el inicio del log, sin snapshots~~ | **Superada por D51.** Se estimó ~1-3 s por millón de registros, pero al MEDIRLO fue **~7-9 s por millón**: a 5.000 órdenes/s se acumula un millón cada ~3 min, así que el tiempo de reinicio crecía sin límite | — | ❌ reemplazada |

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

## Observabilidad (Fase 5)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D42 | **Prometheus (recoge/guarda/alerta) + Grafana (dashboards)**, no una u otra | Son capas distintas; es el stack más extendido y el formato `/metrics` es estándar abierto leíble por casi cualquier plataforma | Datadog/New Relic: comerciales, requieren cuenta; OpenTelemetry: más amplio pero más pesado para un MVP | ✅ |
| D43 | `/metrics` y `pprof` en un **puerto de administración separado** (9102) | No exponer métricas ni perfiles junto a la API pública; en producción ese puerto solo es alcanzable desde la red interna | Mismo puerto: más simple, pero filtra información interna | ✅ |
| D44 | Etiqueta `route` = **patrón** de la ruta, no la URL | Cardinalidad acotada: mil `order_id` no crean mil series (test incluido) | URL real: explota la memoria de Prometheus | ✅ |
| D45 | El secuenciador emite mediciones por una interfaz `Observer` | El paquete no depende de ninguna librería de métricas; se prueba y se cambia sin tocarlo | Importar Prometheus en el secuenciador: acoplamiento | ✅ |
| D46 | **No se loguea cada request** (solo anomalías en `warn`; detalle con `LOG_LEVEL=debug`) | A 5.000 req/s un log por request es ruido y costo; las métricas ya dan el agregado | Log por request: útil en debug, no en producción | ✅ |
| D47 | `/readyz` **sin dependencia de Postgres** | Coherente con D40: el mercado no depende del historial | Readiness que incluya Postgres: un corte del historial sacaría el servicio del balanceador | ✅ |
| D48 | Generador de carga **open-loop** (latencia desde la hora en que debió salir) | Evita *coordinated omission*: si el servidor se atrasa, el cliente no lo oculta | Closed-loop: más simple pero subestima la latencia bajo carga | ✅ |
| D49 | Alertas como **reglas en Prometheus** (8), sin Alertmanager | Muestran *qué* se vigilaría y con qué umbrales; enrutar a Slack/PagerDuty es configuración, no diseño | Alertmanager completo: fuera del MVP | 🟡 |
| D50 | Grafana con **acceso anónimo de solo lectura** y `admin/admin` | Comodidad de demo local: se abre el dashboard sin login | En producción: SSO y sin credenciales por defecto | 🟡 (solo local) |

## Escala de la recuperación (Fase 6)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D51 | **Recuperación = snapshot + cola del journal**; el snapshot es solo una optimización, nunca la fuente de verdad | Medido sobre el mismo journal de 816.904 registros: **7,06 s sin snapshot → 1,10 s con snapshot (cola de 16.893) → 1,02 s tras apagado ordenado**. Ahora depende del tamaño del *estado*, no de cuánto historial haya | Replay completo: lineal en el historial (≈110.000 registros/s) | ✅ |
| D52 | Si el snapshot falta, está corrupto/truncado, es de OTRO journal, su offset no encaja o su estado es inconsistente ⇒ **se ignora y se reproduce todo** | Un snapshot nunca puede dejar el sistema peor que sin él. Probado en 6 variantes (todas llegan al mismo estado) | Abortar el arranque: más estricto, pero sin necesidad: el journal basta | ✅ |
| D53 | Snapshot **atómico** (tmp + fsync + rename + fsync del directorio), escrito **en segundo plano**; el secuenciador solo copia el estado | Una caída deja el snapshot anterior o el nuevo, nunca uno a medias. El secuenciador no espera al disco | Escribir desde el secuenciador: pausas de cientos de ms | ✅ |
| D54 | El snapshot guarda `journal_id` + `offset` del siguiente registro; la lectura verifica la continuidad de la secuencia | Impide aplicar un snapshot a un journal ajeno o con un offset en medio de un registro | — | ✅ |
| D55 | **Snapshot final en el apagado ordenado**; periódico cada `SNAPSHOT_EVERY` registros (200.000) | Un deploy/reinicio normal no reproduce nada | Solo periódico: reinicios normales con cola | ✅ |
| D56 | **Invariante de conservación permanente**: suma de todos los saldos == total depositado (en `CheckInvariants`, `/audit` y al restaurar) | Hallazgo de las pruebas: un snapshot con +777 reales de la nada **no se detectaba**; solo se comprobaba que no hubiera negativos y que las reservas cuadraran | Comprobar solo en tests antes/después | ✅ |

## Idempotencia con ventana (D57)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D57 | **Idempotencia con ventana**: se recuerdan solo los últimos `IDEMPOTENCY_WINDOW` ids aceptados (por defecto **5.000.000**) | Sin límite, la memoria, el snapshot y el arranque crecían para siempre (≈430 M ids/día a 5.000/s). Medido: **≈85-97 bytes por id**; 5 M ids ≈ **409 MiB**; copiar 5 M ids para un snapshot = **19 ms** | Recordar todo: crece sin límite. TTL por reloj: rompe el determinismo del journal. Guardar solo un hash de 8 bytes: menos memoria pero con riesgo (mínimo) de colisión | ✅ |
| D58 | La ventana se mide en **cantidad de órdenes aceptadas, no en tiempo** | Depende solo de la secuencia de comandos ⇒ reproducir el journal reconstruye exactamente lo mismo. A 5.000/s, 5 M ≈ 17 min; a 500/s ≈ 2,8 h (la cobertura en tiempo depende del ritmo) | Ventana por reloj: reinicios olvidarían ids distintos | ✅ |
| D59 | Un id fuera de la ventana **se acepta como orden nueva**; pero si su orden **sigue en el libro**, el libro la rechaza igual (409, sin dejar reservas colgadas) | Un cliente real reintenta a los segundos o minutos; esta es la contrapartida estándar de toda API con claves de idempotencia (p. ej. 24 h en pagos). Que la orden siga abierta se protege siempre | Ventana infinita (D18 original) | ✅ |
| D60 | Al **reproducir el journal no se comprueba la ventana** (`ReplayOrder`) | El journal solo contiene lo que se aceptó, aunque la ventana fuera otra. **Cambiar `IDEMPOTENCY_WINDOW` entre reinicios nunca impide arrancar.** Una mutación de prueba demostró que, si la reproducción comprobara duplicados, un reinicio con otra ventana fallaría con "la reproducción divergió" | Comprobar y fallar: un cambio de configuración dejaría el servicio caído | ✅ |

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

- ~~**L8 — La etapa de build del `Dockerfile` no se pudo ejecutar en el entorno de desarrollo.**~~ **Resuelta:** `docker compose up --build` se ejecutó con éxito en una máquina Windows con Docker Desktop (el build completo, `go mod download` incluido, terminó sin errores).
- **L15 — Puertos del host fijos.** Un PostgreSQL ya instalado en la máquina ocupaba el 5432 y el compose no arrancaba. *Corregido:* puertos configurables por `.env` (`.env.example`), `preflight` los detecta y explica qué hacer, y los scripts los respetan.
- **L9 — El proyector relee el journal desde el inicio al arrancar** (salta lo ya reflejado). Es O(tamaño del log); con segmentos rotados + índice sería O(1). Ver `MEJORAS.md`.
- **L10 — Las cifras de rendimiento se midieron en una máquina de 4 núcleos compartida** con disco virtual. Son una referencia, no una garantía; ver `OBSERVABILIDAD.md` (condiciones y colas largas bajo contención). Repetir en la máquina de la presentación.
- **L11 — Sin trazas distribuidas.** Hay métricas y logs con `request_id`, pero no OpenTelemetry (ver `MEJORAS.md`).
- ~~**L12 — El conjunto de ids vistos crecía sin límite.**~~ **Resuelta con D57** (ventana de idempotencia): memoria, snapshot y arranque quedan acotados. Queda la contrapartida de diseño: un reintento posterior a la ventana se trata como orden nueva (ver D57).
- **L13 — El journal crece sin rotación**: ≈356 bytes por registro (815.000 registros = 290 MiB) ⇒ a 5.000/s ≈ 150 GB al día. Con snapshots ya es posible **truncar/archivar** lo anterior al último snapshot; falta implementar la rotación por segmentos.
- **L14 — La pausa del snapshot crece con el estado**: la parte de ids ya NO pesa (copiar 5.000.000 ids cuesta 19 ms: es un copiado de memoria, no un recorrido del mapa); queda la copia de las órdenes del libro y las billeteras. Medido antes de D57: 25-35 ms (hasta ~120 ms con contención) con 800.000 ids y 170.000 órdenes.
