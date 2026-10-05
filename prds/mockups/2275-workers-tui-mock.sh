#!/usr/bin/env bash
# Design mock for PRD #2275 (workers in the TUI). Static fixture data, no server, not
# shipped code: the real view is built in api/cmd/uzi on the shipped tuiModel.
# bash 3.2 compatible (macOS /bin/bash). Run: bash prds/mockups/2275-workers-tui-mock.sh
#
# Tab order: floor · workers · pulls · ci. Keys 1 2 3 4 select them; the digits appear only in
# the ? help, never in the strip or footers.
# Title line: tabs left, fleet status right-aligned (narrowed to fit; own line only as a last resort).
# Split view (s, auto on tall terminals in the real TUI): TOP pane = floor | workers, BOTTOM pane = pulls | ci.
#   The title shows only the top pane's tabs (`floor · [workers]`), mirroring the bottom divider.
#   1/2 pick + focus the top pane, 3/4 the bottom; tab cycles all four (owning pane follows); ctrl+w switches focus.
#
# Keys
#   1 floor  2 workers  3 pulls  4 ci     j/k or ↑/↓ move   enter/→ drill-in   esc/← back
#   a  scope: workers <-> factory workers        w  width 80 <-> 120
#   z  fleet status (title line) on/off          s  split view on/off (shipped key)
#   tab  next view   ctrl+w  pane focus          ?  legend   q quit
#   t  theme dark <-> light (repaints the terminal background; restored on quit)
#   t, w and z exist only in this mock, for comparing layouts (w is the shipped rework key).
#   Where this mock and the PRD disagree, the PRD wins.

