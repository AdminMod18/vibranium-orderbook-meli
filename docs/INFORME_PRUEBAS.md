# Informe de pruebas

> Todo lo de este informe se reproduce con **un solo comando**: `./scripts/verify-all.sh` (≈ 2-4 min; reinicia el stack desde cero).
> Termina con una tabla PASS/FAIL y no deja fallar nada en silencio.

## 1. Qué se prueba y cómo

| Capa | Qué demuestra | Cómo se ejecuta |
|---|---|---|
| **77 tests de Go** con el detector de carreras (`-race`) | Reglas del libro (precio-tiempo, parciales, precio del maker), reservas, saldos, idempotencia, concurrencia (100 goroutines), journal (cola truncada, CRC, corrupción real), recuperación, snapshots, API y métricas | `go test -race ./...` |
| **Pruebas aleatorias de invariantes** | 20.000 órdenes con cancelaciones: el libro nunca queda cruzado, se conserva el valor, cada trade respeta los límites de precio | dentro de `go test` |
| **Integración con Postgres real** | El historial reproduce al motor; el saldo recalculado desde depósitos+trades == billetera; reinicio del proyector sin duplicados; Postgres caído; journal ajeno rechazado | `TEST_DATABASE_URL=… go test ./internal/history` |
| **Matriz de recuperación** | Caída sin apagado ordenado, apagado ordenado, reinicios encadenados y 6 snapshots inservibles (corrupto, truncado, de otro journal, offset inválido, offset fuera de rango, estado inconsistente): **todos llegan al mismo estado** | `go test ./internal/recovery` |
| **Smoke end-to-end** (22 comprobaciones) | El ciclo completo contra el sistema levantado: cruces, parciales, cancelación, idempotencia, saldo insuficiente, auditoría, historial, métricas, Prometheus y Grafana | `./scripts/smoke.sh` |
| **Carga** | 5.000 órdenes/s con generador *open-loop* y auditoría de saldos al final | `docker compose run --rm loadgen -rate 5000` |
| **6 drills** | Ver §3 | `./scripts/drill-*.sh` |

Cobertura de sentencias por paquete (cada uno con sus propios tests): exchange 89,8 %, engine 83,7 %, recovery 81,4 %, sequencer 76,9 %, api 62,6 %, journal 62,4 %, snapshot 43,2 %, domain 31,8 %. *(La restauración del libro vive en `engine` pero se prueba también desde `exchange` y `recovery`, y Go no cuenta la cobertura entre paquetes; `history` marca 15 % porque sus tests exigen Postgres y se omiten sin `TEST_DATABASE_URL`, que **debe apuntar a una base de pruebas** —el nombre debe contener `test`— porque esos tests hacen `DROP TABLE`.)*

## 2. Capacidad

Entorno: **máquina de 4 núcleos compartida** entre generador, app, Postgres, Prometheus, Grafana y Docker. Disco virtual (fsync muy rápido). Todas las órdenes aceptadas y los saldos auditados en cada escalón.

| Objetivo | Efectivas | p50 | p99 | p99,9 | Cola máx. | CPU app / Postgres / generador |
|---|---|---|---|---|---|---|
| **5.000/s** (el requisito) | 5.000 | 1,2 ms | **16 ms** | 39 ms | – | 1,3 / 0,7 / 0,65 núcleos |
| 8.000/s | 8.000 | 1,5 ms | 146-200 ms | 218-288 ms | 16 | 1,3 / 1,0 / 0,8 |
| 12.000/s | 11.991 | 2,8 ms | 199 ms | 246 ms | 16 | 1,5 / 0,9 / 1,0 |
| 16.000/s | 15.995 | 9,1 ms | 140 ms | 163 ms | 22 | 1,5 / 0,8 / 1,0 |

**Perfil de CPU de la app a 8.000/s** (13,3 s de CPU en 12 s): matching+billeteras **2,3 %** · journal (escritura/fsync) 14 % · JSON 13 % · HTTP/red 33 % · GC 14 %.

