#!/bin/bash
# Runs INSIDE the m1598-img-<ref> container, as root, via `docker run ... bash
# /measure-inner.sh` (run.sh does not pass `--entrypoint`; the image's default
# entrypoint is unset, so `docker run` executes this command+args directly).
# It prepares the writable trees, detects whether this ref's supervisor
# supports the --hold-cache standalone mode, drives N supervised Go-heavy
# commands through the REAL uzi-codex-supervisor + uzi-codex-command-sandbox
# as uid 10003, and prints one NDJSON line of measurements per command plus a
# final summary line to STDOUT, one JSON object per line (NDJSON), with all
# human-readable progress on STDERR. run.sh passes both streams straight
# through to its own stdout/stderr (no redirection or capture inside run.sh
# itself); it is the CALLER of run.sh that redirects each to a separate file,
# as shown in README.md and RESULTS.md's "exact commands run" sections.
#
# See e2e/codex-tmp-measure/README.md for what this measures and what it does
# NOT prove.
set -euo pipefail

N="${UZI_M1598_N:-5}"
log() { printf '%s\n' "$*" >&2; }

# emit writes one NDJSON result line to STDOUT (fd 1), kept strictly separate
# from the human-readable progress log() writes to STDERR. run.sh passes both
# streams straight through to its own stdout/stderr unmodified (no redirection
# or capture inside run.sh itself); it is the CALLER of run.sh that redirects
# each to a separate file, as shown in README.md and RESULTS.md's "exact
# commands run" sections. This is because there is no shared host filesystem
# to bind-mount a result file out of (the docker CLI here talks to a
# remote/sibling daemon over DOCKER_HOST; only the CLI's own stdout/stderr
# streams are guaranteed to reach the invoking shell).
emit() { printf '%s\n' "$1"; }

# safe_embed_json rejects any non-object line: it prints its argument
# unchanged only when it starts with `{` and ends with `}` (callers already
# `tr -d '\n'` first, so no embedded raw newline reaches here), and prints
# the bare literal `null` for anything else -- including a line that merely
# LOOKS like an object at its two ends but is not valid JSON in between (this
# is a cheap shape check, not a parse/validate: jq is not installed in
# Dockerfile.fallback's image, and adding it purely for this check was out of
# scope). Used wherever a line read back from a supervisor/holder log is
# spliced into the NDJSON row THIS script emits, so a malformed, partial, or
# (if a caller ever forgot to separate streams) non-JSON diagnostic line can
# never corrupt this script's own machine-readable output with something
# that isn't at least object-shaped.
safe_embed_json() {
  case "$1" in
    '{'*'}') printf '%s' "$1" ;;
    *) printf 'null' ;;
  esac
}

SETPRIV_BASE=(setpriv --reuid 10003 --regid 10003 --init-groups --no-new-privs
  --bounding-set -all --inh-caps -all --ambient-caps -all --)

# All of this harness's own scratch files (FIFOs, evidence/output logs) live
# under /work, NOT /tmp: /tmp is the exact directory this script measures
# residue in (`tmp_bytes`/`tmp_cmd_dirs` below), so a harness file placed
# there would count as if it were something the supervised commands left
# behind, when it is actually just this driver's own bookkeeping.
LOGDIR=/work/m1598-logs
mkdir -p "$LOGDIR"

# ---- 1. writable API checkout, owned by the command uid ------------------
mkdir -p /work
cp -a /work/api-src /work/api
chown -R 10003:10003 /work/api
chmod -R u+rwX /work/api
WORKROOT=/work

# ---- 2. Landlock mode actually in effect on this kernel -------------------
set +e
"${SETPRIV_BASE[@]}" /usr/local/bin/uzi-codex-command-sandbox --probe
PROBE_RC=$?
set -e
case "$PROBE_RC" in
  0) LANDLOCK_STATE="available" ;;
  10) LANDLOCK_STATE="unavailable (best-effort degrades to unconfined)" ;;
  *) LANDLOCK_STATE="probe-error(rc=$PROBE_RC)" ;;
esac
log "landlock probe rc=$PROBE_RC -> $LANDLOCK_STATE"
emit "{\"event\":\"landlock_probe\",\"rc\":$PROBE_RC,\"state\":\"$LANDLOCK_STATE\"}"

# ---- 3. feature-detect --hold-cache on this ref's supervisor ---------------
CACHE_ROOT=/cache
mkdir -p "$CACHE_ROOT"
chown 10003:10003 "$CACHE_ROOT"
chmod 0700 "$CACHE_ROOT"
PROBE_TOKEN=$(cat /proc/sys/kernel/random/uuid)
set +e
PROBE_OUT=$("${SETPRIV_BASE[@]}" /usr/local/bin/uzi-codex-supervisor --hold-cache \
  --expect-uid 10003 --cache-root "$CACHE_ROOT" --cache-token "$PROBE_TOKEN" \
  </dev/null 2>/dev/null)
