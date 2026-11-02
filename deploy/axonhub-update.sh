#!/bin/bash
# AxonHub one-shot updater for existing Docker Compose deployments.
#
# Usage:
#   bash axonhub-update.sh                 # latest stable release archive
#   bash axonhub-update.sh v1.0.0-beta14   # exact release archive
#   bash axonhub-update.sh dev-1bc55dd     # legacy ghcr dev image
#
# Env:
#   AXONHUB_DIR     deployment directory (default: auto-detect)
#   IMAGE           explicit image reference (registry mode)
#   USE_REGISTRY=1  force registry mode for a v* tag
#   RELEASE_PROXY   release download proxy (default: https://gh-proxy.com/)
#   PULL_IMAGES=1   run docker compose pull in registry mode
#   SKIP_BACKUP=1   skip the pre-update backup (not recommended)

set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()  { echo -e "${BLUE}[INFO]${NC} $1" >&2; }
ok()    { echo -e "${GREEN}[OK]${NC} $1" >&2; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $1" >&2; }
fail()  { echo -e "${RED}[FAIL]${NC} $1" >&2; exit 1; }

GITHUB_REPO="${GITHUB_REPO:-shifuf/axonhub}"
DEFAULT_IMAGE="ghcr.io/shifuf/axonhub:latest"
RELEASE_PROXY="${RELEASE_PROXY-https://gh-proxy.com/}"
AXONHUB_DIR="${AXONHUB_DIR:-}"
IMAGE="${IMAGE:-}"
TARGET="${1:-latest}"
USE_REGISTRY="${USE_REGISTRY:-0}"
PULL_IMAGES="${PULL_IMAGES:-0}"
SKIP_BACKUP="${SKIP_BACKUP:-0}"

find_deploy_dir() {
  local candidates=(
    "${AXONHUB_DIR}"
    "/www/wwwroot/axonhub"
    "/opt/axonhub"
    "/root/axonhub"
  )
  for dir in "${candidates[@]}"; do
    [ -z "$dir" ] && continue
    [ ! -d "$dir" ] && continue
    if [ -f "$dir/docker-compose.yml" ] || [ -f "$dir/compose.yaml" ]; then
      echo "$dir"; return 0
    fi
  done
  local hit
  hit=$(grep -rls --include='docker-compose.yml' --include='compose.yaml' 'axonhub' \
        /www/wwwroot /opt /root /www/docker 2>/dev/null | head -n1 || true)
  if [ -n "$hit" ]; then dirname "$hit"; return 0; fi
  return 1
}

if [ -z "$AXONHUB_DIR" ]; then
  AXONHUB_DIR="$(find_deploy_dir)" || fail "deploy dir not found; use AXONHUB_DIR=/path bash $0"
fi
cd "$AXONHUB_DIR" || fail "cannot enter $AXONHUB_DIR"

COMPOSE_FILE="docker-compose.yml"
[ -f "$COMPOSE_FILE" ] || COMPOSE_FILE="compose.yaml"
[ -f "$COMPOSE_FILE" ] || fail "no docker-compose.yml in $AXONHUB_DIR"

command -v docker >/dev/null 2>&1 || fail "docker not found"
command -v curl >/dev/null 2>&1 || fail "curl not found"
if docker compose version >/dev/null 2>&1; then
  dc() { docker compose -f "$COMPOSE_FILE" "$@"; }
else
  command -v docker-compose >/dev/null 2>&1 || fail "docker compose plugin not found"
  dc() { docker-compose -f "$COMPOSE_FILE" "$@"; }
fi
info "deploy dir: $AXONHUB_DIR"

APP_CID="$(dc ps -q axonhub 2>/dev/null | head -n1)"
CURRENT_IMAGE="$(docker inspect -f '{{.Config.Image}}' "$APP_CID" 2>/dev/null || true)"
if [ -z "$CURRENT_IMAGE" ] && [ -f ".env" ]; then
  CURRENT_IMAGE="$(grep '^AXONHUB_IMAGE=' .env 2>/dev/null | head -n1 | cut -d= -f2- || true)"
fi
[ -n "$CURRENT_IMAGE" ] || CURRENT_IMAGE="$DEFAULT_IMAGE"
REGISTRY_REPO="${CURRENT_IMAGE%%:*}"
case "$REGISTRY_REPO" in
  *axonhub*) ;;
  *) REGISTRY_REPO="ghcr.io/shifuf/axonhub" ;;
