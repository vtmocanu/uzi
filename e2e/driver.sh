#!/usr/bin/env bash
# e2e/driver.sh — PRD #966 M2: the phase-registry DRIVER semantics.
#
# Sourced by run-e2e.sh AT TOP LEVEL (in place of M1's inline `for f in phases;
# do source; done` loop + roll-call), and by e2e/driver.test.sh with fakes. It
# owns: header parse, lane filter, ONLY/SKIP selection, requires validation,
# the errexit-safe subshell-per-phase, provides round-trip, fail-soft + critical,
# end-of-phase quarantine, results (results.tsv / junit.xml / summary.md) and the
# roll-call. It ENDS WITH `exit`, which — because run-e2e.sh sources it last —
# drives the cleanup EXIT trap with the suite's exit code.
#
# DEPENDS (the caller must define these BEFORE sourcing; the hermetic test fakes
# them): functions  say pass fail db_psql_rows boundary_cancel
#         globals    ROOT RUNROOT ENVFILE FORGE EXECUTOR  (MARGINS_FILE optional)
#                    COMPOSE (array; used ONLY by on-red artifact capture, which is
#                    `|| true` throughout so a missing/short COMPOSE never reddens a run)
#
# KNOBS: PHASES_DIR (default $ROOT/e2e/phases), E2E_ONLY / E2E_SKIP (comma-lists
#        of slug globs), E2E_FAULT_PHASE.
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
_contamination=""; _entry_origins=""
CRITICAL_FAILED=0
_FAILED_PROVIDES=""   # space-padded list of provides tokens of phases that FAILed

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

# _record SLUG STATUS SECS MSG TITLE — append one row to the arrays and to
# results.tsv (tab/newline in the message flattened so the TSV stays 4 columns).
_record() {
  local m; m="$(printf '%s' "$4" | tr '\t\n' '  ')"
  [ -z "$_entry_origins" ] || m="$m [contaminated after: $_entry_origins]"
  printf '%s\t%s\n' "$1" "${_boundary_wait:-0}" >> "$RUNROOT/boundary_wait_seconds.tsv"
  _slugs+=("$1"); _statuses+=("$2"); _secs+=("$3"); _msgs+=("$m"); _titles+=("$5")
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$m" >> "$RUNROOT/results.tsv"
}

# _capture_artifacts NN_SLUG SECS — retain the first body/service capture on
# failure. Diagnostic operations have a two-second bound and cannot change the
# recorded status. Missing services before phase 13 are ordinary capture failures.
_capture_artifacts() {
  local nn_slug="$1" secs="${2:-0}" dir svc since limiter
  dir="$RUNROOT/artifacts/$nn_slug"
  touch "$RUNROOT/.keep-rundir" 2>/dev/null || true
  # First capture wins: a failed body may already have richer logs before the
  # boundary fails. Boundary evidence uses its own directory.
  [ -d "$dir" ] && return 0
  mkdir -p "$dir" 2>/dev/null || true
  since="$((secs + 30))s"
  limiter="$(command -v timeout || command -v gtimeout)" || limiter=""
  for svc in api agent forge-fake db; do
    if [ -z "$limiter" ]; then
      printf 'capture unavailable: no timeout/gtimeout limiter\n' > "$dir/$svc.log"; continue
    fi
    "$limiter" --signal=KILL 2 "${COMPOSE[@]}" logs --no-color --since "$since" "$svc" > "$dir/$svc.log" 2>&1 || true
  done
  db_psql_rows "select id,user_id,status,kind from runs order by created_at" 2 > "$dir/runs.txt" 2>/dev/null || true
  db_psql_rows "select status,kind,count(*) from runs group by status,kind order by status,kind" 2 \
    > "$dir/run-counts.txt" 2>/dev/null || true
  [ -f "$RUNROOT/logs/$nn_slug.log" ] && cp "$RUNROOT/logs/$nn_slug.log" "$dir/phase.log" 2>/dev/null || true
  return 0
}