**Lectura honesta**
- El sistema sostuvo **3,2× el requisito** sin rechazar ni perder una orden. Con 16.000/s la cola del secuenciador llegó a 22 de 50.000: **el límite fue el CPU de la máquina, no el diseño**; en producción API, Postgres y generador irían en equipos distintos.
- La latencia de cola (p99) sube desde ~8.000/s por saturación de CPU. Con 5.000/s en condiciones limpias fue de 16-29 ms.
- **Las colas largas aparecen con otra actividad pesada simultánea** (navegador para capturas, parada/arranque de contenedores): p99 252 ms y máximo 825 ms. Es compatible con contención del entorno pero **no está demostrado**; hay que repetirlo en la máquina de la presentación.
- Con 5.000/s el *group commit* agrupó pocos comandos por fsync (≈1-4) porque este disco virtual hace fsync casi instantáneo; en un SSD/EBS real el fsync cuesta milisegundos y el agrupamiento rinde mucho más. **No está medido en disco real.**

## 2b. Prueba de resistencia (soak, R9)

`./scripts/soak.sh` — carga abierta a 5.000 órdenes/s con muestreo por minuto de memoria, GC, snapshots, ventana de idempotencia, disco y lag del historial; al final audita saldos y mide un reinicio ordenado con el estado ya grande. **Qué pasó realmente (dos corridas, no una):**

- **Corrida 1 (22 min planeados, cortada en el minuto 10).** El contenedor del entorno de desarrollo se reinició (infraestructura del sandbox, no un fallo de la app) y mató todos los procesos. Hasta el corte: 5.013 órd/s estables, cola ≤ 5, 0 rechazos, 0 expiradas, heap 102 → 1.119 MiB, pausa de GC máx. 356 ms, pausa del snapshot máx. 469 ms, escritura del snapshot 0,8 → 4,9 s (crece con el estado), journal 1,26 GiB, 3,8 M registros.
- **Efecto colateral valioso: fue un *crash* real, sin apagado ordenado, con 3,8 M registros.** Al volver a levantar Docker, la app se reinició sola: recuperó desde el snapshot (seq 3.694.129) + 118.183 registros de cola en **17,1 s** (arranque en frío mientras `dockerd` también arrancaba, así que es una cota alta), `/audit` ok, sin pérdida ni duplicación.
- **Corrida 2 (12 min encima de ese estado).** 3.600.001 órdenes aceptadas (todas, ninguna rechazada), cola máx. 127, 0 expiradas, **la ventana de idempotencia llegó a su tope de 5.000.000 ids y se mantuvo ahí** (minuto 5 en adelante), journal 2,67 GiB / 7,48 M registros. Auditoría final `ok` (400·10¹² centavos y 400·10⁹ unidades, intactos). El historial en Postgres alcanzó `lag=0` (7.319.850 órdenes, 5.179.674 trades) y `GET /audit/history/{user}` devuelve `consistent=true`; la suma de `balances_from_history` coincide exactamente con el total del motor.
- **Reinicio ordenado con el estado grande** (journal 2.670 MiB, snapshot 268 MiB): **11,8 s**, 0 registros reproducidos.

| Extremo observado (corrida 2) | Valor |
|---|---|
| Heap máx. / RSS máx. | 2.739 MiB / 3.659 MiB |
| Pausa de GC máx. | 679 ms |
| Pausa del secuenciador por snapshot (copia del estado) máx. | 928 ms |
| Escritura del snapshot (fuera del secuenciador) máx. | 9,8 s |
| Disco libre mínimo | 5,8 GiB (el journal crece ≈ 110 MiB/min) |

**Lectura honesta (lo que NO sale bien)**
- **Con el estado grande el sistema queda al borde de 5.000/s en este entorno.** El primer minuto de la corrida 2 se sostuvo a solo 2.005 órd/s (cola 127; arranque en frío con `dockerd` y el entorno ocupados) y después hubo ráfagas de recuperación a 7.066 órd/s. Como el generador mide la latencia desde la hora en que la orden *debía* salir, ese atraso inicial nunca se drenó del todo: **p50 = 3,7 ms pero p90 = 24,6 s y máx. = 33 s**. Es la cifra real de esa corrida y no se debe citar solo el p50. La causa del arranque lento es una hipótesis (entorno frío), no está demostrada; la corrida 1, con el estado creciendo desde cero, sostuvo 5.013 órd/s con cola ≤ 5 durante 10 min.
- **Las pausas de 0,3-0,9 s (GC y copia del snapshot) son visibles** con 5 M ids + millones de órdenes en memoria. No hubo 503 ni expiradas, pero un cliente con timeout < 1 s las sentiría. Relacionado con L17.
- **La memoria no es solo la ventana.** El perfil del heap tras el último reinicio muestra 712 MiB vivos: 305 MiB de la ventana de idempotencia (5 M ids), ≈ 232 MiB del libro restaurado y 172 MiB de basura de la decodificación del snapshot. El pico de 2,7 GiB/RSS 3,7 GiB es la suma de GC (hasta 2× lo vivo) y la copia del estado al hacer el snapshot. **Hay que dimensionar el contenedor a ≥ 4 GiB con la ventana por defecto** o bajar `IDEMPOTENCY_WINDOW`; hoy `docker-compose.yml` no pone límite de memoria ni `GOMEMLIMIT` (L20).
- **Se corrió con Postgres detenido** durante la corrida 1 (para no agotar el disco del sandbox), así que no mide el costo conjunto; en la corrida 2 el proyector se puso al día solo. Entorno compartido de 4 núcleos y disco virtual (fsync casi instantáneo): las cifras no son de producción.

