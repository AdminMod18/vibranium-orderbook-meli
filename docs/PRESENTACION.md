# Guion de la presentación

Objetivo del evaluador: *"Para nosotros es más importante entender cómo llegaste a la solución que el resultado"*. Esta guía ordena **qué contar, qué mostrar y qué responder**.

## Antes (checklist del día anterior)
- [ ] **Agregar como colaboradores** del repositorio a `julianommartins` y `marcos-pinheiro-ml` (GitHub → repositorio → *Settings* → *Collaborators* → *Add people*). El repo debe ser **privado**.
- [ ] Fusionar la rama `claude/focused-babbage-2ppmql` a `main` (o presentar desde esa rama, pero que se sepa cuál es).
- [ ] En una carpeta nueva: `git clone` del repo → `./scripts/preflight.sh` → `docker compose up --build -d` → `./scripts/smoke.sh`. *(Esto prueba exactamente lo que probarán ellos.)*
- [ ] `./scripts/verify-all.sh` en verde en TU máquina (los números del informe son de otra; aquí se confirman los tuyos).
- [ ] Docker Desktop abierto, puertos 8080/3000/9090/5432 libres, internet disponible.
- [ ] Antes de la demo: `docker compose down -v && docker compose up -d` (**libro vacío**; tras pruebas de carga el guion no cruza igual).
- [ ] Abrir en pestañas: Grafana http://localhost:3000, Prometheus http://localhost:9090/alerts, el repo en GitHub.
- [ ] Plan B si Docker falla: `go run ./cmd/server` (sin Postgres) y mostrar el resto; los tests y los informes siguen siendo evidencia.

## Estructura sugerida (≈ 25 min)
| Min | Parte | Qué hacer |
|---|---|---|
| 0-3 | **Cómo entendí el problema** | Un order book no ejecuta al instante; hay órdenes "en negociación"; precio-tiempo; el que llegó primero gana ⇒ el libro es **secuencial por naturaleza**. Escala = 5.000/s con ráfagas de bots |
| 3-8 | **Arquitectura** | El diagrama de abajo. Insistir en: 1 secuenciador + journal como fuente de verdad + historial desacoplado |
| 8-18 | **Demo en vivo** | `./scripts/demo.sh` (Enter para avanzar; ver abajo) |
| 18-22 | **Fallos y límites** | Qué pasa si cae la app / Postgres / hay sobrecarga; y las limitaciones que conozco |
| 22-25 | **Decisiones y qué haría después** | `MEJORAS.md`: hot standby, TTL de idempotencia, rotación del journal, particionar por mercado |

## Diagrama
```
 Bots / clientes ──► API HTTP (stateless, N réplicas) ──► cola ACOTADA ──► 1 SECUENCIADOR ──► matching + billeteras (memoria)
                          │ 503 si la cola se llena            (orden total)        │
                          │ (backpressure)                                          ▼
                          ▼                                       JOURNAL (log de eventos, fsync por LOTE = group commit)
                 Prometheus ◄── /metrics                            │  fuente de verdad: reproducirlo reconstruye todo
                    │                                               ├──► SNAPSHOT periódico (acelera el reinicio; nunca es la verdad)
                 Grafana                                            └──► PROYECTOR ──► Postgres (historial/trazabilidad, desacoplado)
```

## Los 8 mensajes que no pueden faltar
1. **El libro es secuencial a propósito**: el orden de llegada decide quién gana. Un solo hilo, sin locks. Es barato: el matching usa el **2,3 %** del CPU; el resto es red, JSON y disco (eso sí escala).
2. **La concurrencia vive antes del motor**: API stateless + cola acotada. Si la cola se llena: **503 explícito** (backpressure), nunca pérdida silenciosa.
3. **Reservar fondos al ingresar la orden** ⇒ un bot no puede gastar dos veces el mismo saldo.
4. **El journal es la fuente de verdad**: nada se confirma sin `fsync`; el *group commit* reparte ese fsync entre muchas órdenes.
5. **El motor es determinista** ⇒ reproducir el journal reconstruye el mismo estado (y se verifica trade a trade).
6. **El historial no está en el camino caliente**: si Postgres cae, el mercado sigue; al volver se pone al día solo.
7. **Los saldos se auditan solos**: suma de saldos == total depositado, reservas == órdenes abiertas, y el saldo recalculado desde el historial == el del motor (`/audit`, `/audit/history/{user}`).
8. **Todo está medido y reproducible**: `verify-all.sh`, informe con límites honestos.

