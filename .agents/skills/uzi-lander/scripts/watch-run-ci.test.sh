#!/usr/bin/env bash
# Hermetic regressions for immutable-SHA validation/discovery and transient empty listings.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/watch-run-ci.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

FULL_SHA=3579b4f1869b848a53aaf7c941b86896cc5a9f3a
SHORT_SHA=${FULL_SHA:0:8}
BAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
FOLLOW_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
FINAL_SHA=cccccccccccccccccccccccccccccccccccccccc
export FULL_SHA SHORT_SHA BAD_SHA FOLLOW_SHA FINAL_SHA
mkdir -p "$WORK/bin"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
[ -z "${SLEEPS:-}" ] || printf '%s\n' "$1" >> "$SLEEPS"
exit 0
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$CALLS"
if [ "${1:-}" = repo ] && [ "${2:-}" = view ]; then
  printf 'test/repo\n'
  exit 0
fi
if [ "${1:-}" = api ]; then
  # Diagnostic quota reads are not commit-resolution attempts and must not advance
  # the transient-resolution fixture's counter.
  if [ "${2:-}" = rate_limit ]; then
    echo '{"resources":{"core":{"limit":5000,"remaining":10,"reset":2000000010},"graphql":{"limit":5000,"remaining":10,"reset":2000000020}}}'
    exit 0
  fi
  [ "$MODE" != missing ] || exit 1
  if [ "$MODE" = resolve-transient ]; then
    n=0; [ -f "$API_COUNT" ] && n=$(cat "$API_COUNT")
    n=$((n+1)); printf '%s' "$n" > "$API_COUNT"
    [ "$n" -gt 1 ] || exit 1
  fi
  case "${2:-}" in
    "repos/test/repo/commits/$FULL_SHA"|"repos/test/repo/commits/$SHORT_SHA") printf '%s\n' "$FULL_SHA" ;;
    repos/test/repo/commits/main|repos/test/repo/commits/release%2Fnext)
      case "$MODE" in
        follow-head-error) printf '%s\n' "$FOLLOW_SHA"; exit 1 ;;
        follow-invalid) printf 'not-a-sha\n' ;;
        follow-same) printf '%s\n' "$FULL_SHA" ;;
        follow-chain)
          n=0; [ -f "$API_COUNT" ] && n=$(cat "$API_COUNT")
          n=$((n+1)); printf '%s' "$n" > "$API_COUNT"
          if [ "$n" -eq 1 ]; then printf '%s\n' "$FOLLOW_SHA"
          else printf '%s\n' "$FINAL_SHA"; fi ;;
        *) printf '%s\n' "$FOLLOW_SHA" ;;
      esac ;;
    *) exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = run ] && [ "${2:-}" = list ]; then
  case "$MODE" in
    adv-partial)
      # A listing that prints a green push row and then fails: the row must never count.
      printf '501\tcompleted\tsuccess\tCI\tgating\n'
      exit 1 ;;
    adv-*)
      # JSON-list mode: honour the requested --limit (like GitHub's truncation), then run
      # the script's own --jq so the real projection and classification are exercised.
      args=("$@"); limit=""; jqarg=""; commit=""; i=0
      while [ "$i" -lt "${#args[@]}" ]; do
        case "${args[$i]}" in
          --limit) limit="${args[$((i+1))]}" ;;
          --jq) jqarg="${args[$((i+1))]}" ;;
          --commit) commit="${args[$((i+1))]}" ;;
        esac
        i=$((i+1))
      done
      f="$JSONDIR/$commit.json"
      if [ -f "$JSONDIR/$commit.1.json" ]; then
        # Per-call listings: <sha>.<n>.json is the n-th list call; past the last, empty.
        n=0; [ -f "$JSONDIR/listn" ] && n=$(cat "$JSONDIR/listn")
        n=$((n+1)); printf '%s' "$n" > "$JSONDIR/listn"
        f="$JSONDIR/$commit.$n.json"
      fi
      [ -f "$f" ] || f="$JSONDIR/empty.json"
      jq -c --argjson n "$limit" '.[:$n]' "$f" | jq -r "$jqarg"
      exit 0 ;;
  esac
  case "$MODE" in
    follow-*)
      case " $* " in
        *" --commit $FULL_SHA "*) printf '201\tcompleted\tcancelled\tCI\tgating\n' ;;
        *" --commit $FOLLOW_SHA "*)
          case "$MODE" in
            follow-chain) printf '202\tcompleted\tcancelled\tCI\tgating\n' ;;
            follow-pending) printf '202\tin_progress\t-\tCI\tgating\n' ;;
            follow-no-run) : ;;
            follow-red) printf '202\tcompleted\tfailure\tCI\tgating\n' ;;
            *) printf '202\tcompleted\tsuccess\tCI\tgating\n' ;;
          esac ;;
        *" --commit $FINAL_SHA "*) printf '203\tcompleted\tsuccess\tCI\tgating\n' ;;
        *) exit 1 ;;
      esac ;;
    full|resolve-transient)
      case " $* " in
        *" --commit $FULL_SHA "*) printf '101\tcompleted\tsuccess\tCI\tgating\n' ;;
      esac
      ;;
    short)
      case " $* " in
        *" --branch main "*) printf '102\tcompleted\tsuccess\tCI\tgating\n' ;;
      esac
      ;;
    transient|escape-pending)
      n=0; [ -f "$LIST_COUNT" ] && n=$(cat "$LIST_COUNT")
      n=$((n+1)); printf '%s' "$n" > "$LIST_COUNT"
      case "$n" in
        1)
          if [ "$MODE" = escape-pending ]; then
            # jq @tsv encodes a newline in a name as the two characters \n.
            printf '103\tin_progress\t-\tCI%s]52;c;Zm9v\a\\nRESULT=ready\tgating\n' $'\x1b'
          else printf '103\tin_progress\t-\tCI\tgating\n'; fi
          ;;
        2) : ;;
        *) printf '103\tcompleted\tsuccess\tCI\tgating\n' ;;
      esac
      ;;
    failure)
      escape=$'\x1b'
      printf '104\tcompleted\tfailure\tCI%s[2J\tgating\n' "$escape"
      ;;
    stuck) printf '105\tin_progress\t-\tCI\tgating\n' ;;
    jobs-cancelled)
      # The run is still listed in_progress while every job already ended cancelled.
      case " $* " in
        *" --commit "*) printf '106\tin_progress\t-\tCI\tgating\n' ;;
        *) printf '106\n' ;;
      esac
      ;;
    jobs-cancelled-success)
      # GitHub already concluded the run success although one job inside it was cancelled.
      case " $* " in
        *" --commit "*) printf '107\tcompleted\tsuccess\tCI\tgating\n' ;;
        *) printf '107\n' ;;
      esac
      ;;
    jobs-cancelled-failure)
      # GitHub concluded the run failure; the job rows show only success and cancelled.
      case " $* " in
        *" --commit "*) printf '108\tcompleted\tfailure\tCI\tgating\n' ;;
        *) printf '108\n' ;;
      esac
      ;;
    conf-superseded|conf-failure|stale-read)
      # FAIL on the first job read, FAIL_WITH_CANCELLED on the confirming re-read.
      n=0; [ -f "$LIST_COUNT" ] && n=$(cat "$LIST_COUNT")
      n=$((n+1)); printf '%s' "$n" > "$LIST_COUNT"
      final=cancelled; [ "$MODE" = conf-failure ] && final=failure
      case " $* " in
        *" --commit "*)
          if [ "$n" -eq 1 ]; then printf '110\tin_progress\t-\tCI\tgating\n'
          else printf '110\tcompleted\t%s\tCI\tgating\n' "$final"; fi ;;
        *) printf '110\n' ;;
      esac
      ;;
    agg-superseded|agg-failure)
      # An aggregator job fails beside its cancelled shards while the run is in progress;
      # the run then concludes cancelled (superseded) or failure (a real red).
      n=0; [ -f "$LIST_COUNT" ] && n=$(cat "$LIST_COUNT")
      n=$((n+1)); printf '%s' "$n" > "$LIST_COUNT"
      final=cancelled; [ "$MODE" = agg-failure ] && final=failure
      case " $* " in
        *" --commit "*)
          if [ "$n" -eq 1 ]; then printf '109\tin_progress\t-\tCI\tgating\n'
          else printf '109\tcompleted\t%s\tCI\tgating\n' "$final"; fi ;;
        *) printf '109\n' ;;
      esac
      ;;
    *) echo "unknown MODE=$MODE" >&2; exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = run ] && [ "${2:-}" = view ]; then
  if [ "${MODE#adv-}" != "$MODE" ]; then
    # A `--json` field list naming `event` gets run metadata: run 801 is a workflow_dispatch
    # run. Each listed field is printed in order, tab-joined, so a script that read the event
    # in run-ID mode would see workflow_dispatch in the position it asked for.
    prev=""; fields=""
    for a in "$@"; do
      [ "$prev" = --json ] && fields="$a"
      prev="$a"
    done
    case ",$fields," in
      *,event,*)
        row=""; sep=""
        IFS=, read -r -a flds <<< "$fields"
        for f in "${flds[@]}"; do
          case "$f" in
            event) v=workflow_dispatch ;;
            status) v=completed ;;
            conclusion) v=failure ;;
            *) v="" ;;
          esac
          row="$row$sep$v"; sep=$'\t'
        done
        printf '%s\n' "$row"
        exit 0 ;;
    esac
    # Explicit per-run job rows; a run without a fixture is a test bug, not a green run.
    id=""
    for a in "$@"; do
      case "$a" in ''|*[!0-9]*) ;; *) id="$a"; break ;; esac
    done
    [ -f "$JOBSDIR/$id.tsv" ] || { echo "no job fixture for run $id" >&2; exit 1; }
    cat "$JOBSDIR/$id.tsv"
    exit 0
  fi
  if [ "$MODE" = follow-pending ]; then
    printf 'in_progress\t\tCI\thttps://example.invalid/job/202\t202\n'
  elif [ "$MODE" = follow-red ]; then
    printf 'completed\tfailure\tCI\thttps://example.invalid/job/202\t202\n'
  elif [ "$MODE" = failure ]; then
    escape=$'\x1b'
    printf 'completed\tfailure\tlint%s[31m-repo\thttps://github.com/test/repo/actions/runs/104/job/999\t999\n' "$escape"
  elif [ "$MODE" = transient ] || [ "$MODE" = escape-pending ]; then
    n=0; [ -f "$VIEW_COUNT" ] && n=$(cat "$VIEW_COUNT")
    n=$((n+1)); printf '%s' "$n" > "$VIEW_COUNT"
    if [ "$n" -eq 1 ]; then printf 'in_progress\t\tCI\thttps://example.invalid/job/103\t103\n'
    else printf 'completed\tsuccess\tCI\thttps://example.invalid/job/103\t103\n'; fi
  elif [ "$MODE" = stuck ]; then
    printf 'in_progress\t\tCI\thttps://example.invalid/job/105\t105\n'
  elif [ "$MODE" = jobs-cancelled ] || [ "$MODE" = jobs-cancelled-success ] || [ "$MODE" = jobs-cancelled-failure ]; then
    case " $* " in
      *" --json status,conclusion "*)
        case "$MODE" in
          jobs-cancelled-success) printf 'completed\tsuccess\n' ;;
          jobs-cancelled-failure) printf 'completed\tfailure\n' ;;
          *) printf 'in_progress\t\n' ;;
        esac ;;
      *) printf 'completed\tsuccess\tlint\thttps://example.invalid/job/1061\t1061\n'
         printf 'completed\tcancelled\ttest\thttps://example.invalid/job/1062\t1062\n' ;;
    esac
  elif [ "$MODE" = conf-superseded ] || [ "$MODE" = conf-failure ] || [ "$MODE" = stale-read ]; then
    case " $* " in
      *" --json status,conclusion "*)
        n=0; [ -f "$VIEW_COUNT" ] && n=$(cat "$VIEW_COUNT")
        n=$((n+1)); printf '%s' "$n" > "$VIEW_COUNT"
        if [ "$MODE" = stale-read ]; then
          # An in-progress run still carrying an old non-empty conclusion.
          if [ "$n" -le 2 ]; then printf 'in_progress\tfailure\n'; else printf 'completed\tcancelled\n'; fi
        elif [ "$n" -eq 1 ]; then printf 'in_progress\t\n'
        elif [ "$MODE" = conf-failure ]; then printf 'completed\tfailure\n'
        else printf 'completed\tcancelled\n'; fi ;;
      *)
        j=0; [ -f "$JOB_COUNT" ] && j=$(cat "$JOB_COUNT")
        j=$((j+1)); printf '%s' "$j" > "$JOB_COUNT"
        if [ "$j" -eq 1 ] && [ "$MODE" != stale-read ]; then
          printf 'in_progress\t\ttest-agent-shard (1/2)\thttps://example.invalid/job/1101\t1101\n'
        else
          printf 'completed\tcancelled\ttest-agent-shard (1/2)\thttps://example.invalid/job/1101\t1101\n'
        fi
        printf 'completed\tfailure\ttest-agent\thttps://example.invalid/job/1103\t1103\n' ;;
    esac
  elif [ "$MODE" = agg-superseded ] || [ "$MODE" = agg-failure ]; then
    case " $* " in
      *" --json status,conclusion "*)
        n=0; [ -f "$VIEW_COUNT" ] && n=$(cat "$VIEW_COUNT")
        n=$((n+1)); printf '%s' "$n" > "$VIEW_COUNT"
        if [ "$n" -eq 1 ]; then printf 'in_progress\t\n'
        elif [ "$MODE" = agg-failure ]; then printf 'completed\tfailure\n'
        else printf 'completed\tcancelled\n'; fi ;;
      *) printf 'completed\tcancelled\ttest-agent-shard (1/2)\thttps://example.invalid/job/1091\t1091\n'
         printf 'completed\tcancelled\ttest-agent-shard (2/2)\thttps://example.invalid/job/1092\t1092\n'
         printf 'completed\tfailure\ttest-agent\thttps://example.invalid/job/1093\t1093\n' ;;
    esac
  else
    printf 'completed\tsuccess\tCI\thttps://example.invalid/job/%s\t%s\n' "${3:-run}" "${3:-0}"
  fi
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/sleep"
export PATH="$WORK/bin:$PATH"
export JSONDIR="$WORK/json" JOBSDIR="$WORK/jobs"
export CALLS="$WORK/calls" LIST_COUNT="$WORK/list-count" VIEW_COUNT="$WORK/view-count" API_COUNT="$WORK/api-count" JOB_COUNT="$WORK/job-count"