# Boundary evidence lives separately from the first-wins body artifact capture.
# All settlement work shares one deadline; a failed read remains unverifiable even
# if a later sample is clean. Independent sets are read once per sample.
_boundary_error() {
  _boundary_bad=1
  _boundary_msg="${_boundary_msg:+$_boundary_msg; }$*"
  printf '%s\n' "$*" >> "$_boundary_dir/diagnostics"
}
_uuid() { [[ "$1" =~ ^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$ ]]; }
_boundary_remaining() { _remaining=$((_deadline - $(date +%s))); [ "$_remaining" -gt 0 ]; }
_boundary_read() {
  local label="$1" sql="$2" shape="$3" rc=0 line a b rest valid=1
  _read_data=""
  if ! _boundary_remaining; then
    _boundary_error "unverifiable $label: deadline exhausted"
    return 1
  fi
  db_psql_rows "$sql" "$_remaining" > "$_boundary_dir/$label.stdout" 2> "$_boundary_dir/$label.stderr" || rc=$?
  printf '%s\n' "$rc" > "$_boundary_dir/$label.exit"
  [ "$rc" -eq 0 ] || valid=0
  _read_data="$(cat "$_boundary_dir/$label.stdout")"
  if [ -s "$_boundary_dir/$label.stdout" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      IFS='|' read -r a b rest <<< "$line"
      if [ "$shape" = id ] || [ "$shape" = owner ]; then
        _uuid "$line" || valid=0
      else
        # Checking the whole line prevents read's trailing-empty-field ambiguity.
        if [[ ! "$line" =~ ^[^|]+\|[^|]+\|[0-9]+$ ]] || ! _uuid "$a" || ! _uuid "$b" || [[ ! "$rest" =~ ^[0-9]+$ ]]; then valid=0; fi
      fi
    done < "$_boundary_dir/$label.stdout"
  elif [ "$shape" = owner ]; then
    valid=0
  fi
  if [ "$shape" = owner ]; then _uuid "$_read_data" || valid=0; fi
  if [ "$valid" -ne 1 ]; then
    _boundary_error "unverifiable $label (exit $rc; invalid or unavailable rows)"
    _read_data=""
    return 1
  fi
}
_boundary_begin() {
  _boundary_dir="$RUNROOT/boundary/$1"
  mkdir -p "$_boundary_dir"
  _boundary_bad=0; _boundary_msg=""; _baseline_holds=""; _owner=""; _observed_holds=""
  _deadline=$(($(date +%s) + 120))
  local email="${ADMIN_EMAIL:-}"
  email="${email//\'/\'\'}"
  if _boundary_read owner "SELECT id FROM users WHERE email = '$email'" owner; then
    _owner="$_read_data"
    if _boundary_read baseline "SELECT id, run_id, generation FROM recovery_custody_holds WHERE user_id = '$_owner' AND state = 'open'" rows; then
      _baseline_holds="$_read_data"
      _observed_holds="$(printf '%s\n' "$_read_data" | cut -d'|' -f1)"
    fi
  fi
}
# Resolve declarations only after a successful top-level provides round-trip.
# A run handoff grants no custody hold permission.
_boundary_declarations() {
  local names="$1" kind="$2" name value id declaration count
  local tokens=()
  _declared=""
  # Header names are whitespace tokens, never shell pathname patterns.
  read -r -a tokens <<< "$names"
  for name in "${tokens[@]}"; do
    if [[ ! "$name" =~ ^[a-zA-Z_][a-zA-Z_0-9]*$ ]]; then
      _boundary_error "malformed $kind declaration $name"; continue
    fi
    # The snapshot was captured at top level, before _quarantine's locals.
    # A fresh Bash also keeps this helper's locals out of declaration lookup.
    if ! declaration="$(bash -c 'source "$1"; declare -p -- "$2"' bash "$RUNROOT/phase.handoff" "$name" 2>/dev/null)"; then
      _boundary_error "undefined $kind declaration $name"; continue
    fi
    case "$declaration" in
      "declare -a"*|"declare -A"*|"declare -n"*)
        _boundary_error "malformed $kind declaration $name: expected scalar IDs"; continue ;;
    esac
    if ! value="$(bash -c 'source "$1"; [ "${!2+x}" ] || exit 1; printf "%s" "${!2}"' bash "$RUNROOT/phase.handoff" "$name")"; then
      _boundary_error "unset $kind declaration $name"; continue
    fi
    count=0
    # read tokenizes whitespace without pathname expansion of malformed input.
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      count=$((count + 1))
      if _uuid "$id"; then _declared="$_declared $id"
      else _boundary_error "malformed $kind declaration $name: $id"; fi
    done < <(printf '%s\n' "$value" | tr '[:space:]' '\n')
    if [ "$count" -eq 0 ]; then _boundary_error "empty $kind declaration $name"; fi
  done
}
_quarantine() {
  local nn_slug="$1" slug="$2" handoff="$3" holds="$4" body_rc="$5"
  local entered now sample=0 runs workers open id run gen pending cancelled="" run_allow="" hold_allow="" baseline_ids
  entered="$(date +%s)"; _deadline=$((entered + 120))
  if [ "$body_rc" -eq 0 ]; then
    _boundary_declarations "$handoff" handoff; run_allow="$_declared"
    _boundary_declarations "$holds" handoff-holds; hold_allow="$_declared"
  elif [ -n "$handoff$holds" ]; then
    printf '%s\n' 'failed body: declarations unavailable; provides were not exported' >> "$_boundary_dir/diagnostics"
  fi
  baseline_ids="$(printf '%s\n' "$_baseline_holds" | cut -d'|' -f1 | tr '\n' ' ')"
  # At most 120 seconds, no per-run waits. Rejection/read failure is sticky;
  # sibling evidence is still sampled, but cannot repair the failure.
  pending=""
  while :; do
    if ! _boundary_remaining; then
      _boundary_error "boundary timeout:$pending"; break
    fi
    sample=$((sample + 1)); pending=""
    _boundary_read "$sample-runs" "select id from runs where status not in ('completed','failed','cancelled')" id || true
    runs="$_read_data"
    _boundary_read "$sample-workers" "SELECT worker_id, run_id, claim_generation FROM worker_active_runs" rows || true
    workers="$_read_data"
    open=""
    if [ -n "$_owner" ]; then
      _boundary_read "$sample-holds" "SELECT id, run_id, generation FROM recovery_custody_holds WHERE user_id = '$_owner' AND state = 'open'" rows || true
      open="$_read_data"
    else _boundary_error 'unverifiable open holds: owner unavailable'; fi
    for id in $runs; do
      case " $run_allow " in *" $id "*) continue ;; esac
      pending="$pending run=$id"
      case " $cancelled " in *" $id "*) continue ;; esac
      cancelled="$cancelled $id"
      if _boundary_remaining; then
        if ! boundary_cancel "/api/runs/$id/inputs" "$_remaining" "$_boundary_dir/cancel-$id"; then
          _boundary_error "cancel rejected run=$id (see cancel response)"
        fi
      else _boundary_error "cancel deadline run=$id"; fi
    done
    if [ -n "$workers" ]; then
      while IFS='|' read -r id run gen; do
        case " $run_allow " in *" $run "*) continue ;; esac
        pending="$pending worker=$id run=$run generation=$gen"
      done <<< "$workers"
    fi
    if [ -n "$open" ]; then
      while IFS='|' read -r id run gen; do
        _observed_holds="$_observed_holds $id"
        case " $baseline_ids $hold_allow " in *" $id "*) continue ;; esac
        pending="$pending hold=$id run=$run generation=$gen"
      done <<< "$open"
    fi
    printf '%s\n' "$pending" > "$_boundary_dir/$sample-pending"
    for id in runs workers holds; do
      if [ "$sample" -eq 1 ] && [ -f "$_boundary_dir/$sample-$id.stdout" ]; then
        cp "$_boundary_dir/$sample-$id.stdout" "$_boundary_dir/initial-$id"
      fi
      if [ -f "$_boundary_dir/$sample-$id.stdout" ]; then
        cp "$_boundary_dir/$sample-$id.stdout" "$_boundary_dir/final-$id"
      fi
    done
    now="$(date +%s)"
    if [ "$_boundary_bad" -ne 0 ]; then break; fi
    if [ -z "$pending" ] && [ "$now" -lt "$_deadline" ]; then break; fi
    if [ "$now" -ge "$_deadline" ]; then
      _boundary_error "boundary timeout:$pending"; break
    fi
    sleep 1
  done
  _boundary_wait=$(( $(date +%s) - entered ))
  printf '%s\n' "$_boundary_wait" > "$_boundary_dir/boundary_wait_seconds"
  # Supplemental evidence is read-only and bounded independently to two seconds,
  # including after expiry. It is diagnostic only, never another settlement wait.
  local observed sql rc=0
  observed="$(printf '%s\n' $_observed_holds | sort -u)"
  if [ -n "$observed" ]; then
    sql="$(printf "'%s'," $observed)"; sql="${sql%,}"
    db_psql_rows "SELECT id, run_id, generation, state, final_disposition, release_evidence FROM recovery_custody_holds WHERE id IN ($sql)" 2 \
      > "$_boundary_dir/hold-history.stdout" 2> "$_boundary_dir/hold-history.stderr" || rc=$?
    printf '%s\n' "$rc" > "$_boundary_dir/hold-history.exit"
    for id in $observed; do
      if [ "$rc" -ne 0 ] || ! awk -F'|' -v id="$id" '$1==id && NF>=6{found=1} END{exit !found}' "$_boundary_dir/hold-history.stdout"; then
        printf 'hold history/release evidence unavailable: %s\n' "$id" >> "$_boundary_dir/diagnostics"
      fi
    done
  fi
  if [ "$_boundary_bad" -ne 0 ]; then
    _contamination="${_contamination:+$_contamination }$slug"
    touch "$RUNROOT/.keep-rundir"
  fi
  return 0
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
  local n=${#_slugs[@]} i
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
    printf '\n## boundary_wait_seconds\n\n'
    cat "$RUNROOT/boundary_wait_seconds.tsv"
    awk -F'\t' '{total += $2} END {printf "total\t%d\n", total}' "$RUNROOT/boundary_wait_seconds.tsv"
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
: > "$RUNROOT/boundary_wait_seconds.tsv"

for f in "$_PHASES_DIR"/[0-9][0-9]-*.sh; do
  [ -e "$f" ] || continue   # no-match glob guard (nullglob is not set)
  _boundary_wait=0
  _entry_origins="$_contamination"
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
    _record "$slug" FAIL 0 "$m" "$title"
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
  ) > "$RUNROOT/logs/$nn_slug.log" 2>&1
  rc=$?
  set -e
  cat "$RUNROOT/logs/$nn_slug.log"
  secs=$((SECONDS - start))

  if [ "$rc" -eq 0 ]; then
    # 6. provides round-trip — AT TOP LEVEL so the new vars land in this shell and
    #    are inherited by the next phase's subshell. Clear only provided shell vars:
    #    declare without an assignment otherwise retains a stale parent value.
    for v in $provides; do
      case "$v" in env:*) continue ;; esac
      unset -v -- "$v"
    done
    # shellcheck disable=SC1090  # generated declare-file, path is a runtime var
    source "$RUNROOT/phase.env.next"
    # Capture only requested declarations while no boundary helper locals exist.
    # declare -p preserves unset scalars and scalar/array attributes for validation.
    read -r -a _handoff_names <<< "$handoff $handoff_holds"
    : > "$RUNROOT/phase.handoff"
    if [ "${#_handoff_names[@]}" -gt 0 ]; then
      declare -p -- "${_handoff_names[@]}" > "$RUNROOT/phase.handoff" 2>/dev/null || true
    fi
    _phase_status=PASS; _phase_msg=""
  else
    _phase_msg="$(_strip_ansi < "$RUNROOT/logs/$nn_slug.log" | grep 'FAIL ' | tail -1 \
             | sed 's/^[[:space:]]*FAIL[[:space:]]*//')" || true
    # Empty-FAIL-message fallback (PRD #966 M3): a phase that died on a BARE command
    # under `set -e` (no `fail()` call) leaves no `FAIL ` line, so `_phase_msg` is empty and
    # results.tsv/junit would carry an empty <failure> message. Fall back to the last
    # non-empty line of the phase log, clearly marked as not a real fail message.
    if [ -z "$_phase_msg" ]; then
      last="$(_strip_ansi < "$RUNROOT/logs/$nn_slug.log" | grep -v '^[[:space:]]*$' | tail -1)" || true
      _phase_msg="(no fail message; last output: $last)"
    fi
    _requires_intersects_failed "$requires" && _phase_msg="$_phase_msg [suspect_cascade]"
    [ "$racesens" = yes ] && _phase_msg="$_phase_msg — possible race, see margins"
    _phase_status=FAIL
    _capture_artifacts "$nn_slug" "$secs"
    _note_failed "$provides"
    [ "$critical" = yes ] && CRITICAL_FAILED=1
  fi

  # 9. End-of-phase quarantine (after the body ran).
  _quarantine "$nn_slug" "$slug" "$handoff" "$handoff_holds" "$rc"
  secs=$((secs + _boundary_wait))
  if [ "$_boundary_bad" -ne 0 ]; then
    _phase_status=FAIL
    _phase_msg="${_phase_msg:+$_phase_msg; }boundary FAIL: $_boundary_msg"
    [ "$rc" -ne 0 ] || [ -z "$provides" ] || _phase_msg="$_phase_msg [provides suspect]"
    _note_failed "$provides"
    _capture_artifacts "$nn_slug" "$secs"
  fi
  _record "$slug" "$_phase_status" "$secs" "$_phase_msg" "$title"
done

# 10. Results — write the artifacts, then PRINT summary.md to stdout BEFORE we
#     return, so the cleanup trap's teardown cannot hide it from a log reader.
_write_junit
_write_summary
cat "$RUNROOT/summary.md"
_rollcall

# 11. Exit code: every failed body or boundary fails the suite.
_exit=0
for ((_i = 0; _i < ${#_statuses[@]}; _i++)); do
  case "${_statuses[$_i]}" in
    FAIL) _exit=1 ;;
  esac
done
exit "$_exit"
