#!/usr/bin/env bash
# uzi TUI header meter strip: before/after mock (provider glyph + account name, no P/S).
#   bash usage-provider-icons-tui-mock.sh             colour
#   NO_COLOR=1 bash usage-provider-icons-tui-mock.sh  tint stripped (glyphs survive)
#   MARKS=text bash usage-provider-icons-tui-mock.sh  text provider marks (Cl/Cx) instead of glyphs
set -u

if [[ -n "${NO_COLOR:-}" ]]; then
  FAINT="" OK="" WARN="" ALARM="" BRAND="" BOLD="" RST=""
else
  FAINT=$'\e[2m' OK=$'\e[38;5;71m' WARN=$'\e[38;5;130m' ALARM=$'\e[38;5;160m'
  BRAND=$'\e[38;5;136m' BOLD=$'\e[1m' RST=$'\e[0m'
fi

FULL="▰" EMPTY="▱" ACC="▎" MARK="▚▞ uzi"
if [[ "${MARKS:-glyph}" == text ]]; then
  G_CLAUDE="Cl" G_CODEX="Cx"
else
  G_CLAUDE="${CLAUDE_GLYPH:-✻}" G_CODEX="${CODEX_GLYPH:-❂}"
fi
GAP="   "

f() { printf '%s%s%s' "$FAINT" "$*" "$RST"; }

# bar <pct>: 5 cells, tinted by tone
bar() {
  local pct=$1 n=$(( ($1 * 5 + 50) / 100 )) tone=$OK out="" i
  (( pct >= 50 )) && tone=$WARN
  (( pct >= 85 )) && tone=$ALARM
  for ((i = 0; i < 5; i++)); do
    if (( i < n )); then out+="$tone$FULL$RST"; else out+="$FAINT$EMPTY$RST"; fi
  done
  printf '%s' "$out"
}

# win <label> <pct> <reset>: "5h ▰▰▱▱▱ 31% 2h29m"
win() { printf '%s %s %s' "$(f "$1")" "$(bar "$2")" "$(f "$2% $3")"; }
# acct <glyph> <name>: faint glyph, plain name (the name is the identity)
acct() { printf '%s %s' "$(f "$1")" "$2"; }
acc() { f "$ACC"; }

wordmark() { printf '%s%s%s%s  %s\n' "$BRAND" "$BOLD" "$MARK · floor" "$RST" "$(f "pulls  ci")"; }
title() { printf '\n%s%s%s  %s\n' "$BOLD" "$1" "$RST" "$(f "$2")"; }
label() { printf '%s\n' "$(f "── $1")"; }
note() { printf '%s\n' "$(f "   $1")"; }

# ── 1 ────────────────────────────────────────────────────────────────────────
title "1. Your case today" "one Claude token shown (+3 hidden in Settings), one Codex account"
label BEFORE
wordmark
echo "$(acc)team $(win 5h 31 2h29m)${GAP}$(win 7d 62 1d13h)${GAP}$(f codex) $(acc)codex $(win P 62 1d10h)"
note '"codex" = provider tag, 2nd "codex" = bucket name, P = primary window'
label "AFTER (a) glyph marks"
wordmark
echo "$(acct ✻ team)  $(win 5h 31 2h29m)  $(win 7d 62 1d13h)${GAP}$(acct ❂ default)  $(win 7d 62 1d10h)"
label "AFTER (b) text marks"
wordmark
echo "$(acct Cl team)  $(win 5h 31 2h29m)  $(win 7d 62 1d13h)${GAP}$(acct Cx default)  $(win 7d 62 1d10h)"
note 'provider mark + account name on every account; windows named by length on both providers'
note 'cases below use (a); run with MARKS=text to see (b) throughout'

# ── 2 ────────────────────────────────────────────────────────────────────────
title "2. Two Claude tokens, two Codex accounts" "ci-bot also has an extra per-model bucket"
label BEFORE
wordmark
echo "$(acc)team $(win 5h 31 2h29m)  $(win 7d 62 1d13h)${GAP}$(acc)work $(win 5h 12 4h02m)  $(win 7d 20 5d01h)${GAP}$(f codex) $(acc)default codex $(win P 62 1d10h)${GAP}$(acc)ci-bot codex $(win P 18 3h11m)  $(win S 44 4d20h)${GAP}gpt-5.6-sol $(win P 90 0h40m)"
label AFTER
wordmark
echo "$(acct "$G_CLAUDE" team)  $(win 5h 31 2h29m)  $(win 7d 62 1d13h)${GAP}$(acct "$G_CLAUDE" work)  $(win 5h 12 4h02m)  $(win 7d 20 5d01h)${GAP}$(acct "$G_CODEX" default)  $(win 7d 62 1d10h)${GAP}$(acct "$G_CODEX" ci-bot)  $(win 5h 18 3h11m)  $(win 7d 44 4d20h)  $(f "·sol") $(win 5h 90 0h40m)"
note 'default bucket stays unnamed; an extra bucket keeps a short tag (·sol) before its windows'

# ── 3 ────────────────────────────────────────────────────────────────────────
title "3. Narrow terminal" "the combined line does not fit, so it splits in two"
label BEFORE
wordmark
echo "$(acc)team $(win 5h 31 2h29m)  $(win 7d 62 1d13h)"
echo "$(acc)codex $(win P 62 1d10h)"
label AFTER
wordmark
echo "$(acct "$G_CLAUDE" team)  $(win 5h 31 2h29m)  $(win 7d 62 1d13h)"
echo "$(acct "$G_CODEX" default)  $(win 7d 62 1d10h)"
note 'the glyph names the provider on each line, so the split loses nothing'

# ── 4 ────────────────────────────────────────────────────────────────────────
title "4. Codex only" "no Claude token"
label BEFORE
wordmark
echo "$(f codex) $(acc)codex $(win P 62 1d10h)"
label AFTER
wordmark
echo "$(acct "$G_CODEX" default)  $(win 7d 62 1d10h)"

printf '\n  %s Claude account    %s Codex account\n' "$G_CLAUDE" "$G_CODEX"
note "also try: NO_COLOR=1 bash $0   |   MARKS=text bash $0   |   CODEX_GLYPH=◎ bash $0"
echo
