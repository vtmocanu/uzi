# shellcheck shell=bash
# pr-checks-classify.sh — the ONE `gh pr checks` classifier, sourced by both
# watch-pr-ci.sh and watch-prs-ci.sh so a state-policy edit (what counts as a
# failure vs pending vs terminal) cannot land in one watcher and not the other.
#
# Source it, then call `classify <wait_cr>` with a `gh pr checks` dump on stdin.
# wait_cr (default 0) is passed explicitly rather than read from a caller's env var,
# so the lib is self-contained: with 0 the CodeRabbit row is ignored (CI is assessed
# separately), with 1 it is treated like any other check.
#
# Fields of `gh pr checks` (tab-separated): $1=name $2=state $3=elapsed $4=url.
# States seen: pass, fail, pending, skipping. Treated as FAILURE: fail failure
# cancelled timed_out action_required. NON-TERMINAL: pending in_progress queued
# waiting. OK-TERMINAL: everything else (pass skipping neutral success).
#
# Parses with `awk -F'\t'` on purpose: this host's `grep` is ugrep, whose POSIX
# modes mishandle negated classes and brace intervals (repo CLAUDE.md), so
# load-bearing field parsing must not go through a grep pattern.

# Prints one of FAIL / PENDING / GREEN on line 1; when FAIL, the failing rows
# (name<TAB>url) follow, one per line. Reads the dump on stdin. $1 = wait_cr.
classify() {
  awk -F'\t' -v wait_cr="${1:-0}" '
    function isfail(s){ return s=="fail"||s=="failure"||s=="cancelled"||s=="timed_out"||s=="action_required" }
    function ispend(s){ return s=="pending"||s=="in_progress"||s=="queued"||s=="waiting" }
    {
      name=$1; state=$2; url=$4
      if (name=="CodeRabbit" && !wait_cr) next   # CR assessed separately unless --wait-cr
      if (isfail(state)) { fails[nf++]=name "\t" url }
      else if (ispend(state)) pend++
    }
    END {
      if (nf>0){ print "FAIL"; for(i=0;i<nf;i++) print fails[i] }
      else if (pend>0) print "PENDING"
      else print "GREEN"
    }'
}
