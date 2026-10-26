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

## Próximas fases
4. Persistencia/recuperación + docker-compose · 5. Observabilidad · 6. Pruebas de carga y de fallos.
