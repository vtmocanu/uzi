# shellcheck shell=bash
# phase:    git-push-basic-auth
# title:    PRD #97 M1: worker pushes the agent branch over git-over-HTTPS Basic auth (default coverage)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #97 M1 — git-over-HTTPS Basic-auth push (on EVERY default run) + the
# main-push-refused backstop. Two full-wire-only properties the lower layers cannot
# reach.
#
# (a) The default happy path above pushes the agent branch to a LOCAL bare via the
# `insteadOf` rewrite (:559-564), so git ignores all http.* config and the worker's
# `Authorization: Basic` header is NEVER sent — the exact blind spot the shipped
# PRIVATE-TOKEN-vs-Basic bug slipped through (README). This leg makes the worker push
# over git-smart-HTTP for real: we drop the insteadOf rewrite (the agent gitconfig is a
# bind-mounted file GIT_CONFIG_GLOBAL points at, read fresh per git op — no recreate
# needed; remote.origin.url stored on the warm bare is the https URL, so its fetch/push
# now go over HTTPS), run one issue on group/repo, and let the worker fetch+push against
# forge-fake, which 401s any git op lacking a valid Basic uzi-bot:PAT. A run that reaches
# `completed` with its branch on the bare therefore PROVES the worker sent Basic; a
# credential-injection regression turns this red. Scoped to the SINGLE repo on purpose
# (Decision 1): forge-fake routes every repo path to one shared bare, so a smart-HTTP
# happy-path flip would collapse the PRD #42 two-repo independent-bare asserts (:26xx) —
# #42 stays on insteadOf. We restore insteadOf before any later phase, which all rely on
# the local bare.
say "PRD #97 M1: worker pushes the agent branch over git-over-HTTPS Basic auth (default coverage)"
# Drop insteadOf ⇒ the worker's git speaks smart-HTTP to forge-fake. Keep safe.directory:
# this file is the worker's GIT_CONFIG_GLOBAL, and it is the trust the LATER local-bare
# phases need. gitEnv()'s inline pin cannot serve them (it is command scope, which git
# strips before the local receive-pack child that checks ownership); the worker also runs
# GIT_CONFIG_NOSYSTEM=1 (agent/src/git.ts), ruling out /etc/gitconfig — so this global file
# is the one scope that both survives to the child AND this env allows. Every rewrite of
# this file must re-carry it.
cat > "$RUNROOT/agent-gitconfig/gitconfig" <<'EOF'
[safe]
	directory = *
EOF
# POSITIVE transport control: forge-fake's git bare (/gitroot/repo.git) is the SAME host
# dir as the local-path bare (/fakeremote/repo.git), so "branch present" alone cannot tell
# a smart-HTTP push from a local one. Snapshot forge-fake's authenticated-push counter
# before the run and require it to rise — proving the worker's push actually traversed the
# Basic-gated smart-HTTP endpoint (a silently-failed insteadOf flip would leave it flat).
RECV_BEFORE="$(fake_state | jq '.gitStats.receivePackPosts // 0')"
IID_HA="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E git basic-auth","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
{ [ -n "$IID_HA" ] && [ "$IID_HA" != null ]; } || fail "could not create the git-basic-auth issue"
RUN_HA="$(create_run "$REPO_ID" "$IID_HA")" || fail "git-basic-auth run was not created"
wait_status "$RUN_HA" awaiting_approval
apipost "/api/runs/$RUN_HA/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_HA" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
# forge-fake wrote the push into /gitroot/repo.git == $RUNROOT/fakeremote/repo.git. Its
# presence means the worker's fetch AND push both carried a valid Authorization: Basic
# (forge-fake 401s otherwise, which would have failed the run before this point).
git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_HA" \
  || fail "agent/issue-$IID_HA not on the bare — the worker's git-over-HTTPS Basic push did not land"
RECV_AFTER="$(fake_state | jq '.gitStats.receivePackPosts // 0')"
[ "$RECV_AFTER" -gt "$RECV_BEFORE" ] \
  || fail "forge-fake saw NO authenticated git-receive-pack ($RECV_BEFORE -> $RECV_AFTER) — the worker did not push over smart-HTTP (insteadOf flip failed?)"
pass "worker pushed agent/issue-$IID_HA over git-over-HTTPS Basic auth (forge-fake receive-pack $RECV_BEFORE -> $RECV_AFTER, gates on uzi-bot:PAT)"

# Prove the smart-HTTP endpoint genuinely GATES on the Basic credential (not a no-op that
# accepts anything): no credential must 401, the correct Basic header must 200. Probed
# from inside the agent, which resolves forge-fake.e2e and trusts its cert. (Same probe
# the opt-in E2E_GIT_SMART_HTTP variant runs at the happy-path assertions.)
refs_url="https://forge-fake.e2e/group/repo.git/info/refs?service=git-upload-pack"
probe='const https=require("https");const u=new URL(process.argv[1]);const o={hostname:u.hostname,port:443,path:u.pathname+u.search,headers:{}};if(process.argv[2])o.headers.Authorization=process.argv[2];https.get(o,r=>{console.log(r.statusCode);r.resume();}).on("error",e=>{console.error(e.message);process.exit(2);});'
auth="Basic $(printf 'uzi-bot:%s' "$DUMMY_FORGE_PAT" | base64 | tr -d '\r\n')"
code_noauth="$("${COMPOSE[@]}" exec -T agent node -e "$probe" "$refs_url" | tr -d '\r\n')"
[ "$code_noauth" = 401 ] || fail "git smart-HTTP without a credential should 401 (got '$code_noauth')"
code_auth="$("${COMPOSE[@]}" exec -T agent node -e "$probe" "$refs_url" "$auth" | tr -d '\r\n')"
[ "$code_auth" = 200 ] || fail "git smart-HTTP with the correct Basic credential should 200 (got '$code_auth')"
pass "git smart-HTTP auth gate is real: no credential -> 401, correct Basic -> 200"

# Restore the gitconfig to its AT-SETUP state so later phases use the intended transport.
# In the default run that means the insteadOf local-bare rewrite (every later phase pushes
# to the local bare; #42 in particular REQUIRES independent repo.git/repo2.git bares, only
# possible locally). In the opt-in E2E_GIT_SMART_HTTP full run the whole suite is already
# smart-HTTP, so leave it empty — restoring insteadOf here would silently flip the rest of
# that run back to local, breaking its intent (and the #42 shared-bare assertion).
if [ -n "${E2E_GIT_SMART_HTTP:-}" ]; then
  cat > "$RUNROOT/agent-gitconfig/gitconfig" <<'EOF'
[safe]
	directory = *
EOF
  pass "kept the smart-HTTP remote (E2E_GIT_SMART_HTTP: the whole suite stays on smart-HTTP)"
else
  # safe.directory MUST be re-carried here (see the M1 rewrite note above): the remaining
  # phases push to the local bare, and gitEnv()'s command-scope pin is stripped before the
  # receive-pack child that checks ownership, while GIT_CONFIG_NOSYSTEM=1 rules out system
  # scope — so this global file is the only trust that reaches that child in this env.
  cat > "$RUNROOT/agent-gitconfig/gitconfig" <<'EOF'
[safe]
	directory = *
[url "/fakeremote/repo.git"]
	insteadOf = https://forge-fake.e2e/group/repo.git
[url "/fakeremote/repo2.git"]
	insteadOf = https://forge-fake.e2e/group/repo2.git
EOF
  pass "restored the insteadOf local-bare rewrite for the remaining phases"
fi

