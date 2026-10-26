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
./scripts/verify-all.sh          # TODO: tests + smoke + carga + 4 drills, con tabla PASS/FAIL
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
./scripts/drill-kill9.sh          # carga concurrente -> kill -9 -> reinicio -> verifica saldos
./scripts/drill-postgres-down.sh   # Postgres cae en pleno uso: el matching sigue y el historial se reconcilia
./scripts/drill-docker-crash.sh    # crash real (kill -9 al proceso) de la app en el compose: Docker la reinicia y recupera el journal
# tests de integración con Postgres real:
TEST_DATABASE_URL=postgres://postgres:orderbook@localhost:5432/orderbook go test ./internal/history
```
El estado se guarda en `data/journal.log` (`JOURNAL_PATH`).

## Documentación (para sustentar)
- [docs/DECISIONES.md](docs/DECISIONES.md) — registro de decisiones, limitaciones conocidas y preguntas para el equipo evaluador.
- [docs/DEFENSA.md](docs/DEFENSA.md) — explicación fase a fase con preguntas probables.
- [docs/MEJORAS.md](docs/MEJORAS.md) — backlog de arquitectura: preguntas probables y siguiente paso.
- [docs/OBSERVABILIDAD.md](docs/OBSERVABILIDAD.md) — métricas, alertas, dashboard, resultados medidos y runbook.
- [docs/INFORME_PRUEBAS.md](docs/INFORME_PRUEBAS.md) — qué se probó, resultados medidos, hallazgos y limitaciones.
- [docs/PRESENTACION.md](docs/PRESENTACION.md) — guion de la presentación, mensajes clave y preguntas probables.
- [docs/EJECUTAR_LOCAL.md](docs/EJECUTAR_LOCAL.md) — cómo correrlo en tu máquina.