# Following supersession watches the current branch head without resetting the budget.
for m in follow-green follow-chain follow-pending follow-red follow-no-run follow-same follow-head-error follow-invalid; do
  MODE=$m; export MODE
  : > "$CALLS"; rm -f "$API_COUNT"
  set +e
  bash "$SCRIPT" --sha "$FULL_SHA" --follow --branch release/next --repo test/repo --interval 0 --max-ticks 3 > "$WORK/$m.out" 2>&1
  rc=$?
  set -e
  case "$m" in
    follow-green|follow-chain) want=0 ;;
    follow-pending|follow-same) want=2 ;;
    follow-red) want=1 ;;
    *) want=3 ;;
  esac
  [ "$rc" -eq "$want" ] || fail "$m: want exit $want, rc=$rc: $(cat "$WORK/$m.out")"
  if [ "$m" = follow-head-error ] || [ "$m" = follow-invalid ]; then
    if grep -q '^FOLLOW ' "$WORK/$m.out"; then fail "$m: followed an unreadable branch head"; fi
    continue
  fi
  grep -Fq 'api repos/test/repo/commits/release%2Fnext --jq .sha' "$CALLS" || fail "$m: branch ref not resolved safely"
  if [ "$m" = follow-same ]; then
    if grep -q '^FOLLOW ' "$WORK/$m.out"; then fail 'same head printed a switch'; fi
  else
    grep -Fxq "FOLLOW sha=$FULL_SHA -> $FOLLOW_SHA" "$WORK/$m.out" || fail "$m: missing first switch"
    grep -Fq -- "--branch release/next --commit $FOLLOW_SHA " "$CALLS" || fail "$m: lost branch/commit scope"
  fi
  case "$m" in
    follow-chain)
      grep -Fxq "FOLLOW sha=$FOLLOW_SHA -> $FINAL_SHA" "$WORK/$m.out" || fail 'missing second switch'
      grep -Fq "=== sha ${FINAL_SHA:0:8}: every gating run terminal" "$WORK/$m.out" || fail 'wrong final SHA verdict' ;;
    follow-green)
      grep -Fq "=== sha ${FOLLOW_SHA:0:8}: every gating run terminal" "$WORK/$m.out" || fail 'old SHA verdict leaked' ;;
    follow-pending|follow-same|follow-no-run)
      [ "$(grep -c '^run list ' "$CALLS")" -eq 3 ] || fail "$m: reset the tick budget" ;;
  esac
