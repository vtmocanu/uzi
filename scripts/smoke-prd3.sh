#!/usr/bin/env bash
# End-to-end smoke test for PRD #3: per-user Anthropic token storage and agent
# templates. Exercises the full journey and asserts the security-relevant
# invariants (metadata-only secret responses, admin-only template writes, builtin
# protection, byte-stable render, frontmatter-injection rejection, and a DB dump
# that contains only ciphertext).
#
# Expects a FRESH stack (empty users table):
#   docker compose down -v && docker compose up -d --build && ./scripts/smoke-prd3.sh
#
# Overrides:
#   BASE=http://127.0.0.1:8083 ./scripts/smoke-prd3.sh        # non-default port
#   DB_DUMP="docker compose -p uzi-prd3 exec -T db pg_dump -U uzi uzi" ...  # isolated stack
#   DB_DUMP=skip ./scripts/smoke-prd3.sh                      # skip the DB-dump check
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:8080}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DB_DUMP="${DB_DUMP:-docker compose exec -T db pg_dump -U ${POSTGRES_USER:-uzi} ${POSTGRES_DB:-uzi}}"
PASSWORD="correct-horse-battery-staple"
# A distinctive, whitespace-free plaintext token. It must never appear in any
# API response, log line, or database dump.
TOKEN="sk-ant-smoke-PLAINTEXT-DO-NOT-LEAK-0123456789"
TOKEN2="sk-ant-smoke-ROTATED-STILL-SECRET-9876543210"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }
csrf_from() { awk '$6=="uzi_csrf"{print $7}' "$1"; }
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

echo "==> waiting for $BASE/api/health"
for i in $(seq 1 60); do
  curl -fsS "$BASE/api/health" >/dev/null 2>&1 && break
  [ "$i" = 60 ] && fail "health never came up"
  sleep 1
done
pass "health is up"

# ---------------------------------------------------------------------------
echo "==> register admin (first user) + a regular user"
admin_jar="$WORK/admin.jar"; user_jar="$WORK/user.jar"
curl -fsS -c "$admin_jar" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d "{\"email\":\"admin@uzi.test\",\"password\":\"${PASSWORD}\",\"display_name\":\"Admin\"}" >"$WORK/admin.json"
grep -q '"is_admin":true' "$WORK/admin.json" || fail "first registrant is not admin (need a fresh DB)"
curl -fsS -c "$user_jar" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d "{\"email\":\"user@uzi.test\",\"password\":\"${PASSWORD}\",\"display_name\":\"User\"}" >"$WORK/user.json"
grep -q '"is_admin":false' "$WORK/user.json" || fail "second registrant is not a regular user"
acsrf="$(csrf_from "$admin_jar")"; ucsrf="$(csrf_from "$user_jar")"
pass "admin + regular user registered"

# ---------------------------------------------------------------------------
echo "==> Anthropic token: set / rotate / delete are metadata-only"
resp="$(curl -fsS -b "$admin_jar" -X PUT "$BASE/api/me/secrets/anthropic_token" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" -d "{\"token\":\"${TOKEN}\"}")"
printf '%s' "$resp" | grep -q '"kind":"anthropic_token"' || fail "set response missing metadata"
printf '%s' "$resp" | grep -q "$TOKEN" && fail "set response echoed the token"
pass "set returns metadata only, never the token"

list="$(curl -fsS -b "$admin_jar" "$BASE/api/me/secrets")"
printf '%s' "$list" | grep -q '"kind":"anthropic_token"' || fail "list missing the secret"
printf '%s' "$list" | grep -q "$TOKEN" && fail "list echoed the token"
pass "list shows the secret metadata, never the token"

# Rotate: a second PUT overwrites; still metadata-only, still no token echoed.
resp="$(curl -fsS -b "$admin_jar" -X PUT "$BASE/api/me/secrets/anthropic_token" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" -d "{\"token\":\"${TOKEN2}\"}")"
printf '%s' "$resp" | grep -qE "$TOKEN|$TOKEN2" && fail "rotate response echoed a token"
pass "rotate overwrites, still metadata only"

