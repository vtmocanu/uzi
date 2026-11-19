#!/usr/bin/env bash
# e2e/driver.sh — PRD #966 M2: the phase-registry DRIVER semantics.
#
# Sourced by run-e2e.sh AT TOP LEVEL (in place of M1's inline `for f in phases;
# do source; done` loop + roll-call), and by e2e/driver.test.sh with fakes. It
# owns: header parse, lane filter, ONLY/SKIP selection, requires validation,
# the errexit-safe subshell-per-phase, provides round-trip, fail-soft + critical,
# the end-of-phase BOUNDARY (#2892: undeclared residue that does not settle, a rejected
# cancel or an unverifiable read FAILS the phase), results (results.tsv / junit.xml / summary.md) and the
# roll-call. It ENDS WITH `exit`, which — because run-e2e.sh sources it last —
# drives the cleanup EXIT trap with the suite's exit code.
#
# DEPENDS (the caller must define these BEFORE sourcing; the hermetic test fakes
# them): functions  say pass fail db_psql_rows (SQL [SECS]) boundary_cancel
#                   (PATH SECS EVIDENCE_PREFIX)
#         globals    ROOT RUNROOT ENVFILE FORGE EXECUTOR ADMIN_EMAIL  (MARGINS_FILE optional)
#                    COMPOSE (array; used ONLY by on-red artifact capture, which is
#                    `|| true` throughout so a missing/short COMPOSE never reddens a run)
#
# KNOBS: PHASES_DIR (default $ROOT/e2e/phases), E2E_ONLY / E2E_SKIP (comma-lists
#        of slug globs), E2E_FAULT_PHASE, and the hermetic-test-only E2E_BOUNDARY_TIMEOUT (seconds the
#        end-of-phase boundary may wait for NEW residue to settle; default 120) and
#        E2E_BOUNDARY_INTERVAL (seconds between samples; default 1). A value that
#        is not a positive integer falls back to its default.
#
# THE BOUNDARY (#2892). Before each body a snapshot records the owner (the user
# ADMIN_EMAIL names) and that owner's OPEN custody holds (the baseline). After the
# body the boundary samples non-terminal runs, worker_active_runs rows and the
# owner's open holds until nothing NEW is left, a read fails, or the deadline passes.
# RESIDUE is: a non-terminal run not held by a `# handoff:` variable (vars holding run
# IDs); a worker row whose run is not so held (even a DB-terminal run); an open hold in
# neither the baseline nor a `# handoff-holds:` variable (vars holding hold UUIDs).
# An undeclared non-terminal run is cancelled ONCE through the API; the driver never
# writes a run status. The loop ends at the first of: nothing NEW pending, a sticky error
# (unverifiable read, rejected or timed-out cancel, malformed declaration, unusable
# phase-start snapshot), or the deadline. The phase FAILs (the body's own row when the body
# failed too) when residue is still present in the last sample, or a sticky error was
# recorded. An undeclared run the boundary cancels and that then settles is reported in
# the PASS row's message but does not fail the phase. Residue a boundary reports
# becomes KNOWN: later boundaries still sample it but never wait for it (a boundary with only known residue takes one sample), but
# still FAIL while it is present, and every row after a failed boundary is annotated.
# Residue present in the sample a sticky error ended the loop on is recorded as known too,
# even a run cancelled in that same sample that has not settled yet.
#
# 🔴 THE MAIN LOOP AND ITS `source "$RUNROOT/phase.env.next"` ARE AT TOP LEVEL,
# NEVER INSIDE A FUNCTION. `declare -p` emits `declare --`, and sourcing that
# inside a function makes the variable function-local — so a phase's `provides:`
# would never reach the next phase. Case 2 of driver.test.sh proves exactly this
# by mutating the re-source into a function and watching the consumer FAIL.

_PHASES_DIR="${PHASES_DIR:-$ROOT/e2e/phases}"

# In-memory result rows (parallel arrays), declared up front so `${#_slugs[@]}`
# is safe under `set -u` even with zero rows.
_slugs=(); _statuses=(); _secs=(); _msgs=(); _titles=()
CRITICAL_FAILED=0
_FAILED_PROVIDES=""   # space-padded list of provides tokens of phases that FAILed

# Boundary state that outlives one phase (declared at top level so the functions below
# mutate THESE, never function-locals).
# (No associative arrays, namerefs or ${x,,}: this file must run on bash 3.2.)
_KN_K=(); _KN_V=()            # known residue: key / slug of the boundary that first reported it
_CANCEL_ATTEMPTED=" "         # actual requests only; an unsent cancel may be tried later
_contaminated=""              # space-prefixed slugs of phases whose boundary FAILed
_bw_slugs=(); _bw_start=(); _bw_end=(); _bw_total=()
# Per-boundary scratch.
# Space-padded ID sets (" id id "): baseline open holds, declared handoff runs / holds,
# runs already cancelled at this boundary, hold IDs observed in the latest sample.
_BASE=" "; _H_RUNS=" "; _H_HOLDS=" "; _B_CANC=" "; _B_HOLD_OBS=" "; _B_DECL=" "
_R_KEYS=(); _R_DESC=()        # residue keys and (same index) their human identity
_B_NOTE=""; _B_REASONS=(); _B_BREASONS=(); _B_PARSED=()
_B_SR=(); _B_SW=(); _B_SH=()
_OWNER=""; _bdir=""; _B_BUDGET=" "; _B_N=0; _B_DEADLINE=0; _B_READERR=0; _B_HOLDS_OK=0
_B_FAILED=0; _B_MESSAGE=""; _B_START_SECS=0; _B_END_SECS=0; _B_OUT=""

_Q_RUNS="select id from runs where status not in ('completed','failed','cancelled')"
_Q_WORKERS="SELECT worker_id, run_id, claim_generation FROM worker_active_runs"
_UUID_RE='^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'

# --- small pure helpers (safe to be functions: they never source phase.env) ---

