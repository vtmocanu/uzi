#!/usr/bin/env bash
# watch-run-ci.sh — poll GitHub Actions at JOB level and surface a failed job the moment
# it appears, instead of waiting for the whole run to conclude.
#
# Three targets, one classifier:
#   <run-id>        one specific run (a release commit's ci.yml, a rerun).
#   --branch NAME   the LATEST run of --workflow on a branch, re-resolved each tick so a
#                   run minted by a concurrent push is picked up (release flow).
#   --sha SHA       EVERY workflow run for one commit on --branch (default main): the
#                   post-merge watch, pinned to the merge SHA so a supersession by a newer
#                   push is reported as such (exit 4), not as red. SHA is 7-40 hex digits;
#                   it is resolved through GitHub before polling, so a typo fails fast.
#
# Usage:
#   watch-run-ci.sh <run-id> [--interval SECS] [--max-ticks N] [--repo OWNER/REPO]
#   watch-run-ci.sh --branch main [--workflow ci.yml] [--interval SECS] [--max-ticks N]
#   watch-run-ci.sh --sha <sha> [--branch main] [--interval SECS] [--max-ticks N]
#
#   --interval     seconds between polls (default 120; CI jobs are minutes-long, so a
#                  tighter cadence just burns API calls; 60 is plenty for --sha).
#   --max-ticks    give up after N polls (default 40 -> ~80 min at 120s).
#   --repo         OWNER/REPO (default: inferred by gh from the checkout).
#
# Run it in the BACKGROUND (the harness re-invokes you when it exits); do not block a
# foreground turn on it. Polling the whole-run `status` and reacting only at `completed`
# is the bug this replaces: a long job (test-api-store-it) keeps the run in_progress for
# minutes after a fast job (validate-api) has already gone red.
#
# Exit codes (callers branch on these; keep them stable):
#   0  every job terminal and none failed (green). In --sha mode: every run that EXISTS
#      for the SHA. A docs-only or skip-ci-adjacent push can dispatch a PARTIAL workflow
#      set (measured 2026-09-02, f015f1f: only CodeQL ran), so a green here does not prove
#      validate-* ran; confirm a gate fix another way (run the gate locally, or wait for
#      the next real code-change dispatch).
#   1  a job completed with a failing conclusion (name + url printed) — react now
#   2  timed out: still pending after --max-ticks
#   3  usage / gh error; in --sha mode also an invalid/unresolvable SHA or "no run ever
#      appeared for the SHA" (a push to main sometimes spawns none: re-point at a descendant
#      commit rather than waiting)
#   4  --sha mode only: the SHA's runs were cancelled by concurrency (a newer commit
#      superseded it) and nothing failed. `git fetch origin main` and re-watch the CURRENT
#      head, whose run exercises this change plus the newer one. Not a failure.
#
# Design notes:
#  - Parses `gh run view --json jobs` with jq, then classifies with awk -F'\t' (this
#    host's grep is ugrep, whose POSIX modes mishandle negated classes and brace
#    intervals — see repo CLAUDE.md — so load-bearing field parsing uses awk, never a
#    grep pattern).
#  - A job is FAILURE only when status==completed AND conclusion is a failure state.
#    `gh run view --json jobs` can briefly report an in_progress job with a non-null
#    (often failure/cancelled) conclusion right after a run starts (the stale-first-tick
#    read), so gating on status==completed filters that, and a failure is still CONFIRMED
#    with one immediate re-query before exiting.
#  - FAILURE conclusions: failure timed_out action_required startup_failure, plus
#    cancelled in run-id mode (you asked to watch that specific run). In --branch and
#    --sha mode a cancelled RUN is supersession, handled at run level before jobs.
#    OK-TERMINAL: success skipped neutral (path-filtered build-* jobs report skipped).
#    NON-TERMINAL: any job whose status != completed.
#  - Early-exit on the first failure is the whole point: one red job means the run
#    cannot go green, so there is nothing to gain by waiting for the rest.
set -uo pipefail

RUN=""; BRANCH=""; SHA=""; WORKFLOW="ci.yml"; INTERVAL=120; MAX_TICKS=40; REPO=""
while [ $# -gt 0 ]; do
  case "$1" in
    --branch) BRANCH="${2:?}"; shift 2;;
    --sha) SHA="${2:?}"; shift 2;;
    --workflow) WORKFLOW="${2:?}"; shift 2;;
    --interval) INTERVAL="${2:?}"; shift 2;;
    --max-ticks) MAX_TICKS="${2:?}"; shift 2;;
    --repo) REPO="${2:?}"; shift 2;;
    -h|--help) sed -n '2,44p' "$0"; exit 3;;
    *)
      if [ -z "$RUN" ]; then RUN="$1"; shift
      else echo "unexpected arg: $1" >&2; exit 3
      fi
      ;;
  esac