done

# Without --follow, supersession retains exit 4 and never resolves a branch head.
MODE=follow-green; export MODE
: > "$CALLS"
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 3 > "$WORK/no-follow.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 4 ] || fail "without --follow: want exit 4, rc=$rc"
if grep -Fq 'commits/main' "$CALLS"; then fail 'without --follow: resolved branch head'; fi

# --follow is valid only with --sha, never silently applied to a single-run watch.
for mode in branch run; do
  : > "$CALLS"
  set +e
  if [ "$mode" = branch ]; then
    bash "$SCRIPT" --branch main --follow --repo test/repo > "$WORK/invalid-follow.out" 2>&1
  else
    bash "$SCRIPT" 201 --follow --repo test/repo > "$WORK/invalid-follow.out" 2>&1
  fi
  rc=$?
  set -e
  [ "$rc" -eq 3 ] && [ ! -s "$CALLS" ] || fail "--follow $mode did not reject before GitHub"
done

: > "$CALLS"
MODE=full; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/full.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "full SHA was not found through --commit, rc=$rc: $(cat "$WORK/full.out")"
grep -Fq -- "api repos/test/repo/commits/$FULL_SHA --jq .sha" "$CALLS" \
  || fail "full SHA was not validated through the commits API"
grep -q -- "--commit $FULL_SHA" "$CALLS" || fail "full SHA did not use server-side --commit filtering"
grep -q -- '--branch main' "$CALLS" || fail "full SHA dropped the requested branch scope"