export LC_ALL=${LC_ALL:-en_US.UTF-8}   # ${#s} must count characters, not bytes
E=$'\033'
R="${E}[0m"; B="${E}[1m"; F="${E}[2m"
# Colours = the shipped ANDON palette (api/cmd/uzi/tui_render.go newPalette), light and dark
# hex values in truecolor. `t` flips the theme and repaints the terminal background (OSC 11)
# so the light values are judged on a light ground; the original colours return on exit.
# Roles, kept consistent with floor / pulls / ci:
#   TUNG tungsten  wordmark, active tab, cursor ▌        SAGE sage   busy (= a running run)
#   AMB  amber     needs a human: ⚑ holding, the count   STALL stall ▲ warn items, offline
#   RED  alarm     ✕ danger items, disk/cpu >= 90%       WAIT  wait  draining / cordoned (self-resolving holds)
#   GRY  faint     idle, ids, ages, info items, chrome   healthy readings stay default ink (quiet board)
rgb() { printf '%s[%s;2;%d;%d;%dm' "$E" "$1" $((16#${2:1:2})) $((16#${2:3:2})) $((16#${2:5:2})); }
set_theme() { # $1 = dark|light ; value pairs are (light dark) exactly as in newPalette
  local d=1; [ "$1" = light ] && d=0
  pick() { if [ $d = 1 ]; then rgb 38 "$2"; else rgb 38 "$1"; fi; }
  TUNG=$(pick '#7c5200' '#c9a061'); AMB=$(pick '#b45309' '#ffb454'); RED=$(pick '#b91c1c' '#f87171')
  STALL=$(pick '#c2410c' '#fb923c'); SAGE=$(pick '#2f7d4f' '#6fbf8f'); WAIT=$(pick '#0369a1' '#38bdf8')
  GRY=$(pick '#6c6c6c' '#8a8a8a')
  if [ $d = 1 ]; then SEL=$(rgb 48 '#33302a'); else SEL=$(rgb 48 '#f3ead8'); fi
  CYN=$GRY   # issue ids are faint, as on the floor
  [ -z "${MOCK_RENDER:-}" ] && { if [ $d = 1 ]; then printf '%s]11;#0e1016%s\\%s]10;#d8d8d8%s\\' "$E" "$E" "$E" "$E"
    else printf '%s]11;#fbfaf7%s\\%s]10;#1f1f1f%s\\' "$E" "$E" "$E" "$E"; fi; }
  return 0; }
theme=${MOCK_THEME:-dark}; set_theme "$theme"

# ---- fixture (parallel arrays, one index per worker) ---------------------------------
# DTO facts stay SEPARATE (online, draining_since, upgrade, busy, custody); the primary
# STATE word is derived, everything else becomes an ATTENTION item.
NAME=(   "forge-large" "forge-docker" "eph-a10000" "eph-b20000" "forge-m-2" "forge-small" "recovery" "laptop" "chat-box" "b-runner")
OWNER=(  you you you you you you you you you "user B")
ONLINE=( 1 1 1 1 1 0 1 1 1 1)
DRAIN=(  0 0 0 0 1 1 0 0 0 0)      # draining_since set: with active runs = draining, without = cordoned
UPG=(    ok ok ok ok ok failed ok outdated ok ok)
BUSY=(   1 1 1 0 1 0 0 1 1 0)
CUST=(   0 0 0 0 0 0 1 0 0 0)
KIND=(   "host·L" "host·L+dk" "eph·M" "eph·M" "host·M" "host·S" "host·M" "ext" "ext" "host·M")
PRESS=(  "" "" "" "" "" "" "" "" "" "data")   # admin-only: AdminWorkerDTO.disk_pressure_volumes
ACT=(    2 1 1 0 1 0 0 0 0 0)      # api active_runs (run lane only, chat excluded)
CAP=(    3 2 1 1 2 1 2 "?" 1 1)    # "?" = max_concurrent_runs null: unknown, never zero
CPU=(    41 88 12 1 9 "?" 3 22 6 1)
MEMU=(   "5.1/8G" "7.4/8G" "1.9/4G" "0.3/4G" "2.2/4G" "?" "1.1/4G" "2.0G" "0.6/2G" "0.4/2G")
MEMSRC=( cgroup cgroup cgroup cgroup cgroup - cgroup process cgroup cgroup)
DISKL=(  "data" "dind ino" "data" "data" "nix" "?" "data" "data" "data" "data")
DISKP=(  92 97 22 4 78 -1 48 31 12 9)
DATAP=(  92 41 22 4 35 -1 48 31 12 9)
STALE=(  0 0 0 0 0 1 0 0 0 0)
VER=(    0.85.1 0.85.1 0.85.1 0.85.1 0.85.1+gba846d7 0.85.0 0.85.1 0.84.0 0.85.1 0.85.1)
HB=(     3s 2s 4s 5s 6s 2h 5s 9s 3s 6s)
UPT=(    3d4h 11h 38m 1h 2d1h - 2d1h 6d 4h 1d)
CAPS=(   "jvm" "docker jvm" "jvm" "docker" "jvm" "jvm" "-" "docker" "-" "-")
TPLD=(   "node-jvm" "node-jvm" "node-jvm" "node" "node-jvm" "node-jvm" "node" "node-jvm" "-" "-")
TPLR=(   "node-jvm" "node-jvm" "node-jvm" "node" "node-jvm" "node-jvm" "node" "node" "-" "-")
TOKEN=(  "default" "auto (opted-in pool)" "default" "default" "pinned: work-key" "default" "default" "auto (opted-in pool)" "default" "default")
# ephemeral: the binding (ephemeral_run_id) is server-internal and never shown or inferred
EPH=(    "" "" "ephemeral" "ephemeral · lease 8m left (idle, held for a follow-up)" "" "" "" "" "" "")
OUTBOX=( 0 14 0 0 0 0 0 0 0 0)
OUTBOXBLK=( "" "" "" "" "" "" "" "" "" "")
OUTBOXBLK[7]="terminal outcome refused: run was re-claimed (gen 4)"
# reported_runs: "runid|phase|terminal_pending_age|issue|title|stage|engine|gen"  ';'-separated
#   phase = the WORKER's reported phase (running | awaiting_approval | awaiting_input | awaiting_followup)
#   stage = the RUN's own stage from the board cache (separately sourced; absent -> run id)
RUNS=(
  "a1000001|running||#101|Add export button|implementing|claude|3;a1000002|running||#102|Retry flaky upload|reviewing|codex|5"
  "a1000003|running|12m|#103|Retry flaky upload fix|implementing|codex|2"
  "a1000004|awaiting_input||#104|Bug sweep follow-up|planning|codex|1"
  ""
  "a1000005|awaiting_approval||#105|Test hardening|plan gate|claude|1"
  ""
  ""
  ""
  "a1000006|running||||chat|claude|1"
  ""
)

sel=0; rsel=0; view=workers; drill=0; admin=0; width=120; legend=0; strip=1
split=0; top=floor; bottom=ci; focus=top

pad() { local s="$1" w="$2" n=${#1}
  if [ "$n" -ge "$w" ]; then printf '%s' "${s:0:$w}"; else printf '%s%*s' "$s" $((w-n)) ""; fi; }


pctcol() { local p=$1; if [ "$p" -lt 0 ]; then printf '%s' "$GRY"; elif [ "$p" -ge 90 ]; then printf '%s' "$RED"; elif [ "$p" -ge 75 ]; then printf '%s' "$STALL"; else printf '%s' "$R"; fi; }

bar() { local p=$1 w=$2 i fill col
  if [ "$p" -lt 0 ]; then printf "${GRY}%s${R}" "$(printf '%*s' "$w" '' | tr ' ' '·')"; return; fi
  fill=$(( (p*w+50)/100 )); col=$(pctcol "$p"); printf '%s' "$col"
  for ((i=0;i<w;i++)); do if [ $i -lt $fill ]; then printf '▮'; else printf "${GRY}▯${col}"; fi; done; printf '%s' "$R"; }

# primary state: offline > holding > draining|cordoned > busy > idle
state() { local i=$1
  if [ "${ONLINE[$i]}" = 0 ]; then echo offline
  elif [ "${CUST[$i]}" = 1 ]; then echo holding
  elif [ "${DRAIN[$i]}" = 1 ]; then if [ "${ACT[$i]}" -gt 0 ]; then echo draining; else echo cordoned; fi
  elif [ "${BUSY[$i]}" = 1 ]; then echo busy
  else echo idle; fi; }

statecell() { local s=$1 w=$2 g c
  case $s in idle) g='●' c=$GRY;; busy) g='◉' c=$SAGE;; draining) g='◐' c=$WAIT;; cordoned) g='◌' c=$WAIT;; holding) g='⚑' c=$AMB;; offline) g='○' c=$STALL;; esac
  printf '%s%s %s%s' "$c" "$g" "$(pad "$s" $((w-2)))" "$R"; }

