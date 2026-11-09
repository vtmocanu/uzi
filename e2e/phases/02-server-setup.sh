# shellcheck shell=bash
# phase:    server-setup
# title:    PRD #3760: `/server setup` reuses the seeded connection and enables the seeded repo
# critical: no
# lane:     gitlab
# executor: any
# requires: REPO_ID DUMMY_FORGE_PAT
# provides: -
# handoff:  -
# mutates:  -
# restores: -
#
# Runs the API binary's `setup` entry inside the api container, as an operator
# does, against the stack the startup seed built: a gitlab connection to the
# fake and group/repo enabled. Runs before 05-lane-forgejo, which rewrites the
# seeded connection's forge_type. Expected: one JSON report on stdout, exit 0,
# the forge step `present` (reused, never re-verified or overwritten), the
# discovery `done`, group/repo `enabled` with the seeded repo id, and no worker
# requested. The forge token goes in on stdin and must appear on neither stream.
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
