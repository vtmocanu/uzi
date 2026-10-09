#!/usr/bin/env bash
# Hermetic public CLI tests: fixture paths never touch production pricing data.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
mkdir -p .uzi/scratch
scratch=$(mktemp -d .uzi/scratch/pricing-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
export UZI_PRICING_TEST_CODEX_PATH="$scratch/codex.json"
export UZI_PRICING_TEST_ANTHROPIC_PATH="$scratch/anthropic.go"
# TEST-ONLY script override permits negative controls in scratch.
script=${UZI_PRICING_TEST_SCRIPT_PATH:-$root/scripts/pricing-freshness.sh}
checks=0
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
base() {
  cat >"$scratch/base.json" <<'JSON'
{"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":{"verified_at":"2026-01-01","sources":["https://example.com/pricing"],"low":{"uncached_input":0,"cached_input":0,"cache_write":0,"output":0},"high":{"uncached_input":0,"cached_input":0,"cache_write":0,"output":0}}}}
JSON
  cp "$scratch/base.json" "$UZI_PRICING_TEST_CODEX_PATH"
  anthropic 2026-01-01 https://example.com/anthropic
}
anthropic() {
  printf 'const AnthropicPriceFetchedAt = "%s"\nconst AnthropicPriceSourceURL = "%s"\n' "$1" "$2" >"$UZI_PRICING_TEST_ANTHROPIC_PATH"
}
change() { jq "$1" "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"; }
run() {
  rc=0
  bash "$script" "$@" >"$scratch/out" 2>"$scratch/err" || rc=$?
}
ok() {
  run "$@"
  [[ "$rc" == 0 && ! -s "$scratch/err" ]] || die "expected success ($*): $(cat "$scratch/err")"
  checks=$((checks + 1))
}
bad() {
  run "$@"
  [[ "$rc" == 2 && ! -s "$scratch/out" && -s "$scratch/err" ]] || die "expected exit 2, stderr and empty stdout ($*)"
  checks=$((checks + 1))
}
empty_json() {
  ok --today "$1" --json
  [[ $(cat "$scratch/out") == '[]' ]] || die "expected empty findings"
}
expected() {
  local subject=$1 date=$2 reason=$3 source=$4
  jq -cn --arg subject "$subject" --arg date "$date" --arg reason "$reason" --arg source "$source" \
    '[{subject:$subject,date:$date,reason:$reason,sources:[$source]}]' >"$scratch/expected"
  jq -c . "$scratch/out" >"$scratch/actual"
  cmp "$scratch/expected" "$scratch/actual" || die "findings mismatch"
}
# Shared raw corpus is read by the TS module-load and Go loader regressions too.
# Invalid JSON strings are generated only in scratch, never as tracked data files.
unicode_fixtures() {
  cat <<'UNICODE_FIXTURES'
version \ud800|reject|{"version":"\ud800","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \ud800|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800":@ROW@}}
replacement collision \ud800|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800":@ROW@,"\ufffd":@ROW@}}
replacement collision \ud800|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\ud800":@ROW@}}
version \udc00|reject|{"version":"\udc00","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \udc00|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\udc00":@ROW@}}
replacement collision \udc00|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\udc00":@ROW@,"\ufffd":@ROW@}}
replacement collision \udc00|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\udc00":@ROW@}}
version \udfff|reject|{"version":"\udfff","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \udfff|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\udfff":@ROW@}}
replacement collision \udfff|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\udfff":@ROW@,"\ufffd":@ROW@}}
replacement collision \udfff|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\udfff":@ROW@}}
version \uD800\uDBFF|reject|{"version":"\uD800\uDBFF","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \uD800\uDBFF|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\uD800\uDBFF":@ROW@}}
replacement collision \uD800\uDBFF|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\uD800\uDBFF":@ROW@,"\ufffd":@ROW@}}
replacement collision \uD800\uDBFF|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\uD800\uDBFF":@ROW@}}
version \udc00\ud800|reject|{"version":"\udc00\ud800","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \udc00\ud800|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\udc00\ud800":@ROW@}}
replacement collision \udc00\ud800|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\udc00\ud800":@ROW@,"\ufffd":@ROW@}}
replacement collision \udc00\ud800|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\udc00\ud800":@ROW@}}
version \ud800text|reject|{"version":"\ud800text","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \ud800text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800text":@ROW@}}
replacement collision \ud800text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800text":@ROW@,"\ufffd":@ROW@}}
replacement collision \ud800text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\ud800text":@ROW@}}
version \ud800\\text|reject|{"version":"\ud800\\text","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \ud800\\text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800\\text":@ROW@}}
replacement collision \ud800\\text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800\\text":@ROW@,"\ufffd":@ROW@}}
replacement collision \ud800\\text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\ud800\\text":@ROW@}}
version \ud800\"text|reject|{"version":"\ud800\"text","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
key \ud800\"text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800\"text":@ROW@}}
replacement collision \ud800\"text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800\"text":@ROW@,"\ufffd":@ROW@}}
replacement collision \ud800\"text|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@,"\ud800\"text":@ROW@}}
overwritten version high|reject|{"version":"\ud800","version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
overwritten version low|reject|{"version":"\udc00","version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
overwritten subtree|reject|{"models":{"discard":{"text":"\ud800"}},"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
valid \\ud800|accept|{"version":"\\ud800","input_tier_threshold_tokens":1,"models":{"\\ud800":@ROW@}}
valid \\udc00|accept|{"version":"\\udc00","input_tier_threshold_tokens":1,"models":{"\\udc00":@ROW@}}
valid �|accept|{"version":"�","input_tier_threshold_tokens":1,"models":{"�":@ROW@}}
valid \ufffd|accept|{"version":"\ufffd","input_tier_threshold_tokens":1,"models":{"\ufffd":@ROW@}}
valid \uD800\uDC00|accept|{"version":"\uD800\uDC00","input_tier_threshold_tokens":1,"models":{"\uD800\uDC00":@ROW@}}
valid \uDBFF\uDFFF|accept|{"version":"\uDBFF\uDFFF","input_tier_threshold_tokens":1,"models":{"\uDBFF\uDFFF":@ROW@}}
valid 𐀀|accept|{"version":"𐀀","input_tier_threshold_tokens":1,"models":{"𐀀":@ROW@}}
valid é|accept|{"version":"é","input_tier_threshold_tokens":1,"models":{"é":@ROW@}}
valid e\u0301|accept|{"version":"e\u0301","input_tier_threshold_tokens":1,"models":{"e\u0301":@ROW@}}
astral escaped duplicate|accept|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"𐀀":null,"\ud800\udc00":@ROW@}}
astral literal duplicate|accept|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800\udc00":null,"𐀀":@ROW@}}
distinct normalization|accept|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"é":@ROW@,"e\u0301":@ROW@}}
overwritten null|accept|{"version":null,"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
overwritten bad subtree|accept|{"models":{"bad":{"extra":1}},"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
overwritten overflow|accept|{"input_tier_threshold_tokens":1e400,"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
final null|reject|{"version":"fixture","version":null,"input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
final bad row|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@,"model":null}}
final overflow|reject|{"version":"fixture","input_tier_threshold_tokens":1,"input_tier_threshold_tokens":1e400,"models":{"model":@ROW@}}
two values|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}} {"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
version UFFFD first \ud800|reject|{"version":"\ufffd","version":"\ud800","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
version UFFFD last \ud800|reject|{"version":"\ud800","version":"\ufffd","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
version UFFFD first \udc00|reject|{"version":"\ufffd","version":"\udc00","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
version UFFFD last \udc00|reject|{"version":"\udc00","version":"\ufffd","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
overwritten subtree low|reject|{"models":{"discard":{"text":"\udc00"}},"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":@ROW@}}
pair separated by escaped backslash|reject|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"\ud800\\udc00":@ROW@}}
valid escaped quote then backslash u|accept|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"quote\"\\ud800":@ROW@}}
overwritten bad row|accept|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":{"extra":1},"model":@ROW@}}
overwritten row overflow|accept|{"version":"fixture","input_tier_threshold_tokens":1,"models":{"model":{"output":1e400},"model":@ROW@}}
UNICODE_FIXTURES
}
# Shared byte corpus: hex is printable source; consumers assemble raw bytes in scratch.
raw_utf8_fixtures() {
  cat <<'RAW_UTF8_FIXTURES'
ff|reject|ff
overlong two|reject|c0af
overlong three|reject|e080af
overlong four|reject|f08080af
surrogate|reject|eda080
out of range f4|reject|f4908080
out of range f5|reject|f5808080
stray 80|reject|80
stray bf|reject|bf
truncated two|reject|c2
truncated three|reject|e282
truncated four|reject|f09f98
ASCII|accept|41
80|accept|c280
7ff|accept|dfbf
800|accept|e0a080
d7ff|accept|ed9fbf
e000|accept|ee8080
10000|accept|f0908080
10ffff|accept|f48fbfbf
emoji|accept|f09f9880
UFFFD|accept|efbfbd
RAW_UTF8_FIXTURES
}
# Four placements per byte sequence; finite corpus, first failure stops the suite.
base
byte_row=$(jq -c '.models.model' "$scratch/base.json")
expired_row=$(jq -c '.models.model + {promo_review_date:"2026-01-01"}' "$scratch/base.json")
while IFS='|' read -r name verdict hex; do
  bytes=
  for ((i=0; i<${#hex}; i+=2)); do bytes+="\\x${hex:i:2}"; done
  for placement in first last version subtree; do
    {
      printf '{"version":"fixture","input_tier_threshold_tokens":1,'
      case "$placement" in
        first) printf '"models":{"%b":%s,"�":%s}}' "$bytes" "$byte_row" "$expired_row" ;;
        last) printf '"models":{"�":%s,"%b":%s}}' "$expired_row" "$bytes" "$byte_row" ;;
        version) printf '"version":"%b","version":"fixture","models":{"model":%s}}' "$bytes" "$byte_row" ;;
        subtree) printf '"models":{"discard":{"text":"%b"}},"models":{"model":%s}}' "$bytes" "$byte_row" ;;
      esac
    } >"$UZI_PRICING_TEST_CODEX_PATH"
    for mode in text json; do
      args=(--today 2026-01-01)
      [[ "$mode" == text ]] || args+=(--json)
      if [[ "$verdict" == reject ]]; then
        bad "${args[@]}"
        grep -qF 'UTF-8' "$scratch/err" || die "raw UTF8 rejection: $name/$placement/$mode"
      else
        ok "${args[@]}"
        if [[ "$placement" == first && "$mode" == json ]]; then
          jq -e '.[] | select(.subject == "�" and .reason == "promotional review date passed")' "$scratch/out" >/dev/null ||
            die "legitimate UFFFD promotion lost: $name"
        fi
      fi
    done
  done
