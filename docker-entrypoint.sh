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
# A recursive chown over a large archive can take minutes (especially on
# NFS/SMB), so it only runs when the directory itself has the wrong owner,
# or always with PA_CHOWN_RECURSIVE=true.
fix_owner() {
    dir="$1"
    if ! mkdir -p "$dir"; then
        echo "warning: cannot create $dir" >&2
        return 0
    fi
    if [ "${PA_CHOWN_RECURSIVE:-false}" = "true" ] || [ "$(stat -c '%u:%g' "$dir")" != "$PUID:$PGID" ]; then
        echo "setting owner of $dir to $PUID:$PGID"
        chown -R "$PUID:$PGID" "$dir" || echo "warning: chown -R $dir failed; files may not be writable" >&2
    fi
}

fix_owner "$PA_DATA_DIR"
if [ -n "${PA_MEDIA_DIR:-}" ]; then
    fix_owner "$PA_MEDIA_DIR"
fi

exec su-exec "$PUID:$PGID" /usr/local/bin/podcast-archiver