set -e
HAS_CACHE=0
if printf '%s' "$PROBE_OUT" | grep -q '"event":"cache_ready"\|"event":"cache_error"'; then
  HAS_CACHE=1
fi
# The probe above, if it created a directory (cache_ready before EOF on
# /dev/null closed it), retains it as "unattested" -- reap it so it does not
# pollute the residue measurement below.
if [ -n "$PROBE_TOKEN" ] && [ -d "$CACHE_ROOT/$PROBE_TOKEN" ]; then
  "${SETPRIV_BASE[@]}" /usr/local/bin/uzi-codex-supervisor --remove-cache \
    --expect-uid 10003 --cache-root "$CACHE_ROOT" --cache-token "$PROBE_TOKEN" >/dev/null 2>&1 || true
fi
log "cache mode supported: $HAS_CACHE"
emit "{\"event\":\"cache_support\",\"supported\":$HAS_CACHE}"

# ---- 4. start the cache holder (only when supported) -----------------------
RUN_CACHE_DIR=""
HOLD_RELFIFO=""
HOLD_PID=""
if [ "$HAS_CACHE" = "1" ]; then
  RUN_TOKEN=$(cat /proc/sys/kernel/random/uuid)
  # The release channel gets EOF on the holder's stdin the moment its one
  # writer (us) closes -- which needs a PLAIN write-only open on our side,
  # opened AFTER the holder is backgrounded (its own `<"$HOLD_RELFIFO"` open
  # blocks until we do). A self-open read-write `<>` here (as the ctl/ev
  # channels below use, where EOF is never needed) measurably does NOT
  # deliver EOF to the holder's read loop even after `exec {FD}>&-` -- this
  # hung for real during development; do not "simplify" it back to `<>`.
  HOLD_RELFIFO="$LOGDIR/m1598-hold-release-$RUN_TOKEN"
  mkfifo "$HOLD_RELFIFO"
  HOLDLOG="$LOGDIR/m1598-hold-log-$RUN_TOKEN"
  : > "$HOLDLOG"
  # The holder's stderr is kept OUT of $HOLDLOG (production's own launcher
  # ignores the holder's stderr entirely -- it only ever reads its NDJSON
  # stdout). Merging stderr into the same file this script greps for
  # `cache_ready`/`path`/the last-line release confirmation would risk a
  # stray diagnostic line being read back as if it were a real evidence
  # line (see safe_embed_json above for the belt-and-suspenders check on
  # top of this). Captured to a separate file purely for a human to inspect
  # on failure; never parsed.
  HOLDERR="$LOGDIR/m1598-hold-err-$RUN_TOKEN"
  : > "$HOLDERR"
  "${SETPRIV_BASE[@]}" /usr/local/bin/uzi-codex-supervisor --hold-cache \
    --expect-uid 10003 --cache-root "$CACHE_ROOT" --cache-token "$RUN_TOKEN" \
    <"$HOLD_RELFIFO" >"$HOLDLOG" 2>"$HOLDERR" &
  HOLD_PID=$!
  exec {HOLDRELW}>"$HOLD_RELFIFO"
  for _ in $(seq 1 100); do
    grep -q '"event":"cache_ready"' "$HOLDLOG" 2>/dev/null && break
    sleep 0.1
  done
  RUN_CACHE_DIR=$(grep -o '"path":"[^"]*"' "$HOLDLOG" | head -1 | cut -d'"' -f4)
  if [ -z "$RUN_CACHE_DIR" ]; then
    log "FATAL: cache holder never reported cache_ready: $(cat "$HOLDLOG")"
    exit 1
  fi
  log "cache holder ready at $RUN_CACHE_DIR"
  emit "{\"event\":\"cache_holder_ready\",\"path\":\"$RUN_CACHE_DIR\"}"
fi

cache_dir_bytes() {
  if [ -n "$RUN_CACHE_DIR" ] && [ -d "$RUN_CACHE_DIR" ]; then
    du -sb "$RUN_CACHE_DIR" 2>/dev/null | cut -f1
  else
    echo 0
  fi
}

tmp_bytes() { du -sb /tmp 2>/dev/null | cut -f1; }
tmp_cmd_dirs() { find /tmp -maxdepth 1 -type d -name 'uzi-codex-command-*' 2>/dev/null | wc -l; }

PEAK_CACHE=0

