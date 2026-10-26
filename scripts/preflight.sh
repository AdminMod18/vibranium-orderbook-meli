#!/usr/bin/env bash
# Verifica que esta máquina tiene todo lo necesario para correr el proyecto.
# Uso (Git Bash en Windows, Linux o macOS):   ./scripts/preflight.sh
cd "$(dirname "$0")/.."
FAILED=0
ok()   { printf '  [OK]     %s\n' "$1"; }
warn() { printf '  [AVISO]  %s\n           -> %s\n' "$1" "$2"; }
fail() { printf '  [FALTA]  %s\n           -> %s\n' "$1" "$2"; FAILED=1; }

echo "Requisitos para levantar el sistema (docker compose):"
command -v docker >/dev/null 2>&1 && ok "docker: $(docker --version 2>/dev/null)" \
  || fail "docker no está instalado" "instale Docker Desktop: https://www.docker.com/products/docker-desktop"
if command -v docker >/dev/null 2>&1; then
  docker compose version >/dev/null 2>&1 && ok "docker compose: $(docker compose version --short 2>/dev/null)" \
    || fail "docker compose (v2) no disponible" "actualice Docker Desktop"
  docker info >/dev/null 2>&1 && ok "el daemon de Docker responde" \
    || fail "el daemon de Docker no responde" "abra Docker Desktop y espere a que diga 'running'"
fi
command -v curl >/dev/null 2>&1 && ok "curl" || fail "curl no está instalado" "viene con Git Bash; en Linux: apt install curl"

echo; echo "Puertos (si ya los usa NUESTRO stack no es problema):"
ours=""; command -v docker >/dev/null 2>&1 && ours=$(docker compose ps -q 2>/dev/null)
for p in 8080 5432 9090 3000; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    [ -n "$ours" ] && ok "puerto $p en uso (por este stack)" \
      || warn "el puerto $p ya está ocupado por otro programa" "ciérrelo, o cambie el puerto en docker-compose.yml"
  else ok "puerto $p libre"; fi
done

echo; echo "Archivos (saltos de línea):"
if grep -lU $'\r' scripts/*.sh Dockerfile docker-compose.yml 2>/dev/null | grep -q .; then
  fail "hay archivos con saltos de línea de Windows (CRLF); los scripts fallarían" \
       "git config core.autocrlf input && git rm --cached -r . && git reset --hard"
else ok "los scripts y el Dockerfile tienen saltos de línea LF"; fi

echo; echo "Opcional (solo para correr tests y drills fuera de Docker):"
if command -v go >/dev/null 2>&1; then
  ok "go: $(go version | awk '{print $3}')"
else warn "Go no está instalado" "necesario solo para 'go test' y los drills; el sistema corre sin Go (todo va en Docker)"; fi

echo
if [ "$FAILED" = 0 ]; then echo "TODO LISTO. Siguiente paso:  docker compose up --build -d   y luego   ./scripts/smoke.sh"
else echo "Faltan requisitos (arriba). Corríjalos y repita."; exit 1; fi
