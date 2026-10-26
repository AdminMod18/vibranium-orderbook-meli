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
| 🟠 | ¿Qué pasa si Postgres cae? | (Fase 4b) El matching sigue: Postgres es solo lectura/historial y se reconstruye desde el journal | Alertar por *lag* del proyector |
| 🟠 | ¿Cómo se recupera con mucho histórico? | Replay completo (~1-3 s por millón de registros) | **Snapshots** periódicos (libro + saldos + contadores + ids vistos) + rotación de segmentos; arrancar desde el último snapshot |
| 🟠 | ¿Y si el cliente reintenta tras un timeout? | `409` por id repetido (L2) | `GET /orders/{id}` (Fase 4b) y/o devolver el resultado original en el reintento idempotente |
| 🟢 | ¿Qué pasa si el proceso recibe SIGTERM? | Apagado ordenado: drena requests y cola, cierra el journal | — |

## Escala

| Pri | Pregunta | Respuesta hoy | Siguiente paso |
|---|---|---|---|
| 🔴 | ¿Cómo escala más allá de 5.000/s? | Matching ≈ 1M/s por hilo; group commit amortiza el fsync. Falta medirlo de punta a punta (Fase 6) | **Particionar por instrumento**: un secuenciador+journal por mercado |
| 🔴 | ¿Cómo escalan las APIs? | Stateless ⇒ réplicas detrás de un balanceador | Separar API y secuenciador en procesos distintos con cola/log intermedio (gRPC o Kafka) |
| 🟠 | ¿Qué limita el throughput? | Un solo hilo de matching + fsync del lote + red/HTTP | Medir con perfiles (`pprof`); mover serialización JSON a binario si el CPU lo pide |
| 🟠 | ¿Hay backpressure? | Sí: cola acotada ⇒ `503 + Retry-After` | Límite por usuario (rate limit) para que un bot no monopolice la cola |
| 🟢 | ¿Y las lecturas (`/book`, `/balances`)? | Pasan por el secuenciador (vista consistente) | Servir lecturas desde réplicas/read model para no competir con el matching |

## Producto / dominio

| Pri | Pregunta | Respuesta hoy | Siguiente paso |
|---|---|---|---|
| 🔴 | ¿Cómo funciona un order book? | Ver `DEFENSA.md` Fase 1 | — |
| 🟠 | ¿Self-trade? | Permitido (D9) | Prevención configurable (cancelar taker/maker/ambos) |
| 🟠 | ¿Órdenes market / IOC / FOK? | Solo límite (D1) | Se montan sobre el mismo `match()` |
| 🟠 | ¿Comisiones? | No (D20) | Tercera pata en `Settle` |
| 🟢 | ¿Retiros / depósitos reales? | `POST /deposits` solo para pruebas | Servicio de wallets real con saga e idempotencia |

## Observabilidad (Fase 5)
| Pri | Pregunta | Plan |
|---|---|---|
| 🔴 | ¿Cómo sabes que funciona? | Métricas Prometheus: órdenes/s, latencia p50/p99 por endpoint, **profundidad de cola**, tamaño de lote y duración de fsync, trades/s, rechazos por causa |
| 🔴 | ¿Cómo depuras una orden? | Logs estructurados JSON con `order_id`, `user_id`, `seq`; `trace_id` por request |
| 🟠 | ¿Qué alertas pondrías? | Cola > 80%, fsync p99 alto, lag del proyector, `/audit` ≠ ok, reinicios |
| 🟢 | Dashboard | Grafana provisionado en `docker-compose` |

## Seguridad (fuera del MVP, pero mencionable)
Autenticación/autorización por usuario (hoy el `user_id` va en el body), límites de tamaño ya aplicados (1 MB), validación estricta de campos (ya), TLS en el balanceador, rate limiting.

## Preguntas para el equipo evaluador
Ver el final de `DECISIONES.md`.
