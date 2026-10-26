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
| D13 | Cola de entrada **acotada** (10.000 por defecto, ver D69); si se llena ⇒ `503 + Retry-After` | *Backpressure* explícito: mejor rechazar rápido que acumular latencia sin límite y quedarse sin memoria | Cola infinita: oculta la sobrecarga y puede tumbar el proceso | ✅ |
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

## Endurecimiento tras la auditoría (D61-D66)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D61 | `id` de orden y `user_id`: **1-64 caracteres `[A-Za-z0-9._:@+-]`**, validados solo en la ruta EN VIVO | Un id enorme rompía el journal y la ventana de idempotencia no tenía tope en bytes. Al **reproducir** el journal no se vuelve a juzgar el formato (`ReplayOrder`/`ReplayDeposit`): un journal anterior a la regla sigue arrancando | Validar también en el replay: un cambio de reglas dejaría el servicio sin arrancar | ✅ |
| D62 | **Tope de 10.000 trades por orden**; si aún hay precios compatibles, el remanente se **cancela** (no se deja en el libro) y se libera la reserva | Acota el trabajo de un comando en un motor de un solo hilo y el tamaño de su registro. Una orden legítima de ≈ 6.100 cruces tumbaba el servicio | Dejar el remanente en el libro: lo dejaría **cruzado**. Rechazar la orden antes de ejecutar: requeriría simular el cruce | ✅ |
| D63 | Límite de registro del journal **16 MiB** y un test que fija la garantía (peor registro = 5,3 MiB) | Subir un tope sin el otro dejaba un `panic` latente; ahora lo detecta un test | Chunking de registros: más complejo sin necesidad | ✅ |
| D64 | `Depth` en **O(L log L)** | Un `GET /book` con 50.000 niveles bloqueaba el motor 0,8 s | Servir lecturas fuera del secuenciador (copia de lectura): más piezas | ✅ |
| D65 | Scripts destructivos **piden confirmación** (`ASSUME_YES=1` para automatizar); los tests de integración **se niegan a correr** contra una base cuyo nombre no contenga `test` | Evitar borrar datos por accidente | — | ✅ |
| D66 | Postgres, Prometheus, Grafana y métricas **solo en `127.0.0.1`**; la API queda abierta | Claves por defecto no deben quedar expuestas a la red local; la API debe poder llamarla un evaluador | API también en loopback: protege más, pero puede impedir probarla desde otro equipo o contenedor | ✅ |

## Instancia única y consistencia del historial (D67-D68)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D67 | **Candado del sistema operativo** (`flock` en Linux/macOS, `LockFileEx` en Windows) sobre `<journal>.lock`, tomado ANTES de leer o escribir | Dos procesos escribiendo el journal lo corromperían. El candado lo libera el sistema operativo al morir el proceso (incluso con `kill -9`): **nunca queda huérfano**, a diferencia de un archivo PID. Un arranque fallido también lo suelta. Verificado entre procesos y entre contenedores sobre el mismo volumen | Archivo PID con `O_EXCL`: tras un crash dejaría el servicio sin poder arrancar. *Leader election* (etcd/Consul): solo hace falta con réplicas | ✅ |
| D68 | El proyector **solo lee hasta el offset ya sincronizado** (`Journal.DurableBytes`) y, al estar al día, **se detiene si Postgres está por delante del journal** (`ErrHistoryAhead`) | Antes leía el archivo tras el `Flush` y antes del `fsync`: tras un corte de energía, Postgres podía contener órdenes que el journal perdió (y a las que NUNCA se respondió), y las órdenes nuevas con la misma secuencia se **saltaban en silencio**. Una mutación de prueba lo demostró (sin detección: `sano=true, fallos=0`) | Proyectar de todos modos: historial falso. Reconstruir Postgres automáticamente: borraría datos derivados sin que nadie lo decida; se prefiere avisar y que lo decida una persona | ✅ |

