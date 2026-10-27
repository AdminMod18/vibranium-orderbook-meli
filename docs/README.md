# Documentación

## Para evaluar (en este orden)
| Documento | Para qué |
|---|---|
| [DECISIONES.md](DECISIONES.md) | Registro de decisiones, limitaciones conocidas y preguntas abiertas |
| [DEFENSA_TECNICA.md](DEFENSA_TECNICA.md) | Informe técnico: arquitectura, invariantes, fallos, rendimiento, riesgos y preguntas difíciles |
| [OBSERVABILIDAD.md](OBSERVABILIDAD.md) | Métricas, alertas, dashboard y runbook |
| [INFORME_PRUEBAS.md](INFORME_PRUEBAS.md) | Qué se probó, resultados y hallazgos |
| [OPTIMIZACION.md](OPTIMIZACION.md) | Cómo se diagnosticó y mejoró el rendimiento (hipótesis verificadas y rechazadas) |

## Cómo ejecutar
| Documento | Para qué |
|---|---|
| [EJECUTAR_LOCAL.md](EJECUTAR_LOCAL.md) | Levantar y probar en tu máquina (Windows/Linux/macOS) |
| [COMANDOS.md](COMANDOS.md) | Pruebas de resiliencia y escala, y ejecución sin Docker |
| [../jmeter/README.md](../jmeter/README.md) | Planes de JMeter (carga, integridad, recuperación) |

## Evidencia y material de apoyo
| Ruta | Contenido |
|---|---|
| [evidencia/](evidencia/) | Salidas reales de las pruebas (validación final, antes/después) |
| [../jmeter/report/index.html](../jmeter/report/index.html) | Informe visual de rendimiento con JMeter (ábralo en el navegador) |
| [MEJORAS.md](MEJORAS.md) | Backlog de mejoras y siguiente paso de arquitectura |

La plataforma en Kubernetes (cluster, ingress, observabilidad, caos) está en el repositorio `vibranium-orderbook-infra`.
