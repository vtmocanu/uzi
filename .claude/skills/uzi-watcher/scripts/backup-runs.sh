#!/usr/bin/env bash
# One-shot backup of in-flight uzi run work from hosted (k8s) worker PVCs.
#
# Handles BOTH run kinds: an issue run's working clone is /data/runner/<slug>/issue-N
# (files named issue-N.*), a task run's (uzi handoff, no issue iid) is
# /data/runner/<slug>/task-<runid> (files named task-<runid>.*). The per-run "stem"
# below selects which; a task run's whole point here is that its work is often still
# UNCOMMITTED, so the uncommitted.patch + untracked capture is what saves it.
#
# For each run id it resolves worker_id -> pod FRESH each call (so it survives a
# worker roll or a cross-worker migration), execs into the worker container, and
# captures from the live runner working clone /data/runner/<slug>/<stem>:
#   issue-N.tgz               a tarball of:
#     issue-N.bundle            git bundle of the branch (commits not on origin/main)
#     issue-N.uncommitted.patch git diff HEAD  (staged+unstaged tracked changes)
#     issue-N.untracked.tar.gz  new, non-ignored untracked files
#     issue-N.meta.txt          HEAD sha, branch, new-commit log, status, diffstat
#   issue-N.run.json          full `uzi run get --json`
#   issue-N.plan.md           the latest approved plan (milestone breakdown)
#   issue-N.progress.txt      status/health/token + milestones DONE vs LEFT
#   issue-N.log-tail.ndjson   last 80 transcript messages
# Together these fully reconstruct a worker's working tree + run state locally, so
# work is recoverable even if the run is lost before it pushes an MR. See this
# skill's "Recovering a failed run's work from the worker PVC" section.
#
# Usage:  bash backup-runs.sh <RUN_ID> [RUN_ID ...]
# Env (all optional except where noted):
#   UZI_CTX         kube context (default: current `kubectl config current-context`)
#   UZI_WORKER_NS   space-separated worker namespaces to search
#                   (default: "uzi-workers uzi-workers-docker")
#   UZI_REPO_SLUG   worker bare/clone dir stem host+org+repo
#                   (default: derived from this checkout's origin remote)
#   UZI_BACKUP_DIR  output root (default: /tmp/uzi-backups)
#   UZI_KUBECTL / UZI_BIN / UZI_JQ   tool overrides (default: from PATH)
set -u

KUBECTL="${UZI_KUBECTL:-kubectl}"
UZI="${UZI_BIN:-uzi}"
JQ="${UZI_JQ:-jq}"

CTX="${UZI_CTX:-$("$KUBECTL" config current-context 2>/dev/null)}"
NAMESPACES="${UZI_WORKER_NS:-uzi-workers uzi-workers-docker}"
OUTROOT="${UZI_BACKUP_DIR:-/tmp/uzi-backups}"

if [ "$#" -eq 0 ]; then
  echo "usage: backup-runs.sh <RUN_ID> [RUN_ID ...]" >&2
  exit 2
fi
if [ -z "$CTX" ]; then
  echo "error: no kube context (set UZI_CTX or a current-context)" >&2
  exit 2
fi

# Worker dir stem, e.g. github.com+org+repo. Derive from origin unless overridden.
REPO_SLUG="${UZI_REPO_SLUG:-}"
if [ -z "$REPO_SLUG" ]; then
  origin="$(git remote get-url origin 2>/dev/null || true)"
  REPO_SLUG="$(printf '%s' "$origin" | sed -E 's#^[a-z]+://##; s#^[^@]+@##; s#\.git$##; s#[:/]#+#g')"
fi
if [ -z "$REPO_SLUG" ]; then
  echo "error: could not derive UZI_REPO_SLUG (not in a checkout? set it explicitly)" >&2
  exit 2
fi

RUNS=("$@")
TS="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$OUTROOT/$TS"
# Backups hold unpushed repo work + run transcripts; keep them owner-only (0700
# dirs / 0600 files) rather than inheriting a lax 022 umask on a shared host.
umask 077
mkdir -p "$DEST"
LOG="$DEST/backup.log"
log(){ printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }

# --- on-pod capture: emits a tar.gz of the artifacts on stdout, noise on stderr.
# This string runs REMOTELY (`sh -c` in the worker pod); only $REPO_SLUG is
# spliced in here at build time, every other $VAR expands pod-side on purpose.
# shellcheck disable=SC2016
CAPTURE='
set -u
STEM="$1"
CLONE="/data/runner/'"$REPO_SLUG"'/$STEM"
[ -d "$CLONE/.git" ] || { echo "NO_CLONE $CLONE" >&2; exit 3; }
cd "$CLONE" || exit 3
OUT="$(mktemp -d)"
BR="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
HEAD="$(git rev-parse HEAD 2>/dev/null)"
if git rev-parse --verify -q origin/main >/dev/null 2>&1; then
  git bundle create "$OUT/$STEM.bundle" "$BR" --not origin/main >/dev/null 2>&1 \
    || git bundle create "$OUT/$STEM.bundle" "$BR" >/dev/null 2>&1 || :
else
  git bundle create "$OUT/$STEM.bundle" "$BR" >/dev/null 2>&1 || :
fi
git diff HEAD > "$OUT/$STEM.uncommitted.patch" 2>/dev/null || :
git ls-files --others --exclude-standard -z > "$OUT/.untracked" 2>/dev/null || :
if [ -s "$OUT/.untracked" ]; then
  tar --null -T "$OUT/.untracked" -czf "$OUT/$STEM.untracked.tar.gz" 2>/dev/null || :
fi
rm -f "$OUT/.untracked"
{
  echo "stem=$STEM head=$HEAD branch=$BR captured=$(date -u +%FT%TZ)"
  echo "clone_origin_main=$(git rev-parse origin/main 2>/dev/null)"
  echo "merge_base=$(git merge-base HEAD origin/main 2>/dev/null)"
  echo "--- new commits (origin/main..HEAD):"
  git log --oneline origin/main..HEAD 2>/dev/null
  echo "--- git status --porcelain:"
  git status --porcelain 2>/dev/null
  echo "--- git diff --stat HEAD:"
  git diff --stat HEAD 2>/dev/null
} > "$OUT/$STEM.meta.txt" 2>&1
tar czf - -C "$OUT" . 2>/dev/null
rm -rf "$OUT"
'

resolve_pod(){   # $1=worker_id ; prints "ns pod" if found
  local wid="$1" ns pod
  for ns in $NAMESPACES; do
    pod="$("$KUBECTL" --context "$CTX" -n "$ns" get pods -o name 2>/dev/null \
           | grep -m1 "uzi-hw-$wid" | sed 's#pod/##')"
    [ -n "$pod" ] && { printf '%s %s\n' "$ns" "$pod"; return 0; }
  done
  return 1
}