## Cola, plazos y disco (D69-D71)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D69 | El secuenciador **descarta sin ejecutar** los comandos cuyo cliente ya no espera (plazo vencido o desconexión) y los cuenta (`queue_expired_total`); la cola por defecto baja de 50.000 a **10.000** (≈ ritmo × plazo de 2 s) | Medido en la auditoría: de 50 clientes que recibieron timeout, **las 50 órdenes se ejecutaron igual** (ejecuciones fantasma). Una cola mayor que ritmo × plazo solo hace esperar a los clientes lo que luego será un 504, en vez de un 503 inmediato. Con la guarda: 0 de 50 | Dejar la cola grande y confiar en el descarte: funciona, pero la latencia percibida empeora. Solo queda una ventana mínima (el comando ya en ejecución cuando vence el plazo): ahí el cliente puede ver 504 con la orden ejecutada y se recupera con el id (409 / `GET /orders/{id}`) | ✅ |
| D70 | Métrica `journal_disk_free_bytes` + alertas `JournalDiskLow` (< 2 GiB) y `JournalDiskFillingUp` (se llena en < 6 h), y **pruebas unitarias de las 13 reglas de alerta** con `promtool` | Con el disco lleno el servicio hace fail-stop; avisar antes. Ninguna alerta se había disparado jamás en una prueba: ahora cada regla tiene un caso que DEBE dispararla y, cuando importa, otro que NO (p. ej. el 503 de backpressure no cuenta como error del servidor). Una mutación de umbrales las hace fallar | Probar las alertas provocando fallos reales: lento y no determinista. Frenar escrituras antes de llenar el disco (modo degradado): más estados; se prefiere alertar | ✅ |
| D71 | `GOMEMLIMIT=3GiB` y `mem_limit=4g` en el compose; `scripts/seed.sh` opcional para fondear billeteras | El soak midió RSS 3,7 GiB sin límites (L20); un límite blando hace que el GC apriete antes de que el kernel mate el proceso (OOM). Los evaluadores pueden fondear a mano (`POST /deposits`) o con el script, sin que el sistema imponga usuarios | Auto-fondeo dentro del servidor: mezcla datos de prueba con el sistema real y rompe la conservación `saldos == depositado` que audita `/audit` | ✅ (ver validación en INFORME §2b) |

## Optimización guiada por evidencia (D72-D76)

| # | Decisión | Por qué | Alternativas / costo | Estado |
|---|---|---|---|---|
| D72 | El proyector **reanuda por offset**: `projector_state.journal_offset` = inicio del último registro proyectado, en la misma transacción; al arrancar relee **solo ese registro** y exige que sea `last_seq`. Cualquier duda ⇒ lectura completa desde el inicio | El re-escaneo costaba ≈ 60 s y 2-3 núcleos con 7,5 M registros (lineal en el journal) y provocó 504 y p99 de 2,45 s tras un reinicio. Releer el registro `last` es lo que evita confiar a ciegas: un offset al final o adelantado no prueba nada y ocultaría registros nunca proyectados (probado con 2 mutaciones) | Guardar solo el offset sin releer (inseguro); índice de offsets por seq (más estado); proyector en otro proceso (más piezas). Costo: una columna nueva (`ADD COLUMN IF NOT EXISTS`, compatible) | ✅ |
| D73 | `journal.Reader` **reutiliza** su `bufio` y su buffer de payload | Cada `ReadNew` reservaba 1 MiB aunque no hubiera nada nuevo: 49 % de la basura del servidor | Reducir el tamaño del buffer (menos efecto) | ✅ |
| D74 | `snapshot.Write` serializa en **streaming** (mismo formato) | Reservaba 4,4× el archivo (932 MiB por 210 MiB) cada 200.000 registros: picos de heap/RSS | Otro formato binario (más rápido, pero rompe compatibilidad y legibilidad) | ✅ |
| D75 | `SNAPSHOT_EVERY` por defecto **1.000.000** (antes 200.000) | La pausa de captura (≈ 0,2 s por millón de órdenes en el libro) domina el p99: 12 ms sin snapshots, 101-342 ms cada 200.000, 47 ms cada 1 M (A/B intercalado) | Corrección de fondo: réplica de seguimiento que genera los snapshots (duplica memoria, L21). Costo del cambio: ≤ ~1 M de registros que reproducir tras un *crash* (≈ 9 s); el reinicio ordenado sigue sin reproducir nada | ✅ (configurable) |
| D76 | `Book.Depth` en **O(niveles)**: cada nivel guarda su cantidad pendiente (`level.qty`), mantenida de forma incremental | La prueba de 30 min con JMeter se degradó (5.500 → 4.100 órd/s; p99 23 → 87 ms) y el perfil de CPU mostró `Depth` con el 18,4 % de toda la CPU: recorría todas las órdenes de los niveles consultados dentro del secuenciador | Cachear el resultado un instante (stale); mover la lectura fuera del secuenciador (más complejo: necesita copia consistente). La cantidad incremental añade una resta/suma por cruce y es verificable (test de consistencia) | ✅ |