# attention items, worst first: items joined by '^', each "colour~glyph text".
# Severity: alarm = danger (✕); amber ⚑ / stall ▲ / wait ◐◌ = warn; faint = info (·).
# Only danger + warn count toward "need attention".
attention() { local i=$1 out="" r age IFS
  [ "${UPG[$i]}" = failed ] && out+="$RED~✕ upgrade failed^"
  [ -n "${OUTBOXBLK[$i]}" ] && out+="$RED~✕ outbox blocked^"
  [ $admin = 1 ] && [ -n "${PRESS[$i]}" ] && out+="$RED~✕ pressure: ${PRESS[$i]}^"
  case "${DISKL[$i]}" in data|nix) [ "${DISKP[$i]}" -ge 90 ] && out+="$RED~✕ ${DISKL[$i]} ${DISKP[$i]}%^";; esac
  [ "${CUST[$i]}" = 1 ] && out+="$AMB~⚑ unpublished work^"
  if [ -n "${RUNS[$i]}" ]; then IFS=';'; for r in ${RUNS[$i]}; do
    IFS='|' read -r _ _ age _ <<<"$r"; [ -n "$age" ] && out+="$STALL~◷ outcome pending $age^"; IFS=';'; done; unset IFS; fi
  [ "${OUTBOX[$i]}" -gt 0 ] && out+="$STALL~⇡ outbox ${OUTBOX[$i]} queued^"
  [ "${ONLINE[$i]}" = 0 ] && out+="$STALL~○ offline ${HB[$i]}^"
  if [ "${DRAIN[$i]}" = 1 ]; then if [ "${ACT[$i]}" -gt 0 ]; then out+="$WAIT~◐ draining, ${ACT[$i]} run left^"; else out+="$WAIT~◌ cordoned^"; fi; fi
  [ "${UPG[$i]}" = outdated ] && out+="$STALL~↑ outdated ${VER[$i]}^"
  [ "${TPLD[$i]}" != "${TPLR[$i]}" ] && out+="$STALL~▲ template drift^"
  case "${DISKL[$i]}" in *ino*|dind*) [ "${DISKP[$i]}" -ge 90 ] && out+="$STALL~▲ ${DISKL[$i]} ${DISKP[$i]}% (display only)^";; esac
  case "${EPH[$i]}" in *lease*) out+="$GRY~· lease 8m left^";; esac
  [ "${BUSY[$i]}" = 1 ] && [ "${ACT[$i]}" = 0 ] && out+="$GRY~· chat active^"
  printf '%s' "$out"; }

attcell() { local a first rest n=0 x IFS
  a=$(attention "$1"); [ -z "$a" ] && { printf "${GRY}—${R}"; return; }
  first=${a%%^*}; rest=${a#*^}; IFS='^'; for x in $rest; do n=$((n+1)); done; unset IFS
  printf '%s%s%s' "${first%%~*}" "${first#*~}" "$R"; [ $n -gt 0 ] && printf "${F} +%d${R}" $n; }

needs_attention() { local a; a=$(attention "$1"); case "$a" in *"$RED"*|*"$AMB"*|*"$STALL"*|*"$WAIT"*) return 0;; esac; return 1; }

sevrank() { local a; a=$(attention "$1")
  case "$a" in *"$RED"*) echo 0;; *"$AMB"*|*"$STALL"*|*"$WAIT"*) echo 1;; *"$GRY"*) echo 2;; *) echo 3;; esac; }

# rows sorted worst severity first, then name (the real view keeps the cursor by worker ID)
visible_rows() { local i line; ROWS=()
  while read -r line; do ROWS+=("${line##* }"); done < <(
    for i in "${!NAME[@]}"; do
      [ $admin = 0 ] && [ "${OWNER[$i]}" != you ] && continue
      printf '%s %s %s\n' "$(sevrank "$i")" "${NAME[$i]}" "$i"
    done | sort -k1,1n -k2,2); }

scope_label() { [ $admin = 1 ] && echo "factory workers" || echo "workers"; }