## 2d. Pruebas con Apache JMeter

Planes reproducibles en [`jmeter/`](../jmeter/README.md): **integridad financiera** (24-25 pasos con importes exactos), **carga** (gradual, sostenida, estrés; validación de negocio de cada respuesta y conservación de valor al final) y **recuperación tras `kill -9`**. El informe visual con las corridas y la correlación con el motor está en [`jmeter/report/index.html`](../jmeter/report/index.html) y los resultados en `jmeter/results/`. Las cifras y sus límites, en [DEFENSA_TECNICA.md](DEFENSA_TECNICA.md) §6-§7.

## 2c. Fase de optimización

Diagnóstico, hipótesis verificadas/rechazadas, cambios y comparación antes/después: **[OPTIMIZACION.md](OPTIMIZACION.md)** (registros crudos en `evidencia/`). Lo esencial: el p90 de 24,6 s del soak era un transitorio causado por el re-escaneo del journal por el proyector al arrancar (corregido); con el sistema caliente la cola de latencia la domina la pausa del snapshot (p99 12 ms sin snapshots vs 101-342 ms cada 200.000; 47 ms cada 1.000.000).

## 3. Resiliencia

| Drill | Qué se rompe | Qué se verifica | Resultado |
|---|---|---|---|
| `drill-kill9.sh` | `kill -9` al servidor en plena carga | Tras reiniciar: invariantes `ok` y **totales idénticos** | ✅ (10 repeticiones bajo CPU cargada) |
| `drill-docker-crash.sh` | `kill -9` al proceso de la app dentro del compose | Docker la reinicia solo; recupera del volumen; saldos intactos | ✅ (snapshot + cola: 110 ms) |
| `drill-postgres-down.sh` | Se detiene Postgres en pleno uso | El matching sigue (201), el historial responde 503, y al volver se reconcilia hasta `lag=0` y coincide con el motor | ✅ |
| `drill-idempotency.sh` | Ventana diminuta (50 ids) y reinicios con ventanas 5, 0 y 100.000 sobre el mismo journal | Reciente=409; fuera de la ventana=orden nueva; orden en el libro=409; saldos idénticos y auditoría `ok` tras cada reinicio | ✅ |
| `drill-single-instance.sh` | Un 2º proceso (y un 2º contenedor sobre el mismo volumen) intenta abrir el journal; luego `kill -9` al dueño | El 2º se niega (código 1, mensaje claro) sin afectar al 1º; tras la muerte del dueño el candado se libera y el estado recuperado es idéntico | ✅ |
| `drill-overload.sh` | Cola de 8 posiciones + 512 clientes a máxima velocidad | **307.282 rechazos 503** y 50.601 aceptadas; saldos `ok`; el reinicio recupera todo lo aceptado | ✅ (4 repeticiones) |

**Tiempo de recuperación** (mismo journal de 816.904 registros): sin snapshot **7,06 s** · con snapshot tras crash **1,10 s** · tras apagado ordenado **1,02 s**.

## 4. Hallazgos durante las pruebas (lo que las pruebas encontraron de verdad)

Cada ítem es un defecto o una creencia errónea que **una prueba descubrió** y que quedó corregida y cubierta.

