# Cómo correr y probar el proyecto en tu máquina

Pensado para **Windows + Git Bash + Docker Desktop** (también sirve en Linux/macOS).
**Solo necesitas Docker**; Go es opcional (únicamente para `go test` y los drills que corren fuera de Docker).

## 0) Una sola vez
1. Abre **Docker Desktop** y espera a que diga *Running*.
2. Abre **Git Bash** en la carpeta del repo y trae la rama:
   ```bash
   cd ~/OneDrive/Documentos/vibranium-orderbook-meli
   git fetch origin
   git checkout claude/focused-babbage-2ppmql
   git pull
   ```
3. Verifica la máquina:
   ```bash
   ./scripts/preflight.sh
   ```
   Te dice qué falta y cómo arreglarlo (Docker apagado, puertos ocupados, saltos de línea CRLF, etc.).

> **Si `preflight` avisa de CRLF**: Git en Windows convirtió los scripts y fallarán. Ya hay un `.gitattributes` que lo evita; si tu clon es anterior, ejecuta
> `git config core.autocrlf input && git rm --cached -r . && git reset --hard`.

## 0b) Puertos ocupados (si `preflight` lo avisa)
Los puertos del **host** son configurables sin tocar el compose: copia `.env.example` a `.env` y cambia solo el que esté ocupado
(`POSTGRES_PORT`, `API_PORT`, `GRAFANA_PORT`, `PROMETHEUS_PORT`, `METRICS_PORT`). Dentro de Docker nada cambia: la app sigue hablando con Postgres por su puerto interno.
Luego usa las URLs con tu puerto (p. ej. `localhost:8081` si `API_PORT=8081`).

## 1) Levantar todo
```bash
docker compose up --build -d      # la primera vez descarga imágenes y compila: tarda unos minutos
docker compose ps                 # ambos servicios deben decir (healthy)
```
Servicios y puertos:
| Servicio | URL |
|---|---|
| API del order book | http://localhost:8080 |
| Postgres (historial) | localhost:5432 (`POSTGRES_PORT`) · usuario `postgres` · clave `orderbook` · BD `orderbook` |
| **Grafana (dashboard)** | http://localhost:3000 (abre directo; editar: `admin` / `admin`) |
| Prometheus y alertas | http://localhost:9090 · http://localhost:9090/alerts |
| Métricas y pprof | http://localhost:9102/metrics |

## 2) Comprobar que funciona
```bash
./scripts/smoke.sh
```
Recorre el ciclo completo (depósitos, cruce de órdenes, parciales, cancelación, idempotencia, saldo insuficiente, auditoría e historial) y termina con `SMOKE OK`, o te dice qué comprobación falló.

Para ver el dashboard moverse (no necesita Go; corre dentro de Docker y al final audita los saldos):
```bash
docker compose run --rm loadgen -rate 5000 -duration 30s
```
Abre http://localhost:3000 mientras corre.

Para probar a mano (Git Bash; en PowerShell usa `curl.exe` y comillas distintas, o simplemente usa Git Bash):
```bash
curl -XPOST localhost:8080/deposits -d '{"user_id":"alice","brl":100000}'
curl -XPOST localhost:8080/deposits -d '{"user_id":"bob","vib":50}'
curl -XPOST localhost:8080/orders -d '{"id":"s1","user_id":"bob","side":"SELL","price":900,"qty":4}'
curl -XPOST localhost:8080/orders -d '{"id":"b1","user_id":"alice","side":"BUY","price":1000,"qty":10}'
curl localhost:8080/book ; curl localhost:8080/balances/alice ; curl localhost:8080/audit
curl localhost:8080/orders/b1                  # historial (Postgres)
curl localhost:8080/audit/history/alice        # saldo del motor vs recalculado desde el historial
```
Precios en **centavos** de real; cantidades en **unidades** de Vibranium.

## 2b) Todo de una vez
```bash
./scripts/verify-all.sh      # tests + stack desde cero + smoke + carga a 5.000/s + 6 drills; termina con una tabla PASS/FAIL
```
**Atención:** reinicia el stack y **borra los datos locales** (`down -v`): por eso **pide que escribas `SI`** antes de empezar (`ASSUME_YES=1` para automatizar). Con `SKIP_GO=1` se omiten los pasos que requieren Go.

## 2c) Ensayar la demo
```bash
docker compose down -v && docker compose up -d   # libro vacío
./scripts/demo.sh                                  # Enter para avanzar paso a paso (DEMO_AUTO=1 para ensayar sin pausas)
```

## 3) Pruebas de resiliencia ("¿qué pasa si falla X?")
Con el stack arriba:
```bash
./scripts/drill-docker-crash.sh        # mata la app en plena carga; se recupera desde el journal
./scripts/drill-postgres-down.sh       # (antes: docker compose stop orderbook) Postgres cae; el matching sigue. Vacía el historial: pide confirmación
```
Los drills de arriba usan Docker + `curl`. Este otro necesita **Go** instalado:
```bash
./scripts/drill-kill9.sh               # kill -9 al servidor (sin Docker) y verificación de saldos
./scripts/drill-overload.sh            # sobrecarga: cola diminuta + 512 clientes => 503, sin perder nada
./scripts/drill-idempotency.sh         # ventana de idempotencia y reinicios con otra ventana
./scripts/drill-single-instance.sh     # un 2º proceso sobre el mismo journal se rechaza; --docker prueba un 2º contenedor
go test -race ./...                    # toda la batería de tests
```
> En Docker Desktop el proceso de la app vive dentro de una VM: el drill usa `docker kill` y reinicia a mano
> (prueba igual la recuperación del journal). En Linux mata el proceso real y comprueba el reinicio automático.

## 4) Apagar
```bash
docker compose down        # conserva los datos (volúmenes)
docker compose down -v     # borra TODO (journal e historial): empieza de cero
```
**Importante:** el historial de Postgres queda atado al journal que lo generó. Si borras solo uno de los dos volúmenes, el proyector se negará a proyectar (y lo dirá en los logs) en lugar de mezclar historias distintas. Borra ambos con `down -v`.

## Problemas frecuentes
| Síntoma | Causa y solución |
|---|---|
| `Bind for 0.0.0.0:5432 failed: port is already allocated` | Otro programa de tu PC ya usa ese puerto (casi siempre un **PostgreSQL instalado en Windows**, servicio `postgresql-x64-NN`, o un contenedor viejo). **Solución rápida:** `docker compose down`, luego `cp .env.example .env`, edita `POSTGRES_PORT=5433` y vuelve a `docker compose up -d`. Sirve igual para `API_PORT`, `GRAFANA_PORT`, `PROMETHEUS_PORT` y `METRICS_PORT`. Los scripts leen ese `.env` solos. Para saber quién usa el puerto: `netstat -ano \| findstr :5432` y luego `tasklist \| findstr <PID>` |
| `cannot connect to the Docker daemon` | Docker Desktop no está abierto |
| Scripts fallan con `\r: command not found` | Saltos de línea CRLF; ver nota arriba |
| La demo o el smoke dicen "el libro NO está vacío" | Quedaron órdenes de pruebas de carga o drills: `docker compose down -v && docker compose up -d` |
| La app queda `unhealthy` | `docker compose logs orderbook` (los logs son JSON, una línea por evento) |
| `GET /orders/{id}` da 404 recién creada la orden | El historial es eventualmente consistente (ms); reintenta |
