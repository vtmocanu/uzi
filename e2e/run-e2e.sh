#!/usr/bin/env bash
# M6 end-to-end harness for the uzi agent runtime.
#
# Stands up an ISOLATED compose stack (unique -p, per-run --env-file + scratch
# dir) with DUMMY credentials and the STUB executor, then drives the full
# Success-Criteria path with NO live Anthropic session and NO real GitLab:
#
#   seed admin+forge+repo (boot) → issue a worker join token → worker online →
#   create a PRD issue → start a run → plan gate (awaiting_approval) →
#   approve → stub implement → worker pushes agent/issue-N to a local bare remote
#   + opens an MR against the fake GitLab → completed (branch + mr_iid).
#
# Plus restart-resilience (down/up keeping volumes, mid-run at the gate → the run
# is re-queued, re-claimed, and driven to completion with a gapless seq) and the
# server-side cancel path.
#
# Then the PRD #24 MR-close watcher: with the poller sped to ~2s, closing the
# completed run's MR (without merging, via forge-fake's /_e2e mutator) moves the
# card Human Review → In Progress; reopening restores it; and a manual drag is
# never fought (the reopen edge's source-column guard backs off).
#
# Finally (PRD #42, stub-only), the single worker is reconfigured to
# WORKER_MAX_CONCURRENT_RUNS=2 and shown to execute two runs on two DIFFERENT repos
# genuinely concurrently (both parked at the gate at once — a cap-1 worker cannot),
# reporting active_runs=2/cap=2 while live, landing both MRs on independent git
# bare-caches, and re-queuing BOTH in-flight runs together (sweeper at N=2) after a
# mid-run SIGKILL, with a restarted worker completing them. Stated limit: the stub
# is already concurrency-safe, so this covers the loop/server/API path, NOT the M1
# executor kill/reap fix (guarded by an agent/ unit test).
#
# Observable assertions: DB run-state transitions (via the API), gapless
# run_messages seq, branch pushed to the remote, MR recorded by the fake GitLab,
# NO secret (PAT / Anthropic token / worker join token) in container logs or on
# the worker's disk; the M6 /proc hardening — the join token is absent from every
# process's /proc/<pid>/environ; and the PRD #51 M4/M5 uid split — the join token
# secret is 0400 worker:worker, so the worker uid can read it but the runner uid
# (which runs the untrusted agent/checks/provision) is DENIED.
#
# Everything tears down with `down -v`; the user's own `uzi` stack is never
# touched (unique project name, project-scoped volumes, its own env-file).
#
# Executor switch (for the OPTIONAL, user-gated live capstone — see README):
#   UZI_E2E_EXECUTOR=sdk  runs the real Claude Agent SDK instead of the stub.
#   Default is stub. The live path additionally needs a real seeded token and is
#   never exercised automatically.

set -euo pipefail

# --- `--list`: dump the phase registry and exit (PRD #966 M2) ----------------
# Handled BEFORE the `env -i` re-exec below so it needs no docker and no clean
# environment — it only reads the phase-file headers. Prints one row per phase
# (NN slug | lane | critical | title | requires | provides) so a developer can
# see the registry (and the durable slug identifiers for E2E_ONLY / E2E_SKIP)
# without standing up the stack.
if [ "${1:-}" = --list ]; then
  _list_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  printf '%-4s %-26s %-8s %-9s %-16s %-16s %s\n' \
    NN SLUG LANE CRITICAL REQUIRES PROVIDES TITLE
  for _pf in "$_list_root"/e2e/phases/[0-9][0-9]-*.sh; do
    [ -e "$_pf" ] || continue
    _base="$(basename "$_pf" .sh)"
    _nn="${_base%%-*}"
    _slug="${_base#[0-9][0-9]-}"
    _hv() { sed -n "s/^# $1:[[:space:]]*//p" "$_pf" | head -1; }
    printf '%-4s %-26s %-8s %-9s %-16s %-16s %s\n' \
      "$_nn" "$_slug" "$(_hv lane)" "$(_hv critical)" \
      "$(_hv requires)" "$(_hv provides)" "$(_hv title)"
  done
  exit 0
fi