done
if [ -z "$RUN" ] && [ -z "$BRANCH" ] && [ -z "$SHA" ]; then
  echo "usage: watch-run-ci.sh <run-id> | --branch <name> [--workflow ci.yml] | --sha <sha> [--branch main]  [--interval S] [--max-ticks N] [--repo OWNER/REPO]" >&2
  exit 3
fi
if [ -n "$SHA" ] && [ -n "$RUN" ]; then echo "--sha and <run-id> are exclusive" >&2; exit 3; fi
# --sha defaults the branch to main; --branch alone keeps its latest-run semantics.
SHA_MODE=0
if [ -n "$SHA" ]; then SHA_MODE=1; BRANCH="${BRANCH:-main}"; fi

# gh repo flag, spliced as an array so an empty value adds no argument (zsh does not
# word-split, and bash would pass an empty string).
GHR=()
[ -n "$REPO" ] && GHR=(--repo "$REPO")

# Resolve an abbreviated or full SHA through GitHub before the poll starts. Without this,
# a mistyped 40-hex value is indistinguishable from a workflow that has not appeared yet and
# burns the whole wait budget printing "no run yet". One retry absorbs a transient API blip;
# two unreadable results fail closed. Canonicalizing also lets every invocation use gh's
# server-side --commit filter instead of the capped branch-list prefix fallback.
if [ "$SHA_MODE" -eq 1 ]; then
  case "$SHA" in
    *[!0-9A-Fa-f]*|'') echo "watch-run-ci: --sha must be 7-40 hexadecimal digits (got '$SHA')" >&2; exit 3 ;;
  esac
  if [ "${#SHA}" -lt 7 ] || [ "${#SHA}" -gt 40 ]; then
    echo "watch-run-ci: --sha must be 7-40 hexadecimal digits (got ${#SHA})" >&2
    exit 3
  fi
  repo_path="$REPO"
  if [ -z "$repo_path" ]; then
    repo_path="$(gh repo view --json nameWithOwner --jq '.nameWithOwner' 2>/dev/null)"
    if [ -z "$repo_path" ]; then
      echo "watch-run-ci: could not infer OWNER/REPO to resolve --sha (pass --repo)" >&2
      exit 3
    fi
  fi
  resolved_sha=""
  for attempt in 1 2; do
    resolved_sha="$(gh api "repos/${repo_path}/commits/${SHA}" --jq '.sha' 2>/dev/null)"
    [[ "$resolved_sha" =~ ^[0-9A-Fa-f]{40}$ ]] && break
    if [ "$attempt" -lt 2 ]; then
      echo "watch-run-ci: --sha '$SHA' did not resolve on attempt $attempt/2; retrying in 2s" >&2
      sleep 2
    fi
  done
  if ! [[ "$resolved_sha" =~ ^[0-9A-Fa-f]{40}$ ]]; then
    echo "watch-run-ci: --sha '$SHA' did not resolve to a commit in $repo_path after 2 attempts; check the value, auth, and network" >&2
    exit 3
  fi
  SHA="$resolved_sha"
fi

# Resolve the latest run id on a branch (used when --branch given and RUN not fixed).
resolve_run() {
  gh run list "${GHR[@]}" --branch "$BRANCH" --workflow "$WORKFLOW" --limit 1 \
    --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null
}

# --sha mode: every run whose head matches the canonical 40-hex SHA, one line per run:
# databaseId<TAB>status<TAB>conclusion<TAB>workflowName. GitHub's server-side --commit
# filter means a busy main cannot push it out of a capped branch-list window.
runs_for_sha() {
  gh run list "${GHR[@]}" --branch "$BRANCH" --commit "$SHA" --limit 50 \
    --json databaseId,status,conclusion,workflowName \
    --jq '.[] | [.databaseId, .status, (.conclusion // ""), .workflowName] | @tsv' \
    2>/dev/null
}

# Classify one run's jobs (jq TSV on stdin:
# status<TAB>conclusion<TAB>name<TAB>url<TAB>databaseId).
# Prints FAIL / PENDING / GREEN, then the failing rows (name<TAB>url<TAB>id) when FAIL.
# $1 == "1" excludes `cancelled` from the failure set: in --branch/--sha mode a cancelled
# run means it was superseded by a newer push (ci.yml concurrency), NOT a failure, and the
# run-level guard handles it before jobs are read. In explicit run-id mode a cancelled job
# IS a failure (you asked to watch that specific run).
classify() {
  awk -F'\t' -v bmode="${1:-0}" '
    function isfail(c){
      if (c=="failure"||c=="timed_out"||c=="action_required"||c=="startup_failure") return 1
      if (c=="cancelled" && bmode!="1") return 1
      return 0
    }
    {
      status=$1; concl=$2; name=$3; url=$4; id=$5
      if (status!="completed") { pend++; next }        # non-terminal job
      if (isfail(concl)) { fails[nf++]=name "\t" url "\t" id }  # completed AND failed
    }
    END {
      if (nf>0){ print "FAIL"; for(i=0;i<nf;i++) print fails[i] }
      else if (pend>0) print "PENDING"
      else print "GREEN"
    }'
}