: > "$CALLS"
MODE=short; export MODE
set +e
bash "$SCRIPT" --sha "$SHORT_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/short.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "short SHA resolution failed, rc=$rc: $(cat "$WORK/short.out")"
grep -Fq -- "api repos/test/repo/commits/$SHORT_SHA --jq .sha" "$CALLS" \
  || fail "short SHA was not resolved through the commits API"
grep -q -- "--commit $FULL_SHA" "$CALLS" || fail "short SHA did not use its canonical full SHA with --commit"
if grep -Fq -- "--commit $SHORT_SHA " "$CALLS"; then fail "unresolved short SHA reached gh run list"; fi

: > "$CALLS"; rm -f "$API_COUNT"
MODE=resolve-transient; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/resolve-transient.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "one transient commit-resolution failure did not recover, rc=$rc: $(cat "$WORK/resolve-transient.out")"
[ "$(grep -c '^api repos/test/repo/commits/' "$CALLS")" -eq 2 ] || fail "commit resolution did not make exactly two attempts: $(cat "$CALLS")"
grep -Fq 'retrying in 2s' "$WORK/resolve-transient.out" \
  || fail "transient commit-resolution failure did not announce its retry: $(cat "$WORK/resolve-transient.out")"
grep -q '^run list ' "$CALLS" || fail "resolved SHA never entered the polling loop"
[ "$(grep -c '^api rate_limit$' "$CALLS")" -eq 1 ] || fail "transient failure missing its one quota diagnosis"

: > "$CALLS"
MODE=missing; export MODE
set +e
bash "$SCRIPT" --sha "$BAD_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/missing.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "unknown full SHA did not fail fast with exit 3, rc=$rc: $(cat "$WORK/missing.out")"
grep -Fq "did not resolve to a commit" "$WORK/missing.out" \
  || fail "unknown full SHA omitted the resolution error: $(cat "$WORK/missing.out")"
[ "$(grep -c '^api repos/test/repo/commits/' "$CALLS")" -eq 2 ] || fail "unknown full SHA did not exhaust exactly two resolution attempts: $(cat "$CALLS")"
if grep -q '^run list ' "$CALLS"; then fail "unknown full SHA entered the polling loop"; fi
[ "$(grep -c '^api rate_limit$' "$CALLS")" -eq 2 ] || fail "failed resolutions missing their quota diagnoses"

: > "$CALLS"
set +e
bash "$SCRIPT" --sha not-a-sha --repo test/repo --interval 0 --max-ticks 2 > "$WORK/invalid.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "non-hex SHA did not fail with exit 3, rc=$rc: $(cat "$WORK/invalid.out")"
[ ! -s "$CALLS" ] || fail "non-hex SHA called GitHub before local validation: $(cat "$CALLS")"

: > "$CALLS"; rm -f "$LIST_COUNT" "$VIEW_COUNT"
MODE=transient; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 4 > "$WORK/transient.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "pending/empty/green sequence did not recover, rc=$rc: $(cat "$WORK/transient.out")"
grep -q 'workflow listing temporarily empty after runs were seen' "$WORK/transient.out" \
  || fail "transient empty listing used misleading never-seen wording: $(cat "$WORK/transient.out")"
grep -Fxq '[tick 0] pending after 0s: CI' "$WORK/transient.out" \
  || fail "--sha pending tick printed no heartbeat naming the open workflow: $(cat "$WORK/transient.out")"

# --sha defaults to a 60s tick; an explicit --interval still wins.
: > "$CALLS"; rm -f "$LIST_COUNT" "$VIEW_COUNT"; : > "$WORK/sleeps"
MODE=transient; SLEEPS="$WORK/sleeps"; export MODE SLEEPS
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --max-ticks 4 > "$WORK/sha-default.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--sha default-interval run failed, rc=$rc: $(cat "$WORK/sha-default.out")"
[ -s "$WORK/sleeps" ] && [ -z "$(grep -vx 60 "$WORK/sleeps")" ] \
  || fail "--sha default interval is not 60s: $(cat "$WORK/sleeps")"

# Run-id mode keeps its 120s default and also heartbeats.
: > "$CALLS"; rm -f "$VIEW_COUNT"; : > "$WORK/sleeps"
set +e
bash "$SCRIPT" 103 --repo test/repo --max-ticks 3 > "$WORK/run-id.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "run-id pending/green sequence failed, rc=$rc: $(cat "$WORK/run-id.out")"
[ "$(cat "$WORK/sleeps")" = 120 ] || fail "run-id default interval is not 120s: $(cat "$WORK/sleeps")"
grep -Fxq '[tick 0] pending after 0s: run 103, 1 job(s) not completed' "$WORK/run-id.out" \
  || fail "run-id pending tick printed no heartbeat: $(cat "$WORK/run-id.out")"
unset SLEEPS

# An untrusted workflow name reaches the heartbeat as one line with no escape sequence,
# even under xpg_echo, which would turn its TSV-encoded \n back into a real newline.
: > "$CALLS"; rm -f "$LIST_COUNT" "$VIEW_COUNT"
MODE=escape-pending; export MODE
set +e
bash -O xpg_echo "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 4 > "$WORK/escape-pending.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "escape-pending sequence failed, rc=$rc: $(cat "$WORK/escape-pending.out")"
grep -Fq '[tick 0] pending after 0s: CI' "$WORK/escape-pending.out" \
  || fail "escaped workflow name produced no heartbeat: $(cat "$WORK/escape-pending.out")"
if LC_ALL=C grep -Fq $'\033' "$WORK/escape-pending.out"; then
  fail "untrusted workflow name in the heartbeat emitted a raw escape byte: $(cat -v "$WORK/escape-pending.out")"
fi
if grep -q '^RESULT=' "$WORK/escape-pending.out"; then
  fail "untrusted workflow name forged its own output line: $(cat -v "$WORK/escape-pending.out")"