# --- shell hygiene: re-exec under a CLEAN environment ------------------------
# THE HARNESS MUST TEST WHAT THE REPO SHIPS, NOT WHAT THE OPERATOR'S SHELL EXPORTS.
#
# Compose ranks a shell-exported variable ABOVE --env-file (CLAUDE.md), so any var in
# the caller's profile silently replaces docker-compose.yml's `${VAR:-default}` for
# this run. That is not hypothetical and it is not cheap: the PRD #58 XFF gate below
# was developed against a shell exporting the OLD
# TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12,... — so the pre-fix AND post-fix runs both
# tested the same vulnerable value, and BOTH RESULTS WERE MEANINGLESS. A measurement
# taken through a dirty environment is not a weaker measurement, it is not one.
#
# Measured on the author's laptop when that surfaced: **19 of 62** vars
# docker-compose.yml reads were exported in an ordinary dev shell, including real
# UZI_SEED_FORGE_PAT, UZI_SEED_ANTHROPIC_TOKEN and JWT_SECRET.
#
# WHY AN ALLOWLIST AND NOT A LIST OF `unset`s (user decision). The overlay pins the
# dangerous vars it wants to CHOOSE (UZI_SECRET_KEY, the UZI_SEED_* set), and unsetting
# the rest one by one works only while someone remembers to extend the list every time
# docker-compose.yml grows a knob. That is "true by bookkeeping, not by construction" —
# the same shape PRD #58 rejected three times over (narrowing TRUSTED_PROXIES; the
# CIDR-vs-FQDN allowlist; the chart's plaintext-port fallback). Deny-by-default means a
# var added tomorrow cannot leak into a run without someone adding it HERE, on purpose.
# CLAUDE.md already mandates this exact shape for compose smoke tests; e2e was exempted
# only because it was believed immune, which it was not.
#
# WHAT IS DELIBERATELY *NOT* ALLOWED, and why the list must stay this short: every var
# docker-compose.yml reads as `${VAR:-default}` — above all TRUSTED_PROXIES and
# RATE_LIMIT_* — because the assertions here exist to exercise those SHIPPED defaults.
# Passing them through (or pinning them in the overlay) would make the gate assert
# against a value the harness chose, so it would keep passing with the vulnerable
# default restored. Do not add one to buy a local convenience.
#
# What IS allowed: how to reach the machine (PATH/HOME/TMPDIR/TERM/docker daemon
# addressing) and the harness's own knobs. None of these reach the api's config.
#
# WHY THE GATE IS AN ARGUMENT AND NOT AN ENVIRONMENT VARIABLE. The re-exec has to fire
# whenever the caller's environment is dirty, so the "have I sanitized yet?" test must
# not be something that environment can answer. This gate WAS an env sentinel
# (`[ -z "${UZI_E2E_SANITIZED:-}" ]`), and a sentinel is inherited like any other var:
# `export UZI_E2E_SANITIZED=1` skipped the entire re-exec and handed the stack the real
# JWT_SECRET, the real UZI_SEED_FORGE_PAT and the vulnerable TRUSTED_PROXIES, with no
# warning. The realistic path there is accident, not malice — it reads exactly like the
# intended escape hatch, so a developer wanting one var through for debugging finds it by
# reading this very block, and it then lives in their profile forever. It was also the
# same shape the paragraphs above reject three times over: a claim the environment makes
# about itself, trusted without verification.
#
# `$1` cannot be inherited. `env -i` clears the environment, and the argv below is one we
# construct ourselves, so the only way to reach the sanitized branch is through that exec
# (or by typing the flag, which is a deliberate act, not an ambient one). Do not
# "simplify" this back into an env check, and do not swap in a cleverer sentinel — any
# value the ambient environment can supply is this same bug wearing a different hat.
if [ "${1:-}" != "--e2e-sanitized" ]; then
  _e2e_env=()
  for _v in \
    HOME PATH TMPDIR TERM CI \
    DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG DOCKER_CERT_PATH DOCKER_TLS_VERIFY DOCKER_TLS_CERTDIR \
    UZI_E2E_EXECUTOR UZI_E2E_COMPOSE_PROJECT UZI_E2E_COMPLETE_TIMEOUT UZI_E2E_FORGE \
    E2E_RUN_DIR E2E_GIT_SMART_HTTP KEEP_STACK KEEP_RUNDIR \
    PHASES_DIR E2E_ONLY E2E_SKIP E2E_STRICT_LEAKS E2E_FAULT_PHASE
  do
    [ -n "${!_v+set}" ] && _e2e_env+=("$_v=${!_v}")
  done
  exec env -i "${_e2e_env[@]}" bash "${BASH_SOURCE[0]}" --e2e-sanitized "$@"
fi
shift  # drop --e2e-sanitized; safe — this line is reached only when $1 held it

