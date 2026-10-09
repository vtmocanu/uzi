#!/usr/bin/env bash
# Advisory only: stale prices keep pricing; this never fetches or changes rates.
set -euo pipefail
export TZ=UTC
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fail() { printf 'pricing-freshness: %s\n' "$*" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || fail "jq is required"
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
# Count anchored declarations, including malformed ones, before extracting values.
metadata=$(awk '
  /^[[:space:]]*const[[:space:]]+AnthropicPrice(FetchedAt|SourceURL)([[:space:]]|=|$)/ {
    name=$2
    counts[name]++
    line=$0
    if (line !~ /^[[:space:]]*const[[:space:]]+AnthropicPrice(FetchedAt|SourceURL)[[:space:]]*=[[:space:]]*"[^"]*"[[:space:]]*$/) bad=1
    sub(/^[^"]*"/, "", line)
    sub(/"[[:space:]]*$/, "", line)
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
output=$(jq -es --arg today "$today" --arg anthropic_date "$anthropic_date" \
  --arg anthropic_source "$anthropic_source" \
  -f "$root/scripts/pricing-freshness.jq" "$codex") || fail "invalid pricing metadata"
if [[ "$json" == true ]]; then
  printf '%s\n' "$output"
else
  printf '%s\n' "$output" | jq -r '.[] | "\(.subject | tojson | .[1:-1]) | \(.date) | \(.reason) | \(.sources | join(", "))"'
fi
