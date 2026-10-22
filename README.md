# Podcast Archiver

Self-hosted podcast archiver. It mirrors podcast feeds (episodes, artwork, descriptions, chapters stay intact in the original files — nothing is re-encoded), keeps them in sync, and serves each archive as its own podcast RSS feed that you can subscribe to from any podcast app.

- **Per-feed sync mode**: `mirror` follows the source feed (episodes removed by the creator disappear locally), `archive` is immutable (keeps everything). Choose per feed when adding, change anytime.
- **Re-upload handling** per feed: `replace` re-downloads corrected files when the creator re-uploads, `keep` retains your local copy untouched.
- **Deduplicated storage**: episode files are stored content-addressed (SHA-256). If the same feed (or same episode) is archived by multiple users, it is stored on disk exactly once.
- **Per-user archives** via OIDC (Authentik), or single-user local mode without auth.
- **Podcatcher-compatible feed URLs** with revocable tokens (podcast apps can't do OIDC).
- **Archive size prediction** before you commit disk space.
- Single static Go binary, SQLite storage, no transcoding.

## Quick start

```bash
docker run -d --name podcast-archiver \
  -p 8080:8080 \
  -v /path/to/data:/data \
  -e PA_SECRET_KEY="$(openssl rand -hex 32)" \
  -e PA_EXTERNAL_URL="https://archiver.example.com" \
  ghcr.io/prinzwalium/podcast-archiver:latest
```

Or with `compose.yaml` (see the file for all options): `docker compose up -d`.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `PA_SECRET_KEY` | — (required) | ≥32 chars, signs session cookies. `openssl rand -hex 32` |
| `PA_EXTERNAL_URL` | `http://localhost:8080` | Public base URL; used to build feed/media URLs |
| `PA_LISTEN_ADDR` | `:8080` | Listen address |
| `PUID` / `PGID` | `1000` / `1000` | Run the process as this UID/GID (Unraid convention; match your `/data` share ownership) |
| `PA_DATA_DIR` | `/data` (in container) | Database + downloaded media |
| `PA_SYNC_INTERVAL` | `6h` | Feed refresh interval (min `1m`) |
| `PA_MAX_EPISODE_BYTES` | `1073741824` | Hard size cap per episode file |
| `PA_OIDC_ISSUER` | — (unset = single-user mode) | Authentik provider URL, e.g. `https://auth.example.com/application/o/podcast-archiver/` |
| `PA_OIDC_CLIENT_ID` / `PA_OIDC_CLIENT_SECRET` | — | OIDC client credentials (required when issuer is set) |
| `PA_OIDC_REDIRECT_URL` | `<external>/auth/callback` | Override if your proxy path differs |
| `PA_OIDC_ALLOWED_GROUPS` | — (all users) | Comma-separated group allowlist; users outside these groups are denied login |
| `PA_OIDC_ADMIN_GROUPS` | — (no admins) | Comma-separated groups whose members get admin rights |
| `PA_MAX_STORAGE_BYTES` | — (unlimited) | Global disk budget; downloads pause (with a sync event) when exceeded |
| `PA_USER_MAX_BYTES` | — (unlimited) | Default per-user quota; admins can override per user |

### Authentik setup

1. Create an application + provider of type *Explicit/Hybrid* in Authentik with signing key RS256.
2. Redirect URI: `https://your-archiver-host/auth/callback`.
3. Scopes: `openid`, `profile`, `email` — plus a custom property mapping that exposes group memberships, e.g. a scope mapping with expression `return ["groups"]` and the *Group* claim enabled (Authentik's default `offline_access`/profile mappings already include `groups` when the "User info" scope mode is used; verify with a test login).
4. Create Authentik groups (e.g. `podcast-users`, `podcast-admins`) and assign users.
5. Set `PA_OIDC_ALLOWED_GROUPS=podcast-users` and `PA_OIDC_ADMIN_GROUPS=podcast-admins`.
6. Copy the client ID/secret into the environment variables above.

Users outside the allowed groups get a 403 at login. Admin rights sync from the `groups` claim on every login: leaving the admin group in Authentik revokes admin on next sign-in.

### Admin section

Members of the admin group see an **Admin** link in the header. There they can:

- view all users and their deduplicated storage usage,
- set per-user quotas (max feeds / max bytes; 0 = unlimited),
- delete users and their archives (shared, deduplicated files are kept).

Feed URLs for podcast apps use tokens instead of OIDC: open a feed in the web UI and generate a token. Anyone with the token can read that one feed; revoke it anytime.

## Security notes

- Outbound fetches are SSRF-guarded: only public HTTP(S) hosts are allowed (private, loopback, and link-local ranges blocked; redirects re-validated).
- Session cookies are HMAC-signed, HttpOnly, and Secure when `PA_EXTERNAL_URL` is https.
- Run behind a TLS-terminating reverse proxy; never expose the container directly.

## Versioned images

CI builds multi-tag images on every push to `main` and on version tags: `latest`, `vX.Y.Z`, `X.Y`, plus commit SHA tags — published to `ghcr.io/prinzwalium/podcast-archiver`.

## Local development

```bash
PA_SECRET_KEY=0123456789abcdef0123456789abcdef go run ./cmd/podcast-archiver
```
