# Configuration

All configuration is via environment variables, set in `.env` (copied from `.env.example`) and passed into the `api` container by `docker-compose.yml`. `POSTGRES_*` also configures the `db` service directly.

| Var | Default | Notes |
|---|---|---|
| `JWT_SECRET` | — (required) | HS256 signing key for session JWTs. The API refuses to start if it is missing, empty, shorter than 16 characters, or a known placeholder (`change-me`, `secret`, `password`, etc. — see `api/internal/config/config.go`). Generate with `openssl rand -hex 64`. |
| `UZI_SECRET_KEY` | — (required) | Base64-encoded 32-byte master key for AES-256-GCM. The API refuses to start without it (same boot-guard pattern as `JWT_SECRET`). Generate with `openssl rand -base64 32`. It is the platform's one shared encryption-at-rest key: every secret any feature stores (bot PATs, per-user Anthropic tokens, and any future kind) is sealed with it before it reaches Postgres, so a DB dump alone never recovers a plaintext secret. Rotating this key invalidates **all** stored secrets, not just one feature's: every affected user has to reconnect or re-paste theirs. There is no re-encrypt path. |
| `POSTGRES_PASSWORD` | — (required) | Password for the bundled Postgres role. Generate with `openssl rand -hex 24`. Compose refuses to start without it. |
| `POSTGRES_USER` | `uzi` | Postgres role name. |
| `POSTGRES_DB` | `uzi` | Postgres database name. |
| `AUTH_TOKEN_TTL` | `168h` (7 days) | Session JWT lifetime and cookie `MaxAge`, as a Go duration. Rolling refresh (see [auth-design.md](auth-design.md)) slides this on every authenticated request past the halfway point, so active users are never logged out mid-session. |
| `FRONTEND_ORIGIN` | `http://127.0.0.1:8080` | User-facing origin. Its scheme decides the cookie `Secure` flag: `https://` makes cookies `Secure`, anything else does not. Use an `https://` origin behind TLS in production. |
| `RATE_LIMIT_MAX` | `10` | Max requests per window, per (route, client IP), for `/api/auth/register` and `/api/auth/login`. |
| `RATE_LIMIT_WINDOW` | `1m` | Fixed window for the rate limiter, as a Go duration. |
| `TRUSTED_PROXIES` | `10.0.0.0/8,172.16.0.0/12,10.244.0.0/16,127.0.0.1/32` | CIDRs whose direct connections are trusted to speak for a real client via `X-Forwarded-For`. Only requests whose `RemoteAddr` falls in one of these ranges get their `X-Forwarded-For` header honored; everyone else's is ignored. The compose default trusts the private compose network (the nginx hop) — see [auth-design.md](auth-design.md) for why this is safe here. Leave empty to never trust `X-Forwarded-For` and rely on `RemoteAddr` only. |
| `DATABASE_URL` | set by compose | pgx connection string, built from `POSTGRES_*` and the `db` service name. Not meant to be set directly when using compose. |
| `API_ADDR` | `:8080` | Address the `api` binary listens on inside its container. Set by compose; no need to change it. |

Invalid values for `AUTH_TOKEN_TTL`, `RATE_LIMIT_MAX`, `RATE_LIMIT_WINDOW` or an unparseable `TRUSTED_PROXIES` entry fall back to their defaults rather than failing boot (the last one is logged as a warning); only a bad `JWT_SECRET` or missing `DATABASE_URL` refuses to start.

There is no CORS configuration to make, by design: nginx serves the SPA and proxies `/api/*` to the API on the same origin (see [ARCHITECTURE.md](../ARCHITECTURE.md)), so the browser never makes a cross-origin request.