done < <(raw_utf8_fixtures)
# Task owns the GitHub Actions failure annotation; the production bad() contract
# above still requires exit 2 and empty stdout.
printf "::error title=Task 'nudge:pricing' failed::exit status 2\n" >"$scratch/task-annotation"
task_failure_stdout() {
  if [[ "$1" == ci ]]; then
    cmp -s "$scratch/task-annotation" "$scratch/out"
  else
    [[ ! -s "$scratch/out" ]]
  fi
}
# Negative controls use the same assertion as the actual calls below.
cp "$scratch/task-annotation" "$scratch/out"
printf 'extra stdout\n' >>"$scratch/out"
if task_failure_stdout ci; then die "Task annotation plus extra stdout accepted"; fi
checks=$((checks + 1))
cp "$scratch/task-annotation" "$scratch/out"
if task_failure_stdout maintainer; then die "maintainer Task annotation accepted"; fi
checks=$((checks + 1))
# Both environments are explicit, independent of the suite caller's environment.
# The finite loop stops on the first failure; stdin is closed for every Task call.
for task_mode in maintainer ci; do
  base
  anthropic 2026-02-01 https://example.com/anthropic
  task_env=(CI=false GITHUB_ACTIONS=false)
  [[ "$task_mode" == maintainer ]] || task_env=(CI=true GITHUB_ACTIONS=true)
  rc=0
  env "${task_env[@]}" task nudge:pricing -- --today 2026-02-01 --json </dev/null >"$scratch/out" 2>"$scratch/err" || rc=$?
  [[ "$rc" == 0 && ! -s "$scratch/err" ]] || die "Task JSON command failed ($task_mode)"
  jq -e 'type == "array"' "$scratch/out" >/dev/null || die "Task stdout must be a plain JSON array ($task_mode)"
  expected model 2026-01-01 "verified more than 30 days ago" https://example.com/pricing
  checks=$((checks + 1))
  rc=0
  env "${task_env[@]}" task nudge:pricing -- --today invalid --json </dev/null >"$scratch/out" 2>"$scratch/err" || rc=$?
  [[ "$rc" != 0 && -s "$scratch/err" ]] || die "Task invalid input must fail with stderr ($task_mode)"
  task_failure_stdout "$task_mode" || die "unexpected Task failure stdout ($task_mode)"
  checks=$((checks + 1))
