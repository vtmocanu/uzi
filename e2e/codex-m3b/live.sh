#!/usr/bin/env bash
# PRD #1171 m5 — MAINTAINER-ONLY live provider entrypoint. Secret-injection ONLY.
#
# The m3b lifecycle proof runs credential-free everywhere it runs automatically: the host
# `node --test` Block A and run-lifecycle.sh's in-image legs all use a fresh DUMMY credential
# and the localhost loopback fake provider (`--network none`). This script is the ONLY place a
# maintainer can point the same lifecycle at a REAL Codex provider, and it is built so that:
#
#   * An AUTOMATED / no-input invocation (no real credential injected) NEVER reaches a real
#     provider. It runs ONLY the fake/`--dry-run` mode, or — with no arguments and no injected
#     secret — REFUSES with a non-zero exit and a clear message. There is no default that spends
#     a real credential.
#   * A real provider is reached ONLY when a maintainer EXPLICITLY injects the SUBSCRIPTION
#     login (CODEX_M3B_LIVE_LOGIN_JSON, a {"access_token":...,"refresh_token":...} blob from a
#     'codex login') and the real Responses base URL (CODEX_M3B_LIVE_BASE_URL), AND passes the
#     explicit `--live` confirmation. A real credential is never baked, never a default, and
#     never read from an ambient/CI variable other than these purpose-named env vars.
#
# Usage:
#   ./live.sh --dry-run          # fake/loopback mode — no real credential, safe to automate
#   CODEX_M3B_LIVE_LOGIN_JSON='{"access_token":"...","refresh_token":"..."}' \
#   CODEX_M3B_LIVE_BASE_URL=https://api.openai.com/v1 \
#   ./live.sh --live             # real provider — maintainer only, requires the injected login
#   ./live.sh                    # REFUSES (non-zero): no mode chosen and no credential injected
#
# See LIVE-ACCEPTANCE.md for the full env contract and the exact maintainer run.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
MODE="${1:-}"

die() { printf '\nERROR: %s\n' "$*" >&2; exit 2; }
log() { printf '\n### %s\n' "$*"; }

case "$MODE" in
  --dry-run)
    # Fake/loopback mode: exactly the credential-free packaged proof, safe to run unattended.
    # It NEVER touches a real provider even if CODEX_M3B_LIVE_PROVIDER_KEY happens to be set.
    log "live.sh --dry-run: running the credential-free loopback lifecycle proof (no real provider)"
    exec "$HERE/run-lifecycle.sh"
    ;;
  --live)
    # Real provider SUBSCRIPTION mode: requires an EXPLICITLY injected, purpose-named login blob
    # and base URL. Refuse if either is absent rather than falling back to any other source.
    if [ -z "${CODEX_M3B_LIVE_LOGIN_JSON:-}" ]; then
      die "--live requires an explicitly injected CODEX_M3B_LIVE_LOGIN_JSON (a real subscription login blob {\"access_token\":...,\"refresh_token\":...} from a 'codex login'). Refusing: this script never invents, defaults, or reads a real credential from anywhere else. Use --dry-run for the credential-free loopback proof."
    fi
    if [ -z "${CODEX_M3B_LIVE_BASE_URL:-}" ]; then
      die "--live requires CODEX_M3B_LIVE_BASE_URL (the real provider Responses base URL, e.g. https://api.openai.com/v1) alongside the injected login."
    fi
    log "live.sh --live: MAINTAINER real-provider SUBSCRIPTION run against ${CODEX_M3B_LIVE_BASE_URL}"
    log "WARNING: a real coordinated refresh ROTATES the seat's refresh-token family (see LIVE-ACCEPTANCE.md)."
    # Turn live mode on everywhere and hand the injected login + base URL to run-lifecycle.sh,
    # which seeds the real login into the test server (its Go seeds discover the identity and
    # wire the real refresh client) and points the lifecycle suite at the real provider. The
    # credential stays ENV-ONLY (exported, never written to a file). A larger UZI_M3B_TEST_TIMEOUT
    # is advisable for real-provider latency (see LIVE-ACCEPTANCE.md).
    export CODEX_M3B_LIVE=1
    export CODEX_M3B_LIVE_LOGIN_JSON
    export CODEX_M3B_LIVE_BASE_URL
    exec "$HERE/run-lifecycle.sh"
    ;;
  "")
    die "no mode chosen and no credential injected. This is the automated/no-input path and it REFUSES rather than reach any provider. Use --dry-run for the credential-free loopback proof; --live (with an injected CODEX_M3B_LIVE_PROVIDER_KEY) is maintainer-only."
    ;;
  *)
    die "unknown argument '$MODE' (want --dry-run or --live)."
    ;;
esac
