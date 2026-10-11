#!/usr/bin/env bash
# Mock of the uzi TUI with a run progress estimate. Static frames, no server.
# Rows are the live runs at 17:08 UTC 2026-10-09.
# Usage: bash <this file>            (truecolor)
#        NO_COLOR=1 bash <this file> (glyph-only fallback)

if [[ -n "${NO_COLOR:-}" ]]; then
  R='' B='' D='' TU='' AM='' SA='' RD='' CY='' GN='' NEW='' SEL='' SELX=''
else
  R=$'\e[0m' B=$'\e[1m' D=$'\e[2m'
  TU=$'\e[38;2;201;160;97m'   # tungsten (milestones, selection)
  AM=$'\e[38;2;251;191;36m'   # amber: needs you
  SA=$'\e[38;2;134;179;140m'  # sage: agent role
  RD=$'\e[38;2;251;113;133m'  # red: stalled
  CY=$'\e[38;2;56;189;248m'   # cyan: running / blocked-by
  GN=$'\e[38;2;52;211;153m'   # green: ok
  NEW=$'\e[38;2;251;146;60m'  # orange: what is new in this mock
  SEL=$'\e[48;2;46;38;26m' SELX=$'\e[49m'
fi

hr() { printf '%s\n' "${D}──────────────────────────────────────────────────────────────────────────────────────────────────${R}"; }

clear 2>/dev/null
printf '%s\n' "${B}MOCK 1 · board${R}  ${D}(new: the ${R}${NEW}PROG${R}${D} column after MILES)${R}"
hr
printf '%s\n' "  ${D}   ID        STATUS      AGE  MILES  ${R}${NEW}PROG          ${R}${D}  TITLE${R}"
printf '%s\n' "${SEL}${TU}▸${CY}▌● ${TU}9363c1c0${R}${SEL}  ${CY}running ${R}${SEL}    12h  ${TU}▰▰${B}▰${R}${SEL}    ${NEW}70% ${TU}▰▰▰▰▰▰${D}▱▱${R}${SEL}  ${TU}#2507${R}${SEL} Guarded runs end with 'custody final ACK pending'  ${SELX}${R}"
printf '%s\n' "${SEL}  ${TU}▸ ${AM}recovery-docs-validation${R}${SEL} ${D}·${R}${SEL} ${SA}lead${R}${SEL}  running a gate ${D}·${R}${SEL} 12s                                         ${SELX}${R}"
printf '%s\n' "  ${CY}▌● ${D}03e700fc${R}  ${CY}running ${R}    23m  ${TU}${B}▰${R}${D}▱${R}     ${NEW}11% ${TU}▰${D}▱▱▱▱▱▱▱${R}  ${D}#2566${R} LiveDB tests: lease expiry at observed locks"
printf '%s\n' "  ${CY}▌● ${D}63464bf2${R}  ${CY}running ${R}     6h  ${TU}${B}▰${R}${D}▱▱▱▱${R}  ${NEW}11% ${TU}▰${D}▱▱▱▱▱▱▱${R}  ${D}#2170${R} PRD: Code cross-check before publication"
printf '%s\n' "  ${RD}▌◼ ${D}fca7a801${R}  ${CY}running ${R}     5h  ${D}  –  ${R}   ${RD}stalled      ${R}  ${D}PR#2556${R} Rework MR review: agent/issue-2512"
printf '%s\n' "  ${AM}▌? ${D}24c8ec8b${R}  ${AM}input   ${R}    14h  ${TU}▰${D}▱▱▱${R}   ${AM}waits on you ${R}  ${AM}#2396${R} ${AM}Worker memory guard before container OOM${R}"
hr
printf '%s\n' "${D}  PROG cell: <pct> <8-cell bar>. A flag replaces it when a number would mislead:${R}"
printf '%s\n' "${D}  ${R}${RD}stalled${R}${D} · ${R}${AM}waits on you${R}${D} · ⏸ limit · planning · (blank: run has no milestones)${R}"
printf '%s\n' "${D}  Narrow terminal: the bar drops first; the pct stays. NO_COLOR keeps every flag as text.${R}"
echo
echo

printf '%s\n' "${B}MOCK 2 · run detail${R}  ${D}(new: the ${R}${NEW}PROGRESS${R}${D} block above the existing milestone list)${R}"
hr
printf '%s\n' "  ${CY}▌● running${R}  ${TU}9363c1c0${R}  ${TU}#2507${R} Guarded runs end with 'custody final ACK pending'   ${D}codex${R}"
printf '%s\n' "  ${D}elapsed${R} 11h 09m / 18h   ${D}started${R} 05:22   ${D}milestones${R} 2/3"
echo
printf '%s\n' "  ${NEW}${B}PROGRESS${R}  ${B}≈70%${R}  ${D}· milestone 3 of 3 · recovery-docs-validation${R}"
printf '%s\n' "  ${TU}plan ▰ │ ms1 ▰▰▰ │ ms2 ▰▰▰ │ ms3 ▰${D}▱▱${R}"
printf '%s\n' "  ${D}phase${R} ${TU}${B}▸ implement${R}  ${D}(current phase only, from the active role)${R}"
echo
printf '%s\n' "  ${D}MILESTONES${R}"
printf '%s\n' "  ${GN}✓${R} api-completion-receipt        "
printf '%s\n' "  ${GN}✓${R} worker-receipt-ordering       "
printf '%s\n' "  ${TU}▸${R} recovery-docs-validation        ${SA}lead${R}  running a gate ${D}· 12s${R}"
hr
echo

printf '%s\n' "${B}MOCK 3 · detail PROGRESS variants${R}"
hr
printf '%s\n' "  ${NEW}${B}PROGRESS${R}  ${RD}◼ stalled${R} ${D}· since 16:41 (run health)${R}"
printf '%s\n' "  ${NEW}${B}PROGRESS${R}  ${CY}⧗ may be blocked by fca7a801${R} ${D}· open question mentions #2512, which that run is working${R}"
printf '%s\n' "  ${NEW}${B}PROGRESS${R}  ${AM}● waits on you${R} ${D}· plan gate since 16:50${R}"
printf '%s\n' "  ${NEW}${B}PROGRESS${R}  ${D}planning · no milestones frozen yet${R}"
hr
echo
printf '%s\n' "${B}MOCK 4 · follow-up PRD: model-written Now line in the detail PROGRESS block${R}"
hr
printf '%s\n' "  ${NEW}${B}PROGRESS${R}  ${B}≈70%${R}  ${D}· milestone 3 of 3 · recovery-docs-validation${R}"
printf '%s\n' "  ${D}now${R}  lead is running the agent gate for the docs milestone ${D}· model summary · 2m ago${R}"
hr
