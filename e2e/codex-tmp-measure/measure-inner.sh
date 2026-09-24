#!/bin/bash
# Runs INSIDE the m1598-img-<ref> container, as root, via `docker run --entrypoint
# /bin/bash`. It prepares the writable trees, detects whether this ref's
# supervisor supports the --hold-cache standalone mode, drives N supervised
# Go-heavy commands through the REAL uzi-codex-supervisor + uzi-codex-command-sandbox
# as uid 10003, and prints one NDJSON line of measurements per command plus a
# final summary line to STDOUT, one JSON object per line (NDJSON), with all
# human-readable progress on STDERR. run.sh captures the two separately.
#
# See e2e/codex-tmp-measure/README.md for what this measures and what it does
# NOT prove.
set -euo pipefail

N="${UZI_M1598_N:-5}"
log() { printf '%s\n' "$*" >&2; }

# emit writes one NDJSON result line to STDOUT (fd 1), kept strictly separate
# from the human-readable progress log() writes to STDERR. The two are always
# redirected to separate files by the caller (run.sh), never mixed on a
# terminal, because there is no shared host filesystem to bind-mount a result
# file out of (the docker CLI here talks to a remote/sibling daemon over
# DOCKER_HOST; only the CLI's own stdout/stderr streams are guaranteed to
# reach the invoking shell).
emit() { printf '%s\n' "$1"; }

SETPRIV_BASE=(setpriv --reuid 10003 --regid 10003 --init-groups --no-new-privs
  --bounding-set -all --inh-caps -all --ambient-caps -all --)

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
  HOLD_RELFIFO=/tmp/m1598-hold-release-$RUN_TOKEN
  mkfifo "$HOLD_RELFIFO"
  HOLDLOG=/tmp/m1598-hold-log-$RUN_TOKEN
  : > "$HOLDLOG"
  "${SETPRIV_BASE[@]}" /usr/local/bin/uzi-codex-supervisor --hold-cache \
    --expect-uid 10003 --cache-root "$CACHE_ROOT" --cache-token "$RUN_TOKEN" \
    <"$HOLD_RELFIFO" >"$HOLDLOG" 2>&1 &
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
  CTL=/tmp/m1598-ctl-$TOKEN
  EVF=/tmp/m1598-ev-$TOKEN
  mkfifo "$CTL" "$EVF"
  exec {CTLRW}<>"$CTL"
  exec {EVRW}<>"$EVF"
  EVLOG=/tmp/m1598-evlog-$TOKEN
  : > "$EVLOG"
  cat <&"$EVRW" >> "$EVLOG" &
  CATPID=$!
  CMDOUT=/tmp/m1598-cmdout-$TOKEN
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
  DISPOSE_LINE=$(grep '"event":"dispose"' "$EVLOG" 2>/dev/null | tail -1 | tr -d '\n')
  TMPB=$(tmp_bytes)
  CMDDIRS=$(tmp_cmd_dirs)
  CACHEB=$(cache_dir_bytes)
  if [ "$CACHEB" -gt "$PEAK_CACHE" ]; then PEAK_CACHE=$CACHEB; fi

  log "cmd $i/$N: child_seen=$CHILD_SEEN sup_rc=$SUP_RC dur_ms=$DUR_MS tmp_bytes=$TMPB cmd_dirs=$CMDDIRS cache_bytes=$CACHEB downloads=$DOWNLOAD_LINES"
  emit "{\"event\":\"command\",\"i\":$i,\"child_seen\":$CHILD_SEEN,\"sup_rc\":$SUP_RC,\"dur_ms\":$DUR_MS,\"tmp_bytes\":$TMPB,\"tmp_cmd_dirs\":$CMDDIRS,\"cache_bytes\":$CACHEB,\"downloading_lines\":$DOWNLOAD_LINES,\"dispose\":${DISPOSE_LINE:-null}}"

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
  CACHE_REMAINS=0
  [ -d "$RUN_CACHE_DIR" ] && CACHE_REMAINS=1
  log "cache holder released: rc=$HOLD_RC remains=$CACHE_REMAINS last_line=$HOLD_LAST"
  emit "{\"event\":\"cache_release\",\"rc\":$HOLD_RC,\"remains\":$CACHE_REMAINS,\"holder_line\":${HOLD_LAST:-null}}"
  rm -f "$HOLD_RELFIFO" "$HOLDLOG"
fi

FINAL_TMPB=$(tmp_bytes)
FINAL_CMDDIRS=$(tmp_cmd_dirs)
log "final: tmp_bytes=$FINAL_TMPB cmd_dirs=$FINAL_CMDDIRS peak_cache_bytes=$PEAK_CACHE"
emit "{\"event\":\"final\",\"tmp_bytes\":$FINAL_TMPB,\"tmp_cmd_dirs\":$FINAL_CMDDIRS,\"peak_cache_bytes\":$PEAK_CACHE}"
