# Vibranium Order Book (MVP)

Libro de ofertas para negociar Vibranium (desafío técnico). Go 1.24.

```bash
go test -race ./...                                   # tests
go test ./internal/engine -bench Submit -benchmem     # rendimiento del matching
```

Estado: **Fases 1-2 completas** (motor de matching, billeteras con reserva de fondos y liquidación). Ver [docs/DEFENSA.md](docs/DEFENSA.md) para el diseño y sus motivos.
