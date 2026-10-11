#!/usr/bin/env bash
# Hermetic regression for required_contexts / missing_required: unknown (exit 1) is never
# reported as "none required", and every rules page is read.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
# RULES_OUT is the slurped `gh api --paginate --slurp` reply; RULES_RC its exit status.
case "$*" in *'/protection/required_status_checks'*)
  printf 'HTTP/2.0 %s Test\r\n\r\n' "${CLASSIC_HTTP:-404}"
  if [ -n "${CLASSIC_BODY:-}" ]; then printf '%s' "$CLASSIC_BODY"; else printf '{"message":"Branch not protected"}'; fi
  exit "${CLASSIC_RC:-1}" ;;
esac
if [ "${1:-}" = pr ] && [ "${2:-}" = checks ]; then
  printf '%s\n' "$*" > "$CHECKS_CALL"
  printf '%s' "$CHECKS_OUT"; exit "${CHECKS_RC:-0}"
fi
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then
  printf '%s' "$VIEW_OUT"; exit "${VIEW_RC:-0}"
fi
case "$*" in *'--paginate --slurp repos/test/repo/rules/branches/main'*) ;; *) echo "unexpected gh call: $*" >&2; exit 1 ;; esac
printf '%s\n' "$RULES_OUT"
exit "${RULES_RC:-0}"
STUB
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH"
# shellcheck source=lib/required-checks.sh
. "$HERE/required-checks.sh"

rule() { printf '{"type":"required_status_checks","parameters":{"required_status_checks":%s}}' "$1"; }
ok() { # label, want
  got=$(required_contexts test/repo main) || fail "$1: helper failed on a valid reply"
  [ "$got" = "$2" ] || fail "$1: got $got, want $2"
}
bad() { # label
  if got=$(required_contexts test/repo main); then fail "$1: malformed or unreadable rules returned $got"; fi
}

export RULES_OUT='[[]]'; ok "no rules" '[]'
RULES_OUT="[[{\"type\":\"deletion\"},$(rule '[{"context":"b"},{"context":"a"}]')]]"; ok "one page" '["a","b"]'
RULES_OUT="[[{\"type\":\"deletion\"}],[$(rule '[{"context":"late"}]')]]"; ok "second page" '["late"]'
RULES_OUT="[[$(rule '[]')]]"; ok "empty required list" '[]'

RULES_OUT='[[{"type":"required_status_checks","parameters":{}}]]'; bad "missing required_status_checks"
RULES_OUT="[[$(rule '[{"context":""}]')]]"; bad "empty context"
RULES_OUT="[[$(rule '[{"context":7}]')]]"; bad "non-string context"
RULES_OUT='[{"type":"deletion"}]'; bad "unslurped page"
RULES_OUT='{"message":"Not Found"}'; bad "error object"
RULES_OUT='[]'; bad "no pages"
RULES_OUT='[[null]]'; bad "null rule"
RULES_OUT='[[{}]]'; bad "missing rule type"
RULES_OUT='[[{"type":7}]]'; bad "non-string rule type"
RULES_OUT="[[$(rule '[{"context":"a"}]')]]"; export RULES_RC=1; bad "gh page fetch failed"
unset RULES_RC

RULES_OUT='[[]]'; export CLASSIC_HTTP=404 CLASSIC_BODY='{"message":"Required status checks not enabled"}'
ok "classic checks disabled" '[]'
CLASSIC_BODY='{"message":"Not Found"}'; bad "ambiguous classic 404"
CLASSIC_HTTP=403; CLASSIC_BODY='{"message":"Branch not protected"}'; bad "403 is not absence"
CLASSIC_HTTP=500; bad "classic server error"
CLASSIC_HTTP=200; export CLASSIC_RC=0 CLASSIC_BODY='{"contexts":["slow"],"checks":[{"context":"other"}]}'
ok "classic contexts and checks" '["other","slow"]'
RULES_OUT="[[$(rule '[{"context":"ci"}]')]]"; ok "union of both authorities" '["ci","other","slow"]'
CLASSIC_HTTP=403; CLASSIC_RC=1; CLASSIC_BODY='{"message":"Forbidden"}'
ok "rulesets retain legacy behaviour without classic permission" '["ci"]'
RULES_OUT='[[]]'; CLASSIC_HTTP=200; CLASSIC_RC=0
CLASSIC_BODY='{"contexts":"slow"}'; bad "malformed classic contexts"
CLASSIC_BODY='{"checks":[{}]}'; bad "malformed classic check"
CLASSIC_BODY='{}'; bad "classic body missing lists"
CLASSIC_BODY='{"contexts":[],"checks":[]}'; bad "enabled empty classic is not explicit absence"
unset CLASSIC_HTTP CLASSIC_RC CLASSIC_BODY

[ "$(missing_required '["a","b"]' '[{"name":"a","bucket":"pass"}]')" = 1 ] || fail "one absent context not counted"
[ "$(missing_required '["a"]' '[{"name":"a","bucket":"pending"}]')" = 0 ] || fail "a registered context counted missing"
[ "$(missing_required '[]' '[]')" = 0 ] || fail "nothing required, yet missing"

HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
export CHECKS_CALL="$WORK/checks-call" CHECKS_OUT='[{"name":"ci","bucket":"pass"}]'
export VIEW_OUT="{\"headRefOid\":\"$HEAD\",\"baseRefName\":\"main\"}"
got=$(pr_ci_checks test/repo 42 '[]' "$HEAD" main) || fail "all-checks fallback failed"
[ "$got" = "$CHECKS_OUT" ] || fail "fallback payload changed"
grep -qF -- '--required' "$CHECKS_CALL" && fail "none-required queried only required checks"
got=$(pr_ci_checks test/repo 42 '["ci"]' "$HEAD" main) || fail "required mode failed"
grep -qF -- '--required' "$CHECKS_CALL" || fail "required-present query changed"
VIEW_OUT="{\"headRefOid\":\"other\",\"baseRefName\":\"main\"}"
if got=$(pr_ci_checks test/repo 42 '[]' "$HEAD" main 2>/dev/null); then fail "changed head exposed green checks"; fi
[ -z "$got" ] || fail "changed head published stale CI data"
VIEW_OUT="{\"headRefOid\":\"$HEAD\",\"baseRefName\":\"other\"}"
if got=$(pr_ci_checks test/repo 42 '[]' "$HEAD" main 2>/dev/null); then fail "retargeted base kept old no-required proof"; fi
VIEW_OUT='{}'
if got=$(pr_ci_checks test/repo 42 '[]' "$HEAD" main 2>/dev/null); then fail "missing head metadata exposed green checks"; fi
ci_checks_valid '[{"bucket":"pass"},{"bucket":"skipping"}]' || fail "known buckets rejected"
if ci_checks_valid '[{"bucket":"mystery"}]'; then fail "unknown bucket accepted"; fi
if ci_checks_valid '[{}]'; then fail "missing bucket accepted"; fi

echo "PASS required-checks: pages flattened; malformed or unreadable rules are unknown, not none"