fi

# Default tick limits: 80 with --sha, 40 otherwise (~80 min either way).
MODE=stuck; SLEEPS="$WORK/sleeps"; export MODE SLEEPS
: > "$WORK/sleeps"
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo > "$WORK/stuck-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "--sha stuck run did not time out with exit 2, rc=$rc: $(tail -1 "$WORK/stuck-sha.out")"
[ "$(wc -l < "$WORK/sleeps" | tr -d ' ')" -eq 80 ] || fail "--sha default max-ticks is not 80: $(wc -l < "$WORK/sleeps")"
: > "$WORK/sleeps"
set +e
bash "$SCRIPT" 105 --repo test/repo > "$WORK/stuck-run.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "run-id stuck run did not time out with exit 2, rc=$rc: $(tail -1 "$WORK/stuck-run.out")"
[ "$(wc -l < "$WORK/sleeps" | tr -d ' ')" -eq 40 ] || fail "run-id default max-ticks is not 40: $(wc -l < "$WORK/sleeps")"
unset SLEEPS

: > "$CALLS"
MODE=failure; export MODE
set +e
bash -O xpg_echo "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/failure.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "confirmed failed job did not exit 1, rc=$rc: $(cat "$WORK/failure.out")"
grep -Fq 'live log: gh api --allow-escape-sequences repos/test/repo/actions/jobs/999/logs' "$WORK/failure.out" \
  || fail "failed job omitted live-log command: $(cat "$WORK/failure.out")"
grep -Fq 'after run terminal: gh run view 104 --repo test/repo --job 999 --log-failed' "$WORK/failure.out" \
  || fail "failed job omitted terminal log command: $(cat "$WORK/failure.out")"
if LC_ALL=C grep -Fq $'\033' "$WORK/failure.out"; then
  fail "untrusted workflow/job name emitted a raw escape byte: $(cat "$WORK/failure.out")"
fi

: > "$CALLS"
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --interval 0 --max-ticks 2 > "$WORK/failure-derived.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "URL-derived failure case did not exit 1, rc=$rc: $(cat "$WORK/failure-derived.out")"
grep -Fq 'live log: gh api --allow-escape-sequences repos/test/repo/actions/jobs/999/logs' "$WORK/failure-derived.out" \
  || fail "URL-derived repo missing from live-log command: $(cat "$WORK/failure-derived.out")"
grep -Fq 'after run terminal: gh run view 104 --repo test/repo --job 999 --log-failed' "$WORK/failure-derived.out" \
  || fail "URL-derived repo missing from terminal command: $(cat "$WORK/failure-derived.out")"

# A run still listed in_progress whose jobs all ended, one cancelled, is superseded:
# --sha exits 4, never 0 ("none failed" green); --branch keeps re-resolving.
: > "$CALLS"
MODE=jobs-cancelled; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 4 ] || fail "--sha run with cancelled jobs did not report superseded (exit 4), rc=$rc: $(cat "$WORK/jobs-cancelled-sha.out")"
set +e
bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-branch.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "--branch run with cancelled jobs was not kept pending (exit 2), rc=$rc: $(cat "$WORK/jobs-cancelled-branch.out")"
grep -Fq 'jobs cancelled before the run concluded' "$WORK/jobs-cancelled-branch.out" \
  || fail "--branch cancelled-jobs tick printed no supersession line: $(cat "$WORK/jobs-cancelled-branch.out")"

# A run GitHub already concluded success keeps that verdict despite a cancelled job inside it.
MODE=jobs-cancelled-success; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-success-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--sha concluded-success run with a cancelled job was not green, rc=$rc: $(cat "$WORK/jobs-cancelled-success-sha.out")"
set +e
bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-success-branch.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--branch concluded-success run with a cancelled job was not green, rc=$rc: $(cat "$WORK/jobs-cancelled-success-branch.out")"

# A run GitHub already concluded failure stays a failure even when its job rows show only
# success and cancelled.
MODE=jobs-cancelled-failure; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-failure-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "--sha concluded-failure run with a cancelled job was not a failure, rc=$rc: $(cat "$WORK/jobs-cancelled-failure-sha.out")"
set +e
bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-failure-branch.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "--branch concluded-failure run with a cancelled job was not a failure, rc=$rc: $(cat "$WORK/jobs-cancelled-failure-branch.out")"

# An aggregator that fails because its shards were cancelled by concurrency must not
# exit 1 before the run concludes: a run concluding cancelled is superseded (exit 4), a run
# concluding failure stays a failure (exit 1).
for m in agg-superseded agg-failure; do
  MODE=$m; export MODE
  rm -f "$LIST_COUNT" "$VIEW_COUNT"
  set +e
  bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 3 > "$WORK/$m-sha.out" 2>&1
  rc=$?
  set -e
  want=4; [ "$m" = agg-failure ] && want=1
  [ "$rc" -eq "$want" ] || fail "--sha $m: want exit $want, rc=$rc: $(cat "$WORK/$m-sha.out")"
  grep -Fq 'waiting for the run' "$WORK/$m-sha.out" \
    || fail "--sha $m did not wait for the run conclusion: $(cat "$WORK/$m-sha.out")"
  rm -f "$LIST_COUNT" "$VIEW_COUNT"
  set +e
  bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 3 > "$WORK/$m-branch.out" 2>&1
  rc=$?
  set -e
  want=2; [ "$m" = agg-failure ] && want=1
  [ "$rc" -eq "$want" ] || fail "--branch $m: want exit $want, rc=$rc: $(cat "$WORK/$m-branch.out")"
  grep -Fq 'waiting for the run' "$WORK/$m-branch.out" \
    || fail "--branch $m did not wait for the run conclusion: $(cat "$WORK/$m-branch.out")"
done

