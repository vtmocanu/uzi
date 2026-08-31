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

# The three vars docker-compose.yml requires with no default (${VAR:?} / bare).
REQUIRED="JWT_SECRET UZI_SECRET_KEY POSTGRES_PASSWORD"

# has_value FILE VAR — true iff FILE carries a non-empty `VAR=<something>` line.
has_value() { grep -q "^$2=." "$1"; }

# Never clobber an existing .env (that is what "persist" means). But an existing
# file with an unfilled required var is NOT done: report it plainly and fail,
# rather than printing "secrets persist" over a .env that will bounce off the
# compose ${VAR:?} guards. Common trigger: a prior manual `cp .env.example .env`
# with the values left blank.
if [ -e "$ENV_FILE" ]; then
  missing=""
  for v in $REQUIRED; do
    has_value "$ENV_FILE" "$v" || missing="$missing $v"
  done
  if [ -n "$missing" ]; then
    echo "init-env: .env exists but has no value for:${missing}" >&2
    echo "init-env: fill those in, or 'rm .env' and re-run to generate a fresh one. Leaving .env untouched." >&2
    exit 1
  fi
  echo "init-env: .env already exists with all required secrets set; leaving it untouched."
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

# Write to a temp file on the same filesystem, then rename into place. An
# interrupted or failed write can never leave a truncated .env that the
# existence guard above would then lock in.
tmp="$(mktemp "$ENV_FILE.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

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
  ' "$EXAMPLE" >"$tmp"
else
  # No example on disk (unusual): write a minimal but complete .env.
  cat >"$tmp" <<EOF
JWT_SECRET=$jwt
UZI_SECRET_KEY=$key
POSTGRES_PASSWORD=$pw
EOF
fi

# Confirm each required secret landed as the value we just generated, before
# committing the file. This catches a drifted .env.example whose `VAR=` lines no
# longer match the awk guards above (rename, a non-empty placeholder like
# REPLACE_ME, trailing whitespace): in every such case the value read back would
# not equal what we generated, which a bare non-empty check would miss.
# value_of prints the text after the FIRST '=' on VAR's line (so a '=' inside a
# base64 value survives).
value_of() { awk -F= -v k="$2" '$1==k { sub(/^[^=]*=/, ""); print; exit }' "$1"; }
if [ "$(value_of "$tmp" JWT_SECRET)" != "$jwt" ] ||
   [ "$(value_of "$tmp" UZI_SECRET_KEY)" != "$key" ] ||
   [ "$(value_of "$tmp" POSTGRES_PASSWORD)" != "$pw" ]; then
  echo "init-env: generated secrets did not apply cleanly (is .env.example intact?); .env not written" >&2
  exit 1
fi

mv "$tmp" "$ENV_FILE"
trap - EXIT

echo "init-env: wrote $ENV_FILE with freshly generated secrets. Run: docker compose up"
