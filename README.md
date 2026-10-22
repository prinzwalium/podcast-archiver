# Podcast Archiver

Self-hosted podcast archiver. It mirrors podcast feeds (episodes, artwork, descriptions, chapters stay intact in the original files — nothing is re-encoded), keeps them in sync, and serves each archive as its own podcast RSS feed that you can subscribe to from any podcast app.

- **Mirror mode**: the archive follows the source feed. Episodes removed or re-uploaded by the creator are removed/replaced locally on the next sync.
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
| `PA_DATA_DIR` | `/data` (in container) | Database + downloaded media |
| `PA_SYNC_INTERVAL` | `6h` | Feed refresh interval (min `1m`) |
| `PA_MAX_EPISODE_BYTES` | `1073741824` | Hard size cap per episode file |
| `PA_OIDC_ISSUER` | — (unset = single-user mode) | Authentik provider URL, e.g. `https://auth.example.com/application/o/podcast-archiver/` |
| `PA_OIDC_CLIENT_ID` / `PA_OIDC_CLIENT_SECRET` | — | OIDC client credentials (required when issuer is set) |
| `PA_OIDC_REDIRECT_URL` | `<external>/auth/callback` | Override if your proxy path differs |

### Authentik setup

1. Create an application + provider of type *Explicit/Hybrid* in Authentik with signing key RS256.
2. Redirect URI: `https://your-archiver-host/auth/callback`.
3. Scopes: `openid`, `profile`, `email`.
4. Copy the client ID/secret into the environment variables above.

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