# The confirming re-read can turn FAIL into FAIL_WITH_CANCELLED; that must wait for the
# run's conclusion too (cancelled = superseded, failure = red).
for m in conf-superseded conf-failure; do
  MODE=$m; export MODE
  for mode_args in "--sha $FULL_SHA" "--branch main"; do
    rm -f "$LIST_COUNT" "$VIEW_COUNT" "$JOB_COUNT"
    set +e
    # shellcheck disable=SC2086
    bash "$SCRIPT" $mode_args --repo test/repo --interval 0 --max-ticks 3 > "$WORK/$m.out" 2>&1
    rc=$?
    set -e
    case "$m:$mode_args" in
      conf-superseded:--sha*) want=4 ;;
      conf-superseded:*) want=2 ;;
      *) want=1 ;;
    esac
    [ "$rc" -eq "$want" ] || fail "$m ($mode_args): want exit $want, rc=$rc: $(cat "$WORK/$m.out")"
    grep -Fq 'waiting for the run' "$WORK/$m.out" \
      || fail "$m ($mode_args) confirmed a failure without waiting for the run: $(cat "$WORK/$m.out")"
  done
done

# --branch: an in-progress run carrying a stale non-empty conclusion is not concluded.
MODE=stale-read; export MODE
rm -f "$LIST_COUNT" "$VIEW_COUNT" "$JOB_COUNT"
set +e
bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 3 > "$WORK/stale-read.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "--branch stale conclusion on an in-progress run was taken as final, rc=$rc: $(cat "$WORK/stale-read.out")"
grep -Fq 'waiting for the run' "$WORK/stale-read.out" \
  || fail "--branch stale-read did not wait for the run: $(cat "$WORK/stale-read.out")"

# ---- event classification: schedule/workflow_dispatch runs are advisory ----------------
# The stub's JSON-list modes apply the requested --limit and then the script's own --jq,
# so the real projection, limit window and classification run end to end.
adv_reset() {
  rm -rf "$JSONDIR" "$JOBSDIR"; mkdir -p "$JSONDIR" "$JOBSDIR"
  printf '[]\n' > "$JSONDIR/empty.json"
  : > "$CALLS"
}
# adv_run ID STATUS CONCLUSION_JSON NAME EVENT_JSON  -> one run object on stdout
adv_run() {
  jq -nc --argjson id "$1" --arg st "$2" --argjson cc "$3" --arg wn "$4" --argjson ev "$5" \
    '{databaseId:$id,status:$st,conclusion:$cc,workflowName:$wn,event:$ev}'
}
# adv_jobs ID STATUS CONCLUSION  -> the run's single job row (CONCLUSION may be empty)
adv_jobs() {
  printf '%s\t%s\tCI\thttps://example.invalid/job/%s\t%s\n' "$2" "$3" "$1" "$1" > "$JOBSDIR/$1.tsv"
}
# adv_list SHA < run objects  -> the JSON list GitHub would return for that commit
adv_list() { jq -s . > "$JSONDIR/$1.json"; }
# adv_watch NAME ARGS...  -> runs the script, sets rc, output in $WORK/NAME.out
adv_watch() {
  _n="$1"; shift
  set +e
  bash "$SCRIPT" "$@" > "$WORK/$_n.out" 2>&1
  rc=$?
  set -e
}
adv_calls() { grep -c "$1" "$CALLS" || true; }
MODE=adv-json; export MODE

# Case 1: a green push run plus a schedule run with a confirmed failed job: exit 0, the
# failure is only reported.
adv_reset
{ adv_run 401 completed '"success"' CI '"push"'; adv_run 402 completed '"failure"' Nightly '"schedule"'; } | adv_list "$FULL_SHA"
adv_jobs 401 completed success; adv_jobs 402 completed failure
adv_watch adv1 --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 0 ] || fail "adv-case1-schedule-failure-not-gating: want exit 0, rc=$rc: $(cat "$WORK/adv1.out")"
grep -Fxq '  ADVISORY  failure  402  schedule  Nightly' "$WORK/adv1.out" \
  || fail "adv-case1-advisory-line-missing: $(cat "$WORK/adv1.out")"
grep -Fq 'every gating run terminal' "$WORK/adv1.out" || fail "adv-case1-green-summary-missing: $(cat "$WORK/adv1.out")"
# Case 4: the listing request carries the 51-row limit and the event field.
grep -F 'run list' "$CALLS" | grep -Fq -- '--limit 51 --json databaseId,status,conclusion,workflowName,event' \
  || fail "adv-case4-limit-or-json-fields: $(cat "$CALLS")"

# Case 2: a green push run plus a workflow_dispatch run still in progress: exit 0 within
# two ticks, and the advisory state is the status, not a stale conclusion.
adv_reset
{ adv_run 411 completed '"success"' CI '"push"'; adv_run 412 in_progress null Manual '"workflow_dispatch"'; } | adv_list "$FULL_SHA"
adv_jobs 411 completed success; adv_jobs 412 in_progress ""
adv_watch adv2 --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 0 ] || fail "adv-case2-dispatch-pending-not-gating: want exit 0, rc=$rc: $(cat "$WORK/adv2.out")"
grep -Fxq '  ADVISORY  in_progress  412  workflow_dispatch  Manual' "$WORK/adv2.out" \
  || fail "adv-case2-advisory-state-not-status: $(cat "$WORK/adv2.out")"

# Case 3: only a green schedule run never yields green: exit 3 with the ADVISORY block.
adv_reset
adv_run 421 completed '"success"' Nightly '"schedule"' | adv_list "$FULL_SHA"
adv_jobs 421 completed success
adv_watch adv3 --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 3 ] || fail "adv-case3-advisory-only-want-exit-3: rc=$rc: $(cat "$WORK/adv3.out")"
grep -Fq 'NO GATING RUN APPEARED' "$WORK/adv3.out" || fail "adv-case3-no-gating-line-missing: $(cat "$WORK/adv3.out")"
grep -Fq 'waiting for a gating run' "$WORK/adv3.out" || fail "adv-case3-waiting-line-missing: $(cat "$WORK/adv3.out")"
grep -Fxq 'ADVISORY (schedule/workflow_dispatch: reported, never gating) from tick 1' "$WORK/adv3.out" \
  || fail "adv-case3-advisory-header-missing: $(cat "$WORK/adv3.out")"
grep -Fxq '  ADVISORY  success  421  schedule  Nightly' "$WORK/adv3.out" || fail "adv-case3-advisory-line-missing: $(cat "$WORK/adv3.out")"
if grep -Fq 'none failed after' "$WORK/adv3.out"; then fail "adv-case3-advisory-only-went-green"; fi

