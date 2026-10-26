# Vibranium Order Book (MVP)

Libro de ofertas para negociar Vibranium (desafío técnico). Go 1.24.

## Levantar todo (Docker) — guía completa en [docs/EJECUTAR_LOCAL.md](docs/EJECUTAR_LOCAL.md)
```bash
./scripts/preflight.sh            # ¿tengo todo lo necesario?
```
```bash
docker compose up --build        # API :8080 + Postgres :5432
curl localhost:8080/healthz
./scripts/smoke.sh               # prueba de humo end-to-end
./scripts/verify-all.sh          # TODO: tests + smoke + carga + 6 drills, con tabla PASS/FAIL
./scripts/demo.sh                # guion interactivo para la presentación
```

| | |
|---|---|
| API | http://localhost:8080 |
| **Grafana (dashboard)** | http://localhost:3000 |
| Prometheus / alertas | http://localhost:9090 |
| Métricas / pprof | http://localhost:9102 |

```bash
docker compose run --rm loadgen -rate 5000 -duration 30s   # genera carga y audita los saldos al final
```
![Dashboard](docs/img/dashboard-grafana.png)

## Contrato de la API (lo que necesita quien la pruebe)
| Endpoint | Cuerpo / uso | Respuestas |
|---|---|---|
| `POST /deposits` | `{"user_id","brl","vib"}`: **primero hay que fondear las billeteras**; el sistema no tiene registro de usuarios | 204 · 400 |
| `POST /orders` | `{"id"?,"user_id","side":"BUY"\|"SELL","price","qty"}` | 201 (orden + trades) · 400 · 409 id ya procesado · 422 sin saldo · 503 cola llena |
| `DELETE /orders/{id}` | cancela y libera lo reservado | 200 · 404 |
| `GET /balances/{user}` | saldo libre y reservado en BRL y VIB | 200 |
| `GET /book?depth=N` | niveles agregados (N ≤ 100) | 200 |
| `GET /audit` · `GET /audit/history/{user}` | verifica invariantes · compara el saldo del motor con el recalculado desde el historial | 200 |
| `GET /orders/{id}` · `/orders` · `/trades` | historial en Postgres (consistente a los pocos ms) | 200 · 404 · 503 |

**Unidades y reglas:** `price` en **centavos de real** por unidad de Vibranium y `qty` en unidades, ambos **enteros** (un decimal da 400).
`id` de orden y `user_id`: **1-64 caracteres entre `[A-Za-z0-9._:@+-]`** (si no, 400 `invalid_id`). Si se omite el `id`, el servidor genera uno; **enviarlo** permite reintentar sin duplicar (409 si ya se procesó).
Una orden **ejecuta como máximo 10.000 trades**: si todavía hubiera precios compatibles, el remanente se **cancela** (estado `CANCELLED` con `filled` > 0) y se libera lo reservado.
**Seguridad (MVP):** la API no tiene autenticación; los puertos de Postgres, Prometheus, Grafana y métricas solo escuchan en `127.0.0.1` y sus claves (`orderbook`, `admin/admin`) son de demo local.

## Probar sin Docker
```bash
go test -race ./...
go run ./cmd/server            # escucha en :8080 (PORT, QUEUE_SIZE configurables)

curl -XPOST localhost:8080/deposits -d '{"user_id":"alice","brl":100000}'      # centavos
curl -XPOST localhost:8080/deposits -d '{"user_id":"bob","vib":50}'
curl -XPOST localhost:8080/orders   -d '{"id":"s1","user_id":"bob","side":"SELL","price":900,"qty":4}'
curl -XPOST localhost:8080/orders   -d '{"id":"b1","user_id":"alice","side":"BUY","price":1000,"qty":10}'
curl localhost:8080/book
curl localhost:8080/balances/alice
curl localhost:8080/audit      # verifica que los saldos cuadran
curl localhost:8080/orders/b1  # historial de una orden y sus trades (requiere DATABASE_URL)
curl localhost:8080/trades?user_id=alice
curl localhost:8080/audit/history/alice   # saldo del motor vs recalculado solo desde el historial
```

