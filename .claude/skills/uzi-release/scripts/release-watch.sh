#!/usr/bin/env bash
# release-watch.sh — watch a tag's publish workflows (release.yml + brew.yml) to
# green, JOB level, AUTO-RERUNNING a transient publish failure a bounded number of
# times. This is the release analog of watch-run-ci.sh: publish jobs push an image
# tag and sign it (idempotent, network-flavored), so a lone flaked job — a cosign
# installer download, a registry hiccup — should be re-run, not surfaced to a human.
#
#   release-watch.sh <X.Y.Z> [--max-reruns N] [--interval S] [--max-ticks N]
#
# <X.Y.Z> is the release version (leading v optional); the tag ref is vX.Y.Z.
# Owner/repo are inferred by gh from the checkout, so a fork works unedited.
#   --max-reruns  transient failures to auto-rerun before giving up (default 2).
#   --interval    seconds between polls (default 60; publish jobs are 2-3 min).
#   --max-ticks   give up after N polls (default 60 -> ~60 min at 60s).
#
# Run it in the BACKGROUND (the harness re-invokes you when it exits).
#
# TRANSIENT vs REAL, signature-independent by design. A failed job is auto-rerun
# ONLY if it is not a deterministic GATE — the gates are `assert-version`,
# `assert-changelog` and `prep`, which fail for a real reason (wrong Chart version,
# an uncited merge) that a rerun cannot clear. Every other job (publish-*, the
# chart publish, publish-release, brew's build) is idempotent and rerunnable. This
# keys on the JOB'S ROLE, not on an exit code or a step name, so it survives the
# cosign-installer hardening tracked in #945 (which changes the flake's signature
# but not which jobs are gates).
#
# Exit codes:
#   0  both release.yml and brew.yml all-green (after any auto-reruns)
#   1  a non-transient failure, or reruns exhausted (failing jobs printed)
#   2  timed out (still pending after --max-ticks)
#   3  usage / gh error
#
# Parsing uses awk -F'\t' on jq TSV, never a bare grep pattern (ugrep POSIX-mode
# caveats, repo CLAUDE.md).
set -uo pipefail

VERSION=""; MAX_RERUNS=2; INTERVAL=60; MAX_TICKS=60
while [ $# -gt 0 ]; do
  case "$1" in
    --max-reruns) MAX_RERUNS="${2:?}"; shift 2;;
    --interval)   INTERVAL="${2:?}"; shift 2;;
    --max-ticks)  MAX_TICKS="${2:?}"; shift 2;;
    -h|--help)    sed -n '2,30p' "$0"; exit 3;;
    *) if [ -z "$VERSION" ]; then VERSION="$1"; shift
       else echo "unexpected arg: $1" >&2; exit 3; fi ;;
  esac
done
if [ -z "$VERSION" ]; then
  echo "usage: release-watch.sh <X.Y.Z> [--max-reruns N] [--interval S] [--max-ticks N]" >&2
  exit 3
fi
VERSION="${VERSION#v}"; TAG="v$VERSION"
WORKFLOWS="release.yml brew.yml"

# Resolve the run id for one workflow on the tag ref (empty until it appears).
resolve_run() { gh run list --workflow "$1" --branch "$TAG" --limit 1 \
  --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null; }

# Dump a run's jobs as jq TSV (status<TAB>conclusion<TAB>name<TAB>url) into DUMP_OUT;
# return gh's rc so a real gh error is distinguishable from an empty read.
dump() { DUMP_OUT="$(gh run view "$1" --json jobs \
  --jq '.jobs[] | [.status, (.conclusion // ""), .name, .url] | @tsv' 2>&1)"; }

# Classify jobs on stdin. Prints GREEN / PENDING / FAIL; when FAIL, follows with the
# failing rows (name<TAB>url) and a marker line "GATEFAIL=1" if any failing job is a
# deterministic gate (assert-*/prep) — i.e. NOT safe to auto-rerun.
classify() {
  awk -F'\t' '
    function isfail(c){ return (c=="failure"||c=="cancelled"||c=="timed_out"||c=="action_required"||c=="startup_failure") }
    function isgate(n){ return (n ~ /^assert/ || n=="prep") }
    { status=$1; concl=$2; name=$3; url=$4
      if (status!="completed") { pend++; next }
      if (isfail(concl)) { fails[nf]=name "\t" url; if (isgate(name)) gate=1; nf++ } }
    END {
      # PENDING takes priority over FAIL: `gh run rerun --failed` only works on a
      # COMPLETED run, so while any job is still running we WAIT, even if a sibling job
      # has already failed. Reporting FAIL early (the watch-run-ci.sh pattern) would fire
      # the rerun against an in-progress run, which GitHub rejects, turning a transient
      # flake into a hard exit 1 — the exact case cutting v0.74.0 hit. Only once every job
      # is terminal do we evaluate FAIL, at which point the run is rerunnable.
      if (pend>0) print "PENDING"
      else if (nf>0){ print "FAIL"; for(i=0;i<nf;i++) print fails[i]; print "GATEFAIL=" (gate?1:0) }
      else print "GREEN" }'
}