## Demo (`./scripts/demo.sh`)
Pasa por: billeteras → venta que espera → compra que cruza **al precio del vendedor** → parcial → cancelar libera la reserva → protecciones (422, 409) → trazabilidad → **5.000 órdenes/s con Grafana abierto** → `kill -9` a la app → Postgres caído.
Consejo: durante el paso de carga, **mira el dashboard** y cuenta qué se ve (órdenes/s, cola, group commit, fsync). Durante el corte de Postgres, mira el panel del proyector: el lag sube y vuelve a 0 mientras las órdenes siguen entrando.

## Si te preguntan… muestra…
| Pregunta | Respuesta corta | Dónde |
|---|---|---|
| ¿Por qué un solo hilo? | El orden de llegada define el resultado; el matching cuesta µs; lo caro es I/O y escala fuera | perfil de CPU en `DEFENSA.md` (Fase 6) |
| ¿Cómo manejas 5.000/s? | Medido hasta 16.000/s en 4 núcleos compartidos; cola, group commit, backpressure | `INFORME_PRUEBAS.md` §2, Grafana |
| ¿Qué pasa si se cae la app? | Reinicio, snapshot + cola ≈ 1 s; sin snapshot ≈ 7 s por 800 k registros | `drill-docker-crash.sh` |
| ¿Y si se cae Postgres? | El mercado sigue; historial 503; se reconcilia | `drill-postgres-down.sh` |
| ¿Y si el disco falla? | Se pierde el journal: limitación conocida (L6). Producción: volumen replicado / réplica / Kafka | `DECISIONES.md` L6, `MEJORAS.md` |
| ¿Y dos instancias a la vez? | Corromperían el journal: no hay candado (L7). Falta *leader election*/lock | `DECISIONES.md` L7 |
| ¿Cómo evitas el doble gasto? | Reserva de fondos al ingresar + secuenciador único | `smoke.sh` paso 3 |
| ¿Y reintentos de bots? | `id` de orden = clave de idempotencia (409 si se repite) | `DECISIONES.md` D18 |
| ¿Qué pasa con días de operación? | La idempotencia está **acotada por una ventana** (5 M ids ≈ 409 MiB); el journal sí crece (≈150 GB/día a 5.000/s): hay que rotarlo (L13). Los snapshots ya lo permiten | `DECISIONES.md` D57, L13 |
| ¿Y si un bot reintenta mucho después? | Pasada la ventana (≈17 min a 5.000/s) se acepta como nueva; si la orden sigue en el libro, se rechaza igual. Contrapartida estándar de toda API con claves de idempotencia | `drill-idempotency.sh` |
| ¿Cómo sabes que funciona? | Tests con `-race`, smoke, drills, auditoría en vivo y `verify-all.sh` | `INFORME_PRUEBAS.md` |
| ¿Por qué Prometheus y Grafana? | Prometheus recoge/guarda/alerta; Grafana dibuja; formato abierto | `OBSERVABILIDAD.md` |
| ¿Cómo escalarías más? | Particionar por instrumento (1 secuenciador+journal por mercado) y separar API/secuenciador | `MEJORAS.md` |

## Lo que NO hay que prometer
- **No** decir que "escala infinito": se midió 16.000/s en un entorno compartido; el límite de diseño es un secuenciador por mercado.
- **No** decir que hay alta disponibilidad: hay recuperación rápida, no réplica en espera.
- **No** afirmar nada sobre la herramienta de monitoreo de Mercado Libre: se usó el estándar abierto de Prometheus y se puede preguntar.
- Si algo no se probó en su máquina (Windows, Dockerfile completo), decirlo: ver *Limitaciones* en el informe.

## Preguntas para ELLOS (el desafío invita a preguntar)
Ver el final de `DECISIONES.md`: self-trade, órdenes *market*, comisiones, respuesta síncrona vs `202`, qué herramienta de observabilidad usan, y cómo van a sembrar los saldos para su prueba (`POST /deposits`).
