#!/usr/bin/env bash
# reset-recovery-tables.sh — FK-safe clear of the PRD #1296/#1349 recovery tables in a
# THROWAWAY test database. HARNESS ISOLATION ONLY.
#
# WHY THIS EXISTS. The store-it suite (e2e/run-store-it.sh) runs every *LiveDB test in
# five packages against ONE shared throwaway Postgres (`-p 1`), and — as run-store-it.sh
# itself documents for `workers` — nothing truncates a table between package binaries, so
# rows one package's test seeds outlive it. The recovery tables (recovery_custody_holds,
# recovery_captures, recovery_capture_chunks, custody_episode_notices) are seeded by the
# store/handler/recovery/workersvc LiveDB tests and by the e2e custody phase; leaked
# 'open' custody holds can wedge an unrelated later test/phase whose owner then trips the
# custody-admission limit (exactly the 2026-09-14 GitLab-lane wedge, sanitized). This
# script clears those rows so the throwaway DB starts (or resumes) isolated.
#
# 🔴 THIS IS HARNESS HYGIENE, NEVER PRODUCT-FIX EVIDENCE. It proves nothing about the
# #1349 custody contract. The PRODUCT behaviour — that the admission gate blocks at the
# limit and the owner-disposition path frees a slot — is proven by e2e/phases/72-custody-
# lifecycle.sh and the unit/live-DB suites it maps; do NOT cite this reset as evidence
# that custody accumulation is fixed. Deleting the rows only hides accumulation.
#
# FK-SAFE DELETE ORDER (from migration 00223_recovery_archive.sql):
#   recovery_capture_chunks  → recovery_captures  → recovery_custody_holds
# recovery_capture_chunks is ON DELETE CASCADE from recovery_captures, but recovery_captures
# is ON DELETE RESTRICT from recovery_custody_holds — so a hold cannot be deleted while a
# capture references it. Deleting chunks, then captures, then holds satisfies the RESTRICT
# without relying on any cascade. custody_episode_notices (migration 00226) is independent
# of that chain (FK to users, ON DELETE CASCADE) and is cleared last.
#
# NEVER AGAINST A NON-TEST DB. Like the rest of the harness (which fences destructive work
# by the dedicated UZI_TEST_DATABASE_URL var the Go tests refuse to run without, the
# uzi-e2e-<pid> project-name + pid-liveness pattern in reclaim-leaked-e2e.sh, and the
# loopback-only DSN the throwaway Postgres publishes on), this refuses unless the target is
# unmistakably a throwaway: the DSN comes ONLY from --dsn / the first arg / UZI_TEST_DATABASE_URL
# (NEVER the production DATABASE_URL), its host is loopback, and it carries sslmode=disable
# (the throwaway store-it/e2e DSN always does; a real managed Postgres uses TLS). Any miss
# aborts with NO deletion.
#
# Usage:
#   UZI_TEST_DATABASE_URL=postgres://uzi:PASS@127.0.0.1:PORT/uzi?sslmode=disable e2e/reset-recovery-tables.sh
#   e2e/reset-recovery-tables.sh --dsn 'postgres://uzi:PASS@127.0.0.1:PORT/uzi?sslmode=disable'
# Delivery: a host `psql` if present, else a throwaway postgres:17 container on the host
# network (Linux/CI, where the throwaway DB is published on host loopback), matching
# run-store-it.sh's image knob UZI_STORE_IT_PG_IMAGE.
set -euo pipefail

DSN=""
case "${1:-}" in
  --dsn) DSN="${2:-}" ;;
  "")    DSN="${UZI_TEST_DATABASE_URL:-}" ;;
  -*)    echo "reset-recovery-tables: unknown flag '$1' (use --dsn <url> or set UZI_TEST_DATABASE_URL)" >&2; exit 2 ;;
  *)     DSN="$1" ;;
esac

if [ -z "$DSN" ]; then
  echo "reset-recovery-tables: no target DSN — pass --dsn <url> or set UZI_TEST_DATABASE_URL." >&2
  echo "  (The production DATABASE_URL is deliberately NEVER read; this tool only ever" >&2
  echo "   touches a throwaway test database.)" >&2
  exit 2
fi

# --- GUARD (fence by construction — mirror the harness's throwaway-only fences) ---------
# Parse the host out of the DSN in pure shell (no psql needed to make the safety decision).
host=""
if [[ "$DSN" =~ ^postgres(ql)?://([^@/]*@)?([^:/?]+) ]]; then
  host="${BASH_REMATCH[3]}"
fi
case "$host" in
  127.0.0.1|localhost|::1|"[::1]") ;;
  *)
    echo "reset-recovery-tables: REFUSING — DSN host '$host' is not loopback." >&2
    echo "  This clears recovery tables and only ever runs against a loopback throwaway DB." >&2
    exit 3 ;;
esac
case "$DSN" in
  *sslmode=disable*) ;;
  *)
    echo "reset-recovery-tables: REFUSING — DSN does not carry sslmode=disable." >&2
    echo "  The throwaway store-it/e2e Postgres always sets it; a real (TLS) DB never would." >&2
    exit 3 ;;
esac

PGIMAGE="${UZI_STORE_IT_PG_IMAGE:-postgres:17}"

# run_psql — read the SQL script from stdin. Host psql if available, else a throwaway
# postgres container sharing the host network so the loopback DSN resolves.
run_psql() {
  if command -v psql >/dev/null 2>&1; then
    psql "$DSN" -v ON_ERROR_STOP=1 -q
  else
    docker run --rm -i --network host "$PGIMAGE" psql "$DSN" -v ON_ERROR_STOP=1 -q
  fi
}

echo "reset-recovery-tables: clearing recovery tables on loopback throwaway DB (host=$host) — HARNESS ISOLATION ONLY."

# One implicit transaction; each DELETE guarded by to_regclass so a partially-migrated or
# never-migrated throwaway DB (no recovery schema yet) is a clean no-op rather than an
# error. Row counts are RAISEd as NOTICEs (stderr) for the log.
run_psql <<'SQL'
DO $$
DECLARE
  n_chunks   int := 0;
  n_captures int := 0;
  n_holds    int := 0;
  n_notices  int := 0;
BEGIN
  -- FK-safe order: chunks (CASCADE child) → captures (RESTRICT child of holds) → holds.
  IF to_regclass('public.recovery_capture_chunks') IS NOT NULL THEN
    DELETE FROM recovery_capture_chunks;
    GET DIAGNOSTICS n_chunks = ROW_COUNT;
  END IF;
  IF to_regclass('public.recovery_captures') IS NOT NULL THEN
    DELETE FROM recovery_captures;
    GET DIAGNOSTICS n_captures = ROW_COUNT;
  END IF;
  IF to_regclass('public.recovery_custody_holds') IS NOT NULL THEN
    DELETE FROM recovery_custody_holds;
    GET DIAGNOSTICS n_holds = ROW_COUNT;
  END IF;
  -- Independent of the hold chain; cleared last.
  IF to_regclass('public.custody_episode_notices') IS NOT NULL THEN
    DELETE FROM custody_episode_notices;
    GET DIAGNOSTICS n_notices = ROW_COUNT;
  END IF;
  RAISE NOTICE 'reset-recovery-tables: deleted chunks=% captures=% holds=% episode_notices=%',
    n_chunks, n_captures, n_holds, n_notices;
END $$;
SQL

echo "reset-recovery-tables: done."