## Estado
Fases 1-6 completas: motor de matching, billeteras con reserva de fondos, API + secuenciador, journal durable con group commit y recuperación, historial en Postgres, observabilidad (Prometheus + Grafana) y recuperación acotada con snapshots.

## Probar resiliencia
```bash
./scripts/drill-overload.sh        # sobrecarga: cola diminuta + 512 clientes => 503 sin perder nada
./scripts/drill-idempotency.sh     # ventana de idempotencia: reciente=409, antiguo=nuevo, reinicio con otra ventana
./scripts/drill-single-instance.sh # una sola instancia por journal: el 2º proceso/contenedor se rechaza
./scripts/test-alerts.sh           # pruebas unitarias de las 13 reglas de alerta (promtool, vía Docker)
./scripts/seed.sh                   # (opcional) fondea alice/bob/carol/dave/erin; --count 100, --brl, --vib; o a mano con POST /deposits
./scripts/latency-breakdown.sh 10      # descompone la latencia por etapa (cola · matching · fsync · HTTP) con los histogramas de Prometheus
./scripts/soak.sh 20 5000          # prueba de resistencia: 20 min a 5.000/s con muestreo de memoria, snapshots, disco y reinicio final
./scripts/drill-kill9.sh          # carga concurrente -> kill -9 -> reinicio -> verifica saldos
./scripts/drill-postgres-down.sh   # Postgres cae en pleno uso: el matching sigue y el historial se reconcilia
./scripts/drill-docker-crash.sh    # crash real (kill -9 al proceso) de la app en el compose: Docker la reinicia y recupera el journal
# tests de integración con Postgres real:
# (crea una base APARTE: estos tests hacen DROP TABLE y se niegan a correr contra una base cuyo nombre no contenga "test")
docker compose exec -T postgres psql -U postgres -c "CREATE DATABASE orderbook_test"
TEST_DATABASE_URL=postgres://postgres:orderbook@localhost:5432/orderbook_test go test ./internal/history
```
El estado se guarda en `data/journal.log` (`JOURNAL_PATH`).

## Documentación (para sustentar)
- [docs/DECISIONES.md](docs/DECISIONES.md) — registro de decisiones, limitaciones conocidas y preguntas para el equipo evaluador.
- [docs/DEFENSA.md](docs/DEFENSA.md) — explicación fase a fase con preguntas probables.
- [docs/MEJORAS.md](docs/MEJORAS.md) — backlog de arquitectura: preguntas probables y siguiente paso.
- [docs/OBSERVABILIDAD.md](docs/OBSERVABILIDAD.md) — métricas, alertas, dashboard, resultados medidos y runbook.
- [docs/INFORME_PRUEBAS.md](docs/INFORME_PRUEBAS.md) — qué se probó, resultados medidos, hallazgos y limitaciones.
- [docs/DEFENSA_TECNICA.md](docs/DEFENSA_TECNICA.md) — informe de defensa: arquitectura, decisiones, invariantes, recuperación, rendimiento, riesgos y preguntas de entrevista (con etiquetas hecho/hipótesis/limitación/recomendación).
- [jmeter/README.md](jmeter/README.md) — planes de JMeter (carga, integridad, recuperación), cómo ejecutarlos y el informe visual de rendimiento.
- [docs/OPTIMIZACION.md](docs/OPTIMIZACION.md) — fase de optimización: diagnóstico de la latencia, hipótesis verificadas y rechazadas, cambios y resultados antes/después.
- [docs/PRESENTACION.md](docs/PRESENTACION.md) — guion de la presentación, mensajes clave y preguntas probables.
- [docs/EJECUTAR_LOCAL.md](docs/EJECUTAR_LOCAL.md) — cómo correrlo en tu máquina.
