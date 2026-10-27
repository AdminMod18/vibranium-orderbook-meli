# Comandos de consulta

Todo lo que no hace falta para arrancar, pero sirve para probar la escala y la resiliencia. Se ejecuta desde la raíz del repo, en Git Bash/Linux/macOS.

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