vislen() { local s; s=$(printf '%s' "$1" | sed $'s/\033\\[[0-9;]*m//g'); echo ${#s}; }

# right-align $2 after $1 on one line of $width; when the gap would be < 2, $2 takes its own line
rjust() { local gap; gap=$(( width - $(vislen "$1") - $(vislen "$2") ))
  if [ $gap -ge 2 ]; then printf '%s%*s%s\n' "$1" $gap "" "$2"; else printf '%s\n%s\n' "$1" "$2"; fi; }

# Occupancy, not admission capacity: in-use / advertised run-lane slots over ONLINE workers,
# with holds, drains and cordons counted separately (they advertise slots they will not fill).
fleet_counts() { local i s
  FN=0 FON=0 FU=0 FC=0 FUNK=0 FATT=0 FHOLD=0 FDRAIN=0 FCORD=0
  for i in "${ROWS[@]}"; do FN=$((FN+1)); s=$(state "$i")
    case $s in holding) FHOLD=$((FHOLD+1));; draining) FDRAIN=$((FDRAIN+1));; cordoned) FCORD=$((FCORD+1));; esac
    if [ "$s" != offline ]; then FON=$((FON+1)); FU=$((FU+ACT[i]))
      if [ "${CAP[$i]}" = "?" ]; then FUNK=$((FUNK+1)); else FC=$((FC+CAP[i])); fi; fi
    needs_attention "$i" && FATT=$((FATT+1)); done; }

# Fleet status in one of four forms, widest first: 0 full, 1 without the ?cap note,
# 2 without the holding/draining/cordoned counts, 3 scope + slots + attention only.
fleet_form() { local lvl=$1
  printf "${B}%s${R}" "$(scope_label)"
  [ $lvl -le 2 ] && printf " · %d" $FN
  [ $lvl -le 2 ] && printf " · %d online" $FON
  printf " · ${TUNG}%d${R}/%d slots in use" $FU $FC
  [ $lvl -eq 0 ] && [ $FUNK -gt 0 ] && printf "${F} +%d ?cap${R}" $FUNK
  if [ $lvl -le 1 ]; then
    [ $FHOLD -gt 0 ] && printf " · ${AMB}%d holding${R}" $FHOLD
    [ $FDRAIN -gt 0 ] && printf " · ${WAIT}%d draining${R}" $FDRAIN
    [ $FCORD -gt 0 ] && printf " · ${WAIT}%d cordoned${R}" $FCORD
  fi
  [ $FATT -gt 0 ] && printf " · ${AMB}%d need attention${R}" $FATT; }

# widest form that fits in $1 columns (the minimal form when none fits)
fleet_fit() { local avail=$1 lvl s; visible_rows; fleet_counts
  for lvl in 0 1 2 3; do s=$(fleet_form $lvl); [ "$(vislen "$s")" -le "$avail" ] && break; done
  printf '%s' "$s"; }

# Title line. Left: wordmark + tabs (split: only the top pane's tabs, the selected one
# bracketed, mirroring the bottom divider's `pulls · ci`). Right: the fleet status, narrowed
# against the space LEFT after the tabs; its own line only when even the minimal form won't fit.
title() { local t lbl out="${TUNG}${B}▚▚ uzi${R}${F} · ${R}" set="floor workers pulls ci" sep="  " cur=$view avail fs sp=$split
  [ "${1:-}" = detail ] && { sp=0; cur=workers; }
  if [ $sp = 1 ]; then set="floor workers"; sep="${F} · ${R}"; cur=$top; fi
  for t in $set; do
    lbl=$t; [ $t = floor ] && [ $admin = 1 ] && lbl="active runs"
    if [ "$t" = "$cur" ]; then
      if [ $sp = 0 ]; then out+="${TUNG}${B}${lbl}${R}${sep}"
      elif [ $focus = top ]; then out+="${TUNG}${B}[${lbl}]${R}${sep}"   # bracket only in the focused pane
      else out+="${lbl}${sep}"; fi
    else out+="${F}${lbl}${R}${sep}"; fi
  done
  out="${out%"$sep"}"
  # worker detail draws the full strip without the fleet status
  { [ $strip = 1 ] && [ "${1:-}" != detail ]; } || { printf '%s\n' "$out"; return; }
  avail=$(( width - $(vislen "$out") - 2 ))
  fs=$(fleet_fit $avail)
  if [ "$(vislen "$fs")" -le $avail ]; then rjust "$out" "$fs"
  else printf '%s\n%s\n' "$out" "$(fleet_fit $width)"; fi; }

# floor: account meters, the run summary right-aligned on the last meter line
meters() { local m; m="  ${F}claude${R} $(bar 62 4) 62% ${F}5h${R}  ${F}codex${R} $(bar 18 4) 18% ${F}5h${R}"
  rjust "$m" "${F}\$1733+ 7d · 200 runs · 1–5${R}"; }

list_header() {
  if [ $width -ge 120 ]; then
    printf "${F}  %s %s%s %s %s %s %s %s %s %s %s ATTENTION${R}\n" "$(pad NAME 13)" "$( [ $admin = 1 ] && pad OWNER 7 && printf ' ')" "$(pad STATE 10)" "$(pad KIND 9)" "$(pad RUNS 4)" "$(pad CPU 5)" "$(pad MEM $MW)" "$(pad 'DISK (worst)' 19)" "$(pad VERSION $VW)" "$(pad HB 3)"
  else
    printf "${F}  %s %s %s %s ATTENTION${R}\n" "$(pad NAME 13)" "$(pad STATE 10)" "$(pad KIND 9)" "$(pad RUNS 4)"
  fi; }