1. **El proyector se saltaba registros en silencio.** Un Postgres con `last_seq=10012` de otra corrida + un journal nuevo ⇒ ignoraba los primeros 10.012 registros. *Corregido:* registro `genesis` con UUID que ata journal e historial; ante un desajuste el proyector se detiene y avisa (D38).
2. **`docker kill` no activa la política de reinicio.** Docker lo trata como parada manual; parecía que la app "no se recuperaba". Para simular un fallo real se mata el *proceso*; el drill lo hace donde es seguro y avisa donde no (Docker Desktop).
3. **El drill de Postgres traía valores por defecto rotos** (SQL entre comillas partido por el shell). Solo se vio al probarlo con Docker real.
4. **Medí mal el tiempo de recuperación.** Había documentado "1-3 s por millón"; medido fue **7-9 s por millón**: el reinicio crecía sin límite. *Corregido:* snapshots (D51) y la cifra errónea reemplazada en toda la documentación.
5. **Falta de una invariante de conservación.** Un snapshot con +777 reales de la nada **no se detectaba**. *Corregido:* suma de saldos == total depositado, verificada en `/audit` y al restaurar (D56).
6. **Los drills fallaban en silencio** tras renombrar un mensaje de log (`grep` + `set -e`). Motivó `verify-all.sh`, que informa cada paso.
7. **Un drill intermitente** (3 de 6 fallos con la máquina cargada): el servidor escribía su snapshot final al apagarse mientras el script borraba el directorio. *Corregido* esperando al proceso antes de limpiar; **10/10 y 4/4** después.
8. **La idempotencia crecía sin límite** (≈430 M ids/día a 5.000/s). *Corregido* con una ventana (D57-D60). Al probarla aparecieron dos sutilezas: (a) un id olvidado cuya orden sigue en el libro llegaba al libro, que lo rechazaba con *otro* error (habría sido un 500), ahora se traduce a 409 sin dejar reservas colgadas; (b) **cambiar el tamaño de la ventana entre reinicios habría impedido arrancar** (`la reproducción divergió`): lo demostró una prueba de mutación y se evita no comprobando la ventana al reproducir el journal.
10. **Un puerto ocupado en la máquina real impedía arrancar** (`Bind for 0.0.0.0:5432 failed`): un PostgreSQL de Windows usaba el 5432. Solo salió al probar en un equipo ajeno al de desarrollo. *Corregido* con puertos configurables (`.env`) y un `preflight` que lo detecta y explica la salida; reproducido y verificado ocupando el puerto a propósito.
9. **La demo y el smoke exigen un libro vacío** (si no, los cruces no son los esperados tras una prueba de carga): ahora lo comprueban, avisan cómo reiniciar y se limpian solos.

### Hallazgos de la auditoría independiente (y su estado)
11. **Una petición de 400 KB tumbaba el servicio** (`id` sin límite ⇒ registro de journal > 1 MiB ⇒ `panic`). *Corregido:* `id` y `user_id` de 1-64 caracteres `[A-Za-z0-9._:@+-]` (D61), con regresión a nivel HTTP.
12. **Una orden LEGÍTIMA que cruzaba ≈ 6.100 órdenes tumbaba el servicio** (mismo límite de 1 MiB por registro; 171 B/trade). Se halló al intentar arreglar el punto 11. *Corregido:* tope de 10.000 trades por orden con remanente cancelado (D62) y límite de registro de 16 MiB; un test comprueba que **el peor registro posible (5,3 MiB) cabe** (D63).
13. **`GET /book` con 50.000 niveles de precio bloqueaba el motor 0,8 s** (ordenamiento O(L²) dentro del secuenciador). *Corregido:* O(L log L) (D64).
14. **Los tests de integración podían borrar la base viva** (el README los apuntaba a ella) y **`verify-all`/`drill-postgres-down` borraban datos sin preguntar**. *Corregido:* los tests solo corren contra bases cuyo nombre contenga `test`; los scripts piden confirmación (D65).
15. **Postgres, Grafana y Prometheus escuchaban en todas las interfaces con claves por defecto.** *Corregido:* solo `127.0.0.1` (D66).

16. **El historial podía contener órdenes que el journal perdería** (el proyector leía tras el `Flush` y antes del `fsync`) y, si el journal retrocedía, **saltaba en silencio** las órdenes nuevas. *Corregido:* solo lee lo sincronizado y se detiene si Postgres está por delante (D68). Mutación de prueba: sin la detección el proyector queda `sano=true` mientras pierde historial.
17. **Dos instancias corrompían el journal.** *Corregido:* candado del sistema operativo (D67), verificado entre procesos, entre contenedores y tras `kill -9`. Al corregirlo, 4 tests que simulaban una caída dejando el journal abierto empezaron a fallar con `ErrLocked`: era la prueba de que el candado funciona.