done

# Generate decoded controls in files, including NUL, which Bash variables cannot hold.
# The fixed corpus bounds this loop; any failure stops the suite.
for codes in 10 13 13,10 9 0 {0..31}; do
  base
  jq -cn --arg codes "$codes" '$codes | split(",") | map(tonumber) | implode' >"$scratch/control.json"
  jq --slurpfile control "$scratch/control.json" \
    '.models.model.sources = ["https://example.com/pricing" + $control[0]]' \
    "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
  jq -e --slurpfile control "$scratch/control.json" \
    '.models.model.sources[0] | index($control[0]) != null' \
    "$UZI_PRICING_TEST_CODEX_PATH" >/dev/null || die "missing decoded control $codes"
  if [[ "$codes" == 10 ]]; then
    jq -e '.models.model.sources[0] | index("\n") != null and index("\\n") == null' \
      "$UZI_PRICING_TEST_CODEX_PATH" >/dev/null || die "fixture must contain decoded LF"
  fi
  bad --today 2026-01-01
  bad --today 2026-01-01 --json
  cp "$scratch/base.json" "$UZI_PRICING_TEST_CODEX_PATH"
  {
    printf 'const AnthropicPriceFetchedAt = "2026-01-01"\nconst AnthropicPriceSourceURL = "https://example.com/anthropic'
    jq -jr . "$scratch/control.json"
    printf '"\n'
  } >"$UZI_PRICING_TEST_ANTHROPIC_PATH"
  bad --today 2026-01-01
  bad --today 2026-01-01 --json
