#!/usr/bin/env bash
# One-shot backup of in-flight uzi run work from hosted (k8s) worker PVCs.
#
# Handles every run kind: the per-run "stem" equals the run's canonical runner-clone
# slug (agent/src/run-kind.ts deriveCloneKey): issue/chat/judge -> issue-N, task ->
# task-<runid>, self_improve -> uzi-self-improve-<runid>, prompt -> uzi-prompt-<runid>,
# and mr_rework/ci_fix -> slugify(pipeline_ref). A mr_rework run reuses the issue
# branch's clone at /data/runner/<slug>/agent-issue-N (files agent-issue-N.*), NOT a
# mr_rework-<runid> dir; runs.branch is NULL in-flight (claim_assembly.go) so the live
# branch comes from pipeline_ref. A task or mr_rework run's work is often still
# UNCOMMITTED, so the uncommitted.patch + untracked capture is what saves it.
#
# For each run id it resolves worker_id -> pod FRESH each call (so it survives a
# worker roll or a cross-worker migration), then searches every running worker pod
# when that current pod has lost the clone. It prefers the live working clone and
# falls back to refs/uzi-runner/<branch> in a worker bare repo when no clone remains:
#   issue-N.tgz               a tarball of:
#     issue-N.bundle            git bundle of the branch (commits not on origin/main)
#     issue-N.uncommitted.patch git diff HEAD  (staged+unstaged tracked changes)
#     issue-N.untracked.tar.gz  new, non-ignored untracked files
#     issue-N.meta.txt          HEAD sha, branch, new-commit log, status, diffstat
#   issue-N.run.json          full `uzi run get --json`
#   issue-N.plan.md           the latest approved plan (milestone breakdown)
#   issue-N.progress.txt      status/health/token + milestones DONE vs LEFT
#   issue-N.log-tail.ndjson   last 80 transcript messages
# A clone capture fully reconstructs the working tree. A bare-ref fallback preserves
# committed checkpoints only and logs BARE loudly because uncommitted WIP is unavailable.
# See this skill's "Recovering a failed run's work from the worker PVC" section.
#
# Usage:  bash backup-runs.sh <RUN_ID> [RUN_ID ...]
# Env (all optional except where noted):
#   UZI_CTX         kube context (default: current `kubectl config current-context`)
#   UZI_WORKER_NS   space-separated worker namespaces to search
#                   (default: "uzi-workers uzi-workers-docker")
#   UZI_REPO_SLUG   worker bare/clone dir stem host+org+repo
#                   (default: derived from this checkout's origin remote)
#   UZI_RUNNER_BASE worker clone root override (default: /data/runner/<repo-slug>)
#   UZI_REPOS_BASE  worker bare-repo root override (default: /data/repos)
#   UZI_BACKUP_DIR  output root (default: /tmp/uzi-backups)
#   UZI_BACKUP_RETENTION_DAYS  prune timestamped snapshots older than this many
#                   24-hour periods (default: 14; 0 disables pruning)
#   UZI_KUBECTL / UZI_BIN / UZI_JQ / UZI_STAT  tool overrides (default: from PATH)
# Exit: 0 when every active target produced a verified recovery artifact (terminal
# targets may be status-only); 1 when any active target did not; 2 for bad usage.
set -u

KUBECTL="${UZI_KUBECTL:-kubectl}"
UZI="${UZI_BIN:-uzi}"
JQ="${UZI_JQ:-jq}"
STAT="${UZI_STAT:-stat}"

CTX="${UZI_CTX:-$("$KUBECTL" config current-context 2>/dev/null)}"
NAMESPACES="${UZI_WORKER_NS:-uzi-workers uzi-workers-docker}"
OUTROOT="${UZI_BACKUP_DIR:-/tmp/uzi-backups}"
RETENTION_DAYS="${UZI_BACKUP_RETENTION_DAYS:-14}"

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
RUNNER_BASE="${UZI_RUNNER_BASE:-/data/runner/$REPO_SLUG}"
REPOS_BASE="${UZI_REPOS_BASE:-/data/repos}"
case "$RETENTION_DAYS" in
  ''|*[!0-9]*) echo "error: UZI_BACKUP_RETENTION_DAYS must be a non-negative integer (got '$RETENTION_DAYS')" >&2; exit 2 ;;