# Case 5: a listing that prints a row and then fails is unreadable, never a partial verdict.
adv_reset
# A green job row for run 501 exists, so a watcher that consumed the partial row as a run
# would classify it and exit 0 (false green) instead of failing on a missing fixture.
adv_jobs 501 completed success
MODE=adv-partial; export MODE
adv_watch adv5 --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 3 ] || fail "adv-case5-partial-listing-want-exit-3: rc=$rc: $(cat "$WORK/adv5.out")"
grep -Fq "run listing failed for sha=$SHORT_SHA; retrying" "$WORK/adv5.out" || fail "adv-case5-retry-line-missing: $(cat "$WORK/adv5.out")"
if grep -Fq 'none failed after' "$WORK/adv5.out"; then fail "adv-case5-partial-rows-gave-verdict"; fi
[ "$(adv_calls '^run view ')" -eq 0 ] || fail "adv-case5-partial-rows-were-classified: $(cat "$CALLS")"
MODE=adv-json; export MODE

# Case 6: a full 51-row window yields no verdict even though row 1 is green and every
# visible advisory run is green: a failed push run sits at row 51.
adv_reset
{
  adv_run 501 completed '"success"' CI '"push"'
  n=502; while [ "$n" -le 550 ]; do
    ev='"schedule"'; [ $((n % 2)) -eq 0 ] || ev='"workflow_dispatch"'
    adv_run "$n" completed '"success"' "Adv$n" "$ev"; n=$((n+1))
  done
  adv_run 551 completed '"failure"' CI '"push"'
} | adv_list "$FULL_SHA"
n=501; while [ "$n" -le 550 ]; do adv_jobs "$n" completed success; n=$((n+1)); done
adv_jobs 551 completed failure
adv_watch adv6 --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 2 ] || fail "adv-case6-limit-window-want-exit-2: rc=$rc: $(tail -3 "$WORK/adv6.out")"
[ "$(grep -c '^\[tick [0-9]*\] run listing for sha=.* reached --limit 51 (may be truncated); no verdict this tick$' "$WORK/adv6.out" || true)" -eq 2 ] \
  || fail "adv-case6-want-exactly-2-limit-lines: $(cat "$WORK/adv6.out")"
[ "$(adv_calls '^run view ')" -eq 0 ] || fail "adv-case6-limit-tick-classified-runs: $(grep '^run view ' "$CALLS" | head -3)"
if grep -Fq 'none failed after' "$WORK/adv6.out"; then fail "adv-case6-limit-window-gave-green"; fi
grep -Fq "=== sha $SHORT_SHA: run listing reached --limit 51 on the last tick (may be truncated): no verdict; re-arming will not clear this" "$WORK/adv6.out" \
  || fail "adv-case6-closing-limit-line-missing: $(tail -3 "$WORK/adv6.out")"
grep -Fxq 'ADVISORY: none observed in a complete listing' "$WORK/adv6.out" || fail "adv-case6-advisory-cache-was-updated: $(tail -3 "$WORK/adv6.out")"

# Case 7: --follow clears the advisory cache on a SHA switch: A's advisory run must not
# be reported at the end of B's watch (a guard if the base already behaves this way).
adv_reset
{ adv_run 601 completed '"cancelled"' CI '"push"'; adv_run 602 completed '"success"' Nightly '"schedule"'; } | adv_list "$FULL_SHA"
adv_jobs 601 completed cancelled; adv_jobs 602 completed success
printf '[]\n' > "$JSONDIR/$FOLLOW_SHA.json"
adv_watch adv7 --sha "$FULL_SHA" --follow --branch release/next --repo test/repo --interval 0 --max-ticks 3
[ "$rc" -eq 3 ] || fail "adv-case7-follow-want-exit-3: rc=$rc: $(cat "$WORK/adv7.out")"
grep -Fxq "FOLLOW sha=$FULL_SHA -> $FOLLOW_SHA" "$WORK/adv7.out" || fail "adv-case7-no-follow-switch: $(cat "$WORK/adv7.out")"
if grep -Eq '^  ADVISORY  .* 602 ' "$WORK/adv7.out"; then fail "adv-case7-stale-advisory-after-switch: $(cat "$WORK/adv7.out")"; fi
grep -Fxq 'ADVISORY: none observed in a complete listing' "$WORK/adv7.out" || fail "adv-case7-advisory-not-cleared: $(cat "$WORK/adv7.out")"

# Case 8a: a confirmed-failed gating run beside an advisory run exits 1 and still prints
# the ADVISORY block.
adv_reset
{ adv_run 901 completed '"failure"' CI '"push"'; adv_run 902 completed '"success"' Nightly '"schedule"'; } | adv_list "$FULL_SHA"
adv_jobs 901 completed failure; adv_jobs 902 completed success
adv_watch adv8a --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 1 ] || fail "adv-case8a-failed-gating-want-exit-1: rc=$rc: $(cat "$WORK/adv8a.out")"
grep -Fxq '  ADVISORY  success  902  schedule  Nightly' "$WORK/adv8a.out" \
  || fail "adv-case8a-advisory-block-missing-before-exit-1: $(cat "$WORK/adv8a.out")"
# Case 8a2: the other exit-1 path, a concluded-failure run whose jobs show only a
# cancelled job, also prints the ADVISORY block.
adv_reset
{ adv_run 903 completed '"failure"' CI '"push"'; adv_run 904 completed '"success"' Nightly '"schedule"'; } | adv_list "$FULL_SHA"
adv_jobs 903 completed cancelled; adv_jobs 904 completed success
adv_watch adv8a2 --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 1 ] || fail "adv-case8a2-concluded-failure-want-exit-1: rc=$rc: $(cat "$WORK/adv8a2.out")"
grep -Fq 'concluded failure with a cancelled job (exit 1)' "$WORK/adv8a2.out" \
  || fail "adv-case8a2-wrong-exit-1-path: $(cat "$WORK/adv8a2.out")"
grep -Fxq '  ADVISORY  success  904  schedule  Nightly' "$WORK/adv8a2.out" \
  || fail "adv-case8a2-advisory-block-missing-before-exit-1: $(cat "$WORK/adv8a2.out")"