done
base
fixture_row=$(jq -c '.models.model' "$scratch/base.json")
while IFS='|' read -r name verdict raw; do
  printf '%s\n' "${raw//@ROW@/$fixture_row}" >"$UZI_PRICING_TEST_CODEX_PATH"
  if [[ "$verdict" == accept ]]; then
    empty_json 2026-01-01
  else
    bad --today 2026-01-01 --json
  fi
done < <(unicode_fixtures)
base
empty_json 2026-01-31
ok --today 2026-01-31
[[ ! -s "$scratch/out" ]] || die "empty text"
anthropic 2026-02-01 https://example.com/anthropic
ok --today 2026-02-01 --json
expected model 2026-01-01 "verified more than 30 days ago" https://example.com/pricing
ok --today 2026-02-01
[[ $(cat "$scratch/out") == 'model | 2026-01-01 | verified more than 30 days ago | https://example.com/pricing' ]] || die "stale text"
change '.models.model.verified_at = "2026-02-01"'
anthropic 2026-01-01 https://example.com/anthropic
empty_json 2026-01-31
ok --today 2026-02-01 --json
expected Anthropic 2026-01-01 "fetched more than 30 days ago" https://example.com/anthropic
ok --today 2026-02-01
[[ $(cat "$scratch/out") == 'Anthropic | 2026-01-01 | fetched more than 30 days ago | https://example.com/anthropic' ]] || die "Anthropic text"

base
anthropic 2026-02-01 https://example.com/anthropic
change '.models.model.verified_at = "2026-02-01" | .models.model.promo_review_date = "2026-02-16"'
empty_json 2026-02-01
for today in 2026-02-02 2026-02-16 2026-02-17; do
  reason="promotional review date passed"
  [[ "$today" != 2026-02-02 ]] || reason="promotional review due within 14 days"
  ok --today "$today" --json
  expected model 2026-02-16 "$reason" https://example.com/pricing
  ok --today "$today"
  [[ $(cat "$scratch/out") == "model | 2026-02-16 | $reason | https://example.com/pricing" ]] || die "promo text"
