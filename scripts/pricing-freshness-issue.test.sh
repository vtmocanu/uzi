#!/usr/bin/env bash
# Hermetic test for scripts/pricing-freshness-issue.sh with a stub gh.
# Each case seeds the stub's `issue list` answer, runs the script, and asserts
# the exact gh verbs it called, plus the selectors and labels it passed.
# No network, no real gh.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
script="$root/scripts/pricing-freshness-issue.sh"
mkdir -p "$root/.uzi/scratch"
scratch=$(mktemp -d "$root/.uzi/scratch/pricing-issue-test.XXXXXX")
trap 'rm -rf "$scratch"' EXIT

die() { printf 'FAIL: %s\n' "$*"; exit 1; }

cat >"$scratch/gh" <<'STUB'
#!/usr/bin/env bash
# Records "verb subverb" per call in STUB_LOG and the full argv in STUB_ARGS;
# `issue list` prints $STUB_LIST, or fails when STUB_LIST_FAIL=1.
printf '%s %s\n' "$1" "$2" >>"$STUB_LOG"
printf '%s\n' "$*" >>"$STUB_ARGS"
if [[ "$1 $2" == "issue list" ]]; then
  [[ "${STUB_LIST_FAIL:-0}" == 1 ]] && exit 1
  cat "$STUB_LIST"
fi
if [[ "$1 $2" == "issue edit" || "$1 $2" == "issue create" ]]; then
  for ((i = 1; i <= $#; i++)); do
    if [[ "${!i}" == --body-file ]]; then j=$((i + 1)); cp "${!j}" "$STUB_BODY"; fi
  done
fi
exit 0
STUB
chmod +x "$scratch/gh"
export GH="$scratch/gh" STUB_LOG="$scratch/log" STUB_ARGS="$scratch/args" STUB_LIST="$scratch/list" STUB_BODY="$scratch/body"

marker='<!-- uzi-bot:pricing-freshness -->'
bot='{"is_bot":true,"login":"app/github-actions"}'
human='{"is_bot":false,"login":"someone"}'
one='[{"subject":"gpt-6-astra","date":"2026-09-13","reason":"verified more than 30 days ago","sources":["https://developers.openai.com/api/docs/pricing"]}]'

run() { # run <findings-json> <list-json>; prints script stdout
  : >"$STUB_LOG"; : >"$STUB_ARGS"; rm -f "$STUB_BODY"
  printf '%s' "$1" >"$scratch/findings.json"
  printf '%s' "$2" >"$STUB_LIST"
  bash "$script" "$scratch/findings.json"
}
calls() { tr '\n' ',' <"$STUB_LOG"; }
issues() { # issues <number> <state> <author-json> <body>
  jq -cn --argjson n "$1" --arg s "$2" --argjson a "$3" --arg b "$4" '[{number:$n,state:$s,author:$a,body:$b}]'
}

# 1. findings, no issue -> label + create, with the taxonomy labels; the list
#    call carries the ownership selectors.
out=$(run "$one" '[]')
[[ "$out" == "created (1 findings)" ]] || die "create: $out"
[[ $(calls) == "issue list,label create,issue create," ]] || die "create calls: $(calls)"
list_args=$(grep '^issue list ' "$STUB_ARGS")
[[ "$list_args" == *"--label pricing-freshness"* ]] || die "list lacks --label: $list_args"
[[ "$list_args" == *"--author app/github-actions"* ]] || die "list lacks --author: $list_args"
grep -q -- '--label pricing-freshness,area::tooling,priority::low' "$STUB_ARGS" || die "create lacks taxonomy labels"
grep -qF "$marker" "$STUB_BODY" || die "created body lacks marker"
grep -qF '| gpt-6-astra | 2026-09-13 |' "$STUB_BODY" || die "created body lacks row"
body=$(cat "$STUB_BODY")

# 2. same findings, open bot issue with that body -> no edit
list=$(issues 7 OPEN "$bot" "$body")
out=$(run "$one" "$list")
[[ "$out" == "unchanged #7" ]] || die "unchanged: $out"
[[ $(calls) == "issue list," ]] || die "unchanged calls: $(calls)"

# 3. changed findings -> edit only
two=$(jq -c '. + [{"subject":"Anthropic","date":"2026-10-03","reason":"fetched more than 30 days ago","sources":["https://platform.claude.com/docs/en/about-claude/pricing"]}]' <<<"$one")
out=$(run "$two" "$list")
[[ "$out" == "updated #7 (2 findings)" ]] || die "update: $out"
[[ $(calls) == "issue list,issue edit," ]] || die "update calls: $(calls)"

# 4. findings recur on a closed bot issue -> reopen (+ edit when changed)
closed=$(issues 7 CLOSED "$bot" "$body")
out=$(run "$two" "$closed")
[[ "$out" == $'reopened #7\nupdated #7 (2 findings)' ]] || die "reopen: $out"
[[ $(calls) == "issue list,issue reopen,issue edit," ]] || die "reopen calls: $(calls)"

# 5. no findings, open bot issue -> close
out=$(run '[]' "$list")
[[ "$out" == "closed #7" ]] || die "close: $out"
[[ $(calls) == "issue list,issue close," ]] || die "close calls: $(calls)"

# 6. no findings, nothing open -> no writes
out=$(run '[]' "$closed")
[[ "$out" == "no findings, nothing open" ]] || die "noop: $out"
[[ $(calls) == "issue list," ]] || die "noop calls: $(calls)"

# 7. a bot issue WITHOUT the marker is never treated as ours -> create a new one
unmarked=$(issues 9 OPEN "$bot" "human notes")
out=$(run "$one" "$unmarked")
[[ "$out" == "created (1 findings)" ]] || die "unmarked issue adopted: $out"
grep -q 'issue edit' "$STUB_LOG" && die "unmarked issue edited"

# 8. a HUMAN issue carrying the marker is never adopted (local author re-check),
#    even if the server-side --author filter let it through
forged=$(issues 11 OPEN "$human" "$body")
out=$(run "$one" "$forged")
[[ "$out" == "created (1 findings)" ]] || die "human marker issue adopted: $out"
grep -q -E 'issue (edit|close|reopen)' "$STUB_LOG" && die "human marker issue touched"
out=$(run '[]' "$forged")
[[ "$out" == "no findings, nothing open" ]] || die "human marker issue closed: $out"

# 9. a subject containing | or a newline stays one table row
weird='[{"subject":"a|b\nc","date":"2026-01-01","reason":"r","sources":["https://example.com"]}]'
run "$weird" '[]' >/dev/null
[[ $(grep -c '^| a b c |' "$STUB_BODY") == 1 ]] || die "subject not sanitized"

# 10. issue list fails -> exit 2 and no writes
rc=0; STUB_LIST_FAIL=1 run "$one" '[]' >/dev/null 2>&1 || rc=$?
[[ "$rc" == 2 ]] || die "list failure rc=$rc"
[[ $(calls) == "issue list," ]] || die "writes after list failure: $(calls)"

# 11. bad input -> exit 2
printf '{}' >"$scratch/bad.json"
rc=0; bash "$script" "$scratch/bad.json" 2>/dev/null || rc=$?
[[ "$rc" == 2 ]] || die "non-array input rc=$rc"

echo "PASS: pricing-freshness-issue (11 cases)"
