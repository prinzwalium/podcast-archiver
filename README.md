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
| `PA_SECRET_KEY` | — (required) | ≥32 chars; encrypts stored refresh tokens. `openssl rand -hex 32` |
| `PA_EXTERNAL_URL` | `http://localhost:8080` | Public base URL; used to build feed/media URLs |
| `PA_LISTEN_ADDR` | `:8080` | Listen address |
| `PUID` / `PGID` | `1000` / `1000` | Run the process as this UID/GID (Unraid convention; match your `/data` share ownership) |
| `UMASK` | `022` | File permission mask applied to downloaded files and the database |
| `PA_DATA_DIR` | `/data` (in container) | Database and application data | 
| `PA_MEDIA_DIR` | `<PA_DATA_DIR>/media` | Downloaded episode files — point this at a separate bind mount to keep the (large) media archive off your DB volume |
| `PA_SYNC_INTERVAL` | `6h` | Feed refresh interval (min `1m`) |
| `PA_MAX_EPISODE_BYTES` | `1073741824` | Hard size cap per episode file |
| `PA_MAX_FEED_BYTES` | `134217728` | Size cap for a fetched feed document (128 MiB) |
| `PA_MAX_FEEDS_PER_USER` | `100` | Default feed limit per user (`0` = unlimited); admins can override per user |
| `PA_SYNC_WORKERS` | `3` | Feeds synced in parallel (1–32); manual "Sync now" requests go first |
| `PA_SSRF_ALLOW_PRIVATE` | `false` | Development only: allow feeds and downloads from private/loopback addresses |
| `PA_CHOWN_RECURSIVE` | `false` | Container: always `chown -R` the data/media dirs at start (by default only when the directory owner is wrong) |
| `PA_OIDC_ISSUER` | — (unset = single-user mode) | Authentik provider URL, e.g. `https://auth.example.com/application/o/podcast-archiver/` |
| `PA_OIDC_CLIENT_ID` / `PA_OIDC_CLIENT_SECRET` | — | OIDC client credentials (both required when issuer is set) |
| `PA_OIDC_SCOPES` | `openid profile email` | Space-separated scopes. Add `offline_access` to enable group re-checks during a session |
| `PA_OIDC_RECHECK_INTERVAL` | `15m` | With a refresh token: how often group/admin membership is re-checked (`0` = only at login) |
| `PA_SESSION_TTL` | `168h` | Session lifetime (min `1h`) |
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

Users outside the allowed groups get a 403 at login. Admin rights sync from the `groups` claim on every login. To apply group changes during a session too, add `offline_access` to `PA_OIDC_SCOPES` (and the `offline_access` scope mapping to the Authentik provider): the app then refreshes the token every `PA_OIDC_RECHECK_INTERVAL`, ends the session when the refresh is refused or the user left the allowed groups, and updates admin rights. If Authentik is unreachable, sessions are kept and checked again later. Without a refresh token, changes apply at the next login (at most `PA_SESSION_TTL`).

### Admin section

Members of the admin group see an **Admin** link in the header. There they can:

- view all users and their deduplicated storage usage,
- set per-user quotas (max feeds / max bytes; 0 = unlimited),
- delete users and their archives (shared, deduplicated files are kept).

Feed URLs for podcast apps use tokens instead of OIDC: open a feed in the web UI and generate a token. Anyone with the token can read that one feed; revoke it anytime.

## Security notes

- Outbound fetches are SSRF-guarded: only public HTTP(S) hosts are allowed. The check runs on the address actually dialed (so DNS rebinding can't bypass it) and blocks private, loopback, link-local, CGNAT (`100.64.0.0/10`), NAT64 and other special-purpose ranges, for feeds, redirects, downloads and OPML imports.
- Sessions are stored server-side (the cookie holds a random token); signing out or deleting a user ends them immediately. The cookie is HttpOnly, SameSite=Lax and Secure when `PA_EXTERNAL_URL` is https. Login uses PKCE.
- Cross-site form posts are rejected (`Sec-Fetch-Site`/`Origin` check), in single-user mode too.
- Mirror mode never deletes when the source lists no episodes or drops more than half of an archive of 10+ episodes at once (a `mirror_skipped` event is logged), and only deletes an episode after it has been missing for 3 consecutive syncs.
- Run behind a TLS-terminating reverse proxy; never expose the container directly.

## Versioned images

CI builds multi-tag images on every push to `main` and on version tags: `latest`, `vX.Y.Z`, `X.Y`, plus commit SHA tags — published to `ghcr.io/prinzwalium/podcast-archiver`.

## Local development

```bash
PA_SECRET_KEY=0123456789abcdef0123456789abcdef go run ./cmd/podcast-archiver
```