# _hdr FIELD FILE — the single header value for FIELD (e.g. title, critical).
# Same shape M1 used; the leading `# shellcheck shell=bash` line never matches.
_hdr() { sed -n "s/^# $1:[[:space:]]*//p" "$2" | head -1; }

# _slug_of FILE — basename without the NN- prefix and the .sh suffix.
_slug_of() { local b; b="$(basename "$1" .sh)"; printf '%s' "${b#[0-9][0-9]-}"; }

# _strip_ansi — drop SGR escapes so a FAIL line reads as plain text.
_strip_ansi() { sed 's/\x1b\[[0-9;]*m//g'; }

# _xml_escape STR — escape the XML metacharacters for an attribute/text node.
# The replacement `&` is backslash-escaped (`\&`): bash 5.2+ enables
# patsub_replacement by default, so a bare `&` in a `${//}` replacement means
# "the matched text" — leaving it would turn `<` into `<lt;` rather than `&lt;`.
_xml_escape() {
  local s="$1"
  s="${s//&/\&amp;}"; s="${s//</\&lt;}"; s="${s//>/\&gt;}"; s="${s//\"/\&quot;}"
  printf '%s' "$s"
}

# _match_any SLUG COMMA_GLOB_LIST — 0 if SLUG matches any glob in the list.
_match_any() {
  local slug="$1" g IFS=,
  for g in $2; do
    [ -n "$g" ] || continue
    # shellcheck disable=SC2254  # $g is a glob pattern on purpose
    case "$slug" in $g) return 0 ;; esac
  done
  return 1
}

# _find_producer TOKEN — slug of the phase whose `provides:`/`mutates:` lists
# TOKEN (a VAR name or an `env:KEY=VALUE`), else empty. Whole-word match.
_find_producer() {
  local token="$1" f prov mut
  for f in "$_PHASES_DIR"/[0-9][0-9]-*.sh; do
    [ -e "$f" ] || continue
    prov="$(_hdr provides "$f")"
    mut="$(_hdr mutates "$f")"
    case " $prov $mut " in
      *" $token "*) _slug_of "$f"; return 0 ;;
    esac
  done
  return 1
}

# _requires_intersects_failed REQUIRES — 0 if any shell-var token in REQUIRES is
# in a previously-FAILed phase's provides (the suspect_cascade signal).
_requires_intersects_failed() {
  local tok
  for tok in $1; do
    case "$tok" in env:*) continue ;; esac
    case " $_FAILED_PROVIDES " in *" $tok "*) return 0 ;; esac
  done
  return 1
}

# _note_failed PROVIDES — remember a FAILed phase's provides for cascade tagging.
_note_failed() {
  local p
  for p in $1; do _FAILED_PROVIDES="$_FAILED_PROVIDES $p"; done
}

# _record SLUG STATUS SECS MSG TITLE [noran] — append one row to the arrays and to
# results.tsv (tab/newline in the message flattened so the TSV stays 4 columns).
# Once a boundary has FAILed (D5), every later row of a phase that ran carries the
# literal suffix naming the contaminated boundaries: a clean-looking result after a
# dirty boundary is not trustworthy evidence. A SKIP never ran, and a requires-miss FAIL
# (6th argument `noran`) never ran its body, so neither is annotated.
_record() {
  local m; m="$(printf '%s' "$4" | tr '\t\n' '  ')"
  if [ -n "$_contaminated" ] && [ "$2" != SKIP ] && [ "${6:-}" != noran ]; then
    m="$m [ran after contaminated boundary:$_contaminated]"
  fi
  _slugs+=("$1"); _statuses+=("$2"); _secs+=("$3"); _msgs+=("$m"); _titles+=("$5")
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$m" >> "$RUNROOT/results.tsv"
}

# _capture_artifacts NN_SLUG SECS — on a FAIL phase, snapshot evidence into
# $RUNROOT/artifacts/<NN_SLUG>/ and `touch` the .keep-rundir sentinel that makes
# cleanup keep the whole rundir on a red run (PRD #966 M3, D5). EVERYTHING here is
# guarded (`|| true`, `2>/dev/null`) so a capture failure can NEVER change a phase's
# recorded status or the suite exit code — it runs strictly AFTER _record. `--since`
# is a DURATION (elapsed phase seconds + slack), so no wall-clock var is needed and a
# service that is not up yet (e.g. agent before phase 13) simply yields an empty log.
# Nothing is captured on a PASS/SKIP phase (this is only ever called on FAIL).
_capture_artifacts() {
  local nn_slug="$1" secs="${2:-0}" dir svc since
  dir="$RUNROOT/artifacts/$nn_slug"
  touch "$RUNROOT/.keep-rundir" 2>/dev/null || true
  # First capture wins (#990): a later pass for the same slug would OVERWRITE the
  # richer capture's container logs with a narrower window. If the dir already exists,
  # a richer capture ran first: keep it.
  [ -d "$dir" ] && return 0
  mkdir -p "$dir" 2>/dev/null || true
  since="$((secs + 30))s"
  for svc in api agent forge-fake db; do
    "${COMPOSE[@]}" logs --no-color --since "$since" "$svc" > "$dir/$svc.log" 2>&1 || true
  done
  # Bounded (10 s) like every other DB read the driver makes after a failure: a wedged
  # database must not hang the artifact capture.
  db_psql_rows "select id,user_id,status,kind from runs order by created_at" 10 > "$dir/runs.txt" 2>/dev/null || true
  db_psql_rows "select status,kind,count(*) from runs group by status,kind order by status,kind" 10 \
    > "$dir/run-counts.txt" 2>/dev/null || true
  [ -f "$RUNROOT/logs/$nn_slug.log" ] && cp "$RUNROOT/logs/$nn_slug.log" "$dir/phase.log" 2>/dev/null || true
  return 0
}

# --- the end-of-phase boundary ------------------------------------------------

