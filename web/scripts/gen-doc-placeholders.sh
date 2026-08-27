#!/usr/bin/env bash
# Regenerate the clearly-marked placeholder screenshots in docs/img/ (PRD #7 M3).
#
# Dev-only helper (macOS + ImageMagick `magick`); NOT part of the build. The
# placeholders it produces are temporary: Vlad replaces them with real captures
# in a single final commit once the other milestones land (PRD decision log).
# The filenames below are the contract with the docs pages' `![alt](img/...)`
# references, so a real capture is a drop-in swap at the same path.
set -euo pipefail

OUT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../docs/img" && pwd)"
mkdir -p "$OUT"

# macOS system fonts (ImageMagick has no bundled default here).
BOLD="/System/Library/Fonts/Supplemental/Arial Bold.ttf"
REG="/System/Library/Fonts/Supplemental/Arial.ttf"
MONO="/System/Library/Fonts/Supplemental/Courier New.ttf"

# name|title|subtitle  (one per docs/img reference)
entries=(
  "getting-started-register.png|Registration form|first account created becomes admin"
  "gitlab-bot-setup-enable.png|Enable the repo|GitLab project > Members, bot added as Developer"
  "gitlab-bot-setup-connect.png|Connect the bot in uzi|Settings > Forge: paste and verify the bot PAT"
  "board-move-card.png|Board: move a card|drag a PRD card between columns, relabeling the issue"
  "anthropic-token-settings.png|Anthropic token settings|Settings > Anthropic token: paste field and Set status"
  "agent-templates-editor.png|Agent template editor|prompt body with a live Markdown preview"
  "worker-setup-join-token.png|Worker join token|Settings > Workers: generated token, shown once"
  "cli-access-settings.png|Settings > Access|CLI token list: prefix, last used, last IP, Revoke all"
)

for e in "${entries[@]}"; do
  IFS='|' read -r name title subtitle <<<"$e"
  magick -size 1200x750 xc:'#0f172a' \
    -font "$BOLD" -fill '#f59e0b' -gravity north  -pointsize 34 -annotate +0+70 'PLACEHOLDER: screenshot pending' \
    -font "$BOLD" -fill '#e2e8f0' -gravity center -pointsize 46 -annotate +0-30 "$title" \
    -font "$REG"  -fill '#94a3b8' -gravity center -pointsize 26 -annotate +0+45 "$subtitle" \
    -font "$MONO" -fill '#64748b' -gravity south  -pointsize 22 -annotate +0+55 "docs/img/$name" \
    -bordercolor '#f59e0b' -border 3 \
    "$OUT/$name"
  printf 'wrote %s (%s bytes)\n' "$name" "$(stat -f%z "$OUT/$name")"
done