# --- optional args -----------------------------------------------------------
# The ONE supported positional flag: `--profile agent-docker` (PRD #83 M2). It brings up
# the rootless DinD sidecar (dind + dind-init) alongside the worker and runs the
# docker-capable PHASE at the end: sidecar reachability + a toy `docker compose up` + the
# LIVE Decision-3 efficacy test (a sidecar container cannot read the worker's join token).
# The default (no args) is BYTE-IDENTICAL to before. Args survive the env -i re-exec above
# (they ride after --e2e-sanitized). No env knob is used, so nothing new enters the
# compose-read env — the sidecar is opt-in purely by this flag + the compose profile.
DOCKER_PROFILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --profile)
      [ "${2:-}" = "agent-docker" ] || { echo "error: only '--profile agent-docker' is supported (got '${2:-}')" >&2; exit 2; }
      DOCKER_PROFILE="agent-docker"; shift 2 ;;
    *) echo "error: unknown argument '$1' (only '--profile agent-docker' is supported)" >&2; exit 2 ;;
  esac
done

# --- layout ------------------------------------------------------------------
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXECUTOR="${UZI_E2E_EXECUTOR:-stub}"
# UZI_E2E_FORGE selects the forge lane: "gitlab" (default — the full suite,
# byte-identical), "forgejo" (PRD #65 M9 — a focused Forgejo lifecycle against the
# same fake's /api/v1 table) or "github" (PRD #238 M8 — a focused GitHub lifecycle
# against the same fake's /api/v3 table), each run INSTEAD of the GitLab suite. It is a HARNESS
# knob like UZI_E2E_EXECUTOR, NOT a docker-compose.yml ${VAR:-default} var, so it is
# safe in the env -i allowlist above: the allowlist excludes compose-read vars
# precisely so the gate exercises the SHIPPED defaults, and this var never reaches
# the api's config (it only branches the harness).
FORGE="${UZI_E2E_FORGE:-gitlab}"
case "$FORGE" in gitlab|forgejo|github) : ;; *) echo "error: UZI_E2E_FORGE must be gitlab, forgejo or github (got '$FORGE')" >&2; exit 2 ;; esac
PROJECT="${UZI_E2E_COMPOSE_PROJECT:-uzi-e2e-$$}"
# Compose project-name guard (PRD #33 Decision 7): reject an invalid RESOLVED
# project name up front — before any scratch-dir or compose work — so a branch-like
# UZI_E2E_COMPOSE_PROJECT with a slash (e.g. feature/prd-33) fails with a clear
# message instead of docker rejecting it mid-run, after setup has begun. We validate,
# never rewrite: an explicit value is user intent, and silently sanitizing it would
# hide the mismatch from the logs and teardown hints. The rule is Compose's own
# (lowercase alphanumerics, '-', '_', starting alphanumeric); the PID-based default
# uzi-e2e-$$ always passes, so no provenance check is needed — an invalid value is
# always user-set.
project_name_re='^[a-z0-9][a-z0-9_-]*$'
if [[ ! "$PROJECT" =~ $project_name_re ]]; then
  echo "error: invalid compose project name '$PROJECT'" >&2
  echo "  UZI_E2E_COMPOSE_PROJECT must match ${project_name_re} (lowercase alphanumerics," >&2
  echo "  '-' and '_', starting with an alphanumeric). A branch-like name with a '/' is not valid." >&2
  exit 2
fi
# A real SDK agent turn takes minutes (observed ~13m: the seeded template spawns
# a reviewer subagent), where the stub finishes in seconds.
# shellcheck disable=SC2034  # read by phase files via wait_status (PRD #966 M1 split)
[ "$EXECUTOR" = sdk ] && COMPLETE_TIMEOUT_DEFAULT=1800 || COMPLETE_TIMEOUT_DEFAULT=90
RUNROOT="${E2E_RUN_DIR:-${TMPDIR:-/tmp}/uzi-e2e-$$}"
RUNROOT="${RUNROOT%/}"
ENVFILE="$RUNROOT/e2e.env"
# shellcheck disable=SC2034  # read by lib.sh csrf/login/apiget (PRD #966 M1 split)
JAR="$RUNROOT/admin.jar"
# shellcheck disable=SC2034  # read by lib.sh cleanup (PRD #966 M1 split)
KEEP="${KEEP_RUNDIR:-}"

# Dummy credentials (must match the api overlay's seed literals).
# shellcheck disable=SC2034  # read by lib.sh login (PRD #966 M1 split)
ADMIN_EMAIL="admin@uzi.e2e"
# shellcheck disable=SC2034  # read by lib.sh login (PRD #966 M1 split)
ADMIN_PASS="e2e-admin-password-000000"
# shellcheck disable=SC2034  # read by phase files (PRD #966 M1 split)
DUMMY_FORGE_PAT="e2e-dummy-forge-pat-000000"
# shellcheck disable=SC2034  # read by phase files (PRD #966 M1 split)
DUMMY_ANTHROPIC="sk-ant-e2e-dummy-do-not-use-000000"