# Case 8b: supersession (a cancelled push run) beside an advisory run exits 4 and still
# prints the ADVISORY block.
adv_reset
{ adv_run 911 completed '"cancelled"' CI '"push"'; adv_run 912 completed '"success"' Nightly '"schedule"'; } | adv_list "$FULL_SHA"
adv_jobs 911 completed cancelled; adv_jobs 912 completed success
adv_watch adv8b --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 4 ] || fail "adv-case8b-superseded-want-exit-4: rc=$rc: $(cat "$WORK/adv8b.out")"
grep -Fxq '  ADVISORY  success  912  schedule  Nightly' "$WORK/adv8b.out" \
  || fail "adv-case8b-advisory-block-missing-before-exit-4: $(cat "$WORK/adv8b.out")"

# Case 8c: after an advisory-only tick, an empty listing prints the advisory-only waiting
# line, never "no run yet".
adv_reset
adv_run 921 completed '"success"' Nightly '"schedule"' | adv_list "$FULL_SHA.1"
adv_jobs 921 completed success
adv_watch adv8c --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 3
[ "$rc" -eq 3 ] || fail "adv-case8c-want-exit-3: rc=$rc: $(cat "$WORK/adv8c.out")"
grep -Fxq "[tick 1] only advisory (schedule/workflow_dispatch) runs seen for sha=$SHORT_SHA; waiting for a gating run" "$WORK/adv8c.out" \
  || fail "adv-case8c-empty-tick-lacks-advisory-only-line: $(cat "$WORK/adv8c.out")"
if grep -Fq 'no run yet' "$WORK/adv8c.out"; then fail "adv-case8c-empty-tick-said-no-run-yet: $(cat "$WORK/adv8c.out")"; fi

# Case 8d: a limit tick resets the consecutive gh run view failure count. Tick 0 and tick 2
# list one gating run whose job view fails (no job fixture); tick 1 hits the 51-row limit.
# Without the reset the second failure is "consecutive failure 2" and the watch exits 3.
adv_reset
adv_run 931 completed '"success"' CI '"push"' | adv_list "$FULL_SHA.1"
{
  n=941; while [ "$n" -le 991 ]; do adv_run "$n" completed '"success"' "Adv$n" '"schedule"'; n=$((n+1)); done
} | adv_list "$FULL_SHA.2"
adv_run 931 completed '"success"' CI '"push"' | adv_list "$FULL_SHA.3"
adv_watch adv8d --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 3
[ "$rc" -eq 2 ] || fail "adv-case8d-limit-tick-must-reset-gh-errs-want-exit-2: rc=$rc: $(cat "$WORK/adv8d.out")"
if grep -Fq 'gh keeps failing' "$WORK/adv8d.out"; then fail "adv-case8d-gh-errs-not-reset-on-limit-tick: $(cat "$WORK/adv8d.out")"; fi
[ "$(grep -c 'consecutive failure 1' "$WORK/adv8d.out" || true)" -eq 2 ] \
  || fail "adv-case8d-want-two-first-failures: $(cat "$WORK/adv8d.out")"

# Controls: a run whose event is absent, unknown, an object or malformed stays gating.
for ev in 'null' '"repository_dispatch"' '{"x":1}' '"\u0000schedule"'; do
  adv_reset
  adv_run 701 completed '"failure"' CI "$ev" | adv_list "$FULL_SHA"
  adv_jobs 701 completed failure
  adv_watch adv-ctl-ev --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
  [ "$rc" -eq 1 ] || fail "adv-control-event-$ev-stays-gating: want exit 1, rc=$rc: $(cat "$WORK/adv-ctl-ev.out")"
done
adv_reset
adv_run 711 completed '"failure"' CI '"push"' | adv_list "$FULL_SHA"; adv_jobs 711 completed failure
adv_watch adv-ctl-push-fail --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 1 ] || fail "adv-control-failed-push-want-exit-1: rc=$rc: $(cat "$WORK/adv-ctl-push-fail.out")"
adv_reset
adv_run 721 completed '"cancelled"' CI '"push"' | adv_list "$FULL_SHA"; adv_jobs 721 completed cancelled
adv_watch adv-ctl-cancel --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 4 ] || fail "adv-control-cancelled-push-want-exit-4: rc=$rc: $(cat "$WORK/adv-ctl-cancel.out")"
grep -Fqx 'ADVISORY: none observed in a complete listing' "$WORK/adv-ctl-cancel.out" || fail "adv-control-cancel-empty-advisory-line-missing-before-exit-4"
# 50 green push runs stay under the 51-row window: unchanged exit 0.
adv_reset
n=800; while [ "$n" -lt 850 ]; do adv_run "$n" completed '"success"' CI '"push"'; adv_jobs "$n" completed success; n=$((n+1)); done | adv_list "$FULL_SHA"
adv_watch adv-ctl-fifty --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 0 ] || fail "adv-control-fifty-green-push-want-exit-0: rc=$rc: $(tail -3 "$WORK/adv-ctl-fifty.out")"
grep -Fqx 'ADVISORY: none observed in a complete listing' "$WORK/adv-ctl-fifty.out" || fail "adv-control-fifty-empty-advisory-line-missing-before-exit-0"
# Run-ID mode is untouched: a failed workflow_dispatch run watched by ID is red. The stub
# reports run 801 as workflow_dispatch to any `run view --json ...event...`, so a regression
# that read the event here and treated dispatch as advisory would not exit 1.
adv_reset
adv_jobs 801 completed failure
adv_watch adv-ctl-runid 801 --repo test/repo --interval 0 --max-ticks 2
[ "$rc" -eq 1 ] || fail "adv-control-runid-dispatch-failure-want-exit-1: rc=$rc: $(cat "$WORK/adv-ctl-runid.out")"

echo "PASS watch-run-ci: bounded SHA following, final-head verdicts, SHA validation/retry/canonicalization, transient empty recovery, pending heartbeat (xpg_echo-safe), default intervals and tick limits, live failed-job logs, cancelled jobs never green, aggregator red beside cancelled shards waits for the run verdict"