# ---- 5. run N supervised Go-heavy commands ---------------------------------
for i in $(seq 1 "$N"); do
  TOKEN=$(cat /proc/sys/kernel/random/uuid)
  CTL="$LOGDIR/m1598-ctl-$TOKEN"
  EVF="$LOGDIR/m1598-ev-$TOKEN"
  mkfifo "$CTL" "$EVF"
  exec {CTLRW}<>"$CTL"
  exec {EVRW}<>"$EVF"
  EVLOG="$LOGDIR/m1598-evlog-$TOKEN"
  : > "$EVLOG"
  cat <&"$EVRW" >> "$EVLOG" &
  CATPID=$!
  CMDOUT="$LOGDIR/m1598-cmdout-$TOKEN"
  : > "$CMDOUT"

  # The child argv element right after cmdsandbox's own `--` must itself begin
  # with an absolute path (cmdsandbox's parseArgs rejects anything else), so
  # env vars go on the OUTER `env` in front of the whole setpriv+supervisor
  # pipeline: the supervisor's launchChild forks the sandboxed child with
  # os.Environ() (its own inherited env), which the sandbox process in turn
  # hands unchanged to the model command (cmd.Env = os.Environ() there too).
  SANDBOX_ARGS=(--root "$WORKROOT" --tmp "/tmp/uzi-codex-command-$TOKEN" --cwd "$WORKROOT")
  # GOPATH is explicitly pinned under the ephemeral HOME, NOT left at the
  # golang:alpine image's baked-in container env GOPATH=/go: that fixed path
  # would otherwise give every command -- base AND branch -- a free, silently
  # shared, never-measured module cache regardless of --cache, which would
  # hide exactly the boundary this script exists to measure. GOMODCACHE/
  # GOCACHE below (branch only) then override the GOPATH-derived defaults.
  RUN_ENV=(env "HOME=/tmp/uzi-codex-command-$TOKEN" "TMPDIR=/tmp/uzi-codex-command-$TOKEN" \
    "GOPATH=/tmp/uzi-codex-command-$TOKEN/go" "GOTOOLCHAIN=local")
  if [ -n "$RUN_CACHE_DIR" ]; then
    SANDBOX_ARGS+=(--cache "$RUN_CACHE_DIR")
    RUN_ENV+=("GOMODCACHE=$RUN_CACHE_DIR/gomod" "GOCACHE=$RUN_CACHE_DIR/gocache" "npm_config_cache=$RUN_CACHE_DIR/npm")
  fi
  SANDBOX_ARGS+=(--mode best-effort --)

  START_NS=$(date +%s%N)
  "${RUN_ENV[@]}" "${SETPRIV_BASE[@]}" /usr/local/bin/uzi-codex-supervisor --expect-uid 10003 --cleanup-token "$TOKEN" -- \
    /usr/local/bin/uzi-codex-command-sandbox "${SANDBOX_ARGS[@]}" \
    /bin/sh -c 'cd api && go build ./... && go test -count=1 -run XXX_none ./...' \
    3<"$CTL" 4>"$EVF" >"$CMDOUT" 2>&1 &
  SUPPID=$!

  # Wait for the child to exit (bounded by a generous 1800*0.2s = 6 minutes),
  # then drive dispose over fd3. Deliberately does NOT also break early on
  # `kill -0 "$SUPPID"` failing: that raced true during development (before
  # the evidence line was actually flushed/grepped) and made this loop send
  # dispose while the model command -- a real `go build`/`go test` -- was
  # still running, which then shows up as a force-killed dispose and a
  # falsely tiny duration. child_exit in the evidence log is the only signal
  # that means the command actually finished.
  CHILD_SEEN=0
  for _ in $(seq 1 1800); do
    if grep -q '"event":"child_exit"' "$EVLOG" 2>/dev/null; then CHILD_SEEN=1; break; fi
    sleep 0.2
  done
  printf '{"op":"dispose","id":1,"timeoutMs":5000}\n' >&"$CTLRW" || true
  set +e
  wait "$SUPPID"
  SUP_RC=$?
  set -e
  END_NS=$(date +%s%N)
  DUR_MS=$(( (END_NS - START_NS) / 1000000 ))

  kill "$CATPID" 2>/dev/null || true
  wait "$CATPID" 2>/dev/null || true
  exec {CTLRW}>&- || true
  exec {EVRW}>&- || true

  DOWNLOAD_LINES=$(grep -c '^go: downloading' "$CMDOUT" 2>/dev/null || true)
  DOWNLOAD_LINES=${DOWNLOAD_LINES:-0}
  # `|| true` on both pipelines below: under `set -euo pipefail`, an empty
  # grep match (exit 1) propagates as the pipeline's own exit status even
  # though the downstream `tail`/`tr`/second `grep` succeed (pipefail reports
  # the last NON-ZERO stage, not simply the rightmost stage), which would
  # otherwise abort this whole script. An empty match here is not an error --
  # it happens when the supervisor had already exited without a "dispose"
  # line (an abnormal exit writes "abnormal" instead), when the command was
  # killed by dispose before it exited on its own (no "child_exit"), or if
  # the evidence copier was stopped before its last line landed -- so both
  # fields must still resolve to JSON `null` rather than kill the run.
  DISPOSE_LINE=$(grep '"event":"dispose"' "$EVLOG" 2>/dev/null | tail -1 | tr -d '\n' || true)
  # The command's own exit code (doc.go: {"event":"child_exit","code":<int>}),
  # pulled out of the evidence log BEFORE it is deleted below. Recorded per
  # command so a fast run can never silently pass for a warm cache hit when
  # it was in fact a fast failure -- duration and download-line-count alone
  # cannot distinguish the two. `null` when no child_exit line was ever
  # observed (CHILD_SEEN=0, or the wait loop timed out and dispose killed the
  # child before it produced one).
  CHILD_EXIT_CODE=$(grep -o '"event":"child_exit","code":-\{0,1\}[0-9]\{1,\}' "$EVLOG" 2>/dev/null \
    | tail -1 | grep -o -- '-\{0,1\}[0-9]\{1,\}$' || true)
  TMPB=$(tmp_bytes)
  CMDDIRS=$(tmp_cmd_dirs)
  CACHEB=$(cache_dir_bytes)
  if [ "$CACHEB" -gt "$PEAK_CACHE" ]; then PEAK_CACHE=$CACHEB; fi

  DISPOSE_JSON=$(safe_embed_json "${DISPOSE_LINE:-}")

  log "cmd $i/$N: child_seen=$CHILD_SEEN sup_rc=$SUP_RC child_exit_code=${CHILD_EXIT_CODE:-null} dur_ms=$DUR_MS tmp_bytes=$TMPB cmd_dirs=$CMDDIRS cache_bytes=$CACHEB downloads=$DOWNLOAD_LINES"
  emit "{\"event\":\"command\",\"i\":$i,\"child_seen\":$CHILD_SEEN,\"sup_rc\":$SUP_RC,\"child_exit_code\":${CHILD_EXIT_CODE:-null},\"dur_ms\":$DUR_MS,\"tmp_bytes\":$TMPB,\"tmp_cmd_dirs\":$CMDDIRS,\"cache_bytes\":$CACHEB,\"downloading_lines\":$DOWNLOAD_LINES,\"dispose\":$DISPOSE_JSON}"

  rm -f "$CTL" "$EVF" "$EVLOG" "$CMDOUT"
