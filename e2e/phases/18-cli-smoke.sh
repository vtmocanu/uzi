# shellcheck shell=bash
# phase:    cli-smoke
# title:    PRD #97 M2: uzi CLI drives the live api (run list matches + approve advances a run)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: UZI_BIN UZI_TOKEN_VAL
# handoff:  -
# mutates:  -
# restores: -
# --- Leg 2: uzi CLI smoke against the live api -------------------------------
# The uzi CLI (api/cmd/uzi, docs/cli.md) is a second consumer of the SAME API the web UI
# drives; a route/DTO/behavior change that only touches web/ can leave it silently stale
# (CLAUDE.md: "New uzi functionality ⇒ check api/cmd/uzi/"). Build it and drive a thin but
# real flow — a list that must MATCH the api's own view, then a state-changing approve —
# so DTO/route drift in the CLI turns the run red. It runs on the HOST against $BASE (the
# loopback http origin the CLI accepts for 127.0.0.1), authed headless via a minted uzc_
# $UZI_TOKEN — no cookie, no browser (docs/cli.md).
say "PRD #97 M2: uzi CLI drives the live api (run list matches + approve advances a run)"
command -v go >/dev/null 2>&1 || fail "the uzi CLI leg needs 'go' on PATH to build api/cmd/uzi (host tool)"
UZI_BIN="$RUNROOT/uzi"
# Build the throwaway CLI: prefer the clean build so VCS stamping is preserved in a normal
# checkout; fall back to -buildvcs=false ONLY when a linked worktree blocks VCS status
# (the team's PRD layout — a plain `go build` there fails with "error obtaining VCS status").
# The flag rides only this test-binary build, never a product/CI build path. A real compile
# error still surfaces: the fallback build (no 2>/dev/null) prints it before `fail`.
( cd "$ROOT/api" && go build -o "$UZI_BIN" ./cmd/uzi 2>/dev/null ) \
  || ( cd "$ROOT/api" && go build -buildvcs=false -o "$UZI_BIN" ./cmd/uzi ) \
  || fail "could not build the uzi CLI (go build ./cmd/uzi)"
pass "built the uzi CLI binary"

# Mint a uzc_ (user-scope) CLI token from the harness admin session: POST /api/me/cli-tokens
# is cookie-only (RequireAuth + CSRF, via apipost) and returns the plaintext token exactly
# once in .token (handler CreateCLIToken; docs/cli.md "Settings → Access"). scope defaults
# to "user" ⇒ a uzc_ token capped to the owner's (admin's) own authority.
UZI_TOKEN_VAL="$(apipost "/api/me/cli-tokens" '{"name":"e2e-m2-cli-smoke"}' | jq -r '.token')"
{ [ -n "$UZI_TOKEN_VAL" ] && [ "$UZI_TOKEN_VAL" != null ] && [ "${UZI_TOKEN_VAL#uzc_}" != "$UZI_TOKEN_VAL" ]; } \
  || fail "did not mint a uzc_ CLI token via POST /api/me/cli-tokens (got '${UZI_TOKEN_VAL:-<none>}')"
pass "minted a uzc_ CLI token via POST /api/me/cli-tokens (headless \$UZI_TOKEN auth)"

# Run the CLI hermetically: HOME → the scratch rundir so it never reads/writes the
# operator's ~/.config/uzi or ~/.claude; UZI_URL/UZI_TOKEN override any config file;
# UZI_SKILL_AUTO_UPGRADE=0 so it drops no Claude Code skill; UZI_VERSION_CHECK=0 so it
# neither probes GET /api/version before every command nor writes a version-check.json
# into the rundir.
#
# That last one is BELT-AND-BRACES TODAY, and it is worth knowing which: the build
# above passes no -ldflags, so this binary's version is the "dev" default and the skew
# check short-circuits on an unstamped version before it ever resolves a URL. The env
# var is set anyway because that reason evaporates the day someone stamps the e2e
# build — and UZI_URL IS set here, so the hook would fire the moment it does. The cost
# of being wrong is not cosmetic: the printed-instruction assertions further down count
# exact lines across stdout AND stderr, so one extra stderr sentence reddens a check
# about something else entirely.

# (1) `uzi run list --json` must parse AND its run-id set must equal GET /api/runs's — a
# DTO/route drift (renamed envelope key, changed id field, moved route) makes them diverge.
CLI_RUNS="$(uzi_cli run list --json)" || fail "uzi run list --json failed (exit $?)"
echo "$CLI_RUNS" | jq -e 'type=="array"' >/dev/null || fail "uzi run list --json is not a JSON array: $CLI_RUNS"
API_IDS="$(apiget /api/runs | jq -S '[.runs[].id]|sort')"
CLI_IDS="$(echo "$CLI_RUNS" | jq -S '[.[].id]|sort')"
[ "$API_IDS" = "$CLI_IDS" ] \
  || fail "uzi run list run-ids diverge from GET /api/runs (cli=$CLI_IDS api=$API_IDS)"
pass "uzi run list --json parses and matches GET /api/runs ($(echo "$CLI_IDS" | jq 'length') runs)"

# (2) A real state-changing round-trip through the CLI: create + park a run, then
# `uzi run approve` it (the CLI's own submitInput → POST /api/runs/{id}/inputs) and assert
# it advances to completed. Owner-scoped: the uzc_ token owns these runs (minted from the
# same admin session that created them).
IID_CLI="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E cli approve","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
{ [ -n "$IID_CLI" ] && [ "$IID_CLI" != null ]; } || fail "could not create the cli-approve issue"
RUN_CLI="$(create_run "$REPO_ID" "$IID_CLI")" || fail "cli-leg run was not created"
wait_status "$RUN_CLI" awaiting_approval
uzi_cli run approve "$RUN_CLI" >/dev/null || fail "uzi run approve failed (exit $?)"
wait_status "$RUN_CLI" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "uzi run approve drove RUN_CLI past the gate to completed (CLI approve route/DTO intact)"

