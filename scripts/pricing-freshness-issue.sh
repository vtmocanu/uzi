#!/usr/bin/env bash
# Open, update, close or reopen the one tracking issue for `task nudge:pricing`
# findings (PRD #2560 follow-up). Run by .github/workflows/pricing-freshness.yml.
#
# Usage: pricing-freshness-issue.sh <findings.json>
#   findings.json is `task nudge:pricing -- --json` output: a JSON array of
#   {subject, date, reason, sources[]}.
# Env: GH_REPO (owner/repo) and GH_TOKEN for gh; GH (default gh) for tests.
#
# Ownership: the issue is found by the fixed LABEL and the bot-owned MARKER in its
# body, authored by github-actions[bot]. A human issue that merely carries the
# label is never edited. The body ends with a digest of the findings, so the body
# is rewritten only when the findings change.
# Exit: 0 done (including "nothing to do"); 2 bad input or a gh failure.
set -euo pipefail

GH=${GH:-gh}
LABEL=pricing-freshness
# Taxonomy (.agents/skills/issue-triage/references/taxonomy.md); never a sweep label.
TAXONOMY_LABELS=area::tooling,priority::low
MARKER='<!-- uzi-bot:pricing-freshness -->'
BOT_LOGIN='app/github-actions'

die() { printf 'pricing-freshness-issue: %s\n' "$*" >&2; exit 2; }

findings=${1:?usage: pricing-freshness-issue.sh <findings.json>}
[[ -f "$findings" ]] || die "missing findings file: $findings"
jq -e 'type == "array"' "$findings" >/dev/null || die "findings is not a JSON array"
count=$(jq 'length' "$findings")
# sha256sum on Linux CI, shasum on a maintainer's macOS.
sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi; }
digest=$(jq -cS . "$findings" | sha256 | cut -c1-16)

render_body() {
  printf '%s\n\n' "Pricing data needs a human re-check against the official pages."
  printf '%s\n\n' "Found by the weekly \`task nudge:pricing\` run. Verify each row's rates, then bump the table \`version\` and the row's \`verified_at\` (or \`AnthropicPriceFetchedAt\`) and run \`task codex-pricing:sync\`; see \`docs/run-cost.md\`, \"Updating prices\"."
  printf '| subject | date | reason | sources |\n|---|---|---|---|\n'
  jq -r '.[] | "| \(.subject | gsub("[|\n\r]"; " ")) | \(.date) | \(.reason) | \(.sources | join(", ")) |"' "$findings"
  printf '\n%s\n<!-- digest:%s -->\n' "$MARKER" "$digest"
}

# Newest bot-owned issue carrying the label and the marker, any state.
existing=$("$GH" issue list --label "$LABEL" --author "$BOT_LOGIN" --state all --limit 50 \
  --json number,state,body,author) || die "gh issue list failed"
# Re-check ownership locally: the server-side --author filter is the first line,
# this the second, so a human issue carrying the marker is never adopted.
issue=$(jq -c --arg m "$MARKER" --arg bot "$BOT_LOGIN" '[.[] | select(.author.is_bot == true and .author.login == $bot and (.body | contains($m)))] | sort_by(.number) | last // empty' <<<"$existing")

if [[ "$count" -eq 0 ]]; then
  if [[ -n "$issue" && $(jq -r .state <<<"$issue") == OPEN ]]; then
    n=$(jq -r .number <<<"$issue")
    "$GH" issue close "$n" --comment "All pricing findings cleared by the weekly run." >/dev/null || die "gh issue close failed"
    echo "closed #$n"
  else
    echo "no findings, nothing open"
  fi
  exit 0
fi

body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT
render_body >"$body_file"

if [[ -z "$issue" ]]; then
  "$GH" label create "$LABEL" --color FBCA04 --description "Weekly pricing freshness findings" --force >/dev/null || die "gh label create failed"
  "$GH" issue create --title "Pricing data needs re-verification" --label "$LABEL,$TAXONOMY_LABELS" --body-file "$body_file" >/dev/null || die "gh issue create failed"
  echo "created ($count findings)"
  exit 0
fi

n=$(jq -r .number <<<"$issue")
if [[ $(jq -r .state <<<"$issue") != OPEN ]]; then
  "$GH" issue reopen "$n" --comment "Pricing findings recurred." >/dev/null || die "gh issue reopen failed"
  echo "reopened #$n"
fi
if jq -e --arg d "<!-- digest:$digest -->" '.body | contains($d)' <<<"$issue" >/dev/null; then
  echo "unchanged #$n"
else
  "$GH" issue edit "$n" --body-file "$body_file" >/dev/null || die "gh issue edit failed"
  echo "updated #$n ($count findings)"
fi