# Dump one run's jobs as jq TSV into DUMP_OUT; RETURN gh's exit status so the caller
# can tell a real gh failure (bad run id, auth, API down) from an empty-but-successful
# read. stderr is folded into DUMP_OUT (empty on success), so on failure DUMP_OUT
# carries the gh error text for the log. Do NOT call inside a $(...) subshell — that
# would discard the rc this function exists to return.
dump() {
  DUMP_OUT="$(gh run view "${GHR[@]}" "$1" --json jobs \
    --jq '.jobs[] | [.status, (.conclusion // ""), .name, .url, .databaseId] | @tsv' 2>&1)"
}

print_fails() {  # $1 = run id, $2 = workflow name (optional), $3 = verdict text
  run_id="$1"
  workflow="$2"
  label="run $run_id"
  [ -n "$workflow" ] && label="$label ($(printf '%q' "$workflow"))"
  printf '=== %s: FAILED JOB(S) — react now; each completed job has a live log command below ===\n' "$label"
  printf '%s\n' "$3" | tail -n +2 | while IFS=$'\t' read -r name url job_id; do
    repo_path="$REPO"
    if [ -z "$repo_path" ]; then
      repo_path="$(printf '%s\n' "$url" | awk -F/ 'NF >= 5 { print $4 "/" $5; exit }')"
    fi
    [ -n "$repo_path" ] || repo_path="OWNER/REPO"
    safe_name="$(printf '%q' "$name")"
    safe_endpoint="$(printf '%q' "repos/$repo_path/actions/jobs/$job_id/logs")"
    safe_repo="$(printf '%q' "$repo_path")"
    safe_run="$(printf '%q' "$run_id")"
    safe_job="$(printf '%q' "$job_id")"
    printf '  FAIL  %s  %s\n' "$safe_name" "$url"
    printf '    live log: gh api --allow-escape-sequences %s\n' "$safe_endpoint"
    printf '    after run terminal: gh run view %s --repo %s --job %s --log-failed\n' "$safe_run" "$safe_repo" "$safe_job"
  done
}