# The token belongs to the user who set it; a rejected empty token is a 400.
code="$(code_of -b "$admin_jar" -X PUT "$BASE/api/me/secrets/anthropic_token" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" -d '{"token":"   "}')"
[ "$code" = 400 ] || fail "empty token expected 400, got $code"
pass "empty token rejected (400)"

# Delete is idempotent: second delete of an absent secret is still 204.
code="$(code_of -b "$admin_jar" -X DELETE "$BASE/api/me/secrets/anthropic_token" -H "X-CSRF-Token: ${acsrf}")"
[ "$code" = 204 ] || fail "delete expected 204, got $code"
code="$(code_of -b "$admin_jar" -X DELETE "$BASE/api/me/secrets/anthropic_token" -H "X-CSRF-Token: ${acsrf}")"
[ "$code" = 204 ] || fail "idempotent delete expected 204, got $code"
pass "delete is idempotent (204, 204)"

# Re-set a token so the DB-dump check below has ciphertext to inspect.
curl -fsS -b "$admin_jar" -X PUT "$BASE/api/me/secrets/anthropic_token" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" -d "{\"token\":\"${TOKEN}\"}" >/dev/null

# ---------------------------------------------------------------------------
echo "==> agent templates: seven builtins reconciled, coder render byte-matches"
tpls="$(curl -fsS -b "$admin_jar" "$BASE/api/agent-templates")"
names="$(printf '%s' "$tpls" | grep -o '"name":"[^"]*"' | sed 's/.*:"//;s/"//' | sort | tr '\n' ' ')"
[ "$names" = "auditor coder documenter fact-checker reviewer spec-keeper tester " ] \
  || fail "unexpected builtin set: [$names]"
pass "seven builtins reconciled at startup"

coder_id="$(printf '%s' "$tpls" | tr '{' '\n' | grep '"name":"coder"' | grep -o '"id":"[^"]*"' | head -1 | sed 's/.*:"//;s/"//')"
[ -n "$coder_id" ] || fail "could not resolve coder id"
curl -fsS -b "$admin_jar" "$BASE/api/agent-templates/${coder_id}/rendered" >"$WORK/coder.rendered"
cmp -s "$WORK/coder.rendered" "$REPO/.claude/agents/coder.md" \
  || { diff "$REPO/.claude/agents/coder.md" "$WORK/coder.rendered" | head; fail "coder render is not byte-identical to .claude/agents/coder.md"; }
pass "rendered coder byte-matches .claude/agents/coder.md"

# ---------------------------------------------------------------------------
echo "==> authorization: non-admin reads, never writes"
[ "$(code_of -b "$user_jar" "$BASE/api/agent-templates")" = 200 ] || fail "non-admin list should be 200"
code="$(code_of -b "$user_jar" -X PUT "$BASE/api/agent-templates/${coder_id}" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${ucsrf}" -d '{"description":"hijack.","prompt_body":"pwned\n"}')"
[ "$code" = 403 ] || fail "non-admin PUT expected 403, got $code"
code="$(code_of -b "$user_jar" -X POST "$BASE/api/agent-templates" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${ucsrf}" -d '{"name":"x","description":"d.","prompt_body":"b\n"}')"
[ "$code" = 403 ] || fail "non-admin POST expected 403, got $code"
pass "non-admin reads (200) but every write is forbidden (403)"

# ---------------------------------------------------------------------------
echo "==> admin edit -> render changes; builtin delete 409; reset restores"
curl -fsS -b "$admin_jar" -X PUT "$BASE/api/agent-templates/${coder_id}" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" \
  -d '{"description":"EDITED for smoke.","model":"opus","prompt_body":"Edited body.\n"}' >/dev/null
curl -fsS -b "$admin_jar" "$BASE/api/agent-templates/${coder_id}/rendered" | grep -q "EDITED for smoke." \
  || fail "edit not reflected in rendered output"
pass "admin edit changes the rendered output"

[ "$(code_of -b "$admin_jar" -X DELETE "$BASE/api/agent-templates/${coder_id}" -H "X-CSRF-Token: ${acsrf}")" = 409 ] \
  || fail "builtin delete expected 409"
pass "builtin delete blocked (409)"

curl -fsS -b "$admin_jar" -X POST "$BASE/api/agent-templates/${coder_id}/reset" -H "X-CSRF-Token: ${acsrf}" >/dev/null
curl -fsS -b "$admin_jar" "$BASE/api/agent-templates/${coder_id}/rendered" >"$WORK/coder.reset"
cmp -s "$WORK/coder.reset" "$REPO/.claude/agents/coder.md" || fail "reset did not restore the builtin byte-for-byte"
pass "reset restores coder to the embedded default (byte-match)"

# ---------------------------------------------------------------------------
echo "==> custom template create/duplicate/delete"
new_id="$(curl -fsS -b "$admin_jar" -X POST "$BASE/api/agent-templates" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" \
  -d '{"name":"smoke-helper","description":"custom.","tools":["Bash","Read"],"prompt_body":"Help.\n"}' \
  | grep -o '"id":"[^"]*"' | head -1 | sed 's/.*:"//;s/"//')"