# _pos_int VALUE DEFAULT — VALUE when it is a positive integer, else DEFAULT.
_pos_int() {
  if [[ "${1:-}" =~ ^[0-9]+$ ]] && [ "$((10#$1))" -gt 0 ]; then printf '%s' "$((10#$1))"; else printf '%s' "$2"; fi
}
_is_uuid() { [[ "$1" =~ $_UUID_RE ]]; }
# _lc STR — lowercase (bash 3.2 has no ${x,,}).
_lc() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
# _in_set SET ID — 0 when the space-padded SET holds ID.
_in_set() { case "$1" in *" $2 "*) return 0 ;; esac; return 1; }
# _known_get KEY — print the slug that first reported KEY (empty when KEY is not known).
_known_get() {
  local i
  for ((i = 0; i < ${#_KN_K[@]}; i++)); do
    if [ "${_KN_K[$i]}" = "$1" ]; then printf '%s' "${_KN_V[$i]}"; return 0; fi
  done
  return 0
}

# _b_rem — whole seconds left before the boundary deadline (0 when none remain).
_b_rem() {
  local r=$((_B_DEADLINE - $(date +%s)))
  [ "$r" -gt 0 ] || r=0
  printf '%s' "$r"
}

# _b_reason TEXT — record one boundary failure reason (they all FAIL the phase).
_b_reason() { _B_REASONS+=("$1"); }

# _b_read LABEL SQL SECS STEM — ONE bounded settlement read: db_psql_rows with a SECS
# deadline, raw output / exit status / stderr kept as STEM.{out,exit,stderr}. A non-zero
# exit is a sticky `unverifiable` reason (124/137 are the limiter's timeout statuses).
# Sets _B_OUT; returns 1 on a failed read.
_b_read() {
  local label="$1" sql="$2" secs="$3" stem="$4" rc=0
  _B_OUT="$(db_psql_rows "$sql" "$secs" 2> "$stem.stderr")" || rc=$?
  if ! printf '%s\n' "$rc" > "$stem.exit" || ! printf '%s\n' "$_B_OUT" > "$stem.out"; then
    _b_reason "unverifiable $label (evidence write failed)"
    return 1
  fi
  if [ "$rc" -ne 0 ]; then
    case "$rc" in
      124|137) _b_reason "unverifiable $label (timed out, exit $rc)" ;;
      *)       _b_reason "unverifiable $label (psql exit $rc)" ;;
    esac
    return 1
  fi
  return 0
}

# _b_parse KIND TEXT — validate EVERY row of a read into _B_PARSED. KIND runs: one UUID
# per line. KIND triple: `UUID|UUID|integer`. Empty text is zero rows; a blank line, a
# wrong field count or a malformed field fails the whole read (return 1).
_b_parse() {
  local kind="$1" text="$2" line a b c bars
  _B_PARSED=()
  [ -n "$text" ] || return 0
  while IFS= read -r line; do
    if [ "$kind" = runs ]; then
      _is_uuid "$line" || return 1
      _B_PARSED+=("$(_lc "$line")")
    else
      bars="${line//[^|]/}"
      [ "${#bars}" -eq 2 ] || return 1
      IFS='|' read -r a b c <<< "$line"
      _is_uuid "$a" && _is_uuid "$b" && [[ "$c" =~ ^[0-9]+$ ]] || return 1
      _B_PARSED+=("$(_lc "$a")|$(_lc "$b")|$c")
    fi
  done <<< "$text"
  return 0
}

# _b_fetch LABEL SQL KIND — a deadline-bounded, validated sample read into _B_PARSED.
# No call starts once nothing remains; the call gets the remaining whole seconds.
_b_fetch() {
  local label="$1" sql="$2" kind="$3" rem stem rc=0
  stem="$_bdir/last-${label// /-}"
  _B_PARSED=()
  rem="$(_b_rem)"
  if [ "$rem" -le 0 ]; then _b_reason "unverifiable $label (deadline exhausted before read)"; _B_BUDGET="$_B_BUDGET$label "; return 1; fi
  _b_read "$label" "$sql" "$rem" "$stem" || rc=1
  if [ "$rc" -ne 0 ]; then
    case "${_B_REASONS[${#_B_REASONS[@]}-1]}" in *"(timed out, exit "*) _B_BUDGET="$_B_BUDGET$label " ;; esac
  fi
  if [ "$_B_N" -eq 1 ]; then cp "$stem.out" "$_bdir/first-${label// /-}.out" 2>/dev/null || true; fi
  [ "$rc" -eq 0 ] || return 1
  if ! _b_parse "$kind" "$_B_OUT"; then _b_reason "unverifiable $label (malformed row)"; return 1; fi
  return 0
}

# _b_sample — one sample of the three settlement reads (runs, worker rows, owner holds).
_b_sample() {
  local k
  _B_SR=(); _B_SW=(); _B_SH=(); _B_HOLD_OBS=" "; _B_READERR=0; _B_HOLDS_OK=0
  if _b_fetch runs "$_Q_RUNS" runs; then _B_SR=(${_B_PARSED[@]+"${_B_PARSED[@]}"}); else _B_READERR=1; fi
  if _b_fetch workers "$_Q_WORKERS" triple; then _B_SW=(${_B_PARSED[@]+"${_B_PARSED[@]}"}); else _B_READERR=1; fi
  if [ -n "$_OWNER" ]; then
    if _b_fetch holds "SELECT id, run_id, generation FROM recovery_custody_holds WHERE user_id = '$_OWNER' AND state = 'open'" triple; then
      _B_SH=(${_B_PARSED[@]+"${_B_PARSED[@]}"}); _B_HOLDS_OK=1
      for k in ${_B_SH[@]+"${_B_SH[@]}"}; do _B_HOLD_OBS="$_B_HOLD_OBS${k%%|*} "; done
    else
      _B_READERR=1
    fi
  fi
}

