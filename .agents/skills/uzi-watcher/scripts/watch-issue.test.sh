#!/usr/bin/env bash
# Hermetic discovery and handoff checks; no live uzi calls, clocks or production waits.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
export WORK
trap 'rm -rf "$WORK"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
mkdir -p "$WORK/bin"
cp "${WATCH_ISSUE_SCRIPT:-$HERE/watch-issue.sh}" "$WORK/watch-issue.sh"
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
case "$*" in
  'run list --json')
    n=$(cat "$WORK/polls")
    echo $((n + 1)) > "$WORK/polls"
    echo 'uzi: CLI is behind server' >&2
    if [ -f "$WORK/list-$n" ]; then cat "$WORK/list-$n"; else cat "$WORK/fallback"; fi
    [ ! -f "$WORK/fail-list" ] ;;
  "run get "*" --field status") cat "$WORK/status" ;;
  *) echo "unexpected uzi call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/uzi"

R1=11111111-1111-4111-8111-111111111111
R2=22222222-2222-4222-8222-222222222222
P1=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
P2=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
BASE=1700000000.5
OLD=2023-11-14T22:13:20.499999Z
NEW=2023-11-14T22:13:20.500001Z
DEFAULT_STOP=completed,failed,cancelled,awaiting_approval,awaiting_input,paused
reset() {
  rm -f "$WORK"/list-* "$WORK/handoff" "$WORK/fail-list"
  printf '0\n' > "$WORK/polls"
  printf '[]\n' > "$WORK/fallback"
  cat > "$WORK/watch-run.sh" <<'WATCH'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$WORK/handoff"
printf 'RESULT=stop:completed\n'
WATCH
}
row() {
  jq -nc --arg id "$1" --arg repo "$2" --arg path "$3" --arg at "$4" --arg status "$5" --argjson issue "${6:-3584}" \
    '{id:$id,repo_id:$repo,repo_path:$path,created_at:$at,status:$status,issue_iid:$issue}'
}
list() { jq -sc .; }
run() {
  rc=0
  out=$(PATH="$WORK/bin:$PATH" WATCH_ATTENTION_POLLS=0 bash "$WORK/watch-issue.sh" "$@" 2>"$WORK/stderr") || rc=$?
}
handed() {
  [ "$rc" -eq 0 ] || fail "$1: exit $rc: $out"
  [ -f "$WORK/handoff" ] || fail "$1: no handoff: $out"
  [ "$(head -n1 "$WORK/handoff")" = "$2" ] || fail "$1: wrong run: $out"
  case "$out" in *"RUN=$2"*"RESULT=stop:completed"*) ;; *) fail "$1: missing handoff result: $out";; esac
}
elapsed() {
  [ "$rc" -eq 3 ] || fail "$1: expected elapsed, exit $rc: $out"
  [ ! -f "$WORK/handoff" ] || fail "$1: unexpected handoff"
  case "$out" in *'RESULT=elapsed'*) ;; *) fail "$1: missing elapsed result";; esac
}

# New run appears on poll 3: preserve stop set, interval and the remaining budget,
# with no plan/question watermark. CLI stderr skew must not contaminate JSON.
reset
row "$R1" "$P1" g/r "$NEW" queued | list > "$WORK/list-2"
run --repo g/r --since "$BASE" 3584 awaiting_input 0 4
handed appears "$R1"
expected=$(printf '%s\n' "$R1" awaiting_input 0 2)
[ "$(cat "$WORK/handoff")" = "$expected" ] || fail 'handoff arguments/budget/watermark'

# Baseline includes fractional seconds: ignore an older run in the SAME second.
reset
{ row "$R1" "$P1" g/r "$OLD" running; row "$R2" "$P1" g/r "$NEW" queued; } | list > "$WORK/list-0"
run --repo g/r --since "$BASE" 3584 '' 0 2
handed older "$R2"
[ "$(awk 'NR==2' "$WORK/handoff")" = "$DEFAULT_STOP" ] || fail 'default stop set'

# Repository and issue selectors exclude unrelated runs. UUID selection works too.
reset
{ row "$R1" "$P2" elsewhere/r "$NEW" running; row "$R1" "$P1" g/r "$NEW" running 12; row "$R2" "$P1" g/r "$NEW" claimed; } | list > "$WORK/list-0"
run --repo "$P1" --since "$BASE" 3584 '' 0 2
handed selectors "$R2"

# Equivalent RFC3339 offsets preserve fractions rather than comparing strings.
reset
row "$R1" "$P1" g/r 2023-11-15T00:13:20.500001+02:00 running | list > "$WORK/list-0"
run --repo g/r --since "$BASE" 3584 '' 0 1
handed offset "$R1"

