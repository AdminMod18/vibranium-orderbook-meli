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

## Próximas fases
2. Billeteras, reserva de fondos y liquidación · 3. API + cola · 4. Persistencia/recuperación + docker-compose ·
5. Observabilidad · 6. Pruebas de carga y de fallos.
