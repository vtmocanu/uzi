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
RULES_OUT="[[$(rule '[{"context":"a"}]')]]"; export RULES_RC=1; bad "gh page fetch failed"
unset RULES_RC

[ "$(missing_required '["a","b"]' '[{"name":"a","bucket":"pass"}]')" = 1 ] || fail "one absent context not counted"
[ "$(missing_required '["a"]' '[{"name":"a","bucket":"pending"}]')" = 0 ] || fail "a registered context counted missing"
[ "$(missing_required '[]' '[]')" = 0 ] || fail "nothing required, yet missing"

echo "PASS required-checks: pages flattened; malformed or unreadable rules are unknown, not none"