18. **Órdenes "fantasma": un cliente recibía 504 y su orden se ejecutaba igual** (cola de 50.000 con timeout de 2 s). Medido: 50 de 50 se ejecutaban tras el 504. *Corregido (D69):* el secuenciador descarta sin ejecutar los comandos cuyo cliente ya se fue; cola por defecto 10.000; métrica `queue_expired_total` y alerta. Prueba de mutación: sin el descarte el test falla con `executed=51`.
19. **Sin señal de disco lleno** (el journal crece ≈ 110 MiB/min a 5.000/s y no había alerta). *Corregido (D70):* gauge `journal_disk_free_bytes`, alertas `JournalDiskLow` y `JournalDiskFillingUp`, y **26 pruebas unitarias con `promtool` de las 13 reglas** (`./scripts/test-alerts.sh`; cambiar un umbral hace fallar alguna).
20. **El generador de carga contaba "órdenes descartadas" mal** (el contador sumaba vueltas de un bucle de espera: 1.070.095.419 en la corrida 2, un número imposible). Lo detectó la propia prueba de resistencia. *Corregido:* envío bloqueante; si el sistema no sostiene el ritmo se informa "terminó N s después de lo previsto" y la latencia ya lo refleja.

21. **El proyector releía todo el journal en cada arranque** (decodificaba 7,5 M registros para descartarlos: ≈ 60 s, 2-3 núcleos, 58 errores 504 y p99 2,45 s en un reinicio bajo carga). *Corregido:* reanuda por offset y verifica el journal con 1 registro (D72). Mutaciones: sin continuidad o sin exigir releer el registro `last`, los tests fallan.
22. **`ReadNew` reservaba un buffer de 1 MiB por llamada** (49 % de toda la basura del servidor) y **`snapshot.Write` reservaba 4,4× el archivo** (932 MiB por 210 MiB). *Corregido* (D73, D74): −68 % de basura, ciclos de GC 16,9 → 4,6 por minuto.
23. **Cada snapshot pausa al secuenciador** (150-230 ms por millón de órdenes en el libro): domina el p99. *Mitigado* espaciándolos (D75); la corrección de fondo queda como decisión (L21).
24. **Sin espacio en disco el servidor hace fail-stop** (`panic: fsync del journal falló: no space left on device`), se reinicia y no pierde lo confirmado: ocurrió de verdad durante las pruebas y el diseño se comportó como estaba previsto. La alerta `JournalDiskLow` existe para evitarlo.

25. **`GET /book` degradaba el motor al crecer el libro** (hallado por la prueba de 30 min con JMeter, que pide el libro en el 1 % de las iteraciones): `Depth` recorría todas las órdenes de los niveles consultados dentro del secuenciador (18,4 % de la CPU a los 12 min; throughput 5.500 → 4.100 órd/s; p99 23 → 87 ms). *Corregido* (D76): O(niveles). Test de escala (×62 → ×0,8) y de consistencia; detalle en OPTIMIZACION.md §8.

## 5. Limitaciones (lo que NO se probó o no es válido)

- **Windows/Git Bash y Docker Desktop no se pudieron probar** (el desarrollo se hizo en Linux). Los scripts contemplan `.exe`, CRLF y PIDs ajenos, pero requieren que se ejecute `./scripts/preflight.sh` allí.
- ~~El `Dockerfile` completo no se ejecutó de punta a punta en el entorno de desarrollo~~ → **verificado en una máquina Windows con Docker Desktop**: el build completo termina bien. (En el entorno de desarrollo se probó por partes.)
- **Un solo disco y sin réplica en espera** (L3, L6): un fallo del disco pierde el journal. La doble instancia ya se impide (D67), pero eso no es alta disponibilidad.
- **El journal crece sin rotación** (L13 en `DECISIONES.md`): ≈150 GB/día a 5.000/s. La memoria de idempotencia sí está acotada (D57).
- **No se hizo la prueba de ≥ 30 min ni en disco real** (OPTIMIZACION.md §7): el entorno solo tiene disco virtual y espacio para ≈ 10 min con Postgres.
- **La ventana de idempotencia SÍ se llevó a 5 M ids reales de punta a punta** (§2b), pero solo 12 min con el estado lleno y en disco virtual; una prueba de ≥ 30 min en disco real sigue pendiente.
- Cifras de rendimiento medidas en entorno compartido y con disco virtual (L10).

## 6. Reproducir

```bash
./scripts/preflight.sh        # ¿tengo lo necesario?
./scripts/verify-all.sh       # todo: tests + stack + smoke + carga + 6 drills (tabla PASS/FAIL; pide confirmación porque borra volúmenes)
./scripts/demo.sh             # guion interactivo para la presentación
```