# VERSION is as wide as the longest visible version + marker, capped at 18 (never truncated
# while the row has room)
version_width() { local i v m; VW=8; MW=9   # MEM is at least 9 wide (~12.0/16G)
  for i in "${ROWS[@]}"; do m=${#MEMU[$i]}; [ "${STALE[$i]}" = 1 ] && m=$((m+1)); [ $m -gt $MW ] && MW=$m; done
  for i in "${ROWS[@]}"; do v=$(( ${#VER[$i]} + 1 )); [ $v -gt $VW ] && VW=$v; done
  [ $VW -gt 18 ] && VW=18; return 0; }

list_row() { local k=$1 i=$2 active=$3 pre=" " line s
  s=$(state "$i"); [ "$active" = 1 ] && [ $k = $sel ] && pre="${TUNG}▌${R}"
  local runs; runs=$(pad "${ACT[$i]}/${CAP[$i]}" 4)
  if [ $width -ge 120 ]; then
    # stale (offline, last-known) readings: dimmed AND prefixed "~", so it survives NO_COLOR
    local d="" t=" "; [ "${STALE[$i]}" = 1 ] && { d=$F; t="~"; }
    local cpu; if [ "${CPU[$i]}" = "?" ]; then cpu="    ?"; else cpu=$(printf '%s%3d%%' "$t" "${CPU[$i]}"); fi
    local disk; if [ "${DISKP[$i]}" -lt 0 ]; then disk="$(pad '?' 19)"
      # 19 cells: [~]label(8) bar(4) pct; the stale ~ takes a cell from the percentage pad,
      # never from the label
      else local lead="${t# }"; disk="$(pad "${lead}${DISKL[$i]}" $((8 + ${#lead}))) $(bar "${DISKP[$i]}" 4) $(pctcol "${DISKP[$i]}")$(pad "${DISKP[$i]}%" $((5 - ${#lead})))${R}"; fi
    # version marker: ↑ outdated, ✕ upgrade failed (glyph + colour)
    local vm=" "; case "${UPG[$i]}" in outdated) vm="↑";; failed) vm="✕";; esac
    local ver; ver=$(pad "${VER[$i]}$vm" $VW); case "${UPG[$i]}" in outdated) ver="${STALL}${ver}${R}";; failed) ver="${RED}${ver}${R}";; esac
    line="$pre $(pad "${NAME[$i]}" 13) $( [ $admin = 1 ] && pad "${OWNER[$i]}" 7 && printf ' ')$(statecell "$s" 10) $(pad "${KIND[$i]}" 9) $runs ${d}$cpu $(pad "${t# }${MEMU[$i]}" $MW) ${disk}${R} ${ver} ${F}$(pad "${HB[$i]}" 3)${R} $(attcell "$i")"
  else
    line="$pre $(pad "${NAME[$i]}" 13) $(statecell "$s" 10) $(pad "${KIND[$i]}" 9) $runs $(attcell "$i")"
  fi
  if [ "$active" = 1 ] && [ $k = $sel ]; then printf '%s%s%s\n' "$SEL" "$line" "$R"; else printf '%s\n' "$line"; fi; }

footer() { # $1 = wide text, $2 = narrow text
  if [ $width -ge 120 ]; then printf "${F}%s${R}\n" "$1"; else printf "${F}%s${R}\n" "$2"; fi; }

workers_body() { # blank, header, rows (shared by the full screen and the top pane)
  local active=$1 k; version_width; printf '\n'; list_header
  for k in "${!ROWS[@]}"; do list_row "$k" "${ROWS[$k]}" "$active"; done; }

readout() { # selected-row readout: one line at >=120, one item per line below
  local i=${ROWS[$sel]} a x IFS
  printf "\n${F}selected${R} ${B}%s${R}" "${NAME[$i]}"; [ $admin = 1 ] && printf "${F} · owner${R} %s" "${OWNER[$i]}"
  a=$(attention "$i")
  if [ -n "$a" ]; then IFS='^'; for x in $a; do
    if [ $width -ge 120 ]; then printf " ${F}·${R} %s%s%s" "${x%%~*}" "${x#*~}" "$R"; else printf "\n  %s%s%s" "${x%%~*}" "${x#*~}" "$R"; fi
  done; unset IFS; fi; printf '\n'; }

draw_list() { visible_rows; [ $sel -ge ${#ROWS[@]} ] && sel=$((${#ROWS[@]}-1))
  title; workers_body 1; readout
  footer "enter detail · j/k move · a scope · w width ($width) · s split · ? legend · q quit" \
         "↵ detail · j/k · a scope · w width · s split · ? legend · q quit"
  [ $legend = 1 ] && legend_box; }

legend_box() {
  printf "\n${F}state${R} $(statecell idle 6)$(statecell busy 6)$(statecell draining 10)$(statecell cordoned 10)$(statecell holding 9)$(statecell offline 9)\n"
  printf "${F}  one word; draining = cordoned with runs left, cordoned = none left.${R}\n"
  printf "${F}  upgrade, outbox, pending outcomes, disk, drift = ATTENTION items.${R}\n"
  printf "${F}runs${R}  run-lane slots in use / advertised (chat excluded); ${GRY}?${R} = not advertised\n"
  printf "${F}disk${R}  worst labelled reading (ino = inodes). Colour is a visual cue,\n"
  printf "${F}      not the server's disk-pressure threshold; dind + inodes never gate.${R}\n"
  printf "${F}~   ${R}  stale (last-known) sample, also dimmed\n"
  printf "${F}sev ${R}  ${RED}✕${R} danger  ${STALL}▲${R}/${AMB}⚑${R}/${WAIT}◐${R} warn  ${GRY}·${R} info; only danger + warn count as \"need attention\"\n"
  printf "${F}ver ${R}  ↑ outdated  ✕ upgrade failed\n"
  printf "${F}keys${R}  1 floor · 2 workers · 3 pulls · 4 ci (digits are listed here only, never in the strip)\n"; }

kv() { printf "  ${F}%-13s${R} %s\n" "$1" "$2"; }

draw_detail() { visible_rows; local i=${ROWS[$sel]} s a x IFS hd up
  s=$(state "$i"); title detail
  # one header line: breadcrumb, state, kind; uptime/heartbeat right after it when it fits
  hd="${F}worker ›${R} ${B}${NAME[$i]}${R}  $(statecell "$s" $(( ${#s} + 2 )))  ${F}${KIND[$i]}${R}"
  if [ "${ONLINE[$i]}" = 1 ]; then up="up ${UPT[$i]} · heartbeat ${HB[$i]} ago"; else up="last heartbeat ${HB[$i]} ago"; fi
  if [ $(( $(vislen "$hd") + 2 + ${#up} )) -le $width ]; then printf '%s  %s%s%s\n' "$hd" "$F" "$up" "$R"
  else printf '%s\n  %s%s%s\n' "$hd" "$F" "$up" "$R"; fi
  printf "\n${TUNG}attention${R}\n"
  a=$(attention "$i")
  if [ -z "$a" ]; then printf "  ${GRY}no reported warnings${R}\n"; else IFS='^'; for x in $a; do
    case "${x#*~}" in
      *"upgrade failed") printf "  %s✕ upgrade failed${R}: container ${B}seed-nix${R} · ImagePullBackOff · last exit ${F}none${R}\n" "${x%%~*}"
                        printf "    ${F}target 0.85.1, running 0.85.0${R}\n";;
      *"unpublished work") printf "  %s⚑ holding unpublished work${R} from run ${CYN}a1000005${R}\n" "${x%%~*}"
                          printf "    ${F}not spare capacity; counts against your hosted quota${R}\n    ${F}uzi run recovery a1000005${R}\n";;
      *outcome*) printf "  %s%s${R}: run ${CYN}a1000003${R} finished on the worker;\n    ${F}its outcome is not yet delivered/acknowledged${R}\n" "${x%%~*}" "${x#*~}";;
      *"outbox blocked") printf "  %s✕ outbox blocked${R}: %s\n" "${x%%~*}" "${OUTBOXBLK[$i]}";;
      *outbox*) printf "  %s%s${R}: message frames waiting to replay to the api\n" "${x%%~*}" "${x#*~}";;
      *"template drift") printf "  %s▲ template drift${R}: declared ${B}%s${R}, worker reports ${B}%s${R}\n" "${x%%~*}" "${TPLD[$i]}" "${TPLR[$i]}";;
      *draining*) printf "  %s%s${R}: finishes in-flight work, claims nothing new\n" "${x%%~*}" "${x#*~}";;
      *cordoned*) printf "  %s%s${R}: claims nothing new\n" "${x%%~*}" "${x#*~}";;
      *pressure*) printf "  %s%s${R}: sustained fresh pressure reported by the server (admin view)\n" "${x%%~*}" "${x#*~}";;
      *) printf "  %s%s${R}\n" "${x%%~*}" "${x#*~}";;
    esac; done; unset IFS; fi
  printf "\n${TUNG}reported runs${R}  ${F}api active_runs${R} %s${F} / cap${R} %s ${F}(run lane)${R}\n" "${ACT[$i]}" "${CAP[$i]}"
  if [ -n "${RUNS[$i]}" ]; then local r n=0 rid ph age iss ttl stg eng gen nr
    nr=$(printf '%s' "${RUNS[$i]}" | tr ';' '\n' | grep -c .); [ $rsel -ge "$nr" ] && rsel=$((nr-1))
    IFS=';'; for r in ${RUNS[$i]}; do
      IFS='|' read -r rid ph age iss ttl stg eng gen <<<"$r"; n=$((n+1))
      [ -z "$iss" ] && { iss="run"; ttl="$rid"; }
      local cur=" "; [ $((n-1)) = $rsel ] && cur="${TUNG}›${R}"
      printf "%s ${F}%d${R} ${CYN}%-5s${R} %s ${F}%s${R}\n" "$cur" $n "$iss" "$(pad "$ttl" 26)" "$eng"
      printf "      ${F}worker phase${R} %s" "$ph"; [ -n "$age" ] && printf " · ${STALL}outcome pending %s${R}" "$age"
      printf "  ${F}· run stage${R} %s ${F}· gen %s${R}\n" "$stg" "$gen"
      IFS=';'; done; unset IFS
    printf "  ${F}↵/→ open run (esc/← returns here)${R}\n"
  else printf "  ${F}none${R}\n"; fi
  printf "\n${TUNG}resources${R}"; [ "${STALE[$i]}" = 1 ] && printf "  ${F}last-known, stale${R}"; printf "\n"
  if [ "${CPU[$i]}" = "?" ]; then printf "  ${GRY}cpu ?  memory ?  disk ?  (no sample)${R}\n"
  else
    kv "cpu" "${CPU[$i]}%"
    kv "memory" "${MEMU[$i]}$( [ "${MEMSRC[$i]}" = process ] && printf "  ${F}process only${R}")"
    kv "data" "$(bar "${DATAP[$i]}" 12) $(pctcol "${DATAP[$i]}")${DATAP[$i]}%${R}  ${F}inodes 12%${R}"
    if [ "${KIND[$i]}" != ext ]; then local np=47; [ "${DISKL[$i]}" = nix ] && np=${DISKP[$i]}; kv "nix" "$(bar $np 12) $(pctcol $np)$np%${R}"; fi
    [[ "${KIND[$i]}" == *dk* ]] && kv "dind" "$(bar 64 12) 64%  $(pctcol 97)inodes 97%${R}  ${F}display only${R}"
    [ -n "${RUNS[$i]}" ] && printf "  ${F}largest HOME${R}  ${CYN}%s${R} ≥18.4G ${F}(sampled 4m ago)${R}\n" "$(echo "${RUNS[$i]}" | cut -d'|' -f4 | sed 's/^$/run/')"
  fi
  printf "\n${TUNG}configuration${R}\n"
  kv "version" "${VER[$i]}$( case "${UPG[$i]}" in outdated) printf "  ${STALL}outdated${R}";; failed) printf "  ${RED}upgrade failed${R}";; esac)"
  kv "capabilities" "${CAPS[$i]}"
  kv "template" "${TPLD[$i]}$( [ "${TPLD[$i]}" != "${TPLR[$i]}" ] && printf "  ${STALL}≠ reported %s${R}" "${TPLR[$i]}")"
  kv "token" "${TOKEN[$i]}"
  [ -n "${EPH[$i]}" ] && kv "kind" "${EPH[$i]}"
  [ $admin = 1 ] && [ -n "${PRESS[$i]}" ] && kv "disk pressure" "${RED}${PRESS[$i]}${R} ${F}(sustained, server-reported)${R}"
  [ $admin = 1 ] && kv "owner" "${OWNER[$i]}"
  printf '\n'
  if [ $split = 1 ]; then footer "esc/← back to split (selection kept) · j/k select run · ? keys · q quit" "esc/← back to split · j/k run · ? · q quit"
  else footer "esc/← back · j/k select run · ↵/→ open run · ? keys · q quit" "esc/← back · j/k run · ↵/→ open · ? · q quit"; fi
  [ $legend = 1 ] && legend_box; }