done

# ---- 6. release the cache holder, record retention -------------------------
if [ "$HAS_CACHE" = "1" ]; then
  printf '{"op":"release","drained":true}\n' >&"$HOLDRELW"
  exec {HOLDRELW}>&-
  set +e
  wait "$HOLD_PID"
  HOLD_RC=$?
  set -e
  sleep 0.2
  HOLD_LAST=$(tail -1 "$HOLDLOG" | tr -d '\n')
  HOLD_LAST_JSON=$(safe_embed_json "$HOLD_LAST")
  CACHE_REMAINS=0
  [ -d "$RUN_CACHE_DIR" ] && CACHE_REMAINS=1
  log "cache holder released: rc=$HOLD_RC remains=$CACHE_REMAINS last_line=$HOLD_LAST"
  if [ -s "$HOLDERR" ]; then
    log "cache holder stderr (not parsed, for diagnosis only):"
    cat "$HOLDERR" >&2
  fi
  emit "{\"event\":\"cache_release\",\"rc\":$HOLD_RC,\"remains\":$CACHE_REMAINS,\"holder_line\":$HOLD_LAST_JSON}"
  rm -f "$HOLD_RELFIFO" "$HOLDLOG" "$HOLDERR"
fi

FINAL_TMPB=$(tmp_bytes)
FINAL_CMDDIRS=$(tmp_cmd_dirs)
log "final: tmp_bytes=$FINAL_TMPB cmd_dirs=$FINAL_CMDDIRS peak_cache_bytes=$PEAK_CACHE"
emit "{\"event\":\"final\",\"tmp_bytes\":$FINAL_TMPB,\"tmp_cmd_dirs\":$FINAL_CMDDIRS,\"peak_cache_bytes\":$PEAK_CACHE}"