done
base
# Multiple findings are sorted by model; each model's verification precedes promo.
change '.models.z = .models.model | .models.a = .models.model | del(.models.model) | .models.a.promo_review_date = "2026-02-01"'
ok --today 2026-02-01 --json
jq -c '[.[] | [.subject, .reason]]' "$scratch/out" >"$scratch/actual"
printf '%s\n' '[["a","verified more than 30 days ago"],["a","promotional review date passed"],["z","verified more than 30 days ago"],["Anthropic","fetched more than 30 days ago"]]' >"$scratch/expected"
cmp "$scratch/actual" "$scratch/expected" || die "deterministic finding order"
ok --today 2026-02-01
(( $(wc -l <"$scratch/out") == 4 )) || die "one text line per finding"
change '.models = {"line\nbreak": .models.model}'
jq -e '(.models | keys[0]) == "line\nbreak"' "$UZI_PRICING_TEST_CODEX_PATH" >/dev/null || die "fixture must contain actual newline"
ok --today 2026-02-01
(( $(wc -l <"$scratch/out") == 2 )) || die "escaped subject must stay on one line"
[[ $(head -n 1 "$scratch/out") == 'line\nbreak | 2026-01-01 | verified more than 30 days ago | https://example.com/pricing' ]] || die "newline subject must render as escaped text"
base
for mutation in \
  'null' '[]' '.extra=1' 'del(.version)' '.version=null' '.version=""' \
  '.input_tier_threshold_tokens=0' '.input_tier_threshold_tokens=1.5' '.input_tier_threshold_tokens=null' \
  '.models={}' '.models=null' '.models={"":.models.model}' '.models.model=null' \
  'del(.models.model.sources)' '.models.model.sources=[]' '.models.model.sources=null' \
  '.models.model.extra=1' '.models.model.low.extra=1' '.models.model.high.extra=1' \
  '.models.model.low=null' '.models.model.high=null' 'del(.models.model.low.output)' \
  '.models.model.low.output=null' '.models.model.low.output=-1' '.models.model.low.output="0"' \
  '.models.model.high.output=null' '.models.model.promo_review_date=null' 'del(.models.model.verified_at)'; do
  change "$mutation"
  bad --today 2026-01-01 --json
done
# Raw overflow must be parsed by the script, never serialized through jq first.
for field in output input_tier_threshold_tokens; do
  sed "s/\"$field\":[0-9]*/\"$field\":1e400/" "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
  bad --today 2026-01-01 --json
done
printf '{' >"$UZI_PRICING_TEST_CODEX_PATH"
bad --json
cat "$scratch/base.json" "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
bad --json
: >"$UZI_PRICING_TEST_CODEX_PATH"
bad --json
base
for date in 0000-01-01 1900-02-29 2026-02-29 2026-02-30 2026-04-31 2026-00-01 2026-13-01 2026-01-00 2026-1-01 10000-01-01; do
  change ".models.model.verified_at = \"$date\""
  bad --today 2026-01-01 --json
  change ".models.model.promo_review_date = \"$date\""
  bad --today 2026-01-01 --json
  anthropic "$date" https://example.com/anthropic
  cp "$scratch/base.json" "$UZI_PRICING_TEST_CODEX_PATH"
  bad --today 2026-01-01 --json
  anthropic 2026-01-01 https://example.com/anthropic
  bad --today "$date" --json
