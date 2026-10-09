#!/usr/bin/env bash
# Hermetic test for scripts/pricing-freshness-issue.sh with a stub gh.
# Each case seeds the stub's `issue list` answer, runs the script, and asserts
# the exact gh verbs it called. No network, no real gh.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
script="$root/scripts/pricing-freshness-issue.sh"
mkdir -p "$root/.uzi/scratch"
scratch=$(mktemp -d "$root/.uzi/scratch/pricing-issue-test.XXXXXX")
trap 'rm -rf "$scratch"' EXIT

die() { printf 'FAIL: %s\n' "$*"; exit 1; }

cat >"$scratch/gh" <<'STUB'
#!/usr/bin/env bash
# Records "verb subverb" per call; `issue list` prints $STUB_LIST.
printf '%s %s\n' "$1" "$2" >>"$STUB_LOG"
if [[ "$1 $2" == "issue list" ]]; then cat "$STUB_LIST"; fi
if [[ "$1 $2" == "issue edit" || "$1 $2" == "issue create" ]]; then
  for ((i = 1; i <= $#; i++)); do
    if [[ "${!i}" == --body-file ]]; then j=$((i + 1)); cp "${!j}" "$STUB_BODY"; fi
  done
fi
exit 0
STUB
chmod +x "$scratch/gh"
export GH="$scratch/gh" STUB_LOG="$scratch/log" STUB_LIST="$scratch/list" STUB_BODY="$scratch/body"

marker='<!-- uzi-bot:pricing-freshness -->'
one='[{"subject":"gpt-6-astra","date":"2026-09-13","reason":"verified more than 30 days ago","sources":["https://developers.openai.com/api/docs/pricing"]}]'

run() { # run <findings-json> <list-json>; prints script stdout
  : >"$STUB_LOG"; rm -f "$STUB_BODY"
  printf '%s' "$1" >"$scratch/findings.json"
  printf '%s' "$2" >"$STUB_LIST"
  bash "$script" "$scratch/findings.json"
}
calls() { tr '\n' ',' <"$STUB_LOG"; }

# 1. findings, no issue -> label + create
out=$(run "$one" '[]')
[[ "$out" == "created (1 findings)" ]] || die "create: $out"
[[ $(calls) == "issue list,label create,issue create," ]] || die "create calls: $(calls)"
grep -qF "$marker" "$STUB_BODY" || die "created body lacks marker"
grep -qF '| gpt-6-astra | 2026-09-13 |' "$STUB_BODY" || die "created body lacks row"
body=$(cat "$STUB_BODY")

# 2. same findings, open bot issue with that body -> no edit
list=$(jq -cn --arg b "$body" '[{number:7,state:"OPEN",body:$b}]')
out=$(run "$one" "$list")
[[ "$out" == "unchanged #7" ]] || die "unchanged: $out"
[[ $(calls) == "issue list," ]] || die "unchanged calls: $(calls)"

# 3. changed findings -> edit only
two=$(jq -c '. + [{"subject":"Anthropic","date":"2026-10-03","reason":"fetched more than 30 days ago","sources":["https://platform.claude.com/docs/en/about-claude/pricing"]}]' <<<"$one")
out=$(run "$two" "$list")
[[ "$out" == "updated #7 (2 findings)" ]] || die "update: $out"
[[ $(calls) == "issue list,issue edit," ]] || die "update calls: $(calls)"

# 4. findings recur on a closed bot issue -> reopen (+ edit when changed)
closed=$(jq -cn --arg b "$body" '[{number:7,state:"CLOSED",body:$b}]')
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

# 7. a labelled issue WITHOUT the marker is never treated as ours -> create a new one
human=$(jq -cn '[{number:9,state:"OPEN",body:"human notes"}]')
out=$(run "$one" "$human")
[[ "$out" == "created (1 findings)" ]] || die "human issue adopted: $out"
grep -q 'issue edit' "$STUB_LOG" && die "human issue edited"

# 8. a subject containing | or a newline stays one table row
weird='[{"subject":"a|b\nc","date":"2026-01-01","reason":"r","sources":["https://example.com"]}]'
run "$weird" '[]' >/dev/null
[[ $(grep -c '^| a b c |' "$STUB_BODY") == 1 ]] || die "subject not sanitized"

# 9. bad input -> exit 2
printf '{}' >"$scratch/bad.json"
rc=0; bash "$script" "$scratch/bad.json" 2>/dev/null || rc=$?
[[ "$rc" == 2 ]] || die "non-array input rc=$rc"

echo "PASS: pricing-freshness-issue (9 cases)"