## Plataforma

| # | Decisión | Por qué | Estado |
|---|---|---|---|
| D23 | **Go 1.24** | Lenguaje habitual en plataforma/cloud, buena concurrencia, binario único y liviano para Docker | ✅ |
| D24 | Librería estándar (`net/http`) sin frameworks | Menos dependencias = más fácil de levantar y de defender (anti-overengineering) | ✅ |

---

## Limitaciones conocidas (honestidad ante el evaluador)

- ~~**L1 — Estado en memoria.**~~ Resuelta en Fase 4a: journal + recuperación (probado con `scripts/drill-kill9.sh`). Queda la limitación de disco único: ver L6.
- **L6 — El journal vive en UN disco.** Si se pierde el volumen se pierde el log. Mitigación de producción: volumen replicado / réplica en espera / Kafka con replicación (ver `MEJORAS.md`).
- ~~**L7 — No hay candado de proceso único.**~~ **Resuelta con D67:** un segundo proceso (o contenedor) sobre el mismo journal se niega a arrancar. Queda una limitación: el candado impide la doble instancia pero **no es alta disponibilidad** (no hay réplica ni *leader election*; ver L3).
- **L2 — Reintento tras timeout.** Si el cliente corta la conexión después de que la orden entró a la cola, la orden *sí* se procesa pero el cliente no ve el resultado; al reintentar recibe `409`. **Mitigado en 4b**: `GET /orders/{id}` consulta el estado (eventualmente consistente: una orden muy reciente puede tardar unos ms en aparecer). Mejora pendiente: devolver el resultado original en el reintento idempotente.
- **L3 — Un solo secuenciador = un solo punto de falla del mercado.** Mitigación: recuperación rápida desde snapshot+log; en producción, réplica en espera (*hot standby*) consumiendo el mismo log.
- **L4 — Un instrumento.** El diseño ya separa por mercado, pero el MVP solo expone Vibranium/BRL.
- **L5 — `Depth()` se ejecuta dentro del secuenciador.** ~~Recorría todas las órdenes de los niveles consultados~~ (corregido en D76: ahora O(niveles) con la cantidad por nivel mantenida incrementalmente). Sigue ordenando una copia de los niveles (O(L log L)) y bloqueando el motor mientras corre; es de consulta, no del camino caliente.

## Preguntas para hacerle al equipo evaluador (el desafío invita a preguntar)

1. ¿Se desea prevenir el *self-trade*? ¿Cómo (cancelar taker, maker o ambos)?
2. ¿Hay que tratar órdenes *market* o solo *límite*?
3. ¿Las operaciones de prueba serán de un instrumento (Vibranium/BRL)? ¿Con cuántos usuarios y saldos iniciales?
4. ¿Cómo prefieren sembrar saldos para su prueba? (hoy: `POST /deposits`)
5. ¿La respuesta de la API debe ser síncrona (resultado inmediato) o aceptan `202`?
6. ¿Cobran comisiones en el MVP?