esac

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/axonhub-update.XXXXXX")" || fail "cannot create temp dir"
cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT

backup_stamp="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="$AXONHUB_DIR/backups"
mkdir -p "$BACKUP_DIR"

if [ "$SKIP_BACKUP" != "1" ]; then
  info "online backup..."
  if dc ps --quiet postgres 2>/dev/null | grep -q .; then
    if timeout 180 dc exec -T postgres sh -lc 'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
      > "$BACKUP_DIR/axonhub-pg-$backup_stamp.sql"; then
      gzip -f "$BACKUP_DIR/axonhub-pg-$backup_stamp.sql" \
        && ok "Postgres backup: axonhub-pg-$backup_stamp.sql.gz"
    else
      rm -f "$BACKUP_DIR/axonhub-pg-$backup_stamp.sql"
      warn "pg_dump failed/timed out; continuing WITHOUT a database backup"
      warn "press Ctrl+C now to abort and back up manually (SKIP_BACKUP=1 to skip this check)"
      sleep 5
    fi
  fi
  shopt -s nullglob
  for db in "$AXONHUB_DIR"/data/*.db "$AXONHUB_DIR"/*.db; do
    cp -f "$db" "$BACKUP_DIR/$(basename "$db" .db)-$backup_stamp.db"
    ok "SQLite backup: $(basename "$db" .db)-$backup_stamp.db"
  done
  shopt -u nullglob
else
  warn "backup skipped (SKIP_BACKUP=1)"
fi

proxy_url() {
  local url="$1"
  if [ -z "$RELEASE_PROXY" ]; then
    printf '%s\n' "$url"
  else
    printf '%s/%s\n' "${RELEASE_PROXY%/}" "$url"
  fi
}

download_with_fallback() {
  local url="$1"
  local output="$2"
  local candidate
  if [ -n "$RELEASE_PROXY" ]; then
    candidate="$(proxy_url "$url")"
    info "download (mirror): $candidate"
    if curl -fSL --connect-timeout 10 --max-time 600 -o "$output" "$candidate"; then
      return 0
    fi
    warn "mirror download failed, retrying GitHub directly"
  fi
  info "download: $url"
  curl -fSL --connect-timeout 10 --max-time 1800 -o "$output" "$url"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

load_release_image() {
  local tag="$1"
  local archive_name
  local release_url
  local archive_file="$TMP_DIR/axonhub-image.tar.gz"
  local checksum_file="$TMP_DIR/axonhub-image.sha256"

  if [ "$tag" = "latest" ]; then
    archive_name="axonhub-linux-amd64.tar.gz"
    release_url="https://github.com/$GITHUB_REPO/releases/latest/download/$archive_name"
  else
    archive_name="axonhub-${tag}-linux-amd64.tar.gz"
    release_url="https://github.com/$GITHUB_REPO/releases/download/${tag}/$archive_name"
  fi

  info "release image: $archive_name"
  download_with_fallback "$release_url" "$archive_file" || fail "release image download failed"
  download_with_fallback "${release_url}.sha256" "$checksum_file" || fail "release checksum download failed"

  local expected actual
  expected="$(awk 'NF {print $1; exit}' "$checksum_file")"
  actual="$(sha256_of "$archive_file")"
  [ -n "$expected" ] || fail "release checksum is empty"
  [ "$expected" = "$actual" ] || fail "release checksum mismatch (expected $expected, got $actual)"
  ok "sha256 verified"

  local load_output loaded_image
  load_output="$(docker load -i "$archive_file" 2>&1)" || {
    printf '%s\n' "$load_output" >&2
    fail "docker load failed"
  }
  printf '%s\n' "$load_output" >&2
  loaded_image="$(printf '%s\n' "$load_output" | sed -n 's/^Loaded image: //p' | tail -n1)"

  if [ -z "$loaded_image" ] && [ "$tag" != "latest" ]; then
    loaded_image="ghcr.io/shifuf/axonhub:$tag"
  fi
  [ -n "$loaded_image" ] || fail "could not determine the image tag inside the release archive"
  [ "$(docker image inspect -f '{{.Id}}' "$loaded_image" 2>/dev/null || true)" != "" ] \
    || fail "release image was loaded but not found locally: $loaded_image"

  NEW_IMAGE="$loaded_image"
}

resolve_registry_image() {
  local tag="$1"
  if [ -n "$IMAGE" ]; then
    NEW_IMAGE="$IMAGE"
  elif [ "$tag" = "latest" ]; then
    NEW_IMAGE="${REGISTRY_REPO}:latest"
  else
    NEW_IMAGE="${REGISTRY_REPO}:${tag}"
  fi
}

case "$TARGET" in
  latest|v*)
    if [ -n "$IMAGE" ] || [ "$USE_REGISTRY" = "1" ]; then
      resolve_registry_image "$TARGET"
      REGISTRY_MODE=1
    else
      load_release_image "$TARGET"
      REGISTRY_MODE=0
    fi
    ;;
  dev-*)
    resolve_registry_image "$TARGET"
    REGISTRY_MODE=1
    ;;
  *)
    fail "unsupported target '$TARGET' (use latest, vX.Y.Z, or dev-<sha>)"
    ;;
esac
info "target image: $NEW_IMAGE"

if ! grep -q "AXONHUB_IMAGE" "$COMPOSE_FILE"; then
  if grep -Eq "^\s*image:.*axonhub" "$COMPOSE_FILE"; then
    warn "compose file hardcodes the axonhub image; migrating to \${AXONHUB_IMAGE}"
    sed -i "0,/^\s*image:.*axonhub/s||    image: \${AXONHUB_IMAGE:?AXONHUB_IMAGE must be set}|" "$COMPOSE_FILE"
    ok "compose now reads the image from AXONHUB_IMAGE"
  fi
fi
cp -f "$COMPOSE_FILE" "${COMPOSE_FILE}.bak.$backup_stamp" 2>/dev/null || true
[ -f .env ] && cp -f .env ".env.bak.$backup_stamp"

if grep -q '^AXONHUB_IMAGE=' .env 2>/dev/null; then
  sed -i "s|^AXONHUB_IMAGE=.*|AXONHUB_IMAGE=$NEW_IMAGE|" .env
else
  echo "AXONHUB_IMAGE=$NEW_IMAGE" >> .env
fi
ok ".env -> $NEW_IMAGE"

info "recreating container..."
if [ "$REGISTRY_MODE" = "1" ] && [ "$PULL_IMAGES" = "1" ]; then
  dc pull --ignore-pull-failures || warn "image pull failed (common in CN), continuing with local image"
fi
AXONHUB_IMAGE="$NEW_IMAGE" dc up -d --force-recreate --no-deps axonhub || fail "container recreate failed"

sleep 2
APP_CID="$(dc ps -q axonhub)"
RUNNING_IMAGE="$(docker inspect -f '{{.Config.Image}}' "$APP_CID" 2>/dev/null || true)"
if [ "$RUNNING_IMAGE" != "$NEW_IMAGE" ]; then
  warn "container runs \"$RUNNING_IMAGE\", expected \"$NEW_IMAGE\"; retrying with explicit image"
  AXONHUB_IMAGE="$NEW_IMAGE" dc up -d --force-recreate --no-deps axonhub \
    || fail "forced recreate with explicit image failed"
  APP_CID="$(dc ps -q axonhub)"
  RUNNING_IMAGE="$(docker inspect -f '{{.Config.Image}}' "$APP_CID" 2>/dev/null || true)"
fi
info "container image: $RUNNING_IMAGE"
[ "$RUNNING_IMAGE" = "$NEW_IMAGE" ] || warn "container image differs from target; check docker compose project name"

HOST_PORT="$(grep -oP 'AXONHUB_HOST_PORT=\K[0-9]+' .env 2>/dev/null || echo 8090)"
BIND_ADDRESS="$(grep '^AXONHUB_BIND_ADDRESS=' .env 2>/dev/null | head -n1 | cut -d= -f2- || true)"
CONTAINER_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$APP_CID" 2>/dev/null | awk '{print $1}')"
health_url="http://127.0.0.1:${HOST_PORT}/health"
[ -n "$CONTAINER_IP" ] && health_url="http://${CONTAINER_IP}:8090/health"
info "health check $health_url ..."
healthy=0
for _ in $(seq 1 30); do
  if curl -fsS --max-time 5 "$health_url" >/dev/null 2>&1; then healthy=1; break; fi
  sleep 2
done

if [ "$healthy" = "1" ]; then
  RUNNING_VERSION="$(curl -fsS --max-time 5 "$health_url" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -n1)"
  info "running version: ${RUNNING_VERSION:-unknown}"
fi

if [ "$healthy" != "1" ]; then
  dc logs --tail 50 axonhub >&2 || true
  warn "health check failed, rolling back..."
  OLD_IMAGE="$(grep '^AXONHUB_IMAGE=' ".env.bak.$backup_stamp" 2>/dev/null | head -n1 | cut -d= -f2-)"
  if [ -n "$OLD_IMAGE" ] && [ "$OLD_IMAGE" != "$NEW_IMAGE" ]; then
    sed -i "s|^AXONHUB_IMAGE=.*|AXONHUB_IMAGE=$OLD_IMAGE|" .env
    AXONHUB_IMAGE="$OLD_IMAGE" dc up -d --force-recreate --no-deps axonhub || true
    for _ in $(seq 1 15); do
      if curl -fsS --max-time 5 "$health_url" >/dev/null 2>&1; then
        warn "rolled back to $OLD_IMAGE; check logs and retry"
        exit 1
      fi
      sleep 2
    done
  fi
  fail "update failed and could not recover; backups in $BACKUP_DIR / .env.bak.$backup_stamp"
fi

detect_public_ip() {
  local candidate
  for url in https://api.ipify.org https://ifconfig.me/ip https://ipinfo.io/ip; do
    candidate="$(curl -fsS --connect-timeout 4 --max-time 8 "$url" 2>/dev/null | tr -d '[:space:]' || true)"
    if printf '%s' "$candidate" | grep -Eq '^[0-9]{1,3}(\.[0-9]{1,3}){3}$'; then
      printf '%s\n' "$candidate"; return 0
    fi
  done
  candidate="$(curl -fsS --connect-timeout 4 --max-time 8 https://www.cloudflare.com/cdn-cgi/trace 2>/dev/null | sed -n 's/^ip=//p' | head -n1 || true)"
  [ -n "$candidate" ] && printf '%s\n' "$candidate"
}

PUBLIC_IP="$(detect_public_ip || true)"
ok "update complete: $NEW_IMAGE"
echo
echo "verify:"
echo "  curl http://127.0.0.1:${HOST_PORT}/health"
if [ -n "$PUBLIC_IP" ]; then
  echo "  public health: curl http://${PUBLIC_IP}:${HOST_PORT}/health"
  echo "  panel: http://${PUBLIC_IP}:${HOST_PORT}"
fi
case "${BIND_ADDRESS:-127.0.0.1}" in
  127.*|localhost) warn "AxonHub currently binds to loopback; configure AXONHUB_BIND_ADDRESS plus firewall/reverse proxy for public access" ;;
esac
echo "  docker compose logs -f axonhub"
echo "  backups: $BACKUP_DIR"
