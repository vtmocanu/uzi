#!/usr/bin/env bash
# Advisory only: stale prices keep pricing; this never fetches or changes rates.
set -euo pipefail
export TZ=UTC
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fail() { printf 'pricing-freshness: %s\n' "$*" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v od >/dev/null 2>&1 || fail "od is required"
command -v awk >/dev/null 2>&1 || fail "awk is required"
today=$(date -u +%Y-%m-%d) || fail "cannot read UTC date"
json=false
while (( $# )); do
  case "$1" in
    --today)
      (( $# >= 2 )) || fail "--today requires YYYY-MM-DD"
      today=$2
      shift 2 ;;
    --json) json=true; shift ;;
    *) fail "unknown argument: $1" ;;
  esac
done
# Named TEST-ONLY path overrides; normal calls read the repository's canonical files.
codex=${UZI_PRICING_TEST_CODEX_PATH:-$root/agent/src/codex/codex-pricing.json}
anthropic=${UZI_PRICING_TEST_ANTHROPIC_PATH:-$root/api/internal/anthropicprice/anthropicprice.go}
[[ -r "$codex" && -r "$anthropic" ]] || fail "pricing source is unreadable"
# jq raw input replaces malformed bytes. Check decimal bytes before invoking jq.
# The FSM keeps at most three pending continuations; one failure rejects the file.
# pipefail also rejects an od read/instrument failure, with no findings printed.
if ! od -A n -t u1 -v < "$codex" | LC_ALL=C awk '
  {
    for (i=1; i<=NF; i++) {
      b=$i
      if (remaining) {
        if (b < minimum || b > maximum) { bad=1; exit 1 }
        remaining--
        minimum=128; maximum=191
      } else if (b <= 127) {
        continue
      } else {
        minimum=128; maximum=191
        if (b >= 194 && b <= 223) remaining=1
        else if (b >= 224 && b <= 239) {
          remaining=2
          if (b == 224) minimum=160
          if (b == 237) maximum=159
        } else if (b >= 240 && b <= 244) {
          remaining=3
          if (b == 240) minimum=144
          if (b == 244) maximum=143
        } else { bad=1; exit 1 }
      }
    }
  }
  END { if (bad || remaining) exit 1 }
'; then
  fail "expected valid UTF-8 bytes (od/awk byte validation failed)"
fi
# Count anchored declarations, including malformed ones, before extracting values.
metadata=$(awk '
  /^[[:space:]]*const[[:space:]]+AnthropicPrice(FetchedAt|SourceURL)([[:space:]]|=|$)/ {
    name=$2
    counts[name]++
    line=$0
    if (line !~ /^[[:space:]]*const[[:space:]]+AnthropicPrice(FetchedAt|SourceURL)[[:space:]]*=[[:space:]]*"[^"]*"[[:space:]]*$/) bad=1
    sub(/^[^"]*"/, "", line)
    sub(/"[[:space:]]*$/, "", line)
    if (line ~ /[[:cntrl:]]/) bad=1
    values[name]=line
  }
  END {
    if (bad || counts["AnthropicPriceFetchedAt"] != 1 || counts["AnthropicPriceSourceURL"] != 1) exit 2
    print values["AnthropicPriceFetchedAt"]
    print values["AnthropicPriceSourceURL"]
  }
' "$anthropic") || fail "expected exactly one valid Anthropic date and source constant"
anthropic_date=${metadata%%$'\n'*}
anthropic_source=${metadata#*$'\n'}
# Capture before printing: no partial findings or success-shaped JSON on failure.
output=$(jq -eRs --arg today "$today" --arg anthropic_date "$anthropic_date" \
  --arg anthropic_source "$anthropic_source" \
  -f "$root/scripts/pricing-freshness.jq" "$codex") || fail "invalid pricing metadata"
if [[ "$json" == true ]]; then
  printf '%s\n' "$output"
else
  printf '%s\n' "$output" | jq -r '.[] | "\(.subject | tojson | .[1:-1]) | \(.date) | \(.reason) | \(.sources | join(", "))"'
fi
