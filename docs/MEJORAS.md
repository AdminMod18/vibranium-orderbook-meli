# Backlog de mejoras y preguntas de arquitectura

Documento vivo. El desafío evalúa sobre todo la **arquitectura** (escala, resiliencia, observabilidad) para un rol de Platform.
Cada ítem es una pregunta que un evaluador podría hacer, la respuesta de hoy y el siguiente paso.
Prioridad: 🔴 lo preguntarán seguro · 🟠 probable · 🟢 bonus.

## Estado de los hallazgos de la auditoría independiente
| # | Hallazgo | Estado |
|---|---|---|
| R1 | `id`/`user_id` sin límite tumbaba el servicio | ✅ Corregido (D61) |
| — | Una orden legítima que cruzaba ≈ 6.100 órdenes tumbaba el servicio | ✅ Corregido (D62, D63) |
| R4 | `GET /book` O(L²) dentro del secuenciador | ✅ Corregido (D64); `/audit` y la copia del snapshot siguen siendo O(órdenes) (L17) |
| R6 | Puertos y claves por defecto expuestos | ✅ Corregido (D66); la API sigue sin autenticación a propósito (L16) |
| R10 | Scripts destructivos sin confirmación; tests contra la base viva; deriva documental | ✅ Corregido (D65) |
| R2 | El proyector leía el journal antes del fsync y no detectaba que Postgres estuviera adelantado | ✅ **Corregido (D68)**; residual en L18 |
| R3 | Sin candado de instancia única sobre el journal | ✅ **Corregido (D67)**; no es alta disponibilidad (L3) |
| R5 | Cola de 50.000 con timeout de 2 s ⇒ órdenes que se ejecutaban tras un 504 (medido: 50 de 50) | ✅ **Corregido (D69):** se descartan sin ejecutar; cola por defecto 10.000; métrica y alerta `ExpiredCommands` |
| R7 | Journal sin rotación, sin alerta de disco, sin réplica | ✅ **Alerta y métrica de disco (D70)** · ⏳ rotación del journal y réplica siguen en P2 (L13, L19) |
| R8 | Interoperabilidad: los evaluadores deben fondear billeteras con `/deposits`; precios enteros en centavos | ⏳ **Pendiente de DECISIÓN:** preguntar a los evaluadores; ver el contrato en el README |
| R9 | Rendimiento medido con fsync virtual, ≤ 100 s y generador co-ubicado | ⏳ Pendiente (P1): prueba de ≥ 30 min en disco real |

## Resiliencia

| Pri | Pregunta del evaluador | Respuesta hoy | Siguiente paso |
|---|---|---|---|
| 🔴 | ¿Qué pasa si se cae el secuenciador? | Reinicio + replay del journal (ms). Probado con `kill -9`. Mientras reinicia hay indisponibilidad (no se pierde nada confirmado) | **Hot standby**: segunda instancia consumiendo el mismo log, promovida por *leader election* ⇒ RTO de segundos |
| 🔴 | ¿Y si se pierde el disco? | Se pierde el journal (L6) | Volumen replicado (EBS multi-AZ / réplica síncrona) o mover el log a **Kafka/Redpanda** con replicación (`acks=all`) |
| 🔴 | ¿Qué pasa si dos instancias arrancan a la vez? | **Se rechaza la segunda** (candado del SO, D67); si muere la primera el candado se libera solo | *Leader election* + réplica en espera para alta disponibilidad real |
| 🟠 | ¿Qué pasa si Postgres cae? | **Probado** (`drill-postgres-down.sh`): el matching sigue, historial `503`, se reconcilia solo al volver | Alertar por *lag* del proyector (Fase 5) |
| 🔴 | ¿Cuánto tarda en recuperarse? | **Hecho (Fase 6):** snapshot + cola ≈ 1 s; sin snapshot ≈ 7-9 s por millón de registros (medido) | Rotación de segmentos del journal y archivado de lo anterior al snapshot (L13) |
| 🔴 | ¿Qué pasa tras días de operación? | **Idempotencia acotada** por ventana (D57: 5 M ids ≈ 409 MiB). Queda el journal: ≈150 GB/día a 5.000/s (L13) | **Rotación de segmentos** del journal y archivado de lo anterior al último snapshot |
| 🟠 | ¿Y si el cliente reintenta tras un timeout? | `409`, y `GET /orders/{id}` permite consultar el estado | Devolver el resultado original en el reintento idempotente (guardar el resultado por `order_id`) |
| 🟢 | ¿Qué pasa si el proceso recibe SIGTERM? | Apagado ordenado: drena requests y cola, cierra el journal | — |

## Escala