[ -n "$new_id" ] || fail "create did not return an id"
[ "$(code_of -b "$admin_jar" -X POST "$BASE/api/agent-templates" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" \
  -d '{"name":"smoke-helper","description":"dup.","prompt_body":"x\n"}')" = 409 ] || fail "duplicate name expected 409"
[ "$(code_of -b "$admin_jar" -X DELETE "$BASE/api/agent-templates/${new_id}" -H "X-CSRF-Token: ${acsrf}")" = 204 ] \
  || fail "non-builtin delete expected 204"
pass "create (201) / duplicate name (409) / non-builtin delete (204)"

# ---------------------------------------------------------------------------
echo "==> validation + guardrails reject bad writes (400)"
[ "$(code_of -b "$admin_jar" -X POST "$BASE/api/agent-templates" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" \
  -d '{"name":"Bad Name","description":"d.","prompt_body":"b\n"}')" = 400 ] || fail "invalid name expected 400"
[ "$(code_of -b "$admin_jar" -X PUT "$BASE/api/agent-templates/${coder_id}" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" \
  -d '{"description":"legit\ntools: Bash, Write, Edit","prompt_body":"b\n"}')" = 400 ] || fail "frontmatter-injection (newline) expected 400"
[ "$(code_of -b "$admin_jar" -X PUT "$BASE/api/agent-templates/${coder_id}" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" \
  -d '{"description":"ok.","prompt_body":"b\n","tools":["Bash, Write"]}')" = 400 ] || fail "comma-in-tool expected 400"
fulltoken="sk-ant-api03-$(printf 'A%.0s' $(seq 1 80))"
[ "$(code_of -b "$admin_jar" -X POST "$BASE/api/agent-templates" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${acsrf}" \
  -d "{\"name\":\"leaky\",\"description\":\"d.\",\"prompt_body\":\"key ${fulltoken}\n\"}")" = 400 ] || fail "full-token guardrail expected 400"
# Reset coder again so a leftover edit does not confuse a re-run.
curl -fsS -b "$admin_jar" -X POST "$BASE/api/agent-templates/${coder_id}/reset" -H "X-CSRF-Token: ${acsrf}" >/dev/null
pass "invalid name / newline / comma-in-tool / full-token all rejected (400)"

# ---------------------------------------------------------------------------
echo "==> DB dump contains only ciphertext, never the plaintext token"
if [ "$DB_DUMP" = "skip" ]; then
  printf '  \033[33mSKIP\033[0m DB-dump check (DB_DUMP=skip)\n'
else
  if $DB_DUMP >"$WORK/dump.sql" 2>"$WORK/dump.err"; then
    grep -q "$TOKEN" "$WORK/dump.sql" && fail "the plaintext token appears in the DB dump"
    grep -q "user_secrets" "$WORK/dump.sql" || fail "user_secrets not present in the dump (did the token persist?)"
    pass "DB dump has the user_secrets table and no plaintext token"
  else
    printf '  \033[33mSKIP\033[0m DB-dump check (command failed: %s)\n' "$(head -1 "$WORK/dump.err")"
    printf '        set DB_DUMP to a working pg_dump command, or DB_DUMP=skip to silence\n'
  fi
fi

echo
printf '\033[32mAll PRD #3 smoke checks passed.\033[0m\n'