# _b_residue — residue of the latest sample into _R_KEYS / _R_DESC. Runs and worker rows
# get no baseline exemption; holds compare by ID against the baseline and declarations.
# Known keys stay residue even when a later phase explicitly declares them.
_b_residue() {
  local k id w r g
  _R_KEYS=(); _R_DESC=()
  for id in ${_B_SR[@]+"${_B_SR[@]}"}; do
    if [ -z "$(_known_get "run:$id")" ] && _in_set "$_H_RUNS" "$id"; then continue; fi
    _R_KEYS+=("run:$id"); _R_DESC+=("run=$id")
  done
  for k in ${_B_SW[@]+"${_B_SW[@]}"}; do
    IFS='|' read -r w r g <<< "$k"
    if [ -z "$(_known_get "worker:$w/$r/$g")" ] && _in_set "$_H_RUNS" "$r"; then continue; fi
    _R_KEYS+=("worker:$w/$r/$g"); _R_DESC+=("worker=$w run=$r generation=$g")
  done
  for k in ${_B_SH[@]+"${_B_SH[@]}"}; do
    IFS='|' read -r id r g <<< "$k"
    if [ -z "$(_known_get "hold:$id")" ]; then
      ! _in_set "$_BASE" "$id" || continue
      ! _in_set "$_H_HOLDS" "$id" || continue
    fi
    _R_KEYS+=("hold:$id"); _R_DESC+=("hold=$id run=$r generation=$g")
  done
}

# _b_count_new — how many residue keys were not known when this boundary began.
_b_count_new() {
  local k
  _B_PEND=0
  for k in ${_R_KEYS[@]+"${_R_KEYS[@]}"}; do
    if [ -z "$(_known_get "$k")" ]; then _B_PEND=$((_B_PEND + 1)); fi
  done
}

# _b_cancel_new — cancel each undeclared non-terminal run at most ONCE per driver, through
# boundary_cancel bounded by the remaining seconds. Attempted runs are never re-cancelled. A
# non-2xx or transport failure is sticky (FAIL even if the run later turns terminal).
_b_cancel_new() {
  local k id rem pre rc ex code
  for k in ${_R_KEYS[@]+"${_R_KEYS[@]}"}; do
    case "$k" in run:*) ;; *) continue ;; esac
    id="${k#run:}"
    ! _in_set "$_CANCEL_ATTEMPTED" "$id" || continue
    ! _in_set "$_B_CANC" "$id" || continue
    _B_CANC="$_B_CANC$id "
    rem="$(_b_rem)"
    if [ "$rem" -le 0 ]; then _b_reason "cancel not sent run=$id (deadline exhausted)"; continue; fi
    _CANCEL_ATTEMPTED="$_CANCEL_ATTEMPTED$id "
    pre="$_bdir/cancel-$id"
    rc=0
    boundary_cancel "/api/runs/$id/inputs" "$rem" "$pre" || rc=$?
    [ "$rc" -ne 0 ] || continue
    ex="$(cat "$pre.exit" 2>/dev/null)" || ex=""
    code="$(cat "$pre.http" 2>/dev/null)" || code=""
    case "$ex" in
      28|124|137) _b_reason "cancel timed out run=$id (exit $ex)" ;;
      *)          _b_reason "cancel rejected run=$id http=${code:-000}" ;;
    esac
  done
}