# floor rows: the worker cell is drawn only at >=120 columns (dropped at 80). A run with no
# worker yet reads `no worker yet`; a finished run whose (ephemeral) worker is gone reads `—`.
floor_row() { # pre glyph issue title stage worker
  local wk=""; [ $width -ge 120 ] && wk="${F}$6${R}"
  printf "%s %s ${CYN}%s${R} %s %s %s\n" "$1" "$2" "$3" "$(pad "$4" 26)" "$(pad "$5" 12)" "$wk"; }
floor_rows() { local act=$1 pre=" "; [ "$act" = 1 ] && pre="${TUNG}▌${R}"
  floor_row "$pre" "${SAGE}◉${R}" "#101" "Add export button" "implementing" "forge-large"
  floor_row " " "${SAGE}◉${R}" "#102" "Retry flaky upload" "reviewing" "forge-large"
  floor_row " " "${SAGE}◉${R}" "#103" "Retry flaky upload fix" "implementing" "forge-docker"
  floor_row " " "${AMB}◆${R}" "#105" "Test hardening" "plan gate" "forge-m-2"
  floor_row " " "${GRY}○${R}" "#106" "Docker build cache" "queued" "no worker yet"
  floor_row " " "${GRY}✓${R}" "#099" "Seed fixture refresh" "completed" "—"; }