| Pri | Pregunta | Respuesta hoy | Siguiente paso |
|---|---|---|---|
| 🔴 | ¿Cómo escala más allá de 5.000/s? | Matching ≈ 1M/s por hilo; group commit amortiza el fsync. Falta medirlo de punta a punta (Fase 6) | **Particionar por instrumento**: un secuenciador+journal por mercado |
| 🔴 | ¿Cómo escalan las APIs? | Stateless ⇒ réplicas detrás de un balanceador | Separar API y secuenciador en procesos distintos con cola/log intermedio (gRPC o Kafka) |
| 🟠 | ¿Qué limita el throughput? | **Medido con pprof:** el matching usa el 2,3 % del CPU; el resto es red 33 %, fsync 14 %, JSON 13 %, GC 14 %. Se llegó a 16.000/s (3,2× el objetivo) en 4 núcleos compartidos | JSON → binario (protobuf) y reutilizar buffers para bajar GC/CPU; separar API y secuenciador en máquinas distintas |
| 🟠 | ¿Hay backpressure? | Sí: cola acotada ⇒ `503 + Retry-After` | Límite por usuario (rate limit) para que un bot no monopolice la cola |
| 🟢 | ¿Y las lecturas (`/book`, `/balances`)? | Pasan por el secuenciador (vista consistente) | Servir lecturas desde réplicas/read model para no competir con el matching |

## Producto / dominio

| Pri | Pregunta | Respuesta hoy | Siguiente paso |
|---|---|---|---|
| 🔴 | ¿Cómo funciona un order book? | Ver `DEFENSA.md` Fase 1 | — |
| 🟠 | ¿Self-trade? | Permitido (D9) | Prevención configurable (cancelar taker/maker/ambos) |
| 🟠 | ¿Órdenes market / IOC / FOK? | Solo límite (D1) | Se montan sobre el mismo `match()` |
| 🟠 | ¿Comisiones? | No (D20) | Tercera pata en `Settle` |
| 🟢 | ¿Y si necesito idempotencia más larga que la ventana (p. ej. 24 h)? | Se sube `IDEMPOTENCY_WINDOW` (memoria lineal ≈ 90 B/id) | Almacén externo con TTL (Redis/Postgres) para claves de idempotencia, o fingerprint de 128 bits en vez del id completo |
| 🟢 | ¿Retiros / depósitos reales? | `POST /deposits` solo para pruebas | Servicio de wallets real con saga e idempotencia |

## Historial
| Pri | Pregunta | Respuesta hoy | Siguiente paso |
|---|---|---|---|
| 🟠 | ¿Cómo se reconstruye el historial? | Vaciar tablas y reiniciar: el proyector relee el journal | Rotación de segmentos + índice para no releer todo (L9) |
| 🟠 | ¿Escala el proyector? | Un lote de hasta 5.000 registros por transacción con pipeline | Particionar por tiempo; COPY para cargas masivas; consumidor Kafka |
| 🟢 | ¿Dos proyectores? | No soportado (D41) | *Advisory lock* de Postgres para elegir líder |

## Observabilidad
| Pri | Pregunta | Estado |
|---|---|---|
| 🔴 | ¿Cómo sabes que funciona? | ✅ Métricas Prometheus + dashboard Grafana provisionado (ver `OBSERVABILIDAD.md`) |
| 🔴 | ¿Cómo depuras una orden? | ✅ Logs JSON con `request_id`/`order_id`; `GET /orders/{id}` con historial |
| 🟠 | ¿Qué alertas pondrías? | ✅ 13 reglas en Prometheus, **cada una con prueba unitaria (promtool)** · ⏭ Alertmanager → Slack/PagerDuty |
| 🟠 | ¿Y las trazas? | ⏭ **OpenTelemetry**: span por request que atraviese cola → matching → fsync; *exemplars* para saltar de una métrica a su traza |
| 🟠 | ¿SLOs? | ⏭ Definir SLO (p.ej. 99 % de órdenes < 100 ms) y alertar por *burn rate* |
| 🟢 | ¿Métricas por instrumento? | ⏭ Etiqueta `instrument` al particionar el mercado (cardinalidad baja) |
| 🟢 | ¿Quién protege `/metrics` y `pprof`? | Puerto de administración aparte (hecho); en producción, red interna / mTLS |

## Seguridad (fuera del MVP, pero mencionable)
Autenticación/autorización por usuario (hoy el `user_id` va en el body), límites de tamaño ya aplicados (1 MB), validación estricta de campos (ya), TLS en el balanceador, rate limiting.

## Preguntas para el equipo evaluador
Ver el final de `DECISIONES.md`.
