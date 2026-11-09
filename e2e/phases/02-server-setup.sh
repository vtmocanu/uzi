# shellcheck shell=bash
# phase:    server-setup
# title:    PRD #3760: `/server setup` reuses the seeded connection and enables the seeded repo, then a disabled columnless one
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID DUMMY_FORGE_PAT
# provides: -
# handoff:  -
# mutates:  group/repo (REPO_ID) disabled and its board_columns rows deleted (test DB), then re-enabled with new column rows by setup
# restores: setup itself re-enables the repo and recreates the four default columns; an EXIT trap re-enables it and reads the board (which recreates missing columns) if the phase fails first
#
# Runs the API binary's `setup` entry inside the api container, as an operator
# does, against the stack the startup seed built: a gitlab connection to the
# fake and group/repo enabled. Runs before 05-lane-forgejo, which rewrites the
# seeded connection's forge_type. Expected: one JSON report on stdout, exit 0,
# the forge step `present` (reused, never re-verified or overwritten), the
# discovery `done`, group/repo `enabled` with the seeded repo id, and no worker
# requested. The forge token goes in on stdin and must appear on neither stream.
# Then the first-time case: group/repo is turned off and its column rows deleted
# (the state of a repo discovery listed but nobody enabled), and setup must enable
# it and create its default board columns itself.
say "server setup: reuse the seeded connection and enable group/repo"
setup_token="$DUMMY_FORGE_PAT"
setup_out="$RUNROOT/server-setup.json"
setup_err="$RUNROOT/server-setup.stderr"
setup_rc=0
printf '%s' "$setup_token" | "${COMPOSE[@]}" exec -T api /server setup \
  --forge-type gitlab --forge-base-url https://forge-fake.e2e \
  --forge-token-file - --repos group/repo >"$setup_out" 2>"$setup_err" || setup_rc=$?
[ "$setup_rc" -eq 0 ] || fail "server setup exited $setup_rc (want 0); stderr: $(head -c 2000 "$setup_err")"
jq -e . "$setup_out" >/dev/null || fail "server setup stdout is not one JSON document"
[ "$(jq -r '.status' "$setup_out")" = "ok" ] || fail "server setup report status: $(jq -c . "$setup_out")"
[ "$(jq -r '.forge.status' "$setup_out")" = "present" ] \
  || fail "the seeded connection must be reused (forge present): $(jq -c '.forge' "$setup_out")"
[ "$(jq -r '.discovery.status' "$setup_out")" = "done" ] \
  || fail "discovery must be done: $(jq -c '.discovery' "$setup_out")"
setup_repo="$(jq -c '.repos[] | select(.path == "group/repo")' "$setup_out")"
[ "$(jq -r '.status' <<<"$setup_repo")" = "enabled" ] || fail "group/repo must be enabled: $setup_repo"
[ "$(jq -r '.repo_id' <<<"$setup_repo")" = "$REPO_ID" ] \
  || fail "setup enabled a different repo row than the seed: $setup_repo (seeded $REPO_ID)"
[ "$(jq -r '.worker.status' "$setup_out")" = "not_requested" ] \
  || fail "no worker was requested: $(jq -c '.worker' "$setup_out")"
if grep -qF "$setup_token" "$setup_out" "$setup_err"; then
  fail "the forge token reached setup's stdout or stderr"
fi
pass "server setup reused the connection and enabled group/repo (exit 0, token on neither stream)"

# --- first-time case: a disabled repo with no board columns -----------------
say "server setup: enable a disabled, columnless group/repo"
# Fail-safe: a fail() below exits this subshell with the repo off and columnless,
# which would break every later gitlab-lane phase. The board read recreates missing
# columns. Dropped with `trap - EXIT` once setup has restored the repo.
trap 'db_psql "UPDATE repos SET enabled = true WHERE id = '\''$REPO_ID'\''" >/dev/null 2>&1 || true; apiget "/api/repos/$REPO_ID/board" >/dev/null 2>&1 || true' EXIT
db_psql "UPDATE repos SET enabled = false WHERE id = '$REPO_ID'" >/dev/null
db_psql "DELETE FROM board_columns WHERE repo_id = '$REPO_ID'" >/dev/null
[ "$(db_psql "SELECT enabled::text || ':' || (SELECT count(*) FROM board_columns WHERE repo_id = '$REPO_ID') FROM repos WHERE id = '$REPO_ID'")" = "false:0" ] \
  || fail "could not stage a disabled, columnless group/repo"
first_out="$RUNROOT/server-setup-first.json"
first_err="$RUNROOT/server-setup-first.stderr"
first_rc=0
printf '%s' "$setup_token" | "${COMPOSE[@]}" exec -T api /server setup \
  --forge-type gitlab --forge-base-url https://forge-fake.e2e \
  --forge-token-file - --repos group/repo >"$first_out" 2>"$first_err" || first_rc=$?
[ "$first_rc" -eq 0 ] || fail "first-time server setup exited $first_rc (want 0); stderr: $(head -c 2000 "$first_err")"
[ "$(jq -r '.status' "$first_out")" = "ok" ] || fail "first-time setup report status: $(jq -c . "$first_out")"
first_repo="$(jq -c '.repos[] | select(.path == "group/repo")' "$first_out")"
[ "$(jq -r '.status' <<<"$first_repo")" = "enabled" ] || fail "first-time setup must enable group/repo: $first_repo"
[ "$(jq -r '.repo_id' <<<"$first_repo")" = "$REPO_ID" ] || fail "first-time setup enabled another repo row: $first_repo"
[ "$(db_psql "SELECT enabled::text FROM repos WHERE id = '$REPO_ID'")" = "true" ] \
  || fail "group/repo is not enabled in the database after setup"
first_cols="$(db_psql "SELECT string_agg(label_name, ',' ORDER BY position) FROM board_columns WHERE repo_id = '$REPO_ID'")"
[ "$first_cols" = "Planned,In Progress,Human Review,Later" ] \
  || fail "setup must create the default board columns before enabling; got '$first_cols'"
if grep -qF "$setup_token" "$first_out" "$first_err"; then
  fail "the forge token reached first-time setup's stdout or stderr"
fi
trap - EXIT
pass "server setup enabled a disabled, columnless group/repo and created its four board columns"
