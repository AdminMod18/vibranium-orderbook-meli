# Validación final

Commit: `4f2ed7c` (+ cambios sin commitear del informe) · rama `claude/focused-babbage-2ppmql` · fecha 2026-10-26T14:52Z

| # | Comando | Duración | Resultado | Log |
|---|---|---|---|---|
| 1 | gofmt -l . (debe estar vacío) y go vet ./... | 29 s | **PASA** | `docs/evidencia/validacion/01.log` |
| 2 | TEST_DATABASE_URL=…/orderbook_test go test -race -count=1 ./...  (incluye integración con PostgreSQL real) | 62 s | **PASA** | `docs/evidencia/validacion/02.log` |
| 3 | ./scripts/test-alerts.sh (promtool: 13 reglas, 26 casos) | 2 s | **PASA** | `docs/evidencia/validacion/03.log` |
| 4 | ./scripts/verify-all.sh (tests, alertas, stack, smoke, carga, drills de crash/Postgres/sobrecarga/idempotencia/instancia única) | 98 s | **PASA** | `docs/evidencia/validacion/04.log` |
| 5 | ./jmeter/run-jmeter.sh integridad  (WITH_HISTORY=true: 25 pasos, incluye el historial en Postgres) | 6 s | **PASA** | `docs/evidencia/validacion/05.log` |
| 6 | ./jmeter/check-recovery.sh  (carga JMeter, SIGKILL, reinicio, mismos saldos, integridad) | 62 s | **FALLA (rc=1)** | `docs/evidencia/validacion/06.log` |
| 7 | ./scripts/drill-idempotency.sh  (ventana, reintentos y reinicios con otra ventana) | 3 s | **PASA** | `docs/evidencia/validacion/07.log` |
