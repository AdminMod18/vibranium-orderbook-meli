#!/usr/bin/env bash
# Pruebas unitarias de las reglas de alerta con promtool (dentro de la imagen oficial de Prometheus).
# No toca ningún dato ni servicio: solo lee ops/prometheus/*.yml.
set -euo pipefail
cd "$(dirname "$0")/.."
IMG=prom/prometheus:v2.55.1
command -v docker >/dev/null 2>&1 || { echo "Falta Docker"; exit 2; }
docker run --rm -v "$PWD/ops/prometheus:/etc/prometheus:ro" --entrypoint promtool "$IMG" check rules /etc/prometheus/alerts.yml
docker run --rm -v "$PWD/ops/prometheus:/etc/prometheus:ro" --entrypoint promtool "$IMG" test rules /etc/prometheus/alerts_test.yml
