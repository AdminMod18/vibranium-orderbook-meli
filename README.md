# Vibranium Order Book (MVP)

Libro de ofertas para negociar Vibranium (desafío técnico). Go 1.24.

## Probar
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
```

## Estado
Fases 1-3 completas: motor de matching, billeteras con reserva de fondos, API + secuenciador.

## Documentación (para sustentar)
- [docs/DECISIONES.md](docs/DECISIONES.md) — registro de decisiones, limitaciones conocidas y preguntas para el equipo evaluador.
- [docs/DEFENSA.md](docs/DEFENSA.md) — explicación fase a fase con preguntas probables.