# Equal-to-baseline and whole-second timestamps are eligible too.
for at in 2023-11-14T22:13:20.500000Z 2023-11-14T22:13:21Z; do
  reset
  row "$R1" "$P1" g/r "$at" running | list > "$WORK/list-0"
  run --repo g/r --since "$BASE" 3584 '' 0 1
  handed timestamp "$R1"
done

# A new terminal run is still an outcome. Old terminal history is not a match.
for status in completed failed cancelled; do
  reset
  { row "$R1" "$P1" g/r "$OLD" "$status"; row "$R2" "$P1" g/r "$NEW" "$status"; } | list > "$WORK/list-0"
  run --repo g/r --since "$BASE" 3584 '' 0 1
  handed "$status" "$R2"
done
reset
row "$R1" "$P1" g/r "$OLD" completed | list > "$WORK/fallback"
run --repo g/r --since "$BASE" 3584 '' 0 2
elapsed old-terminal

# Explicit pre-filing baseline permits a run already present at watcher startup;
# default-now excludes it, but accepts a future timestamp without a test clock hook.
reset
row "$R1" "$P1" g/r "$NEW" awaiting_input | list > "$WORK/fallback"
run --repo g/r 3584 '' 0 1
elapsed default-baseline
run --repo g/r --since "$BASE" 3584 '' 0 1
handed pre-filing "$R1"
reset
future=$(jq -nr 'now + 3600 | todateiso8601')
row "$R1" "$P1" g/r "$future" running | list > "$WORK/fallback"
run --repo g/r 3584 '' 0 1
handed default-now "$R1"

# An empty/invalid listing or failed CLI cannot bind a run; expiry is bounded.
reset
printf 'not JSON\n' > "$WORK/list-0"
run --repo g/r --since "$BASE" 3584 '' 0 3
elapsed malformed
[ "$(cat "$WORK/polls")" -eq 3 ] || fail 'discovery budget'
case "$out" in *'REARM='*'--since 1700000000.5'*'RESULT=elapsed'*) ;; *) fail 'rearm baseline missing';; esac
reset
row "$R1" "$P1" g/r "$NEW" running | list > "$WORK/fallback"
touch "$WORK/fail-list"
run --repo g/r --since "$BASE" 3584 '' 0 1
elapsed failed-cli

# The delegate's attention/error result and exit code must survive exec unchanged.
reset
row "$R1" "$P1" g/r "$NEW" running | list > "$WORK/fallback"
printf '#!/usr/bin/env bash\nprintf "RESULT=needs_attention\\n"\nexit 4\n' > "$WORK/watch-run.sh"
run --repo g/r --since "$BASE" 3584 '' 0 1
[ "$rc" -eq 4 ] || fail 'delegate exit code lost'
case "$out" in *'RESULT=needs_attention'*) ;; *) fail 'delegate result lost';; esac

# A path shared by different repository IDs needs explicit UUID disambiguation.
reset
{ row "$R1" "$P1" g/r "$NEW" running; row "$R2" "$P2" g/r "$NEW" running; } | list > "$WORK/list-0"
run --repo g/r --since "$BASE" 3584 '' 0 1
[ "$rc" -eq 2 ] && [ ! -f "$WORK/handoff" ] || fail 'ambiguous repository path'

# Validation happens before any CLI call; max=0 also expires without a request.
reset
run --since "$BASE" 3584 '' 0 1
[ "$rc" -eq 2 ] && [ "$(cat "$WORK/polls")" -eq 0 ] || fail 'required repo'
run --repo g/r --since invalid 3584 '' 0 1
[ "$rc" -eq 2 ] && [ "$(cat "$WORK/polls")" -eq 0 ] || fail 'invalid baseline'
run --repo g/r --since "$BASE" 3584 '' 0 0
elapsed zero-budget
[ "$(cat "$WORK/polls")" -eq 0 ] || fail 'zero budget queried CLI'

# Exercise actual delegation into watch-run, including an already parked run.
for status in awaiting_approval awaiting_input completed failed cancelled; do
  reset
  cp "$HERE/watch-run.sh" "$WORK/watch-run.sh"
  row "$R1" "$P1" g/r "$NEW" "$status" | list > "$WORK/fallback"
  printf '%s\n' "$status" > "$WORK/status"
  run --repo g/r --since "$BASE" 3584 '' 0 1
  [ "$rc" -eq 0 ] || fail "actual delegate $status: $out"
  case "$out" in *"RUN=$R1"*"STOP=$status"*"RESULT=stop:$status"*) ;; *) fail "actual delegate $status: $out";; esac
done
printf 'PASS: watch-issue discovery and delegation\n'