done
# macOS libc can reject year 0001 in jq's mktime/gmtime. Probe the script's own
# strptime | mktime | gmtime path: macOS fails at mktime, without a "gmtime/1:" prefix.
# Only that exact capability error skips this one positive case.
gmtime_capability=$(jq -cner '
  try ("0001-01-01" | strptime("%Y-%m-%d") | mktime | gmtime | [.[0], .[1] + 1, .[2]]
    | if . == [1,1,1] then "supported" else error("unexpected year 0001 components") end)
  catch if . == "invalid gmtime representation" or . == "gmtime/1: invalid gmtime representation"
    then "unsupported" else error(.) end
' </dev/null) || die "year 0001 gmtime capability probe failed"
for date in 0001-01-01 9999-12-31 2000-02-29 2024-02-29; do
  if [[ "$date" == 0001-01-01 && "$gmtime_capability" == unsupported ]]; then
    printf 'SKIP: positive 0001-01-01; jq gmtime is unsupported by this libc\n'
    continue
  fi
  change ".models.model.verified_at = \"$date\" | .models.model.promo_review_date = \"$date\""
  anthropic "$date" https://example.com/anthropic
  ok --today "$date" --json
done
base
for source in 'http://example.com' 'https://' 'https://user@example.com' 'https://[::1]' \
  'https://-bad.com' 'https://bad-.com' 'https://a..b' 'https://a.' 'https://256.0.0.1' \
  'https://a:0' 'https://a:65536' 'https://a:1e2' 'https://a/%' 'https://a/%0' \
  'https://a/%GG' 'https://a/ space' $'https://\u00e9.com' "https://a/\\"; do
  jq --arg source "$source" '.models.model.sources=[$source]' "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
  bad --today 2026-01-01 --json
  cp "$scratch/base.json" "$UZI_PRICING_TEST_CODEX_PATH"
  anthropic 2026-01-01 "$source"
  bad --today 2026-01-01 --json
done
base
for source in 'https://a' 'https://127.0.0.255:65535/a?b=%20#c' "https://a:0001/!\$&'()*+,;=:@/?#%2f+-"; do
  jq --arg source "$source" '.models.model.sources=[$source]' "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
  anthropic 2026-01-01 "$source"
  empty_json 2026-01-01
done
# Host length boundaries: 63-byte labels and 253-byte host are valid.
label=$(printf '%063d' 0)
host="$label.$label.$label.${label:0:61}"
for source in "https://$host" "https://${label}0.com" "https://${host}0"; do
  jq --arg source "$source" '.models.model.sources=[$source]' "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
  anthropic 2026-01-01 https://example.com/anthropic
  if [[ "$source" == "https://$host" ]]; then empty_json 2026-01-01; else bad --json; fi
done
base
change '.models.model.verified_at = "2026-01-01\\n"'
bad --today 2026-01-01 --json
for tier in low high; do
  for bucket in uncached_input cached_input cache_write output; do
    change "del(.models.model.$tier.$bucket)"
    bad --json
    change ".models.model.$tier.$bucket = null"
    bad --json
  done
done
base
# Duplicate keys obey final-value semantics, including an overwritten raw overflow.
sed 's/"output":0/"output":1e400,"output":0/g' "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
empty_json 2026-01-01
sed 's/"output":0/"output":0,"output":1e400/g' "$scratch/base.json" >"$UZI_PRICING_TEST_CODEX_PATH"
bad --json
base
for name in AnthropicPriceFetchedAt AnthropicPriceSourceURL; do
  anthropic 2026-01-01 https://example.com/anthropic
  awk -v name="$name" '$2 != name' "$UZI_PRICING_TEST_ANTHROPIC_PATH" >"$scratch/metadata"
  cp "$scratch/metadata" "$UZI_PRICING_TEST_ANTHROPIC_PATH"
  bad --json
  anthropic 2026-01-01 https://example.com/anthropic
  printf 'const %s = "duplicate"\n' "$name" >>"$UZI_PRICING_TEST_ANTHROPIC_PATH"
  bad --json
  anthropic 2026-01-01 https://example.com/anthropic
  printf 'const %s = broken\n' "$name" >>"$UZI_PRICING_TEST_ANTHROPIC_PATH"
  bad --json
done
base
for args in --unknown --today; do bad "$args"; done
bad --today ''
bad --today --json
bad --json extra
UZI_PRICING_TEST_CODEX_PATH="$scratch/missing" bad --json
UZI_PRICING_TEST_ANTHROPIC_PATH="$scratch/missing" bad --json
# A present but failing od must fail closed before jq, in both output modes.
base
mkdir -p "$scratch/bin"
printf '#!/bin/sh\nexit 1\n' >"$scratch/bin/od"
chmod +x "$scratch/bin/od"
PATH="$root/$scratch/bin:$PATH" bad --today 2026-01-01
PATH="$root/$scratch/bin:$PATH" bad --today 2026-01-01 --json
# UTC normalization applies to date math under arbitrary caller timezones.
for zone in UTC Pacific/Honolulu Pacific/Kiritimati; do
  TZ="$zone" ok --today 2026-02-01 --json
  if [[ "$zone" == UTC ]]; then cp "$scratch/out" "$scratch/utc"; else cmp "$scratch/utc" "$scratch/out" || die "timezone drift"; fi
done
ok --json
printf 'PASS: %s pricing freshness CLI checks\n' "$checks"
