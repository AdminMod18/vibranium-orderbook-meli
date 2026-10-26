# Backlog de mejoras y preguntas de arquitectura

Documento vivo. El desafío evalúa sobre todo la **arquitectura** (escala, resiliencia, observabilidad) para un rol de Platform.
Cada ítem es una pregunta que un evaluador podría hacer, la respuesta de hoy y el siguiente paso.
Prioridad: 🔴 lo preguntarán seguro · 🟠 probable · 🟢 bonus.

## Resiliencia

| Pri | Pregunta del evaluador | Respuesta hoy | Siguiente paso |
|---|---|---|---|
| 🔴 | ¿Qué pasa si se cae el secuenciador? | Reinicio + replay del journal (ms). Probado con `kill -9`. Mientras reinicia hay indisponibilidad (no se pierde nada confirmado) | **Hot standby**: segunda instancia consumiendo el mismo log, promovida por *leader election* ⇒ RTO de segundos |
| 🔴 | ¿Y si se pierde el disco? | Se pierde el journal (L6) | Volumen replicado (EBS multi-AZ / réplica síncrona) o mover el log a **Kafka/Redpanda** con replicación (`acks=all`) |
| 🔴 | ¿Qué pasa si dos instancias arrancan a la vez? | Corrompen el journal (L7) | Lock de archivo (`flock`) como mínimo; en producción *lease* en etcd/Consul (fencing token) |
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
| 🟠 | ¿Qué alertas pondrías? | ✅ 10 reglas en Prometheus · ⏭ Alertmanager → Slack/PagerDuty |
| 🟠 | ¿Y las trazas? | ⏭ **OpenTelemetry**: span por request que atraviese cola → matching → fsync; *exemplars* para saltar de una métrica a su traza |
| 🟠 | ¿SLOs? | ⏭ Definir SLO (p.ej. 99 % de órdenes < 100 ms) y alertar por *burn rate* |
| 🟢 | ¿Métricas por instrumento? | ⏭ Etiqueta `instrument` al particionar el mercado (cardinalidad baja) |
| 🟢 | ¿Quién protege `/metrics` y `pprof`? | Puerto de administración aparte (hecho); en producción, red interna / mTLS |

## Seguridad (fuera del MVP, pero mencionable)
Autenticación/autorización por usuario (hoy el `user_id` va en el body), límites de tamaño ya aplicados (1 MB), validación estricta de campos (ya), TLS en el balanceador, rate limiting.

## Preguntas para el equipo evaluador
Ver el final de `DECISIONES.md`.