# After a rerun, wait until the run leaves the `completed` state (re-queued) so the
# next tick does not re-detect the just-cleared failure and burn another rerun.
wait_requeued() {
  local id="$1" st
  for _ in 1 2 3 4 5 6; do
    st="$(gh run view "$id" --json status --jq '.status' 2>/dev/null)"
    [ "$st" != "completed" ] && return 0
    sleep 10
  done
  return 0   # proceed anyway; the budget still bounds us
}

tick=0; gh_errs=0
# Per-workflow rerun budget, so a flake that burns release.yml's retries does not
# leave brew.yml with none. Indirect vars (printf -v / ${!key}) keep it bash-3.2-safe;
# key = RR_<workflow name with non-alnum -> _>.
for wf in $WORKFLOWS; do printf -v "RR_${wf//[^a-zA-Z0-9]/_}" '%s' "$MAX_RERUNS"; done
while [ "$tick" -lt "$MAX_TICKS" ]; do
  all_green=1; any_pending=0
  for wf in $WORKFLOWS; do
    id="$(resolve_run "$wf")"
    if [ -z "$id" ]; then any_pending=1; all_green=0
      echo "[tick $tick] $wf: no run yet for $TAG; waiting"; continue; fi

    if ! dump "$id"; then
      gh_errs=$((gh_errs+1)); all_green=0; any_pending=1
      echo "[tick $tick] gh run view failed for $wf run $id (consecutive $gh_errs): $DUMP_OUT" >&2
      [ "$gh_errs" -ge 3 ] && { echo "gh keeps failing; giving up (exit 3)" >&2; exit 3; }
      continue
    fi
    gh_errs=0
    [ -z "$DUMP_OUT" ] && { any_pending=1; all_green=0; echo "[tick $tick] $wf run $id: no jobs yet; waiting"; continue; }

    verdict="$(printf '%s\n' "$DUMP_OUT" | classify)"
    head1="$(printf '%s\n' "$verdict" | head -1)"
    case "$head1" in
      GREEN) : ;;                       # this workflow done
      PENDING) any_pending=1; all_green=0 ;;
      FAIL)
        all_green=0
        # Confirm with one re-query (stale-first-tick guard).
        dump "$id" || true
        verdict="$(printf '%s\n' "$DUMP_OUT" | classify)"
        [ "$(printf '%s\n' "$verdict" | head -1)" != "FAIL" ] && { any_pending=1; echo "[tick $tick] $wf run $id: a fail cleared on re-query; continuing"; continue; }

        gatefail="$(printf '%s\n' "$verdict" | awk -F= '/^GATEFAIL=/{print $2}')"
        failrows="$(printf '%s\n' "$verdict" | sed '1d;/^GATEFAIL=/d')"
        rrkey="RR_${wf//[^a-zA-Z0-9]/_}"; rrleft="${!rrkey}"

        if [ "$gatefail" = "1" ] || [ "$rrleft" -le 0 ]; then
          reason="non-transient gate failure"
          [ "$gatefail" != "1" ] && reason="reruns exhausted ($MAX_RERUNS used for $wf)"
          echo "=== $wf run $id: FAILED ($reason) — react now (gh run view --job <id> --log-failed) ==="
          printf '%s\n' "$failrows" | awk -F'\t' 'NF{printf "  FAIL  %-28s %s\n",$1,$2}'
          exit 1
        fi

        echo "=== $wf run $id: transient failure, auto-rerunning ($rrleft rerun(s) left for $wf) ==="
        printf '%s\n' "$failrows" | awk -F'\t' 'NF{printf "  rerun <- %-28s %s\n",$1,$2}'
        if gh run rerun "$id" --failed >/dev/null 2>&1; then
          printf -v "$rrkey" '%s' "$((rrleft-1))"; any_pending=1
          wait_requeued "$id"
        else
          echo "=== $wf run $id: 'gh run rerun --failed' itself failed; surfacing ==="
          exit 1
        fi
        ;;
    esac
  done

  if [ "$all_green" -eq 1 ]; then
    used=0; for wf in $WORKFLOWS; do k="RR_${wf//[^a-zA-Z0-9]/_}"; used=$((used + MAX_RERUNS - ${!k})); done
    echo "=== $TAG: release.yml AND brew.yml all-green after $((tick*INTERVAL))s (reruns used: $used) ==="
    exit 0
  fi
  [ "$any_pending" -eq 1 ] || true
  sleep "$INTERVAL"; tick=$((tick+1))
done

echo "=== $TAG: still pending after $((MAX_TICKS*INTERVAL))s (max-ticks reached) ==="
exit 2