- ~~**L8 — La etapa de build del `Dockerfile` no se pudo ejecutar en el entorno de desarrollo.**~~ **Resuelta:** `docker compose up --build` se ejecutó con éxito en una máquina Windows con Docker Desktop (el build completo, `go mod download` incluido, terminó sin errores).
- **L18 — Dos límites del proyector, aceptados:** (a) `SetDurable` funciona **dentro del mismo proceso** que escribe el journal; si el proyector corriera como proceso aparte necesitaría un archivo auxiliar con el offset sincronizado; (b) si el journal retrocede y luego **vuelve a crecer por encima** del `last_seq` de Postgres *antes* de que arranque el proyector, la detección por secuencia no lo ve (haría falta guardar una huella del último registro proyectado).
- **L20 — Memoria del contenedor sin dimensionar:** con la ventana por defecto (5 M ids) y millones de órdenes, el soak midió heap máx. 2,7 GiB y RSS 3,7 GiB (vivo ≈ 0,7 GiB; el resto es holgura del GC y la copia del snapshot, L17). `docker-compose.yml` no fija límite ni `GOMEMLIMIT`. Mitigación aplicada en `docker-compose.yml`: `GOMEMLIMIT=3GiB` (el GC aprieta antes del tope) y `mem_limit=4g` del contenedor; ambos se cambian con `GOMEMLIMIT` y `APP_MEM_LIMIT` en `.env`. Validación en INFORME §2b.
- **L21 — La pausa del snapshot sigue siendo O(órdenes en el libro):** ≈ 0,2 s por millón; con millones de órdenes en espera, cada snapshot (cada 1 M de registros) detiene al secuenciador cientos de ms y eso es lo que queda en el p99.9. Solo se espació (D75). La salida de fondo es tomar los snapshots desde una réplica de seguimiento (duplica la memoria); no se hizo sin una decisión.
- **L22 — Sin prueba de ≥ 30 min ni en disco real** (el entorno ofrece disco virtual y ≈ 10 min de espacio con Postgres). Ver OPTIMIZACION.md §7.
- **L19 — El historial en Postgres también crece sin límite y ocupa MÁS disco que el journal:** medido, ≈ **585 bytes por orden** en tablas e índices (480.000 órdenes = 281 MB) frente a ≈ 380 bytes del journal, más hasta ~1 GB de WAL. A 5.000/s son ≈ 10 GB/h de historial + ≈ 6,6 GB/h de journal. No hay retención ni particionado (P2).
- **L15 — Puertos del host fijos.** Un PostgreSQL ya instalado en la máquina ocupaba el 5432 y el compose no arrancaba. *Corregido:* puertos configurables por `.env` (`.env.example`), `preflight` los detecta y explica qué hacer, y los scripts los respetan.
- **L16 — La API no tiene autenticación**: cualquiera puede depositar y **cancelar cualquier orden por su id** (`DELETE /orders/{id}` no comprueba al dueño). Aceptado por el desafío (no hace falta login); en producción requiere identidad y autorización.
- **L17 — `GET /audit`, la copia del snapshot y las lecturas corren dentro del secuenciador**: con 1.000.000 de órdenes abiertas `/audit` bloquea el motor ≈ 0,55 s y la copia del snapshot ≈ 0,6 s (medido). Mitigación pendiente: lecturas desde una copia, fuera del camino de escritura.
- **L9 — ~~El proyector relee el journal desde el inicio al arrancar~~ → CORREGIDO en D72:** reanuda por offset y solo relee el último registro proyectado (antes: O(tamaño del log), ≈ 60 s con 7,5 M registros). Si el offset guardado no sirve, vuelve a leer desde el inicio. Sigue sin haber segmentos rotados (L13).
- **L10 — Las cifras de rendimiento se midieron en una máquina de 4 núcleos compartida** con disco virtual. Son una referencia, no una garantía; ver `OBSERVABILIDAD.md` (condiciones y colas largas bajo contención). Repetir en la máquina de la presentación.
- **L11 — Sin trazas distribuidas.** Hay métricas y logs con `request_id`, pero no OpenTelemetry (ver `MEJORAS.md`).
- ~~**L12 — El conjunto de ids vistos crecía sin límite.**~~ **Resuelta con D57** (ventana de idempotencia): memoria, snapshot y arranque quedan acotados. Queda la contrapartida de diseño: un reintento posterior a la ventana se trata como orden nueva (ver D57).
- **L13 — El journal crece sin rotación**: ≈356 bytes por registro (815.000 registros = 290 MiB) ⇒ a 5.000/s ≈ 150 GB al día. Con snapshots ya es posible **truncar/archivar** lo anterior al último snapshot; falta implementar la rotación por segmentos.
- **L14 — La pausa del snapshot crece con el estado**: la parte de ids ya NO pesa (copiar 5.000.000 ids cuesta 19 ms: es un copiado de memoria, no un recorrido del mapa); queda la copia de las órdenes del libro y las billeteras. Medido antes de D57: 25-35 ms (hasta ~120 ms con contención) con 800.000 ids y 170.000 órdenes.
