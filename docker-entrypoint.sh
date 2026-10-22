#!/bin/sh
set -e

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

# Create/group the runtime user if the requested IDs don't exist yet.
if ! getent group "$PGID" >/dev/null 2>&1; then
    addgroup -g "$PGID" app >/dev/null 2>&1 || true
fi
if ! getent passwd "$PUID" >/dev/null 2>&1; then
    adduser -u "$PUID" -G "$PGID" -D -H app >/dev/null 2>&1 || true
fi

# Ensure the data dir is writable by the runtime user before dropping privileges.
mkdir -p "$PA_DATA_DIR" 2>/dev/null || true
chown -R "$PUID:$PGID" "$PA_DATA_DIR" 2>/dev/null || true

exec su-exec "$PUID:$PGID" /usr/local/bin/podcast-archiver