# _b_declared NAMES — set _B_DECL to the space-padded lowercase UUIDs the declared variables
# hold. An empty or unset variable declares nothing; a non-UUID value (or an unusable name)
# is a boundary reason.
_b_declared() {
  local name v id ids=()
  _B_DECL=" "
  for name in $1; do
    case "$name" in env:*) continue ;; esac
    if ! [[ "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then _b_reason "malformed declaration $name (not a variable name)"; continue; fi
    v="${!name:-}"
    # A variable may hold several whitespace-separated UUIDs; each must be one.
    ids=(); read -r -a ids <<< "$(printf '%s' "$v" | tr '\n\t' '  ')"   # split on any whitespace, incl. newlines; no pathname expansion
    for id in ${ids[@]+"${ids[@]}"}; do
      if _is_uuid "$id"; then _B_DECL="$_B_DECL$(_lc "$id") "; else _b_reason "malformed declaration $name (not a UUID: $id)"; fi
    done
  done
}

# _boundary_begin NN_SLUG — the phase-start snapshot (D1). Resolves the owner (exactly one
# UUID) and the owner's open holds, each read bounded to 10 s. A failure is a boundary
# reason (the phase FAILs) but the body still runs. A hold already in the known-residue
# map never becomes an exempt baseline hold.
_boundary_begin() {
  local t0 stem k id
  t0="$(date +%s)"
  _bdir="$RUNROOT/boundary/$1"
  _OWNER=""; _BASE=" "; _B_REASONS=()
  if ! rm -rf "$_bdir" || ! mkdir -p "$_bdir"; then
    _b_reason "unverifiable boundary evidence (staging unavailable)"
  fi
  if [ -z "${ADMIN_EMAIL:-}" ] || [[ "$ADMIN_EMAIL" == *\'* ]]; then
    _b_reason "unverifiable owner (ADMIN_EMAIL unusable)"
  elif _b_read owner "SELECT id FROM users WHERE email = '$ADMIN_EMAIL'" 10 "$_bdir/begin-owner"; then
    if [ -n "$_B_OUT" ] && [[ "$_B_OUT" != *$'\n'* ]] && _is_uuid "$_B_OUT"; then
      _OWNER="$(_lc "$_B_OUT")"
    else
      _b_reason "unverifiable owner (malformed row)"
    fi
  fi
  if [ -n "$_OWNER" ]; then
    stem="$_bdir/begin-baseline-holds"
    if _b_read "baseline holds" "SELECT id, run_id, generation FROM recovery_custody_holds WHERE user_id = '$_OWNER' AND state = 'open'" 10 "$stem"; then
      if _b_parse triple "$_B_OUT"; then
        for k in ${_B_PARSED[@]+"${_B_PARSED[@]}"}; do
          id="${k%%|*}"
          if [ -z "$(_known_get "hold:$id")" ]; then _BASE="$_BASE$id "; fi
        done
      else
        _b_reason "unverifiable baseline holds (malformed row)"
      fi
    fi
  fi
  _B_BREASONS=(${_B_REASONS[@]+"${_B_REASONS[@]}"})
  _B_START_SECS=$(( $(date +%s) - t0 ))
}

# _boundary_end NN_SLUG SLUG HANDOFF HOLDS BODY_RC — the end-of-phase boundary (D3/D3a/D4).
# Sets _B_FAILED (0/1), _B_MESSAGE (`boundary: …`) and _B_END_SECS.
_boundary_end() {
  local nn_slug="$1" slug="$2" handoff="$3" holds="$4" body_rc="$5"
  local t0 timeout interval rem k kn i timed_out=0 final=0 fbud=0 n0=0 b="" parts=() sv_sr=() sv_sw=() sv_sh=() sv_obs="" sv_ok=0 kept=()
  t0="$(date +%s)"
  timeout="$(_pos_int "${E2E_BOUNDARY_TIMEOUT:-}" 120)"
  interval="$(_pos_int "${E2E_BOUNDARY_INTERVAL:-}" 1)"
  _B_DEADLINE=$((t0 + timeout))
  _B_REASONS=(${_B_BREASONS[@]+"${_B_BREASONS[@]}"})
  _H_RUNS=" "; _H_HOLDS=" "; _B_CANC=" "; _B_N=0; _R_KEYS=(); _R_DESC=(); _B_NOTE=
  _B_SR=(); _B_SW=(); _B_SH=(); _B_HOLD_OBS=" "; _B_HOLDS_OK=0
  if [ "$body_rc" -eq 0 ]; then
    _b_declared "$handoff"; _H_RUNS="$_B_DECL"
    _b_declared "$holds"; _H_HOLDS="$_B_DECL"
  fi

  while :; do
    _B_N=$((_B_N + 1))
    # Every sample can exhaust the budget, including one outside the final window.
    sv_sr=(${_B_SR[@]+"${_B_SR[@]}"}); sv_sw=(${_B_SW[@]+"${_B_SW[@]}"}); sv_sh=(${_B_SH[@]+"${_B_SH[@]}"})
    sv_obs="$_B_HOLD_OBS"; sv_ok="$_B_HOLDS_OK"; n0=${#_B_REASONS[@]}
    _B_BUDGET=" "
    _b_sample
    if [ "$_B_BUDGET" != " " ]; then
      # Only the READS that ran out of budget fall back to the previous sample's rows; in
      # the final window their read-budget reasons become the timeout. Successful reads, and every cancel reason
      # (rejected, timed out, not sent), stand.
      ! _in_set "$_B_BUDGET" runs || _B_SR=(${sv_sr[@]+"${sv_sr[@]}"})
      ! _in_set "$_B_BUDGET" workers || _B_SW=(${sv_sw[@]+"${sv_sw[@]}"})
      if _in_set "$_B_BUDGET" holds; then _B_SH=(${sv_sh[@]+"${sv_sh[@]}"}); _B_HOLD_OBS="$sv_obs"; _B_HOLDS_OK="$sv_ok"; fi
      kept=()
      for ((i = 0; i < ${#_B_REASONS[@]}; i++)); do
        k="${_B_REASONS[$i]}"
        if [ "$final" -eq 1 ] && [ "$i" -ge "$n0" ]; then
          case "$k" in
            "unverifiable runs ("*|"unverifiable workers ("*|"unverifiable holds ("*)
              case "$k" in *"(deadline exhausted before read)"|*"(timed out, exit "*) continue ;; esac ;;
          esac
        fi
        kept+=("$k")
      done
      _B_REASONS=(${kept[@]+"${kept[@]}"})
      # This observation is incomplete, so the boundary cannot be clean: FAIL and say
      # which reads went unread (their previous rows are still reported as residue).
      b="${_B_BUDGET# }"
      if [ "$final" -eq 1 ]; then _b_reason "final sample incomplete: ${b% } unread"
      else _b_reason "sample incomplete: ${b% } unread"; fi
      printf 'sample out of budget:%s(previous rows kept for those reads)\n' "$_B_BUDGET" > "$_bdir/final-budget.txt" 2>/dev/null || true
      fbud=1
    fi
    _b_residue
    _b_cancel_new
    [ "$fbud" -eq 0 ] || { timed_out=1; break; }
    # Any sticky reason (unverifiable read, rejected or timed-out cancel, phase-start or
    # declaration error) ends the loop after this sample: more samples cannot clear it.
    [ "${#_B_REASONS[@]}" -eq 0 ] || break
    _b_count_new
    [ "$_B_PEND" -gt 0 ] || break             # nothing NEW left to wait for
    [ "$final" -eq 0 ] || { timed_out=1; break; }
    rem="$(_b_rem)"
    # Less than a full interval left: take ONE last observation inside the remaining budget
    # (sleeping until 1 s before the deadline), so residue that settles in that final window
    # is seen, then stop. Never sleep past the deadline into a sample-less timeout.
    if [ "$rem" -le 0 ]; then timed_out=1; break; fi
    if [ "$rem" -le "$interval" ]; then
      [ "$rem" -le 1 ] || sleep $((rem - 1))
      final=1; continue
    fi
    sleep "$interval"
    rem="$(_b_rem)"
    if [ "$rem" -le 0 ]; then timed_out=1; break; fi
  done

  # Verdict: clean only with no residue at all (new or known) in the last sample, every
  # read successful, and no cancel rejected (all of those are in _B_REASONS).
  _B_FAILED=0
  if [ "${#_B_REASONS[@]}" -gt 0 ] || [ "${#_R_KEYS[@]}" -gt 0 ]; then _B_FAILED=1; fi

  if [ "$_B_FAILED" -eq 1 ]; then
    parts=(${_B_REASONS[@]+"${_B_REASONS[@]}"})
    for ((i = 0; i < ${#_R_KEYS[@]}; i++)); do
      k="${_R_KEYS[$i]}"; kn="$(_known_get "$k")"
      if [ -n "$kn" ]; then
        parts+=("still present from $kn: ${_R_DESC[$i]}")
      else
        parts+=("${_R_DESC[$i]}")
      fi
    done
    _boundary_history
    [ -z "${_B_HISTORY:-}" ] || parts+=("$_B_HISTORY")
    _B_MESSAGE="boundary: $(_join '; ' "${parts[@]}")"
    [ "$timed_out" -eq 0 ] || _B_MESSAGE="$_B_MESSAGE [not settled within ${timeout}s]"
    [ "$body_rc" -eq 0 ] || _B_MESSAGE="$_B_MESSAGE (declarations unavailable: body failed)"
    # Remember what this boundary reported so later boundaries do not each wait it out.
    for k in ${_R_KEYS[@]+"${_R_KEYS[@]}"}; do
      if [ -z "$(_known_get "$k")" ]; then _KN_K+=("$k"); _KN_V+=("$slug"); fi
    done
    for ((i = 0; i < ${#_KN_K[@]}; i++)); do printf '%s\t%s\n' "${_KN_K[$i]}" "${_KN_V[$i]}"; done \
      | sort > "$_bdir/known-residue.txt" || _B_MESSAGE="$_B_MESSAGE; boundary evidence write failed"
  else
    _B_MESSAGE=""
    # A run the boundary cancelled that then settled is not residue: keep the trace in the
    # PASS row's message (non-failing).
    for k in $_B_CANC; do _B_NOTE="${_B_NOTE:+$_B_NOTE; }boundary cancelled undeclared run $k (settled)"; done
  fi
  printf '%s\n' ${_B_REASONS[@]+"${_B_REASONS[@]}"} > "$_bdir/reasons.txt" 2>/dev/null || true
  _B_END_SECS=$(( $(date +%s) - t0 ))
}

# _join SEP ITEM... — print the items joined by SEP.
_join() {
  local sep="$1" out="" first=1 x; shift
  for x in "$@"; do
    if [ "$first" -eq 1 ]; then out="$x"; first=0; else out="$out$sep$x"; fi
  done
  printf '%s' "$out"
}

# _boundary_history — on a FAILED boundary, AFTER the loop, ONE read of the release
# history of the holds involved (pending/known holds seen, plus baseline holds that are no
# longer open) with its OWN fixed 5 s bound, even if the deadline is spent. Sets
# _B_HISTORY to a compact `history: …` string, or `release evidence unavailable (exit N)`
# (verdict unchanged: stderr is kept under the evidence dir). Skipped for an empty set.
_boundary_history() {
  local k id ids=() list="" rc=0 out line a b c d e f bars items=()
  local hs=" "
  _B_HISTORY=""
  for k in ${_R_KEYS[@]+"${_R_KEYS[@]}"} ${_KN_K[@]+"${_KN_K[@]}"}; do
    case "$k" in hold:*) hs="$hs${k#hold:} " ;; esac   # pending holds and every known hold
  done
  if [ "$_B_HOLDS_OK" -eq 1 ]; then
    for id in $_BASE; do
      _in_set "$_B_HOLD_OBS" "$id" || hs="$hs$id "
    done
  fi
  [ "$hs" != " " ] || return 0
  while IFS= read -r id; do ids+=("'$id'"); done < <(printf '%s\n' $hs | sort -u)
  list="$(_join ',' "${ids[@]}")"
  out="$(db_psql_rows "SELECT id, run_id, generation, state, final_disposition, release_evidence FROM recovery_custody_holds WHERE id IN ($list)" 5 2> "$_bdir/history.stderr")" || rc=$?
  if ! printf '%s\n' "$rc" > "$_bdir/history.exit" || ! printf '%s\n' "$out" > "$_bdir/history.out"; then
    _B_HISTORY="release evidence unavailable (evidence write failed)"; return 0
  fi
  if [ "$rc" -ne 0 ]; then _B_HISTORY="release evidence unavailable (exit $rc)"; return 0; fi
  while IFS= read -r line; do
    bars="${line//[^|]/}"
    if [ "${#bars}" -ne 5 ]; then _B_HISTORY="release evidence unavailable (exit 0)"; return 0; fi
    IFS='|' read -r a b c d e f <<< "$line"
    if ! _is_uuid "$a" || ! [[ "$c" =~ ^[0-9]+$ ]]; then _B_HISTORY="release evidence unavailable (exit 0)"; return 0; fi
    items+=("hold=$a state=${d:-none} disposition=${e:-none}")
  done <<< "$out"
  if [ "${#items[@]}" -eq 0 ]; then _B_HISTORY="release evidence unavailable (exit 0)"; return 0; fi
  _B_HISTORY="history: $(_join ', ' "${items[@]}")"
}

# _boundary_publish NN_SLUG — copy the staged boundary evidence next to the failed phase's
# artifacts. (Staged under $RUNROOT/boundary/ while sampling, never under artifacts/.)
_boundary_publish() {
  local dest="$RUNROOT/artifacts/$1/boundary"
  mkdir -p "$dest" 2>/dev/null || true
  cp -R "$_bdir/." "$dest/" 2>/dev/null || true
}

# --- results writers ---------------------------------------------------------

_write_junit() {
  local n=${#_slugs[@]} fails=0 skips=0 i slug status secs msg
  for ((i = 0; i < n; i++)); do
    case "${_statuses[$i]}" in
      FAIL) fails=$((fails + 1)) ;;
      SKIP) skips=$((skips + 1)) ;;
    esac
  done
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<testsuite name="e2e" tests="%s" failures="%s" skipped="%s">\n' "$n" "$fails" "$skips"
    for ((i = 0; i < n; i++)); do
      slug="$(_xml_escape "${_slugs[$i]}")"
      status="${_statuses[$i]}"
      secs="${_secs[$i]}"
      msg="$(_xml_escape "${_msgs[$i]}")"
      case "$status" in
        FAIL)
          printf '  <testcase name="%s" time="%s"><failure message="%s"/></testcase>\n' "$slug" "$secs" "$msg" ;;
        SKIP)
          printf '  <testcase name="%s" time="%s"><skipped/></testcase>\n' "$slug" "$secs" ;;
        *)
          printf '  <testcase name="%s" time="%s"/>\n' "$slug" "$secs" ;;
      esac
    done
    printf '</testsuite>\n'
  } > "$RUNROOT/junit.xml"
}

_write_summary() {
  local n=${#_slugs[@]} i sum=0
  {
    printf '# E2E summary\n\n'
    printf '| phase | status | secs | message |\n'
    printf '|---|---|---|---|\n'
    for ((i = 0; i < n; i++)); do
      printf '| %s | %s | %s | %s |\n' "${_slugs[$i]}" "${_statuses[$i]}" "${_secs[$i]}" "${_msgs[$i]}"
    done
    printf '\n## wait_* margins (tightest 20; headroom = ceiling - actual)\n\n'
    if [ -n "${MARGINS_FILE:-}" ] && [ -s "${MARGINS_FILE:-}" ]; then
      printf '```\n'
      awk -F'\t' '{ printf "%d\t%-46s waited %ss of %ss (headroom %ss)\n", $2-$1, substr($3,1,46), $1, $2, $2-$1 }' \
        "$MARGINS_FILE" | sort -n -k1,1 | cut -f2- | sed -n '1,20p'
      printf '```\n'
    else
      printf '_(no margin data)_\n'
    fi
    printf '\n## Boundary waits\n\n'
    if [ "${#_bw_slugs[@]}" -eq 0 ]; then
      printf '_(none)_\n\n'
    else
      printf '| phase | start_secs | end_secs | total_secs |\n'
      printf '|---|---|---|---|\n'
      for ((i = 0; i < ${#_bw_slugs[@]}; i++)); do
        printf '| %s | %s | %s | %s |\n' "${_bw_slugs[$i]}" "${_bw_start[$i]}" "${_bw_end[$i]}" "${_bw_total[$i]}"
        sum=$((sum + _bw_total[i]))
      done
      printf '\n'
    fi
    printf 'Total boundary seconds: %s\n' "$sum"
  } > "$RUNROOT/summary.md"
  return 0
}

_rollcall() {
  local n=${#_slugs[@]} i fails=0 failing=""
  for ((i = 0; i < n; i++)); do
    [ "${_statuses[$i]}" = FAIL ] && { fails=$((fails + 1)); failing="$failing ${_slugs[$i]}"; }
  done
  printf '\n'
  if [ "$fails" -eq 0 ]; then
    printf '\033[32mAll E2E checks passed.\033[0m (executor=%s)\n' "${EXECUTOR:-?}"
  else
    printf '\033[31mE2E FAILED: %s phase(s):%s\033[0m\n' "$fails" "$failing"
  fi
  for ((i = 0; i < n; i++)); do
    printf '  - [%s] %s\n' "${_statuses[$i]}" "${_titles[$i]}"
  done
}

# --- the driver loop (TOP LEVEL — see the banner at the top of this file) -----

mkdir -p "$RUNROOT/logs"
: > "$RUNROOT/results.tsv"
: > "$RUNROOT/boundary_waits.tsv"

for f in "$_PHASES_DIR"/[0-9][0-9]-*.sh; do
  [ -e "$f" ] || continue   # no-match glob guard (nullglob is not set)
  nn_slug="$(basename "$f" .sh)"
  slug="${nn_slug#[0-9][0-9]-}"
  title="$(_hdr title "$f")"
  critical="$(_hdr critical "$f")"
  lane="$(_hdr lane "$f")"
  requires="$(_hdr requires "$f")"; [ "$requires" = "-" ] && requires=""
  provides="$(_hdr provides "$f")"; [ "$provides" = "-" ] && provides=""
  handoff="$(_hdr handoff "$f")";  [ "$handoff" = "-" ] && handoff=""
  handoff_holds="$(_hdr handoff-holds "$f")"; [ "$handoff_holds" = "-" ] && handoff_holds=""
  racesens="$(_hdr race-sensitive "$f")"   # "yes" on a phase with timing/concurrency asserts

  # 2. Lane filter (M1 behaviour): source a lane phase only when it matches $FORGE.
  case "$lane" in ""|any|"$FORGE") ;; *) continue ;; esac

  # 8. After a critical failure every remaining phase is SKIP, not run.
  if [ "$CRITICAL_FAILED" = 1 ]; then
    _record "$slug" SKIP 0 "after critical failure" "$title"
    continue
  fi

  # 3. Selection. A `critical: yes` phase always runs, ignoring ONLY/SKIP.
  if [ "$critical" != yes ]; then
    if [ -n "${E2E_ONLY:-}" ] && ! _match_any "$slug" "$E2E_ONLY"; then
      _record "$slug" SKIP 0 "deselected (E2E_ONLY)" "$title"; continue
    fi
    if [ -n "${E2E_SKIP:-}" ] && _match_any "$slug" "$E2E_SKIP"; then
      _record "$slug" SKIP 0 "deselected (E2E_SKIP)" "$title"; continue
    fi
  fi

  # 4. requires: validation BEFORE the body. A miss is a hard FAIL naming this
  #    phase, the missing token, and the phase that would have produced it.
  miss_tok=""
  for tok in $requires; do
    case "$tok" in
      env:*)
        if ! grep -qxF -- "${tok#env:}" "$ENVFILE" 2>/dev/null; then miss_tok="$tok"; break; fi ;;
      *)
        if [ -z "${!tok:-}" ]; then miss_tok="$tok"; break; fi ;;
    esac
  done
  if [ -n "$miss_tok" ]; then
    producer="$(_find_producer "$miss_tok")" || true
    [ -n "$producer" ] || producer="declared by no phase"
    m="requires: $miss_tok not satisfied for phase $slug (provided by: $producer)"
    _requires_intersects_failed "$requires" && m="$m [suspect_cascade]"
    [ "$racesens" = yes ] && m="$m — possible race, see margins"
    _record "$slug" FAIL 0 "$m" "$title" noran
    _capture_artifacts "$nn_slug" 0
    _note_failed "$provides"
    [ "$critical" = yes ] && CRITICAL_FAILED=1
    continue
  fi

  # 5. Per-phase execution — THE ERREXIT-SAFE SHAPE. Never `( … ) || rc=$?` and
  #    never `if ( … )`: both inherit errexit-off, so a bare failing command in
  #    the phase body would read PASS. Guard against a stale phase.env.next first.
  printf '[phase] %s\n' "$nn_slug"
  : > "$RUNROOT/phase.env.next"
  # Declarations are per phase: forget the variables THIS phase may declare (at top
  # level, so the unset reaches the shell the body's subshell is forked from) unless the
  # phase also `requires:` them, then snapshot the owner and baseline holds (D1/D2).
  for name in $handoff $handoff_holds; do
    case "$name" in env:*) continue ;; esac
    case " $requires " in *" $name "*) continue ;; esac
    unset "$name" 2>/dev/null || true
  done
  _boundary_begin "$nn_slug"
  start=$SECONDS
  set +e
  # shellcheck disable=SC1090  # $f is a runtime phase path, not statically resolvable
  ( set -euo pipefail
    [ "${E2E_FAULT_PHASE:-}" = "$slug" ] && fail "injected fault: $slug"
    source "$f"
    # Round-trip only SHELL-VAR provides across the subshell. An `env:KEY=VALUE`
    # provides token is an ENVFILE fact (declared so `requires: env:…` can name its
    # producer via _find_producer, same as `mutates:`), NOT a shell variable — a bare
    # `declare -p "env:KEY=VALUE"` errors ("not found") and, under this subshell's
    # `set -e`, would redden the phase after its body already passed. Skip it, exactly
    # as the requires: validation special-cases `env:*`.
    for v in $provides; do
      case "$v" in env:*) continue ;; esac
      declare -p "$v"
    done > "$RUNROOT/phase.env.next"
    # The declared handoff / handoff-holds variables cross the same way, so the driver's
    # top-level shell can read them for the boundary. An unset name clears the parent's
    # inherited value, and a
    # name already in `provides` is not emitted twice.
    for v in $handoff $handoff_holds; do
      case "$v" in env:*) continue ;; esac
      case " $provides " in *" $v "*) continue ;; esac
      if [ -n "${!v+x}" ]; then declare -p "$v"; else printf 'unset %s\n' "$v"; fi
    done >> "$RUNROOT/phase.env.next"
  ) > "$RUNROOT/logs/$nn_slug.log" 2>&1
  rc=$?
  set -e
  cat "$RUNROOT/logs/$nn_slug.log"
  secs=$((SECONDS - start))

  if [ "$rc" -eq 0 ]; then
    # 6. provides round-trip — AT TOP LEVEL so the new vars (and the declared handoff
    #    vars) land in this shell and are inherited by the next phase's subshell.
    # shellcheck disable=SC1090  # generated declare-file, path is a runtime var
    source "$RUNROOT/phase.env.next"
    msg=""
  else
    msg="$(_strip_ansi < "$RUNROOT/logs/$nn_slug.log" | grep 'FAIL ' | tail -1 \
             | sed 's/^[[:space:]]*FAIL[[:space:]]*//')" || true
    # Empty-FAIL-message fallback (PRD #966 M3): a phase that died on a BARE command
    # under `set -e` (no `fail()` call) leaves no `FAIL ` line, so `msg` is empty and
    # results.tsv/junit would carry an empty <failure> message. Fall back to the last
    # non-empty line of the phase log, clearly marked as not a real fail message.
    if [ -z "$msg" ]; then
      last="$(_strip_ansi < "$RUNROOT/logs/$nn_slug.log" | grep -v '^[[:space:]]*$' | tail -1)" || true
      msg="(no fail message; last output: $last)"
    fi
    _requires_intersects_failed "$requires" && msg="$msg [suspect_cascade]"
    [ "$racesens" = yes ] && msg="$msg — possible race, see margins"
  fi

  # 9. End-of-phase boundary (after the body ran, and — on PASS — after the provides were
  #    sourced, so declared handoff vars are readable). Its own seconds are recorded apart.
  _boundary_end "$nn_slug" "$slug" "$handoff" "$handoff_holds" "$rc"
  _bw_slugs+=("$slug"); _bw_start+=("$_B_START_SECS"); _bw_end+=("$_B_END_SECS")
  _bw_total+=("$((_B_START_SECS + _B_END_SECS))")
  printf '%s\t%s\t%s\t%s\n' "$slug" "$_B_START_SECS" "$_B_END_SECS" "$((_B_START_SECS + _B_END_SECS))" \
    >> "$RUNROOT/boundary_waits.tsv"

  if [ "$rc" -eq 0 ] && [ "$_B_FAILED" -eq 0 ]; then
    _record "$slug" PASS "$secs" "$_B_NOTE" "$title"
  else
    if [ "$rc" -eq 0 ]; then
      fmsg="$_B_MESSAGE"
    elif [ "$_B_FAILED" -eq 1 ]; then
      fmsg="$msg; $_B_MESSAGE"
    else
      fmsg="$msg${_B_NOTE:+; $_B_NOTE}"
    fi
    _record "$slug" FAIL "$secs" "$fmsg" "$title"
    # Window = body seconds + the boundary's own seconds (the evidence spans all of it).
    _capture_artifacts "$nn_slug" "$((secs + _B_START_SECS + _B_END_SECS))"
    if [ "$_B_FAILED" -eq 1 ]; then
      _boundary_publish "$nn_slug"
      _contaminated="$_contaminated $slug"
    fi
    _note_failed "$provides"
    # A critical phase stops the suite only on a BODY failure; a failed boundary
    # contaminates later rows but they still run.
    if [ "$rc" -ne 0 ] && [ "$critical" = yes ]; then CRITICAL_FAILED=1; fi
  fi
done

# 10. Results — write the artifacts, then PRINT summary.md to stdout BEFORE we
#     return, so the cleanup trap's teardown cannot hide it from a log reader.
_write_junit
_write_summary
cat "$RUNROOT/summary.md"
_rollcall

# 11. Exit code: 1 iff any FAIL, else 0.
_exit=0
for ((_i = 0; _i < ${#_statuses[@]}; _i++)); do
  case "${_statuses[$_i]}" in
    FAIL) _exit=1 ;;
  esac
done
exit "$_exit"
