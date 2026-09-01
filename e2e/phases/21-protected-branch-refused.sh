# shellcheck shell=bash
# phase:    protected-branch-refused
# title:    PRD #97 M1: the fake remote refuses a push to main under BOTH transports (protected-branch backstop)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# (b) main-push-refused backstop (Decision 2). The fake bare carries a pre-receive hook
# refusing refs/heads/main (installed at setup, in both bares). Self-test it across BOTH
# transports — receive-pack runs in the AGENT image for a local push and in FORGE-FAKE
# for smart-HTTP — from a neutral harness git client (not the worker, whose SDK guardrails
# already refuse a push higher up; this proves the REMOTE's own filter). Every exec points
# GIT_CONFIG_GLOBAL at /e2e-git/neutral (safe.directory=* only, NO insteadOf), so each is a
# plain git that reaches forge-fake over real smart-HTTP for an https URL and trusts the
# bind-mounted bare; the container keeps GIT_SSL_CAINFO so smart-HTTP trusts the cert.
say "PRD #97 M1: the fake remote refuses a push to main under BOTH transports (protected-branch backstop)"
MREJ=/tmp/e2e-main-reject
B64AUTH="$(printf 'uzi-bot:%s' "$DUMMY_FORGE_PAT" | base64 | tr -d '\r\n')"
# Stage a FAST-FORWARD commit on top of main, so the ONLY thing that can reject a main
# push is the hook (never a non-fast-forward). The clone is local (no credential needed).
"${COMPOSE[@]}" exec -e GIT_CONFIG_GLOBAL=/e2e-git/neutral -T agent sh -c "
  set -e
  rm -rf $MREJ
  git clone -q /fakeremote/repo.git $MREJ
  git -C $MREJ config user.email e2e@uzi.e2e
  git -C $MREJ config user.name 'E2E main-reject'
  git -C $MREJ config commit.gpgsign false
  git -C $MREJ checkout -q main
  echo 'protected-branch probe' >> $MREJ/README.md
  git -C $MREJ commit -qam 'e2e: main-reject probe (must never land)'
" || fail "could not stage the main-reject probe commit inside the agent"
pass "staged a fast-forward main-probe commit (only the pre-receive hook can now reject a main push)"

# LOCAL transport (hook fires in the agent image): main REFUSED, a non-main branch ACCEPTED.
if "${COMPOSE[@]}" exec -e GIT_CONFIG_GLOBAL=/e2e-git/neutral -T agent \
    git -C "$MREJ" push /fakeremote/repo.git HEAD:refs/heads/main >/dev/null 2>&1; then
  fail "local-transport push to main was NOT refused (pre-receive hook missing/ineffective)"
fi
pass "local transport: push to main refused by the fake's pre-receive hook"
"${COMPOSE[@]}" exec -e GIT_CONFIG_GLOBAL=/e2e-git/neutral -T agent \
    git -C "$MREJ" push /fakeremote/repo.git HEAD:refs/heads/e2e-mainreject-local >/dev/null 2>&1 \
  || fail "local-transport push of a non-main branch was refused (hook is a blanket deny — wrong)"
pass "local transport: a non-main branch push is accepted (hook rejects only main)"

# SMART-HTTP transport (hook fires in the forge-fake image): main REFUSED, branch ACCEPTED.
# Basic auth is required by forge-fake; supply it inline so the ONLY rejection reason left
# is the hook (auth-over-smart-HTTP was already proven green in (a)).
if "${COMPOSE[@]}" exec -e GIT_CONFIG_GLOBAL=/e2e-git/neutral -T agent \
    git -C "$MREJ" -c http.extraHeader="Authorization: Basic $B64AUTH" \
    push https://forge-fake.e2e/group/repo.git HEAD:refs/heads/main >/dev/null 2>&1; then
  fail "smart-HTTP push to main was NOT refused (pre-receive hook not portable to the forge-fake image?)"
fi
pass "smart-HTTP transport: push to main refused by the fake's pre-receive hook (portable to forge-fake)"
"${COMPOSE[@]}" exec -e GIT_CONFIG_GLOBAL=/e2e-git/neutral -T agent \
    git -C "$MREJ" -c http.extraHeader="Authorization: Basic $B64AUTH" \
    push https://forge-fake.e2e/group/repo.git HEAD:refs/heads/e2e-mainreject-smarthttp >/dev/null 2>&1 \
  || fail "smart-HTTP push of a non-main branch was refused (auth or hook wrong on the forge-fake image)"
pass "smart-HTTP transport: a non-main branch push is accepted (hook rejects only main)"

# Keep the bares pristine for later phases: drop the two probe branches + the scratch clone.
"${COMPOSE[@]}" exec -e GIT_CONFIG_GLOBAL=/e2e-git/neutral -T agent sh -c "
  git --git-dir=/fakeremote/repo.git update-ref -d refs/heads/e2e-mainreject-local 2>/dev/null || true
  git --git-dir=/fakeremote/repo.git update-ref -d refs/heads/e2e-mainreject-smarthttp 2>/dev/null || true
  rm -rf $MREJ
" || true