tick=0; gh_errs=0; seen_any=0
while [ "$tick" -lt "$MAX_TICKS" ]; do

  if [ "$SHA_MODE" -eq 1 ]; then
    # ---- --sha mode: aggregate over every run for the commit -------------------------
    runs="$(runs_for_sha)"
    if [ -z "$runs" ]; then
      if [ "$seen_any" -eq 1 ]; then
        echo "[tick $tick] workflow listing temporarily empty after runs were seen for sha=${SHA:0:8}; retrying"
      else
        echo "[tick $tick] no run yet for sha=${SHA:0:8} on $BRANCH; retrying"
      fi
      sleep "$INTERVAL"; tick=$((tick+1)); continue
    fi
    seen_any=1
    pending=0; superseded=0; failed=0
    while IFS=$'\t' read -r rid rstatus rconcl wname; do
      [ -z "$rid" ] && continue
      if [ "$rstatus" = "completed" ] && [ "$rconcl" = "cancelled" ]; then
        superseded=$((superseded+1)); echo "[tick $tick] $wname run $rid cancelled (superseded)"; continue
      fi
      if ! dump "$rid"; then
        gh_errs=$((gh_errs+1))
        echo "[tick $tick] gh run view failed for run $rid (consecutive failure $gh_errs): $DUMP_OUT" >&2
        if [ "$gh_errs" -ge 2 ]; then echo "gh keeps failing on run $rid; giving up (exit 3)" >&2; exit 3; fi
        pending=1; continue
      fi
      gh_errs=0
      if [ -z "$DUMP_OUT" ]; then pending=1; continue; fi
      verdict="$(printf '%s\n' "$DUMP_OUT" | classify 1)"
      case "$(printf '%s\n' "$verdict" | head -1)" in
        FAIL)
          # Confirm with one immediate re-query to shake off a stale first-tick read.
          if dump "$rid"; then
            verdict2="$(printf '%s\n' "$DUMP_OUT" | classify 1)"
            if [ "$(printf '%s\n' "$verdict2" | head -1)" = "FAIL" ]; then
              print_fails "$rid" "$wname" "$verdict2"; failed=1; break
            fi
            echo "[tick $tick] $wname: a fail cleared on re-query (stale read); continuing"; pending=1
          else
            pending=1
          fi
          ;;
        PENDING) pending=1 ;;
        GREEN) : ;;
      esac
    done <<< "$runs"
    [ "$failed" -eq 1 ] && exit 1
    if [ "$pending" -eq 0 ]; then
      if [ "$superseded" -gt 0 ]; then
        echo "=== sha ${SHA:0:8}: $superseded run(s) cancelled by concurrency, none failed — superseded; re-watch the current $BRANCH head (exit 4) ==="
        exit 4
      fi
      echo "=== sha ${SHA:0:8}: every run terminal, none failed after $((tick*INTERVAL))s ==="
      printf '%s\n' "$runs" | awk -F'\t' '{printf "  %-10s %-9s %s\n",$3,$1,$4}'
      exit 0
    fi
    sleep "$INTERVAL"; tick=$((tick+1)); continue
  fi

  # ---- run-id / --branch mode -------------------------------------------------------
  # With --branch and no fixed run id, re-resolve each tick (a concurrent push mints a
  # newer run; a rerun keeps the same id). With an explicit run id, keep it.
  cur="$RUN"
  if [ -z "$cur" ] || [ -n "$BRANCH" ]; then
    r="$(resolve_run)"; [ -n "$r" ] && cur="$r"
  fi
  if [ -z "$cur" ]; then echo "[tick $tick] no run found for branch=$BRANCH workflow=$WORKFLOW; retrying"; sleep "$INTERVAL"; tick=$((tick+1)); continue; fi

  # A nonzero gh rc is a real error (bad run id, auth, API down), NOT empty jobs.
  # Tolerate one transient blip, then honor the documented exit 3 rather than silently
  # waiting out every tick and exiting 2.
  if ! dump "$cur"; then
    gh_errs=$((gh_errs+1))
    echo "[tick $tick] gh run view failed for run $cur (consecutive failure $gh_errs): $DUMP_OUT" >&2
    if [ "$gh_errs" -ge 2 ]; then echo "gh keeps failing on run $cur; giving up (exit 3)" >&2; exit 3; fi
    sleep "$INTERVAL"; tick=$((tick+1)); continue
  fi
  gh_errs=0
  out="$DUMP_OUT"
  if [ -z "$out" ]; then echo "[tick $tick] run $cur reported no jobs yet (transient?); retrying"; sleep "$INTERVAL"; tick=$((tick+1)); continue; fi

  # Branch mode only: a run cancelled by ci.yml concurrency was superseded by a newer
  # push — re-resolve to the newer run next tick instead of reporting a red. (In
  # explicit run-id mode a cancelled run is a genuine failure and falls through.)
  if [ -n "$BRANCH" ]; then
    concl="$(gh run view "${GHR[@]}" "$cur" --json conclusion --jq '.conclusion // ""' 2>/dev/null)"
    if [ "$concl" = "cancelled" ]; then
      echo "[tick $tick] run $cur cancelled (superseded by a newer push); re-resolving next tick"
      sleep "$INTERVAL"; tick=$((tick+1)); continue
    fi
  fi

  bmode=0; [ -n "$BRANCH" ] && bmode=1
  verdict="$(printf '%s\n' "$out" | classify "$bmode")"
  case "$(printf '%s\n' "$verdict" | head -1)" in
    FAIL)
      # Confirm with one immediate re-query to shake off a stale first-tick read. If the
      # re-query itself gh-errors, skip confirmation this tick rather than exit 1 on a
      # fail we could not reconfirm.
      if dump "$cur"; then
        verdict2="$(printf '%s\n' "$DUMP_OUT" | classify "$bmode")"
        if [ "$(printf '%s\n' "$verdict2" | head -1)" = "FAIL" ]; then
          print_fails "$cur" "" "$verdict2"; exit 1
        fi
        echo "[tick $tick] a fail cleared on re-query (stale read); continuing"
      fi
      ;;
    GREEN)
      echo "=== run $cur: all jobs terminal, none failed after $((tick*INTERVAL))s ==="
      exit 0
      ;;
    PENDING) : ;;  # keep waiting
  esac

  sleep "$INTERVAL"; tick=$((tick+1))
done

if [ "$SHA_MODE" -eq 1 ] && [ "$seen_any" -eq 0 ]; then
  echo "=== sha ${SHA:0:8}: NO RUN APPEARED on $BRANCH within $((MAX_TICKS*INTERVAL))s (the push may have spawned no workflow run; re-point at a descendant commit) ==="
  exit 3
fi
echo "=== ${RUN:-${SHA:+sha:${SHA:0:8}}}${RUN:-${SHA:-branch:$BRANCH}}: still pending after $((MAX_TICKS*INTERVAL))s (max-ticks reached) ==="
exit 2
