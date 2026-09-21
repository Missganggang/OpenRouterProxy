#!/usr/bin/env bash
# Update an existing deployment, preserving its configuration and database.
set -euo pipefail
umask 077
APP_DIR=/opt/openroute
SRC_DIR="${1:-/tmp/openroute-deploy}"
HEALTH_URL=https://x.aarcx.com/api/v1/health
BACKUP_DIR="$APP_DIR/backups/deploy-$(date -u +%Y%m%dT%H%M%SZ)"

[ "$(id -u)" -eq 0 ] || { echo 'Run as root' >&2; exit 1; }
for binary in openroute node-binaries/amd64/rel_nodeclient node-binaries/amd64v3/rel_nodeclient node-binaries/arm64/rel_nodeclient; do
  [ -s "$SRC_DIR/$binary" ] || { echo "Missing $binary" >&2; exit 1; }
  head -c 4 "$SRC_DIR/$binary" | grep -q ELF || { echo "Invalid ELF: $binary" >&2; exit 1; }
done
[ -x "$APP_DIR/openroute" ] || { echo 'Existing panel not found' >&2; exit 1; }
mkdir -p "$BACKUP_DIR"
cp -p "$APP_DIR/openroute" "$BACKUP_DIR/openroute"
cp -p "$APP_DIR/config.yml" "$BACKUP_DIR/config.yml"
if [ -d "$APP_DIR/node-binaries" ]; then
  cp -a "$APP_DIR/node-binaries" "$BACKUP_DIR/node-binaries"
fi
if [ -d "$APP_DIR/public" ]; then
  cp -a "$APP_DIR/public" "$BACKUP_DIR/public"
fi
if [ -d "$SRC_DIR/public" ]; then
  [ -s "$SRC_DIR/public/index.html" ] || { echo 'Missing frontend index.html' >&2; exit 1; }
fi
python3 - "$APP_DIR/data.db" "$BACKUP_DIR/data.db" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as source:
    with sqlite3.connect(sys.argv[2]) as backup:
        source.backup(backup)
PY

rollback() {
  trap - ERR
  echo "Update failed; restoring panel from $BACKUP_DIR" >&2
  systemctl stop openroute || true
  install -m 0755 "$BACKUP_DIR/openroute" "$APP_DIR/openroute.new"
  mv -f "$APP_DIR/openroute.new" "$APP_DIR/openroute"
  if [ -d "$BACKUP_DIR/node-binaries" ]; then
    cp -a "$BACKUP_DIR/node-binaries/." "$APP_DIR/node-binaries/"
  fi
  if [ -d "$BACKUP_DIR/public" ]; then
    mkdir -p "$APP_DIR/public"
    cp -a "$BACKUP_DIR/public/." "$APP_DIR/public/"
  fi
  systemctl start openroute
  exit 1
}
trap rollback ERR

# Stage new clients before the short panel restart.
for arch in amd64 amd64v3 arm64; do
  mkdir -p "$APP_DIR/node-binaries/$arch"
  install -m 0755 "$SRC_DIR/node-binaries/$arch/rel_nodeclient" "$APP_DIR/node-binaries/$arch/rel_nodeclient.new"
  mv -f "$APP_DIR/node-binaries/$arch/rel_nodeclient.new" "$APP_DIR/node-binaries/$arch/rel_nodeclient"
  if [ -s "$SRC_DIR/node-binaries/$arch/version.txt" ]; then
    install -m 0644 "$SRC_DIR/node-binaries/$arch/version.txt" "$APP_DIR/node-binaries/$arch/version.txt"
  fi
done
install -m 0755 "$SRC_DIR/openroute" "$APP_DIR/openroute.new"
systemctl stop openroute
if [ -d "$SRC_DIR/public" ]; then
  mkdir -p "$APP_DIR/public"
  # Retain old hashed assets for browser tabs opened before the update.
  cp -a "$SRC_DIR/public/." "$APP_DIR/public/"
fi
mv -f "$APP_DIR/openroute.new" "$APP_DIR/openroute"
systemctl start openroute
healthy=0
for attempt in $(seq 1 20); do
  if curl -fsS --max-time 3 "$HEALTH_URL" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("code") == 0 else 1)' 2>/dev/null; then
    healthy=1
    break
  fi
  sleep 1
done
[ "$healthy" -eq 1 ]
systemctl is-active --quiet openroute
trap - ERR
echo "Panel updated; backup: $BACKUP_DIR"
sha256sum "$APP_DIR/openroute" "$APP_DIR/node-binaries/"*/rel_nodeclient