esac
RETENTION_DAYS=$((10#$RETENTION_DAYS))
if [ -z "$REPO_SLUG" ]; then
  echo "error: could not derive UZI_REPO_SLUG (not in a checkout? set it explicitly)" >&2
  exit 2
fi

# Backups hold unpushed repo work + run transcripts; keep them owner-only (0700
# dirs / 0600 files) rather than inheriting a lax 022 umask on a shared host.
umask 077
mkdir -p "$OUTROOT"
OUTROOT="$(cd "$OUTROOT" && pwd -P)" || exit 1
RUNS=("$@")
TS="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$OUTROOT/$TS"
if ! mkdir "$DEST" 2>/dev/null; then
  DEST="$(mktemp -d "$OUTROOT/${TS}.XXXXXX")" || exit 1
fi
LOG="$DEST/backup.log"
log(){ printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
# UZI_CTX unset means the kubeconfig current-context, which is shared across sessions and
# can be switched under a running loop; say so once so a later "no pod" WARN is legible.
[ -n "${UZI_CTX:-}" ] || log "WARN UZI_CTX unset; using kubeconfig current-context '$CTX' (pass UZI_CTX explicitly)"
failures=0
recoverable=0

path_mtime(){
  local value
  value="$("$STAT" -f %m "$1" 2>/dev/null || true)"
  # GNU stat accepts -f as "filesystem" mode and exits 0 while printing a
  # non-numeric result for %m. Validate the value, not only the exit status,
  # before falling back to GNU's -c spelling.
  case "$value" in ''|*[!0-9]*) value="$("$STAT" -c %Y "$1" 2>/dev/null || true)" ;; esac
  case "$value" in ''|*[!0-9]*) return 1 ;; esac
  printf '%s\n' "$value"
}

canonical_link_dir(){
  local link="$1" target
  target="$(readlink "$link" 2>/dev/null)" || return 1
  case "$target" in
    /*) ;;
    *) target="$(dirname "$link")/$target" ;;
  esac
  [ -d "$target" ] || return 1
  (cd "$target" && pwd -P)
}

prune_old_backups(){
  [ "$RETENTION_DAYS" -gt 0 ] || return 0
  local root now cutoff latest latest_attempt dir base mtime
  root="$(cd "$OUTROOT" 2>/dev/null && pwd -P)" || return 1
  # The name/path checks below already constrain every deletion, but reject broad
  # roots too: a mis-set UZI_BACKUP_DIR must fail closed before any rm is reachable.
  case "$root" in ''|/|"${HOME:-/nonexistent}") log "WARN refusing unsafe backup prune root '$root'"; return 1 ;; esac
  now="$(date +%s)"
  cutoff=$((now - RETENTION_DAYS * 86400))
  latest="$(canonical_link_dir "$OUTROOT/latest" || true)"
  latest_attempt="$(canonical_link_dir "$OUTROOT/latest-attempt" || true)"
  for dir in "$root"/*; do
    [ -d "$dir" ] && [ ! -L "$dir" ] || continue
    [ "$(dirname "$dir")" = "$root" ] || continue
    base="$(basename "$dir")"
    printf '%s' "$base" | grep -Eq '^[0-9]{8}T[0-9]{6}Z([.][A-Za-z0-9]+)?$' || continue
    [ "$dir" != "$latest" ] && [ "$dir" != "$latest_attempt" ] || continue
    mtime="$(path_mtime "$dir")" || { log "WARN could not stat backup for pruning: $dir"; continue; }
    if [ "$mtime" -lt "$cutoff" ]; then
      rm -rf -- "$dir"
      log "PRUNE removed backup older than ${RETENTION_DAYS}d: $dir"
    fi
  done
}

# --- on-pod capture: emits a tar.gz of the artifacts on stdout, noise on stderr.
# This string runs REMOTELY (`sh -c` in the worker pod); only $REPO_SLUG is
# spliced in here at build time, every other $VAR expands pod-side on purpose.
# shellcheck disable=SC2016
CAPTURE='
set -u
STEM="$1"
REALMAIN="${2:-}"
# Runner working-clone root, forwarded by the host as $3 (env does not cross
# `kubectl exec`, so a host UZI_RUNNER_BASE must be passed as an argument). Empty
# selects the default on-pod path; a value overrides it (a local fake clone under
# test, or a non-standard runner mount).
RUNNER_BASE="${3:-/data/runner/'"$REPO_SLUG"'}"
CLONE="$RUNNER_BASE/$STEM"
[ -d "$CLONE/.git" ] || { echo "NO_CLONE $CLONE" >&2; exit 3; }
cd "$CLONE" || exit 3
# kubectl exec enters as the worker container user, while the runner clone can
# belong to another UID. Trust only the verified clone for this capture;
# never change persistent Git configuration or trust every directory.
git(){ command git -c safe.directory="$CLONE" "$@"; }
OUT="$(mktemp -d)"
BR="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
HEAD="$(git rev-parse HEAD 2>/dev/null)"
[ -n "$BR" ] && [ -n "$HEAD" ] || { echo "invalid clone Git metadata: $CLONE" >&2; exit 7; }
# Exclude the TRUE public remote main (REALMAIN, read from the shared BARE repo by
# the host) so the bundle prerequisite is a commit that exists on the forge and is
# recoverable off-PVC. The working clone advances its OWN origin/main to a private
# worker checkpoint commit, so --not origin/main would silently drop every committed
# milestone up to that checkpoint and leave a bundle nothing off the PVC can apply.
BASE=""
if [ -n "$REALMAIN" ] && git cat-file -e "$REALMAIN" 2>/dev/null; then
  BASE="$REALMAIN"
elif git rev-parse --verify -q origin/main >/dev/null 2>&1; then
  BASE="origin/main"
fi
if [ -n "$BASE" ]; then
  # Only bundle when the branch carries commits the public base does not. With
  # none (the run has committed nothing beyond main yet — its work is still in the
  # uncommitted.patch), git refuses an empty bundle and a naive `|| git bundle
  # create "$BR"` fallback would instead dump the FULL history: tens of MB, every
  # byte already on the forge and useless for recovery. Skip it — the
  # uncommitted.patch + untracked capture hold the work and the host logs PART.
  if [ -n "$(git rev-list "$BASE..$BR" 2>/dev/null | head -n1)" ]; then
    git bundle create "$OUT/$STEM.bundle" "$BR" --not "$BASE" >/dev/null 2>&1 \
      || git bundle create "$OUT/$STEM.bundle" "$BR" >/dev/null 2>&1 || :
  fi
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
  echo "real_remote_main=${REALMAIN:-unknown}"
  echo "bundle_base=${BASE:-<none: full-history bundle>}"
  if [ -n "$BASE" ]; then
    echo "merge_base=$(git merge-base HEAD "$BASE" 2>/dev/null)"
    echo "--- new commits ($BASE..HEAD  = what the bundle carries):"
    git log --oneline "$BASE..HEAD" 2>/dev/null
  else
    echo "merge_base=(none; full-history bundle)"
    echo "--- commits (full history = what the bundle carries; count + newest 10):"
    echo "count=$(git rev-list --count HEAD 2>/dev/null)"
    git log --oneline -10 HEAD 2>/dev/null
  fi
  echo "--- git status --porcelain:"
  git status --porcelain 2>/dev/null
  echo "--- git diff --stat HEAD:"
  git diff --stat HEAD 2>/dev/null
} > "$OUT/$STEM.meta.txt" 2>&1
# Pipe through gzip -c rather than `tar czf -`: BusyBox/bsdtar pad the gzip stream
# to a block boundary with trailing NULs, which the host `gzip -t` integrity check
# rejects as "trailing garbage" and mis-reports as a truncated transfer. gzip -c
# emits one clean member on every worker tar implementation.
tar cf - -C "$OUT" . 2>/dev/null | gzip -c
rm -rf "$OUT"
'

# Bare-repo fallback: emits a tar.gz containing committed history + metadata only.
# It deliberately creates no uncommitted.patch: without a live clone there is no
# honest source for WIP. The host labels this BARE rather than OK.
# shellcheck disable=SC2016
BARE_CAPTURE='
set -u
STEM="$1"
REF="$2"
REALMAIN="${3:-}"
REPOS_BASE="${4:-/data/repos}"
BARE="$REPOS_BASE/'"$REPO_SLUG"'.git"
git --git-dir="$BARE" show-ref --verify --quiet "$REF" || exit 4
OUT="$(mktemp -d)"
trap '\''rm -rf "$OUT"'\'' EXIT
HEAD="$(git --git-dir="$BARE" rev-parse "$REF" 2>/dev/null)"
BASE=""
if [ -n "$REALMAIN" ] && git --git-dir="$BARE" cat-file -e "$REALMAIN" 2>/dev/null; then
  BASE="$REALMAIN"
elif git --git-dir="$BARE" rev-parse --verify -q refs/remotes/origin/main >/dev/null 2>&1; then
  BASE="refs/remotes/origin/main"
fi
if [ -n "$BASE" ]; then
  [ -n "$(git --git-dir="$BARE" rev-list "$BASE..$REF" 2>/dev/null | head -n1)" ] || exit 5
  git --git-dir="$BARE" bundle create "$OUT/$STEM.bundle" "$REF" --not "$BASE" >/dev/null 2>&1 || exit 6
else
  git --git-dir="$BARE" bundle create "$OUT/$STEM.bundle" "$REF" >/dev/null 2>&1 || exit 6
fi
{
  echo "stem=$STEM head=$HEAD ref=$REF captured=$(date -u +%FT%TZ) source=bare"
  echo "real_remote_main=${REALMAIN:-unknown}"
  echo "bundle_base=${BASE:-<none: full-history bundle>}"
  echo "uncommitted_capture=unavailable (live clone missing)"
  if [ -n "$BASE" ]; then
    echo "merge_base=$(git --git-dir="$BARE" merge-base "$REF" "$BASE" 2>/dev/null)"
    echo "--- new commits ($BASE..$REF = what the bundle carries):"
    git --git-dir="$BARE" log --oneline "$BASE..$REF" 2>/dev/null
  else
    echo "--- commits (full history bundle; newest 10):"
    git --git-dir="$BARE" log --oneline -10 "$REF" 2>/dev/null
  fi
} > "$OUT/$STEM.meta.txt" 2>&1
tar cf - -C "$OUT" . 2>/dev/null | gzip -c
'

resolve_pod(){   # $1=worker_id ; prints "ns pod" if found
  # Only a Running pod is exec-able. A worker roll (release fleet upgrade, node
  # eviction) leaves the old ReplicaSet's dead pod behind, and it sorts BEFORE the
  # live one, so a plain `grep -m1` grabs the corpse and every exec fails with
  # "cannot exec into a container in a completed pod". Filter to Running server-side.
  local wid="$1" ns pod
  for ns in $NAMESPACES; do
    pod="$("$KUBECTL" --context "$CTX" -n "$ns" get pods \
             --field-selector=status.phase=Running -o name 2>/dev/null \
           | grep -m1 "uzi-hw-$wid" | sed 's#pod/##')"
    [ -n "$pod" ] && { printf '%s %s\n' "$ns" "$pod"; return 0; }
  done
  return 1
}

list_pods(){
  local ns p
  for ns in $NAMESPACES; do
    while IFS= read -r p; do
      [ -n "$p" ] && printf '%s %s\n' "$ns" "${p#pod/}"
    done < <("$KUBECTL" --context "$CTX" -n "$ns" get pods \
      --field-selector=status.phase=Running -o name 2>/dev/null)
  done
}

pod_has_clone(){
  local ns="$1" pod="$2" stem="$3" branch="$4" rid="$5" clone journal
  [ -n "$branch" ] || return 1
  clone="$RUNNER_BASE/$stem"
  # shellcheck disable=SC2016  # $1/$2 expand in the remote sh, not in this host shell.
  "$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- \
    sh -c '[ -d "$1/.git" ]' _ "$clone" >/dev/null 2>&1 || return 1
  journal="$("$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- \
    git --git-dir="$REPOS_BASE/$REPO_SLUG.git" config --get "uzi-recovery.$branch.clone" 2>/dev/null)" || return 1
  # shellcheck disable=SC2016  # $rid/$clone are jq variables supplied with --arg.
  printf '%s' "$journal" | "$JQ" -e --arg rid "$rid" --arg clone "$clone" \
    '.runId == $rid and .clonePath == $clone' >/dev/null 2>&1
}

pod_has_ref(){
  local ns="$1" pod="$2" ref="$3" branch="$4" rid="$5" owner
  [ -n "$branch" ] || return 1
  "$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- \
    git --git-dir="$REPOS_BASE/$REPO_SLUG.git" show-ref --verify --quiet "$ref" >/dev/null 2>&1 || return 1
  owner="$("$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- \
    git --git-dir="$REPOS_BASE/$REPO_SLUG.git" config --get "uzi-trackowner.$branch.owner" 2>/dev/null)" || return 1
  [ "$owner" = "$rid" ]
}

for RID in "${RUNS[@]}"; do
  J="$("$UZI" run get "$RID" --json 2>/dev/null)"
  st="$(printf '%s' "$J" | "$JQ" -r '.status // ""' 2>/dev/null)"
  if [ -z "${st:-}" ]; then
    log "WARN $RID: status unreadable (CLI/API); skipping this cycle"
    failures=1
    continue
  fi
  iid="$(printf '%s' "$J" | "$JQ" -r '.issue_iid // .issue // ""' 2>/dev/null)"
  kind="$(printf '%s' "$J" | "$JQ" -r '.kind // ""' 2>/dev/null)"
  wid="$(printf '%s' "$J" | "$JQ" -r '.worker_id // ""' 2>/dev/null)"
  mr="$(printf '%s' "$J" | "$JQ" -r '.mr_web_url // ""' 2>/dev/null)"
  branch="$(printf '%s' "$J" | "$JQ" -r '.branch // ""' 2>/dev/null)"
  pref="$(printf '%s' "$J" | "$JQ" -r '.pipeline_ref // ""' 2>/dev/null)"

  # The "stem" is BOTH the on-pod working-clone dir name and the output-file prefix,
  # so it must equal the run's canonical runner-clone slug (agent/src/run-kind.ts
  # deriveCloneKey). Derive it from the fields the CLI DTO exposes for an IN-FLIGHT
  # run, which is the only state backups matter for:
  #   issue/chat/judge -> issue-<iid>
  #   task             -> task-<runid>
  #   self_improve     -> uzi-self-improve-<runid>   (branch is uzi/self-improve/<runid>)
  #   prompt           -> uzi-prompt-<runid>         (branch is uzi/prompt-<runid>)
  #   mr_rework/ci_fix  -> slugify(pipeline_ref)      ("/" -> "-")
  # runs.branch is NULL until completion (claim_assembly.go: a mr_rework/ci_fix run
  # sources its live branch from pipeline_ref), so keying mr_rework off .branch would
  # fall back to a mr_rework-<runid> dir that never exists and the capture would
  # silently produce a status snapshot only. slugify: replace "/" with "-".
  RUN_BRANCH=""
  case "$kind" in
    issue|chat|judge|"")
      if [ -n "$iid" ]; then STEM="issue-$iid"; LBL="#$iid"; RUN_BRANCH="agent/issue-$iid"
      else STEM="run-$RID"; LBL="run ${RID%%-*}"; fi ;;
    task)
      STEM="task-$RID"; LBL="task ${RID%%-*}"; RUN_BRANCH="$branch"
      [ -n "$RUN_BRANCH" ] || RUN_BRANCH="uzi/task/$RID" ;;
    self_improve)
      STEM="uzi-self-improve-$RID"; LBL="self_improve ${RID%%-*}"; RUN_BRANCH="uzi/self-improve/$RID" ;;
    prompt)
      STEM="uzi-prompt-$RID"; LBL="prompt ${RID%%-*}"; RUN_BRANCH="uzi/prompt-$RID" ;;
    *)  # mr_rework / ci_fix (and any future branch-scoped kind): use pipeline_ref, the
        # live branch in-flight; fall back to branch (populated once completed), else a
        # unique best-effort name. ci_fix's rare default-branch case (ci-fix/pipeline-<id>)
        # needs pipeline_id, which the DTO does not expose, so it is not reconstructed.
      src="$pref"; [ -n "$src" ] || src="$branch"
      if [ -n "$src" ]; then STEM="$(printf '%s' "$src" | tr '/' '-')"; LBL="$kind ${RID%%-*}"; RUN_BRANCH="$src"
      else STEM="${kind:-run}-$RID"; LBL="${kind:-run} ${RID%%-*}"; fi ;;
  esac
  TRACK_REF=""
  [ -n "$RUN_BRANCH" ] && TRACK_REF="refs/uzi-runner/$RUN_BRANCH"

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
    queued)
      # A queued run has no worker clone yet. Keep its status in the snapshot and
      # try for work on the next cycle after a worker claims it.
      log "SNAP $RID ($LBL) status=queued (status saved; awaiting worker capture)"
      continue ;;
  esac
  if [ -z "$wid" ]; then
    # A branch name is not a run identity: a fresh queued run on an issue can reuse
    # agent/issue-N after an older run. Without a current worker binding, an all-pod
    # search could therefore capture stale work and call it this run's backup.
    log "FAIL $RID ($LBL) status=$st: no current worker_id; status saved, refusing an unbound all-worker search"
    failures=1
    continue
  fi

  preferred=""
  preferred="$(resolve_pod "$wid" || true)"
  ns=""; pod=""; capture_kind=""
  if [ -n "$preferred" ]; then
    ns="${preferred%% *}"; pod="${preferred#* }"
    if pod_has_clone "$ns" "$pod" "$STEM" "$RUN_BRANCH" "$RID"; then capture_kind="clone"; fi
  fi

  # A run may have resumed on a new worker. Search all running worker pods for its
  # clone before falling back to the bare ref. A rolled Docker pod loses its emptyDir clone.
  if [ -z "$capture_kind" ]; then
    while read -r cns cpod; do
      [ -n "$cpod" ] || continue
      if [ -n "$preferred" ] && [ "$cns $cpod" = "$preferred" ]; then continue; fi
      if pod_has_clone "$cns" "$cpod" "$STEM" "$RUN_BRANCH" "$RID"; then
        ns="$cns"; pod="$cpod"; capture_kind="clone"; break
      fi
    done < <(list_pods)
  fi

  # No clone survived. Preserve the latest committed checkpoint from whichever
  # persistent worker still owns the runner tracking ref.
  if [ -z "$capture_kind" ] && [ -n "$TRACK_REF" ]; then
    if [ -n "$preferred" ]; then
      ns="${preferred%% *}"; pod="${preferred#* }"
      if pod_has_ref "$ns" "$pod" "$TRACK_REF" "$RUN_BRANCH" "$RID"; then capture_kind="bare"; fi
    fi
    if [ -z "$capture_kind" ]; then
      while read -r cns cpod; do
        [ -n "$cpod" ] || continue
        if [ -n "$preferred" ] && [ "$cns $cpod" = "$preferred" ]; then continue; fi
        if pod_has_ref "$cns" "$cpod" "$TRACK_REF" "$RUN_BRANCH" "$RID"; then
          ns="$cns"; pod="$cpod"; capture_kind="bare"; break
        fi
      done < <(list_pods)
    fi
  fi

  if [ -z "$capture_kind" ]; then
    log "FAIL $RID ($LBL): status saved, but no live clone or durable tracking ref found in ctx=$CTX ns=[$NAMESPACES]"
    failures=1
    continue
  fi

  # The clone's origin/main is a private worker checkpoint, so read the TRUE public
  # remote main from the shared bare repo and pass it to the capture: the bundle then
  # excludes a forge-recoverable base instead of silently dropping checkpointed work.
  REALMAIN="$("$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- \
    git --git-dir="$REPOS_BASE/$REPO_SLUG.git" rev-parse -q --verify refs/remotes/origin/main 2>/dev/null \
    | tr -d '[:space:]')"
  f="$DEST/$STEM.tgz"
  # The capture streams a ~10-20 MB gzip out of the pod over `kubectl exec`, and
  # that stream can be TRUNCATED mid-transfer (observed once the bundle grows past
  # ~13 MB). A truncated .tgz still carries the bundle member's NAME in an early
  # tar header, so the old name-only `tar tzf | grep` logged a false OK on a file
  # whose bundle DATA was cut off. So verify the gzip END-TO-END (`gzip -t` plus a
  # full `tar tzf` listing) and RETRY the whole capture a few times, since the
  # truncation is transient — a re-exec usually succeeds within seconds.
  cap_rc=1
  tmp="$f.part"
  for cap_try in 1 2 3; do
    # Write each attempt to a scratch path, NEVER straight to $f: a later attempt
    # whose exec dies before emitting any stdout must not wipe a nonempty archive an
    # earlier attempt already produced — a truncated archive is still the best
    # forensic artifact we have. Promote to $f only when the attempt produced bytes.
    rm -f "$tmp"
    if [ "$capture_kind" = "clone" ]; then
      "$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- \
        sh -c "$CAPTURE" _ "$STEM" "$REALMAIN" "$RUNNER_BASE" > "$tmp" 2>>"$LOG"
    else
      "$KUBECTL" --context "$CTX" -n "$ns" exec "$pod" -c worker -- \
        sh -c "$BARE_CAPTURE" _ "$STEM" "$TRACK_REF" "$REALMAIN" "$REPOS_BASE" > "$tmp" 2>>"$LOG"
    fi
    kc_rc=$?
    if [ "$kc_rc" -ne 0 ]; then
      log "WARN $RID ($LBL): exec/capture attempt $cap_try exit=$kc_rc; see $LOG"
    fi
    if [ ! -s "$tmp" ]; then
      # A typed missing-source/no-new-commits result is deterministic; retrying the same
      # pod three times cannot create a clone or tracking ref. Other failures may be a
      # transient kubectl stream break, so retry those.
      case "$kc_rc" in 3|4|5) break ;; esac
      [ "$kc_rc" -ne 0 ] && continue
      break
    fi
    # Nonempty output — even on a nonzero exec exit, kubectl may have streamed a
    # PARTIAL archive; keep it as the best-so-far and let the verify below decide.
    mv -f "$tmp" "$f"
    if gzip -t "$f" 2>/dev/null && tar tzf "$f" >/dev/null 2>&1; then
      cap_rc=0
      break
    fi
    log "WARN $RID ($LBL): truncated .tgz on attempt $cap_try ($(du -h "$f" | cut -f1)); retrying"
  done
  rm -f "$tmp"
  if [ "$cap_rc" -eq 0 ]; then
    # A verified-intact archive. The git bundle carries the committed work, so if
    # it is absent, say so (PART) rather than OK — the patch/untracked/status are
    # still useful, but there are no committed commits to restore.
    if tar tzf "$f" 2>/dev/null | grep -qF "$STEM.bundle"; then
      if [ "$capture_kind" = "bare" ]; then
        log "BARE $RID ($LBL) status=$st pod=$pod ref=$TRACK_REF -> $f ($(du -h "$f" | cut -f1)); committed history only, uncommitted WIP unavailable"
      else
        log "OK   $RID ($LBL) status=$st worker=$wid pod=$pod -> $f ($(du -h "$f" | cut -f1))"
      fi
      recoverable=1
    else
      log "PART $RID ($LBL): verified .tgz but WITHOUT a git bundle (uncommitted/status only) -> $f"
      recoverable=1
    fi
  elif [ -s "$f" ]; then
    log "FAIL $RID ($LBL): .tgz still truncated after retries -> $f kept for forensics; see $LOG"
    failures=1
  else
    log "FAIL $RID ($LBL): empty artifact from $capture_kind source; see $LOG"
    rm -f "$f"
    failures=1
  fi
done

ln -sfn "$DEST" "$OUTROOT/latest-attempt"
if [ "$failures" -eq 0 ] && [ "$recoverable" -eq 1 ]; then
  ln -sfn "$DEST" "$OUTROOT/latest"
  log "DONE -> $DEST  (latest + latest-attempt updated)"
elif [ "$failures" -eq 0 ]; then
  log "DONE -> $DEST  (status-only; latest unchanged, latest-attempt updated)"
else
  log "INCOMPLETE -> $DEST  (latest preserved, latest-attempt updated)"
  prune_old_backups || log "WARN backup retention prune did not complete"
  exit 1
fi
prune_old_backups || log "WARN backup retention prune did not complete"
