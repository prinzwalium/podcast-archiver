#!/bin/sh
set -e

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

# UMASK: default 022; applied before dropping privileges so downloaded
# episodes and the database get predictable permissions on the host.
if [ -n "${UMASK:-}" ]; then
    umask "${UMASK}"
else
    umask 022
fi

# Create/group the runtime user if the requested IDs don't exist yet.
if ! getent group "$PGID" >/dev/null 2>&1; then
    addgroup -g "$PGID" app >/dev/null 2>&1 || true
fi
if ! getent passwd "$PUID" >/dev/null 2>&1; then
    adduser -u "$PUID" -G "$PGID" -D -H app >/dev/null 2>&1 || true
fi

# Ensure the data and media dirs are writable by the runtime user before
# dropping privileges. PA_MEDIA_DIR may point at a separate bind mount.
mkdir -p "$PA_DATA_DIR" 2>/dev/null || true
chown -R "$PUID:$PGID" "$PA_DATA_DIR" 2>/dev/null || true
if [ -n "${PA_MEDIA_DIR:-}" ]; then
    mkdir -p "$PA_MEDIA_DIR" 2>/dev/null || true
    chown -R "$PUID:$PGID" "$PA_MEDIA_DIR" 2>/dev/null || true
fi

exec su-exec "$PUID:$PGID" /usr/local/bin/podcast-archiver
