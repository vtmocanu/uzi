#!/usr/bin/env bash
# Generate the local docker-compose secrets ONCE and persist them, so a fresh
# checkout's `docker compose up` just works (issue #894).
#
# Writes .env only if it is ABSENT. This is generate-once-and-persist, never
# regenerate. That is a hard requirement, not a nicety:
#   - UZI_SECRET_KEY encrypts secrets at rest (AES-256-GCM); regenerating it makes
#     every previously stored PAT / Anthropic token undecryptable, a data-loss
#     footgun, so it MUST stay stable.
#   - POSTGRES_PASSWORD initializes the pgdata volume on first run; changing it
#     later does not re-init an existing volume, so it also has to persist.
#   - JWT_SECRET is milder (regenerating just drops sessions).
#
# The three ${VAR:?} guards in docker-compose.yml stay untouched: a non-local
# misconfig (compose up with no .env) still fails loudly. This only removes the
# by-hand step on the supported laptop path. The k8s/Helm deploy stays on explicit
# secrets and never uses this.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/.env"
EXAMPLE="$ROOT/.env.example"

if [ -e "$ENV_FILE" ]; then
  echo "init-env: .env already exists; leaving it untouched (secrets persist)."
  exit 0
fi

if ! command -v openssl >/dev/null 2>&1; then
  echo "init-env: openssl not found; install it or write .env by hand from .env.example" >&2
  exit 1
fi

# JWT_SECRET: HS256 signing key (128 hex chars). UZI_SECRET_KEY: base64-encoded
# 32-byte master key; the two formats DIFFER and neither is interchangeable
# (UZI_SECRET_KEY refuses to boot on non-base64). POSTGRES_PASSWORD: 48 hex chars.
jwt="$(openssl rand -hex 64)"
key="$(openssl rand -base64 32)"
pw="$(openssl rand -hex 24)"

umask 077  # .env holds secrets: create it 0600.

if [ -f "$EXAMPLE" ]; then
  # Start from the fully-documented example so the user keeps every option
  # (OIDC, Slack, seeds, tuning knobs) at hand, and fill only the three empty
  # required assignments. awk matches the exact `VAR=` lines; openssl hex/base64
  # output carries no backslashes, so passing the values via -v is safe.
  awk -v jwt="$jwt" -v key="$key" -v pw="$pw" '
    $0 == "JWT_SECRET="        { print "JWT_SECRET=" jwt; next }
    $0 == "UZI_SECRET_KEY="    { print "UZI_SECRET_KEY=" key; next }
    $0 == "POSTGRES_PASSWORD=" { print "POSTGRES_PASSWORD=" pw; next }
    { print }
  ' "$EXAMPLE" >"$ENV_FILE"
else
  # No example on disk (unusual): write a minimal but complete .env.
  cat >"$ENV_FILE" <<EOF
JWT_SECRET=$jwt
UZI_SECRET_KEY=$key
POSTGRES_PASSWORD=$pw
EOF
fi

echo "init-env: wrote $ENV_FILE with freshly generated secrets. Run: docker compose up"
