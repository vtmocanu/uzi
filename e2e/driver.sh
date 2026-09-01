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
# them): functions  say pass fail db_psql apipost apipost_code wait_status
#         globals    ROOT RUNROOT ENVFILE FORGE EXECUTOR  (MARGINS_FILE optional)
#
# KNOBS: PHASES_DIR (default $ROOT/e2e/phases), E2E_ONLY / E2E_SKIP (comma-lists
#        of slug globs), E2E_STRICT_LEAKS, E2E_FAULT_PHASE.
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
  _slugs+=("$1"); _statuses+=("$2"); _secs+=("$3"); _msgs+=("$m"); _titles+=("$5")
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$m" >> "$RUNROOT/results.tsv"
}

# _quarantine SLUG HANDOFF — end-of-phase LEAK sweep. Enumerate non-terminal runs
# across ALL users; any id not held by a variable named in HANDOFF is a LEAK:
# cancel via the API, fall back to a DB update on a non-2xx, then confirm it
# reaches `cancelled`. Non-fatal by default; contributes to exit 1 under strict.
# Runs only when db_psql is usable (a failing db_psql short-circuits to a no-op).
_quarantine() {
  local slug="$1" handoff="$2" ids id name v held code forced
  ids="$(db_psql "select id from runs where status not in ('completed','failed','cancelled')" 2>/dev/null)" || return 0
  [ -n "$ids" ] || return 0
  held=""
  for name in $handoff; do
    v="${!name:-}"
    [ -n "$v" ] && held="$held $v"
  done
  for id in $ids; do
    [ -n "$id" ] || continue
    case " $held " in *" $id "*) continue ;; esac   # declared handoff — not a leak
    forced=""
    # apipost_code both POSTs the cancel and hands back the HTTP status (owner-scoped,
    # so a 4xx here is expected for a run another session created).
    code="$(apipost_code "/api/runs/$id/inputs" '{"kind":"cancel","body":""}')" || code="000"
    case "$code" in
      2*) ;;
      *) db_psql "update runs set status='cancelled' where id='$id'" >/dev/null 2>&1 || true
         forced=" (api cancel refused, forced via DB)" ;;
    esac
    wait_status "$id" cancelled 10 || true
    _record "$slug" LEAK 0 "leaked run $id cancelled${forced}" "$slug (quarantine)"
  done
}

# --- results writers ---------------------------------------------------------

_write_junit() {
  local n=${#_slugs[@]} fails=0 skips=0 i slug status secs msg
  for ((i = 0; i < n; i++)); do
    case "${_statuses[$i]}" in
      FAIL) fails=$((fails + 1)) ;;
      SKIP) skips=$((skips + 1)) ;;
      LEAK) [ "${E2E_STRICT_LEAKS:-}" = 1 ] && fails=$((fails + 1)) ;;
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
        LEAK)
          # A LEAK is a <failure> ONLY under strict; otherwise a non-fatal note, so a
          # green-but-leaky run still parses as a passing suite (D4).
          if [ "${E2E_STRICT_LEAKS:-}" = 1 ]; then
            printf '  <testcase name="%s" time="%s"><failure message="%s"/></testcase>\n' "$slug" "$secs" "$msg"
          else
            printf '  <testcase name="%s" time="%s"><system-out>%s</system-out></testcase>\n' "$slug" "$secs" "$msg"
          fi ;;
        *)
          printf '  <testcase name="%s" time="%s"/>\n' "$slug" "$secs" ;;
      esac
    done
    printf '</testsuite>\n'
  } > "$RUNROOT/junit.xml"
}

_write_summary() {
  local n=${#_slugs[@]} i any=0
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
    printf '\n## Leaks\n\n'
    for ((i = 0; i < n; i++)); do
      [ "${_statuses[$i]}" = LEAK ] && { printf '- %s\n' "${_msgs[$i]}"; any=1; }
    done
    [ "$any" = 0 ] && printf '_(none)_\n'
  } > "$RUNROOT/summary.md"
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
    _record "$slug" FAIL 0 "$m" "$title"
    _note_failed "$provides"
    [ "$critical" = yes ] && CRITICAL_FAILED=1
    continue
  fi

  # 5. Per-phase execution — THE ERREXIT-SAFE SHAPE. Never `( … ) || rc=$?` and
  #    never `if ( … )`: both inherit errexit-off, so a bare failing command in
  #    the phase body would read PASS. Guard against a stale phase.env.next first.
  printf '[phase] %s\n' "$nn_slug"
  : > "$RUNROOT/phase.env.next"
  start=$SECONDS
  set +e
  # shellcheck disable=SC1090  # $f is a runtime phase path, not statically resolvable
  ( set -euo pipefail
    [ "${E2E_FAULT_PHASE:-}" = "$slug" ] && fail "injected fault: $slug"
    source "$f"
    for v in $provides; do declare -p "$v"; done > "$RUNROOT/phase.env.next"
  ) > "$RUNROOT/logs/$nn_slug.log" 2>&1
  rc=$?
  set -e
  cat "$RUNROOT/logs/$nn_slug.log"
  secs=$((SECONDS - start))

  if [ "$rc" -eq 0 ]; then
    _record "$slug" PASS "$secs" "" "$title"
    # 6. provides round-trip — AT TOP LEVEL so the new vars land in this shell and
    #    are inherited by the next phase's subshell.
    # shellcheck disable=SC1090  # generated declare-file, path is a runtime var
    source "$RUNROOT/phase.env.next"
  else
    msg="$(_strip_ansi < "$RUNROOT/logs/$nn_slug.log" | grep 'FAIL ' | tail -1 \
             | sed 's/^[[:space:]]*FAIL[[:space:]]*//')" || true
    _requires_intersects_failed "$requires" && msg="$msg [suspect_cascade]"
    _record "$slug" FAIL "$secs" "$msg" "$title"
    _note_failed "$provides"
    [ "$critical" = yes ] && CRITICAL_FAILED=1
  fi

  # 9. End-of-phase quarantine (after the body ran).
  _quarantine "$slug" "$handoff"
done

# 10. Results — write the artifacts, then PRINT summary.md to stdout BEFORE we
#     return, so the cleanup trap's teardown cannot hide it from a log reader.
_write_junit
_write_summary
cat "$RUNROOT/summary.md"
_rollcall

# 11. Exit code: 1 iff any FAIL (or any LEAK under strict), else 0.
_exit=0
for ((_i = 0; _i < ${#_statuses[@]}; _i++)); do
  case "${_statuses[$_i]}" in
    FAIL) _exit=1 ;;
    LEAK) [ "${E2E_STRICT_LEAKS:-}" = 1 ] && _exit=1 ;;
  esac
done
exit "$_exit"
