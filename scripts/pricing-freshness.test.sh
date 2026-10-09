#!/usr/bin/env bash
# Hermetic public CLI tests: fixture paths never touch production pricing data.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
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
[[ $(wc -l <"$scratch/out") == 4 ]] || die "one text line per finding"
change '.models = {"line\\nbreak": .models.model}'
ok --today 2026-02-01
[[ $(wc -l <"$scratch/out") == 2 ]] || die "escaped subject must stay on one line"
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
for date in 0001-01-01 9999-12-31 2000-02-29 2024-02-29; do
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
# UTC normalization applies to date math under arbitrary caller timezones.
for zone in UTC Pacific/Honolulu Pacific/Kiritimati; do
  TZ="$zone" ok --today 2026-02-01 --json
  if [[ "$zone" == UTC ]]; then cp "$scratch/out" "$scratch/utc"; else cmp "$scratch/utc" "$scratch/out" || die "timezone drift"; fi
done
ok --json
printf 'PASS: %s pricing freshness CLI checks\n' "$checks"
