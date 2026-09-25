#!/usr/bin/env bash
# uzi TUI header meter strip: before/after mock.
# Proposal: no provider icons (a terminal cannot draw the web logos). Every account is
# named; a single faint "codex" tag leads the Codex group (Claude stays untagged, as
# today); the repeated default "codex" bucket name is dropped; P/S become 5h/7d.
#   bash 1653-usage-provider-icons-tui-mock.sh             colour
#   NO_COLOR=1 bash 1653-usage-provider-icons-tui-mock.sh  tint stripped
set -u

if [[ -n "${NO_COLOR:-}" ]]; then
  FAINT="" OK="" WARN="" ALARM="" BRAND="" BOLD="" RST=""
else
  FAINT=$'\e[2m' OK=$'\e[38;5;71m' WARN=$'\e[38;5;130m' ALARM=$'\e[38;5;160m'
  BRAND=$'\e[38;5;136m' BOLD=$'\e[1m' RST=$'\e[0m'
fi
FULL="▰" EMPTY="▱" ACC="▎" MARK="▚▞ uzi"
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
# acct <name> [hot]: accent bar (alarm-tinted when hot) + account name
acct() {
  local tone=$FAINT
  [[ "${2:-}" == hot ]] && tone=$ALARM
  printf '%s%s%s%s' "$tone" "$ACC" "$RST" "$1"
}
tag() { f "codex "; }

wordmark() { printf '%s%s%s%s  %s\n' "$BRAND" "$BOLD" "$MARK · floor" "$RST" "$(f "pulls  ci")"; }
title() { printf '\n%s%s%s  %s\n' "$BOLD" "$1" "$RST" "$(f "$2")"; }
label() { printf '%s\n' "$(f "── $1")"; }
note() { printf '%s\n' "$(f "   $1")"; }

# ── 1 ────────────────────────────────────────────────────────────────────────
title "1. Today's case" "one Claude token shown (+3 hidden in Settings), one Codex account"
label BEFORE
wordmark
echo "$(acct team) $(win 5h 31 2h29m)${GAP}$(win 7d 62 1d13h)${GAP}$(tag)$(acct codex) $(win P 62 1d10h)"
note '"codex" tag, then the bucket name "codex" (the account name is hidden), then P = primary window'
label AFTER
wordmark
echo "$(acct team) $(win 5h 31 2h29m)  $(win 7d 62 1d13h)${GAP}$(tag)$(acct default) $(win 7d 62 1d10h)"
note 'one "codex" tag for the group; the account is named; window named by its length, as on the web'

# ── 2 ────────────────────────────────────────────────────────────────────────
title "2. Two Claude tokens, two Codex accounts" "ci-bot also has an extra per-model bucket"
label BEFORE
wordmark
echo "$(acct team) $(win 5h 31 2h29m)  $(win 7d 62 1d13h)${GAP}$(acct work) $(win 5h 12 4h02m)  $(win 7d 20 5d01h)${GAP}$(tag)$(acct default) codex $(win P 62 1d10h)${GAP}$(acct ci-bot hot) codex $(win P 18 3h11m)  $(win S 44 4d20h)${GAP}gpt-5.6-sol $(win P 90 0h40m)"
label AFTER
wordmark
echo "$(acct team) $(win 5h 31 2h29m)  $(win 7d 62 1d13h)${GAP}$(acct work) $(win 5h 12 4h02m)  $(win 7d 20 5d01h)${GAP}$(tag)$(acct default) $(win 7d 62 1d10h)${GAP}$(acct ci-bot hot) $(win 5h 18 3h11m)  $(win 7d 44 4d20h)  $(f "gpt-5.6-sol") $(win 5h 90 0h40m)"
note 'the default "codex" bucket stays unnamed; an extra bucket keeps its name before its windows'

# ── 3 ────────────────────────────────────────────────────────────────────────
title "3. Narrow terminal" "the combined line does not fit, so it splits in two"
label BEFORE
wordmark
echo "$(acct team) $(win 5h 31 2h29m)  $(win 7d 62 1d13h)"
echo "$(acct codex) $(win P 62 1d10h)"
note 'no tag on the Codex line: only P/S (vs 5h/7d) says which provider it is'
label AFTER
wordmark
echo "$(acct team) $(win 5h 31 2h29m)  $(win 7d 62 1d13h)"
echo "$(tag)$(acct default) $(win 7d 62 1d10h)"
note 'with 5h/7d on both lines the P/S cue is gone, so the "codex" tag stays on the split line'

# ── 4 ────────────────────────────────────────────────────────────────────────
title "4. Codex only" "no Claude token"
label BEFORE
wordmark
echo "$(tag)$(acct codex) $(win P 62 1d10h)"
label AFTER
wordmark
echo "$(tag)$(acct default) $(win 7d 62 1d10h)"

echo
note "also try: NO_COLOR=1 bash $0"
echo