draw_floor() { title; meters; printf '\n'; floor_rows 1; printf '\n'
  footer "tab next · z fleet status ($( [ $strip = 1 ] && echo on || echo off)) · s split · ? keys · q quit" "tab · z fleet · s split · ? · q quit"
  [ $legend = 1 ] && legend_box; }

draw_split() { visible_rows; [ $sel -ge ${#ROWS[@]} ] && sel=$((${#ROWS[@]}-1))
  local tf=0 bf=0; [ $focus = top ] && tf=1 || bf=1
  title
  # top pane: floor keeps its meters + run summary; workers hides both and uses the same
  # table as full screen (wide at >= 120 columns)
  if [ $top = floor ]; then meters; printf '\n'; floor_rows $tf
  else workers_body $tf; readout; fi
  printf "${GRY}%s${R}\n" "$(printf '%*s' "$width" '' | tr ' ' '━')"
  # bottom pane: pulls | ci, the shown one bracketed (bold when focused)
  if [ $bottom = pulls ]; then
    printf " %s${F} · ci${R}\n" "$( [ $bf = 1 ] && printf "${TUNG}${B}[pulls]${R}" || printf 'pulls')"
  else
    printf " ${F}pulls · ${R}%s\n" "$( [ $bf = 1 ] && printf "${TUNG}${B}[ci]${R}" || printf 'ci')"
  fi
  case $bottom in
    ci) printf "%s ${SAGE}✓${R} main     ci.yml  0a1b2c3d  4m\n  ${RED}✕${R} pr-107  ci.yml  4e5f6a7b  12m\n" "$( [ $bf = 1 ] && printf "${TUNG}▌${R}" || printf ' ')";;
    pulls) printf "%s ${CYN}#107${R} Label taxonomy   ${SAGE}approved${R}\n  ${CYN}#108${R} dind disk         ${AMB}review${R}\n" "$( [ $bf = 1 ] && printf "${TUNG}▌${R}" || printf ' ')";;
  esac
  printf '\n'
  if [ $top = workers ] && [ $focus = top ]; then
    footer "↵/→ open worker · ctrl+w focus bottom · tab next · a scope · s leave split · q quit" "↵ worker · ^w focus · tab · a scope · s · q"
  else
    footer "ctrl+w focus ($focus) · tab next · s leave split · q quit" "^w focus · tab · s · q"; fi; }