for RID in "${RUNS[@]}"; do
  J="$("$UZI" run get "$RID" --json 2>/dev/null)"
  st="$(printf '%s' "$J" | "$JQ" -r '.status // ""' 2>/dev/null)"
  if [ -z "${st:-}" ]; then
    log "WARN $RID: status unreadable (CLI/API); skipping this cycle"
    continue
  fi
  iid="$(printf '%s' "$J" | "$JQ" -r '.issue_iid // .issue // ""' 2>/dev/null)"
  kind="$(printf '%s' "$J" | "$JQ" -r '.kind // ""' 2>/dev/null)"
  wid="$(printf '%s' "$J" | "$JQ" -r '.worker_id // ""' 2>/dev/null)"
  mr="$(printf '%s' "$J" | "$JQ" -r '.mr_web_url // ""' 2>/dev/null)"

  # The "stem" is BOTH the on-pod working-clone dir name and the output-file prefix.
  # An issue run keeps its historical issue-N.* naming; a task/chat run (no issue iid)
  # is task-<runid>.* / <kind>-<runid>.*, matching /data/runner/<slug>/task-<runid>.
  if [ -n "$iid" ]; then
    STEM="issue-$iid"; LBL="#$iid"
  elif [ -n "$kind" ] && [ "$kind" != "issue" ]; then
    STEM="$kind-$RID"; LBL="$kind ${RID%%-*}"
  else
    STEM="run-$RID";  LBL="run ${RID%%-*}"
  fi

  # --- status/progress snapshot (ALWAYS, even if parked or terminal: what was
  #     done, what is left, so a backup is self-describing without the code) ---
  printf '%s' "$J" | "$JQ" . > "$DEST/$STEM.run.json" 2>/dev/null || :
  TL="$(mktemp)"
  if "$UZI" run logs "$RID" --json > "$TL" 2>/dev/null; then
    "$JQ" -rs 'map(select(.kind=="plan"))|last|.payload.plan_md // empty' "$TL" \
      > "$DEST/$STEM.plan.md" 2>/dev/null || :
    tail -n 80 "$TL" > "$DEST/$STEM.log-tail.ndjson" 2>/dev/null || :
  fi
  rm -f "$TL"
  {
    echo "run=$RID  stem=$STEM  captured=$(date -u +%FT%TZ)"
    printf '%s' "$J" | "$JQ" -r '"status=\(.status)  health=\(.health_reason//"ok")  worker=\(.worker_id//"-")  token=\(.anthropic_secret_label//"-")/\(.anthropic_bind_mode//"-")  mr=\(.mr_web_url//"none")"' 2>/dev/null
    echo "--- milestones_completed (DONE):"
    printf '%s' "$J" | "$JQ" -r '(.milestones_completed//[])[]' 2>/dev/null
    echo "--- milestones (plan-frozen; status/title if present = what is LEFT):"
    printf '%s' "$J" | "$JQ" -r '(.milestones//[])[] | if type=="object" then "  \(.id//.key//.number//"?")\t\(.status//"?")\t\(.title//.name//"")" else "  \(tostring)" end' 2>/dev/null
    echo "(full plan: $STEM.plan.md ; recent transcript: $STEM.log-tail.ndjson)"
  } > "$DEST/$STEM.progress.txt" 2>/dev/null || :

  case "$st" in
    completed|failed|cancelled)
      log "SNAP $RID ($LBL) status=$st mr=${mr:-none} (status saved; no worker capture)"
      continue ;;
  esac
  if [ -z "$wid" ]; then
    log "SNAP $RID ($LBL) status=$st: no worker_id, parked/unclaimed (status saved)"
    continue
  fi
  if ! read -r ns pod < <(resolve_pod "$wid"); then
    log "WARN $RID ($LBL): status saved, but no pod for worker $wid in [$NAMESPACES]"
    continue
  fi
  f="$DEST/$STEM.tgz"
  # The capture streams a ~10-20 MB gzip out of the pod over `kubectl exec`, and
  # that stream can be TRUNCATED mid-transfer (observed once the bundle grows past
  # ~13 MB). A truncated .tgz still carries the bundle member's NAME in an early
  # tar header, so the old name-only `tar tzf | grep` logged a false OK on a file
  # whose bundle DATA was cut off. So verify the gzip END-TO-END (`gzip -t` plus a
  # full `tar tzf` listing) and RETRY the whole capture a few times, since the
  # truncation is transient — a re-exec usually succeeds within seconds.
  cap_rc=1
  for cap_try in 1 2 3; do
    if ! "$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- sh -c "$CAPTURE" _ "$STEM" > "$f" 2>>"$LOG"; then
      log "WARN $RID ($LBL): exec/capture attempt $cap_try failed; see $LOG"
      continue
    fi
    if [ ! -s "$f" ]; then
      # Empty is a missing clone, not a truncation; retrying will not help.
      break
    fi
    if gzip -t "$f" 2>/dev/null && tar tzf "$f" >/dev/null 2>&1; then
      cap_rc=0
      break
    fi
    log "WARN $RID ($LBL): truncated .tgz on attempt $cap_try ($(du -h "$f" | cut -f1)); retrying"
  done
  if [ "$cap_rc" -eq 0 ]; then
    # A verified-intact archive. The git bundle carries the committed work, so if
    # it is absent, say so (PART) rather than OK — the patch/untracked/status are
    # still useful, but there are no committed commits to restore.
    if tar tzf "$f" 2>/dev/null | grep -qF "$STEM.bundle"; then
      log "OK   $RID ($LBL) status=$st worker=$wid pod=$pod -> $f ($(du -h "$f" | cut -f1))"
    else
      log "PART $RID ($LBL): verified .tgz but WITHOUT a git bundle (uncommitted/status only) -> $f"
    fi
  elif [ -s "$f" ]; then
    log "FAIL $RID ($LBL): .tgz still truncated after retries -> $f kept for forensics; see $LOG"
  else
    log "FAIL $RID ($LBL): empty artifact (clone missing?); see $LOG"
    rm -f "$f"
  fi
done

ln -sfn "$DEST" "$OUTROOT/latest"
log "DONE -> $DEST  (latest -> $OUTROOT/latest)"
