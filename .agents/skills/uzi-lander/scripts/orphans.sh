#!/usr/bin/env bash
# orphans.sh: which uzi run PRs and in-flight runs have nobody landing them.
#
# Usage: orphans.sh [--repo OWNER/REPO] [--json]
#
# One row per open PR whose head is agent/* or uzi/* (a run's branch), and one per
# non-terminal run that can open a PR (kinds issue, ci_fix, prompt, self_improve, task) and
# has none yet. Each row carries the run, its issue and status, and the board claim from
# claims.sh: '#<PR>' (a lander holds the PR) or 'run-<RUN_ID>' (the dispatching session
# claimed the run at hand-off). VERDICT:
#   claimed             '#<PR>' held by a live or unverifiable session: message the owner
#   originator-claimed  'run-<RUN_ID>' held by a live or unverifiable session: it lands it
#   orphan              no claim, or only a dead owner's: ask (references/orphans.md), then claim
# Liveness is the session-peers registry alone (claims.sh registry_live): an owner it cannot
# verify is never an orphan, however old its last heartbeat.
#
# Exit: 0 table printed; 2 usage; 3 a lookup failed or was unreadable (gh, uzi, claims):
# nothing is printed, so an unknown is never reported as an orphan.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO=""; JSON=0
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="${2:?}"; shift 2;;
    --json) JSON=1; shift;;
    -h|--help) sed -n '2,20p' "$0"; exit 2;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done
die() { echo "orphans: $*" >&2; exit 3; }
if [ -z "$REPO" ]; then
  REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) || die "cannot infer --repo"
fi

prs=$(gh pr list --repo "$REPO" --state open --limit 200 --json number,headRefName,title 2>/dev/null) || die "gh pr list failed"
printf '%s' "$prs" | jq -e 'type=="array"' >/dev/null 2>&1 || die "gh pr list unreadable"
repos=$(uzi repo list --json 2>/dev/null) || die "uzi repo list failed"
repo_id=$(printf '%s' "$repos" | jq -er --arg p "$REPO" '[.[]|select(.path_with_namespace==$p)|.id]|first // empty' 2>/dev/null) \
  || die "repo $REPO not found in uzi repo list"
runs=$(uzi run list --json 2>/dev/null) || die "uzi run list failed"
printf '%s' "$runs" | jq -e 'type=="array"' >/dev/null 2>&1 || die "uzi run list unreadable"
claims=$("$HERE/claims.sh" list --json 2>/dev/null) || die "claims.sh list failed (an unreadable claim file?)"
printf '%s' "$claims" | jq -e 'type=="array"' >/dev/null 2>&1 || die "claims.sh list unreadable"

# The lookups go to jq through files, never argv: a real run list is megabytes, past ARG_MAX.
TMP=$(mktemp -d) || die "mktemp failed"
trap 'rm -rf "$TMP"' EXIT
printf '%s' "$prs" > "$TMP/prs.json" && printf '%s' "$runs" > "$TMP/runs.json" \
  && printf '%s' "$claims" > "$TMP/claims.json" || die "cannot stage lookups"
rows=$(jq -n --slurpfile prs "$TMP/prs.json" --slurpfile runs "$TMP/runs.json" --slurpfile claims "$TMP/claims.json" --arg repo "$repo_id" '
  def one_array($v; $n): if ($v|length) == 1 and ($v[0]|type) == "array" then $v[0]
    else error("\($n) lookup is not exactly one JSON array") end;
  one_array($prs; "pr") as $prs | one_array($runs; "run") as $runs | one_array($claims; "claim") as $claims |
  def claim($k): [$claims[]|select(.key==$k)]|first;
  def verdict($pc; $rc):
    if $pc != null and $pc.registry_live != "dead" then "claimed"
    elif $rc != null and $rc.registry_live != "dead" then "originator-claimed"
    else "orphan" end;
  def row($pr; $title; $r):
    (claim(if $pr == null then "" else "#\($pr)" end)) as $pc
    | (if $r == null then null else claim("run-\($r.id)") end) as $rc
    | ($pc // $rc) as $c
    | {pr:$pr, run:($r.id // null), issue:($r.issue_iid // null), status:($r.status // null),
       title:$title, claim:($c.key // null), owner:($c.owner // null), live:($c.registry_live // null),
       verdict:verdict($pc; $rc)};
  ($runs|map(select(.repo_id==$repo))) as $mine
  | ($prs|map(select(.headRefName|test("^(agent|uzi)/")))) as $runprs
  | [ $runprs[] as $p
      | ([$mine[]|select(.mr_iid==$p.number and .kind!="mr_rework")]|max_by(.created_at)) as $r
      | row($p.number; $p.title; $r) ]
  + [ $mine[]
      | select((.status|IN("completed","failed","cancelled")|not)
               and (.kind|IN("issue","ci_fix","prompt","self_improve","task"))
               and (.mr_iid == null))
      | row(null; (.title // ""); .) ]') || die "join failed"

if [ "$JSON" -eq 1 ]; then printf '%s\n' "$rows"; exit 0; fi
printf '%-6s %-10s %-6s %-18s %-20s %-8s %s\n' PR RUN ISSUE STATUS CLAIM_OWNER LIVE VERDICT
printf '%s' "$rows" | jq -r '.[]|[(if .pr then "#\(.pr)" else "-" end), ((.run // "-")|.[0:8]),
    (if .issue then "#\(.issue)" else "-" end), (.status // "-"), (.owner // "-"), (.live // "-"), .verdict]|@tsv' \
  | awk -F'\t' '{printf "%-6s %-10s %-6s %-18s %-20s %-8s %s\n",$1,$2,$3,$4,$5,$6,$7}'
exit 0