draw() { printf '%s[H%s[2J' "$E" "$E"
  if [ $drill = 1 ]; then draw_detail; return; fi
  if [ $split = 1 ]; then draw_split; return; fi
  case "$view" in
    workers) draw_list;;
    floor) draw_floor;;
    *) title; printf "\n  ${F}(the %s view is unchanged and not drawn in this mock)${R}\n" "$view";;
  esac; }

next_view() { # tab: floor -> workers -> pulls -> ci -> floor; in split the owning pane takes it
  local cur=$view nxt; [ $split = 1 ] && { [ $focus = top ] && cur=$top || cur=$bottom; }
  case $cur in floor) nxt=workers;; workers) nxt=pulls;; pulls) nxt=ci;; ci) nxt=floor;; esac
  if [ $split = 1 ]; then case $nxt in floor|workers) top=$nxt; focus=top;; *) bottom=$nxt; focus=bottom;; esac
  else view=$nxt; fi; }

can_drill() { [ $split = 0 ] && [ "$view" = workers ] && return 0
  [ $split = 1 ] && [ $top = workers ] && [ $focus = top ] && return 0; return 1; }

if [ -z "${MOCK_RENDER:-}" ]; then
  cleanup() { printf '%s[?25h%s[?1049l%s]110%s\\%s]111%s\\' "$E" "$E" "$E" "$E" "$E" "$E"; }
  trap cleanup EXIT
  printf '%s[?1049h%s[?25l' "$E" "$E"
fi

while :; do
  draw
  IFS= read -rsn1 key || exit 0
  if [ "$key" = "$E" ]; then read -rsn2 -t 1 rest; key="$E$rest"; fi
  visible_rows; n=${#ROWS[@]}
  case "$key" in
    q) exit 0;;
    1|2|3|4) drill=0
       if [ $split = 1 ]; then
         case $key in 1) top=floor; focus=top;; 2) top=workers; focus=top;; 3) bottom=pulls; focus=bottom;; 4) bottom=ci; focus=bottom;; esac
       else case $key in 1) view=floor;; 2) view=workers;; 3) view=pulls;; 4) view=ci;; esac; fi;;
    j|"${E}[B") if [ $drill = 1 ]; then rsel=$((rsel+1)); elif [ $sel -lt $((n-1)) ]; then sel=$((sel+1)); fi;;
    k|"${E}[A") if [ $drill = 1 ]; then [ $rsel -gt 0 ] && rsel=$((rsel-1)); elif [ $sel -gt 0 ]; then sel=$((sel-1)); fi;;
    ""|$'\r'|l|"${E}[C") can_drill && { drill=1; rsel=0; };;
    "$E"|h|"${E}[D") drill=0;;
    a) admin=$((1-admin)); sel=0;;
    w) if [ $width = 120 ]; then width=80; else width=120; fi;;
    z) strip=$((1-strip));;
    s) if [ $drill = 0 ]; then
         if [ $split = 0 ]; then split=1; focus=top
           case $view in floor|workers) top=$view;; pulls|ci) bottom=$view; focus=bottom;; esac
         else split=0; [ $focus = top ] && view=$top || view=$bottom; fi; fi;;
    $'\x17') [ $split = 1 ] && [ $drill = 0 ] && { [ $focus = top ] && focus=bottom || focus=top; };;
    $'\t') [ $drill = 0 ] && next_view;;
    \?) legend=$((1-legend));;
    t) if [ $theme = dark ]; then theme=light; else theme=dark; fi; set_theme "$theme";;
  esac
done
