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
#   * A real provider is reached ONLY when a maintainer EXPLICITLY injects the credential
#     (CODEX_M3B_LIVE_PROVIDER_KEY) AND passes the explicit `--live` confirmation. A real
#     credential is never baked, never a default, and never read from an ambient/CI variable
#     other than this one purpose-named env var.
#
# Usage:
#   ./live.sh --dry-run          # fake/loopback mode — no real credential, safe to automate
#   CODEX_M3B_LIVE_PROVIDER_KEY=sk-... \
#   CODEX_M3B_LIVE_BASE_URL=https://... \
#   ./live.sh --live             # real provider — maintainer only, requires the injected key
#   ./live.sh                    # REFUSES (non-zero): no mode chosen and no credential injected
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
    # Real provider mode: requires an EXPLICITLY injected, purpose-named credential. Refuse if
    # it is absent rather than falling back to any other source.
    if [ -z "${CODEX_M3B_LIVE_PROVIDER_KEY:-}" ]; then
      die "--live requires an explicitly injected CODEX_M3B_LIVE_PROVIDER_KEY (a real provider credential). Refusing: this script never invents, defaults, or reads a real credential from anywhere else. Use --dry-run for the credential-free loopback proof."
    fi
    if [ -z "${CODEX_M3B_LIVE_BASE_URL:-}" ]; then
      die "--live requires CODEX_M3B_LIVE_BASE_URL (the real provider base URL) alongside the injected key."
    fi
    log "live.sh --live: MAINTAINER real-provider run against ${CODEX_M3B_LIVE_BASE_URL}"
    log "(this path is intentionally left for a maintainer to wire the injected key into a"
    log " real-provider harness build; it is NEVER exercised by an automated/no-input run.)"
    # Deliberately NOT auto-wiring the injected secret into a container run here: the real
    # provider harness is a maintainer step, and this script's contract is only to GUARD the
    # credential boundary (refuse without an explicit key + confirmation), not to spend it
    # unattended. A maintainer extends this branch behind their own review.
    die "real-provider execution is a deliberate manual maintainer step; wire the injected CODEX_M3B_LIVE_PROVIDER_KEY into your run here under review. No unattended real-provider run is performed."
    ;;
  "")
    die "no mode chosen and no credential injected. This is the automated/no-input path and it REFUSES rather than reach any provider. Use --dry-run for the credential-free loopback proof; --live (with an injected CODEX_M3B_LIVE_PROVIDER_KEY) is maintainer-only."
    ;;
  *)
    die "unknown argument '$MODE' (want --dry-run or --live)."
    ;;
esac
