# Vibranium Order Book (MVP)

Libro de ofertas para negociar Vibranium (desafío técnico). Go 1.24.

```bash
go test -race ./...                                   # tests
go test ./internal/engine -bench Submit -benchmem     # rendimiento del matching
```

Estado: **Fase 1 completa** (motor de matching). Ver [docs/DEFENSA.md](docs/DEFENSA.md) para el diseño y sus motivos.