WEB_PORT="$(( 20000 + (RANDOM % 20000) ))"
# shellcheck disable=SC2034  # read by lib.sh api helpers (PRD #966 M1 split)
BASE="http://127.0.0.1:${WEB_PORT}"
# forge-fake's /_e2e surface, published on the next loopback port (see the
# fake_post/fake_state helpers below).
FAKE_PORT="$(( WEB_PORT + 1 ))"
# shellcheck disable=SC2034  # read by lib.sh fake_post/fake_state (PRD #966 M1 split)
FAKE_BASE="https://127.0.0.1:${FAKE_PORT}"

COMPOSE=(docker compose -p "$PROJECT" --project-directory "$ROOT" --env-file "$ENVFILE"
  -f "$ROOT/docker-compose.yml" -f "$ROOT/e2e/docker-compose.e2e.yml" --profile agent)
# PRD #83 M2: also activate the DinD sidecar profile when `--profile agent-docker` was
# passed, so `dind` + `dind-init` come up for the docker-capable phase. `down` ignores
# profiles, so the teardown hint below still removes everything.
[ -n "$DOCKER_PROFILE" ] && COMPOSE+=(--profile "$DOCKER_PROFILE")

# --- shared helpers (PRD #966 M1) --------------------------------------------
# The output/api/forge-fake helpers, cleanup + its EXIT trap, wait_*/report_margins,
# create_run, login, the /_e2e helpers, and the three cross-phase helpers
# (uzi_cli, run_printed_instructions, wait_msg_kind) live in e2e/lib.sh, sourced
# here so both this entry script and every phase file see them. $ROOT is set above.
source "$ROOT/e2e/lib.sh"

# --- build + bring up the control plane (no worker yet) ----------------------
# Reclaim leaked artifacts from prior ABORTED e2e runs BEFORE we build, so a
# previous killed run's leftover project cannot ENOSPC-poison this one's worker.
# Best-effort and safe by construction (see e2e/reclaim-leaked-e2e.sh): it only
# ever tears down definitely-dead `uzi-e2e-<pid>` projects, never `uzi`/store-it/
# custom names, never a live concurrent run. `|| true`: this runs under
# `set -euo pipefail` and must never fail the run.
"$ROOT/e2e/reclaim-leaked-e2e.sh" "$PROJECT" || true

say "building images"
"${COMPOSE[@]}" build >/dev/null

say "starting db + api + web + forge-fake"
"${COMPOSE[@]}" up -d --wait db api web forge-fake
wait_http
pass "control plane up; boot seed provisioned admin + forge + repo"

login
REPO_ID="$(apiget /api/repos | jq -r '.repos[0].id // empty')"
[ -n "$REPO_ID" ] || fail "no enabled repo after seed (expected group/repo)"
pass "admin logged in; repo $REPO_ID enabled"

# Open the board once up front so its columns are seeded (as labels on the fake).
# An unopened board has no columns, which would let the run-lifecycle's
# single-column moves leave a card carrying two column labels; seeding first keeps
# the column state clean for the run-lifecycle path and the PRD #24 MR-close phase.
apiget "/api/repos/$REPO_ID/board" >/dev/null
pass "board columns seeded"

# =============================================================================

# --- phase registry driver (PRD #966 M2) -------------------------------------
# All driver semantics — lane filter, ONLY/SKIP selection, requires validation,
# the errexit-safe subshell-per-phase, provides round-trip, fail-soft + critical,
# end-of-phase quarantine, results (results.tsv / junit.xml / summary.md) and the
# roll-call — live in e2e/driver.sh. It is sourced HERE at top level (never inside
# a function) so its loop and its `source "$RUNROOT/phase.env.next"` run in this
# shell: a `declare -p`-dumped var re-sourced inside a function becomes
# function-local, which would silently break every phase's `provides:`.
#
# driver.sh reads the functions (say/pass/fail/db_psql/apipost/apipost_code/
# wait_status) and globals (ROOT/RUNROOT/ENVFILE/FORGE/EXECUTOR) established above,
# plus the E2E_ONLY / E2E_SKIP / E2E_STRICT_LEAKS / E2E_FAULT_PHASE knobs (kept in
# the env -i allowlist at the top of this file). It ENDS WITH `exit`, so this is
# the last thing the entry script runs and its exit drives the cleanup trap.
# shellcheck disable=SC1090  # $ROOT is a runtime path; driver.sh is sourced by design
source "$ROOT/e2e/driver.sh"
