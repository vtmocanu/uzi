#!/bin/sh
# PRD #1156 M3a — in-container image-profile validation harness.
#
# Runs as the WORKER uid (10001) after the REAL root entrypoint established the A1 uid
# split (UZI_UID_SPLIT=1) on the ACTUAL hardened worker image: writable root filesystem,
# NO --read-only, NO --user override, so the on-disk ownership this test must prove is the
# real production posture. It drops to the untrusted RUNNER uid (10002) through the exact
# production setpriv wrapper (agent/src/runner-uid.ts setprivRunnerArgs) wherever a control
# must act as the untrusted uid. See README.md for what each control proves.
#
# It never edits the launcher/supervisor and bakes no test-only escape hatch into them;
# every control drives the ACTUAL packaged launcher (launch-cli / launchCodexRoot) and the
# ACTUAL static root-owned supervisor, or reads real on-disk ownership.
#
# Usage: controls.sh main   (controls 1, 2a, 3, 4, 5 — hardened, no-new-privileges ON)
#        controls.sh nnp    (control 2b — a container variant WITHOUT no-new-privileges)
set -u

MODE="${1:-main}"

SUP=/usr/local/bin/uzi-codex-supervisor
NODE=/usr/local/bin/node
STUB=/m3a/.bin/stub-child
EVQ=/m3a/evq.mjs
TSX=/app/node_modules/.bin/tsx
LAUNCH_CLI=/app/src/codex/launch-cli.ts
SETPRIV=/bin/setpriv
DATA=/data/runner/m3a
WTMP="${TMPDIR:-/tmp}"

PASS=0
FAIL=0
CLI_PID=""
HOLDER_PID=""
CLI_ERR=""
CLI_OUT=""
FIFO=""

hdr()  { printf '\n========== %s ==========\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '  [PASS] %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '  [FAIL] %s\n' "$*"; }
note() { printf '  [note] %s\n' "$*"; }

# Drop to the untrusted runner uid via the SAME setpriv argv the production launcher uses
# (runner-uid.ts setprivRunnerArgs): reuid/regid runner, init-groups, clear inheritable +
# ambient caps (the bounding-set clear is a documented best-effort no-op without SETPCAP).
as_runner() {
  "$SETPRIV" --reuid runner --regid runner --init-groups \
    --bounding-set -all --inh-caps -all --ambient-caps -all -- "$@"
}

# ---- launch-cli lifecycle helpers -------------------------------------------------------
# Launch the REAL packaged launcher over a held-open fifo so the run is disposed on a
# controlled EOF (the production shutdown-on-EOF path), not immediately.
launch_cli() {
  _spec="$1"; _tag="$2"
  FIFO="$WTMP/m3a-$_tag.fifo"
  CLI_ERR="$WTMP/m3a-$_tag.err"
  CLI_OUT="$WTMP/m3a-$_tag.out"
  rm -f "$FIFO" "$CLI_ERR" "$CLI_OUT" 2>/dev/null || true
  mkfifo "$FIFO"
  ( cd /app && exec "$TSX" "$LAUNCH_CLI" "$_spec" ) <"$FIFO" >"$CLI_OUT" 2>"$CLI_ERR" &
  CLI_PID=$!
  ( exec sleep 3600 ) >"$FIFO" &
  HOLDER_PID=$!
}

# Poll launch-cli's stderr until it emits `ready`, or fails (`spec-error`/`launch-failed`),
# or the process dies. Returns 0 on ready.
wait_ready() {
  _i=0
  while [ "$_i" -lt 40 ]; do
    if grep -q '"event":"ready"' "$CLI_ERR" 2>/dev/null; then return 0; fi
    if grep -qE '"event":"(spec-error|launch-failed)"' "$CLI_ERR" 2>/dev/null; then return 1; fi
    if ! kill -0 "$CLI_PID" 2>/dev/null; then return 1; fi
    sleep 1
    _i=$((_i + 1))
  done
  return 1
}

# Close the held-open stdin (EOF => launch-cli disposes) and wait for exit, bounded by a
# watchdog so a hang fails the control instead of stalling the run.
dispose_cli() {
  kill "$HOLDER_PID" 2>/dev/null || true
  ( sleep 45; kill -TERM "$CLI_PID" 2>/dev/null; sleep 3; kill -KILL "$CLI_PID" 2>/dev/null ) &
  _wd=$!
  wait "$CLI_PID" 2>/dev/null || true
  kill "$_wd" 2>/dev/null || true
  rm -f "$FIFO" 2>/dev/null || true
}

# Poll (as runner) for a marker file to appear. $1 path, $2 max seconds.
wait_marker() {
  _i=0
  while [ "$_i" -lt "$2" ]; do
    if as_runner test -e "$1"; then return 0; fi
    sleep 1
    _i=$((_i + 1))
  done
  return 1
}

# True if $1 appears as a whitespace-separated word in the set $2.
in_set() {
  for _x in $2; do
    [ "$_x" = "$1" ] && return 0
  done
  return 1
}

# Write a TRUSTED launch spec. $1 path $2 kind $3 markerDir(=ownedDataRoot base) $4 stubMode
# $5 cwd $6 credential(empty => omit, for kind=command).
write_spec() {
  _p="$1"; _kind="$2"; _md="$3"; _mode="$4"; _cwd="$5"; _cred="$6"
  _credline=""
  [ -n "$_cred" ] && _credline=", \"credentialValue\": \"$_cred\""
  cat > "$_p" <<EOF
{
  "ownedDataRoot": "$_md/run",
  "provider": { "name": "fakeprov", "baseUrl": "http://127.0.0.1:9/v1", "envKey": "FAKE_PROVIDER_API_KEY"$_credline },
  "model": "gpt-5-codex",
  "codexBin": "$STUB",
  "supervisorBin": "$SUP",
  "kind": "$_kind",
  "childArgv": ["$_mode", "$_md"],
  "cwd": "$_cwd"
}
EOF
}

# ---- Control 1: channel nondumpability (fd3/fd4) + positive control ---------------------
control_1() {
  hdr "Control 1: channel boundary (nondumpability of supervisor fd3/fd4) + positive control"
  _md="$DATA/c1"; as_runner rm -rf "$_md" 2>/dev/null; as_runner mkdir -p "$_md"
  _cwd="$DATA/c1-cwd"; as_runner mkdir -p "$_cwd"

  # POSITIVE CONTROL: a default-dumpable runner process P holds fd 7 open on a readable
  # file; ANOTHER same-uid runner process CAN read /proc/<P>/fd/7. This proves the later
  # denial is due to nondumpability, not a blanket /proc restriction.
  # P self-exits after a short sleep (no kill+wait, which would print a job "Terminated"
  # notice); it stays alive well past the immediate fd-read probe below.
  as_runner sh -c 'exec 7</etc/os-release; echo "$$" > "$1/P.pid"; : > "$1/P.ready"; exec sleep 20' sh "$_md" &
  if wait_marker "$_md/P.ready" 20; then
    _P="$(as_runner cat "$_md/P.pid" 2>/dev/null)"
    _pc="$(as_runner sh -c 'cat "/proc/$1/fd/7" 2>/dev/null | head -1' sh "$_P")"; _prc=$?
    if [ "$_prc" -eq 0 ] && [ -n "$_pc" ]; then
      ok "positive control: same-uid runner READ /proc/$_P/fd/7 (default dumpable): '$_pc'"
    else
      bad "positive control failed to read /proc/$_P/fd/7 (rc=$_prc content='$_pc')"
    fi
  else
    bad "positive-control process P did not start"
    _P=""
  fi

  # NEGATIVE: launch the supervisor via the real launcher with a probe-fd stub child.
  _spec="$WTMP/spec1.json"
  write_spec "$_spec" command "$_md" probe-fd "$_cwd" ""
  launch_cli "$_spec" c1
  if ! wait_ready; then
    bad "control 1: launch-cli did not reach ready"
    note "launch-cli stderr: $(as_runner cat "$CLI_ERR" 2>/dev/null)"
    dispose_cli
    [ -n "$_P" ] && as_runner kill "$_P" 2>/dev/null
    return
  fi
  _sup="$("$NODE" "$EVQ" "$CLI_ERR" ready supervisorPid 2>/dev/null)"

  # Stub-side probe (the untrusted child itself attempts the forge).
  if wait_marker "$_md/probe-done" 20; then
    _probe="$(as_runner cat "$_md/probe.txt" 2>/dev/null)"
    if printf '%s' "$_probe" | grep -q 'fd3 rc=1' \
      && printf '%s' "$_probe" | grep -q 'fd4 rc=1' \
      && printf '%s' "$_probe" | grep -qi 'permission denied' \
      && ! printf '%s' "$_probe" | grep -q 'rc=0'; then
      ok "stub child (runner) DENIED /proc/$_sup/fd/{3,4}"
      note "$(printf '%s' "$_probe" | tr '\n' '|')"
    else
      bad "stub probe result unexpected: $(printf '%s' "$_probe" | tr '\n' '|')"
    fi
  else
    bad "control 1: stub probe never completed"
  fi

  # Independent side-probe as ANOTHER runner process (belt and braces).
  _s3="$(as_runner sh -c 'cat "/proc/$1/fd/3" 2>&1 >/dev/null' sh "$_sup")"; _s3rc=$?
  _s4="$(as_runner sh -c 'cat "/proc/$1/fd/4" 2>&1 >/dev/null' sh "$_sup")"; _s4rc=$?
  if [ "$_s3rc" -ne 0 ] && [ "$_s4rc" -ne 0 ] && printf '%s%s' "$_s3" "$_s4" | grep -qi 'permission denied'; then
    ok "independent runner side-probe DENIED fd3 and fd4 (nondumpable supervisor /proc)"
    note "fd3='$_s3' fd4='$_s4'"
  else
    bad "side-probe not denied: fd3 rc=$_s3rc [$_s3] fd4 rc=$_s4rc [$_s4]"
  fi

  dispose_cli
  _clean="$("$NODE" "$EVQ" "$CLI_ERR" final clean 2>/dev/null)"
  [ "$_clean" = "true" ] && ok "control 1 disposed clean (drained)" || bad "control 1 dispose not clean (final: $(grep -o '"event":"final".*' "$CLI_ERR" 2>/dev/null))"
}

# ---- Control 2 helper: drive the supervisor DIRECTLY as runner ---------------------------
# Runs the supervisor with fd3</dev/null (control) and fd4>evidence.json (evidence). exec
# in the inner sh so the setpriv exit code IS the supervisor exit code. Sets _prof_exit.
_prof_run() {
  _uid="$1"; _md="$2"; _flag="${3:-}"
  as_runner rm -rf "$_md" 2>/dev/null; as_runner mkdir -p "$_md"
  as_runner sh -c '
    sup="$1"; uid="$2"; stub="$3"; md="$4"; flag="$5"
    if [ -n "$flag" ]; then
      exec "$sup" "$flag" --expect-uid "$uid" -- "$stub" marker "$md" 3</dev/null 4>"$md/evidence.json"
    else
      exec "$sup" --expect-uid "$uid" -- "$stub" marker "$md" 3</dev/null 4>"$md/evidence.json"
    fi
  ' sh "$SUP" "$_uid" "$STUB" "$_md" "$_flag"
  _prof_exit=$?
}

# ---- Control 2a: profile fail-before-fork on wrong --expect-uid, no bypass ---------------
control_2a() {
  hdr "Control 2a: profile fail-before-fork on WRONG --expect-uid (no child, no bypass)"
  _md="$DATA/c2a"
  _prof_run 9999 "$_md"
  _ev="$(as_runner cat "$_md/evidence.json" 2>/dev/null)"
  case "$_ev" in
    *'"reason":"profile:uid"'*) ok "wrong --expect-uid 9999 -> abnormal profile:uid" ;;
    *) bad "expected profile:uid abnormal, got: $_ev" ;;
  esac
  note "evidence: $_ev"
  [ "$_prof_exit" -ne 0 ] && ok "supervisor exited non-zero ($_prof_exit)" || bad "supervisor exit was 0"
  if as_runner test -e "$_md/launched"; then bad "stub 'launched' marker present: a child WAS forked"; else ok "no child forked (fail-before-fork; 'launched' marker absent)"; fi

  # No bypass: an unknown flag is REJECTED as invalid arguments, never honored.
  _md2="$DATA/c2a-flag"
  _prof_run 10002 "$_md2" --disable-profile
  _ev2="$(as_runner cat "$_md2/evidence.json" 2>/dev/null)"
  case "$_ev2" in
    *'invalid arguments'*) ok "unknown flag --disable-profile REJECTED (invalid arguments): no profile-disable flag exists" ;;
    *) bad "expected invalid-arguments rejection, got: $_ev2" ;;
  esac
  if as_runner test -e "$_md2/launched"; then bad "child forked under --disable-profile"; else ok "no child forked under --disable-profile"; fi
}

# ---- Control 2b: NoNewPrivs=0 container -> fail-closed -----------------------------------
control_nnp() {
  hdr "Control 2b: NoNewPrivs=0 container -> supervisor fails-closed (profile:noNewPrivs)"
  _nnp="$(grep -i 'NoNewPrivs' /proc/self/status 2>/dev/null || true)"
  note "container /proc/self/status -> ${_nnp:-<none>}"
  _md="$DATA/c2b"
  _prof_run 10002 "$_md"
  _ev="$(as_runner cat "$_md/evidence.json" 2>/dev/null)"
  case "$_ev" in
    *'"reason":"profile:noNewPrivs"'*) ok "correct uid but NoNewPrivs=0 -> abnormal profile:noNewPrivs" ;;
    *) bad "expected profile:noNewPrivs abnormal, got: $_ev" ;;
  esac
  note "evidence: $_ev"
  [ "$_prof_exit" -ne 0 ] && ok "supervisor exited non-zero ($_prof_exit) fail-closed" || bad "supervisor exit was 0"
  if as_runner test -e "$_md/launched"; then bad "stub 'launched' marker present: a child WAS forked"; else ok "no child forked (fail-before-fork)"; fi
}

# ---- Control 3: immutability by OWNERSHIP (runner uid 10002) -----------------------------
deny_write() {
  _t="$1"
  _o="$(stat -c '%U:%G %a' "$_t" 2>/dev/null || echo '?')"
  as_runner sh -c 'echo x >> "$1"' sh "$_t" 2>/dev/null; _a=$?
  as_runner sh -c 'chmod u+w "$1"' sh "$_t" 2>/dev/null; _c=$?
  as_runner sh -c 'cp /bin/true "$1"' sh "$_t" 2>/dev/null; _r=$?
  as_runner sh -c 'rm -f "$1"' sh "$_t" 2>/dev/null; _d=$?
  if [ "$_a" -ne 0 ] && [ "$_c" -ne 0 ] && [ "$_r" -ne 0 ] && [ "$_d" -ne 0 ] && [ -e "$_t" ]; then
    ok "runner DENIED append/chmod/replace/rm of $_t ($_o)"
  else
    _ex=n; [ -e "$_t" ] && _ex=y
    bad "runner modified $_t ($_o): append=$_a chmod=$_c replace=$_r rm=$_d stillExists=$_ex"
  fi
}

control_3() {
  hdr "Control 3: immutability by OWNERSHIP (runner uid 10002)"
  deny_write "$SUP"
  deny_write "$NODE"
  deny_write /app/src/codex/launcher.ts
  _nm="$(find /app/node_modules -maxdepth 3 -type f 2>/dev/null | head -1)"
  if [ -n "$_nm" ]; then deny_write "$_nm"; else bad "no /app/node_modules file found to probe"; fi

  # CONTRAST: the /nix interpreter is RUNNER-OWNED and thus replaceable by the untrusted
  # uid — which is exactly why it cannot be the trust anchor and the static root-owned
  # supervisor is required.
  _py="$(readlink -f /opt/uzi-toolchain/bin/python3 2>/dev/null || true)"
  if [ -z "$_py" ] || ! as_runner test -f "$_py"; then
    _py="$(as_runner sh -c 'for f in /opt/uzi-toolchain/bin/*; do t=$(readlink -f "$f"); [ -f "$t" ] && { echo "$t"; break; }; done' 2>/dev/null)"
  fi
  if [ -z "$_py" ]; then
    bad "could not resolve a /nix toolchain interpreter to contrast against"
    return
  fi
  _po="$(stat -c '%U:%G %a' "$_py" 2>/dev/null || echo '?')"
  _owned=n; as_runner test -O "$_py" && _owned=y
  _supowned=n; as_runner test -O "$SUP" && _supowned=y
  _before="$(as_runner sha256sum "$_py" 2>/dev/null | cut -d' ' -f1)"
  _mode="$(stat -c '%a' "$_py" 2>/dev/null)"
  as_runner chmod u+w "$_py" 2>/dev/null; _chm=$?
  _nowW=n; as_runner test -w "$_py" && _nowW=y
  as_runner chmod "$_mode" "$_py" 2>/dev/null
  _after="$(as_runner sha256sum "$_py" 2>/dev/null | cut -d' ' -f1)"
  as_runner sh -c '_p="/nix/.uzi-m3a-writeprobe.$$"; echo probe > "$_p" && rm -f "$_p"' 2>/dev/null; _nixw=$?
  if [ "$_owned" = y ] && [ "$_supowned" = n ] && [ "$_chm" -eq 0 ] && [ "$_nowW" = y ] \
    && [ -n "$_before" ] && [ "$_before" = "$_after" ] && [ "$_nixw" -eq 0 ]; then
    ok "runner OWNS the /nix interpreter $_py ($_po): can chmod it writable (round-trip, sha256 unchanged) and can write under /nix; supervisor is NOT runner-owned"
    note "=> the /nix interpreter cannot be the trust anchor; the static root-owned supervisor is required"
  else
    bad "nix contrast unexpected: py=$_py owned=$_owned supOwned=$_supowned chmod=$_chm nowWritable=$_nowW before=$_before after=$_after nixWrite=$_nixw"
  fi
}

# ---- Control 4: disposal via ECHILD+__WALL reaps a differently-grouped grandchild --------
control_4() {
  hdr "Control 4: disposal via ECHILD+__WALL reaps a differently-grouped grandchild"
  _md="$DATA/c4"; as_runner rm -rf "$_md" 2>/dev/null; as_runner mkdir -p "$_md"
  _cwd="$DATA/c4-cwd"; as_runner mkdir -p "$_cwd"
  _spec="$WTMP/spec4.json"
  write_spec "$_spec" command "$_md" grandchild "$_cwd" ""
  launch_cli "$_spec" c4
  if ! wait_ready; then
    bad "control 4: launch-cli did not reach ready"
    note "launch-cli stderr: $(as_runner cat "$CLI_ERR" 2>/dev/null)"
    dispose_cli
    return
  fi
  _C="$("$NODE" "$EVQ" "$CLI_ERR" ready started.childPid 2>/dev/null)"
  if ! wait_marker "$_md/grandchild.ready" 20; then bad "control 4: grandchild never started"; dispose_cli; return; fi
  _G="$(as_runner cat "$_md/grandchild.pid" 2>/dev/null)"
  _cpg="$(as_runner cat "$_md/child.pgid" 2>/dev/null)"
  _gpg="$(as_runner cat "$_md/grandchild.pgid" 2>/dev/null)"
  if [ -n "$_C" ] && [ -n "$_G" ] && [ "$_cpg" != "$_gpg" ]; then
    ok "child pid=$_C pgid=$_cpg ; grandchild pid=$_G pgid=$_gpg (DIFFERENT process groups)"
  else
    bad "process tree setup unexpected: C=$_C G=$_G childPgid=$_cpg grandchildPgid=$_gpg"
  fi

  dispose_cli
  _state="$("$NODE" "$EVQ" "$CLI_ERR" final dispose.state 2>/dev/null)"
  _auth="$("$NODE" "$EVQ" "$CLI_ERR" final dispose.authority 2>/dev/null)"
  _reaped="$("$NODE" "$EVQ" "$CLI_ERR" final dispose.reaped 2>/dev/null)"
  _clean="$("$NODE" "$EVQ" "$CLI_ERR" final clean 2>/dev/null)"
  [ "$_state" = "drained" ] && ok "dispose state=drained" || bad "dispose state=$_state (want drained)"
  [ "$_auth" = "ECHILD+__WALL" ] && ok "authority=ECHILD+__WALL" || bad "authority=$_auth (want ECHILD+__WALL)"
  note "reaped=[$_reaped]"
  if in_set "$_C" "$_reaped"; then ok "reaped includes the child ($_C)"; else bad "child $_C NOT in reaped [$_reaped]"; fi
  if in_set "$_G" "$_reaped"; then ok "reaped includes the differently-grouped grandchild ($_G)"; else bad "grandchild $_G NOT in reaped [$_reaped]"; fi
  [ "$_clean" = "true" ] && ok "control 4 clean disposal (supervisor exit 0)" || bad "control 4 not clean"
  if as_runner kill -0 "$_C" 2>/dev/null; then bad "child $_C survived dispose"; else ok "child $_C gone after dispose"; fi
  if as_runner kill -0 "$_G" 2>/dev/null; then bad "grandchild $_G survived dispose"; else ok "grandchild $_G gone after dispose"; fi
}

# ---- Control 5: fresh per-launch HOME/CODEX_HOME/XDG/TMPDIR owned by runner 0700 + the
#      env-delivery allowlist actually reaching the child (fail-closed regression guard) ----
control_5() {
  hdr "Control 5: fresh per-launch trees owned by runner uid 10002 mode 0700 + env-delivery allowlist reaches the child"
  _md="$DATA/c5"; as_runner rm -rf "$_md" 2>/dev/null; as_runner mkdir -p "$_md"
  _cwd="$DATA/c5-cwd"; as_runner mkdir -p "$_cwd"
  _cred="sk-m3a-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  M3A_ENV_CANARY="leak-canary-must-not-reach-child"; export M3A_ENV_CANARY
  _spec="$WTMP/spec5.json"
  write_spec "$_spec" provider "$_md" env "$_cwd" "$_cred"
  launch_cli "$_spec" c5
  if ! wait_ready; then
    bad "control 5: launch-cli did not reach ready"
    note "launch-cli stderr: $(as_runner cat "$CLI_ERR" 2>/dev/null)"
    dispose_cli
    return
  fi
  # The mandate is "owned by uid 10002 and mode 0700" = runner-owned, OWNER-ONLY access.
  # The trees also carry an inherited setgid bit (raw mode 2700) because the chosen
  # ownedDataRoot sits under the production setgid /data/runner (2775) — a benign
  # inheritance that does NOT widen access (group/other still get nothing). Assert the
  # owner-only 0700 access bits + uid and note the setgid separately.
  _odr="$_md/run"
  _setgid_seen=n
  for _d in home codex xdg-config xdg-cache xdg-data xdg-state tmp; do
    _u="$(as_runner stat -c '%u' "$_odr/$_d" 2>/dev/null || echo NA)"
    _perm="$(as_runner stat -c '%a' "$_odr/$_d" 2>/dev/null || echo NA)"
    _low="${_perm#"${_perm%???}"}"
    [ "${#_perm}" -eq 4 ] && _setgid_seen=y
    if [ "$_u" = "10002" ] && [ "$_low" = "700" ]; then
      ok "$_d -> uid 10002, owner-only 0700 (raw mode $_perm)"
    else
      bad "$_d uid='$_u' mode='$_perm' (want uid 10002, owner-only 700)"
    fi
  done
  [ "$_setgid_seen" = y ] && note "trees carry an inherited setgid bit (raw 2700) from the setgid /data/runner parent (2775); access stays owner-only 0700"
  _cu="$(as_runner stat -c '%u' "$_odr/codex/config.toml" 2>/dev/null || echo NA)"
  _cp="$(as_runner stat -c '%a' "$_odr/codex/config.toml" 2>/dev/null || echo NA)"
  { [ "$_cu" = "10002" ] && [ "$_cp" = "600" ]; } && ok "config.toml -> uid 10002 mode 0600" || bad "config.toml uid='$_cu' mode='$_cp' (want uid 10002 mode 600)"

  # ── Env-delivery regression guard (fail-closed) ──────────────────────────────────────
  # With the supervisor fix (launchChild sets Env: os.Environ(), and setpriv passed the
  # launcher's replaced-env allowlist through to the supervisor unchanged), the child must
  # observe EXACTLY the allowlist + the provider credential and NOTHING from the host/worker.
  # This drove `launch-cli` above with kind:"provider" + a dummy credential and a stub child
  # in `env` mode; assert the dumped child environment rather than merely noting its count.
  _expect_keys="HOME CODEX_HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME TMPDIR PATH SHELL LANG TERM FAKE_PROVIDER_API_KEY"
  if wait_marker "$_md/env-done" 20; then
    _cnt="$(as_runner cat "$_md/child-env-count" 2>/dev/null || echo 0)"
    _envtxt="$(as_runner cat "$_md/child-env.txt" 2>/dev/null || true)"
    _keys="$(printf '%s\n' "$_envtxt" | grep -v '^[[:space:]]*$' | cut -d= -f1 | sort)"

    # (a) NON-EMPTY: the fix delivers env (pre-fix the child saw an empty env, count 0).
    if [ "$_cnt" -gt 0 ] 2>/dev/null && [ -n "$_envtxt" ]; then
      ok "child env NON-EMPTY: $_cnt vars (pre-fix the child saw 0 -> the fix forwards the allowlist)"
    else
      bad "child env EMPTY (count='$_cnt'): the supervisor did NOT forward the allowlist to the child"
    fi

    # (b) EXACTLY the allowlist keys — every expected key present + non-empty, and no extras.
    _missing=""; _emptyval=""
    for _k in $_expect_keys; do
      if printf '%s\n' "$_keys" | grep -qx "$_k"; then
        _v="$(printf '%s\n' "$_envtxt" | grep "^$_k=" | head -1 | cut -d= -f2-)"
        [ -n "$_v" ] || _emptyval="$_emptyval $_k"
      else
        _missing="$_missing $_k"
      fi
    done
    _extra=""
    for _k in $_keys; do
      in_set "$_k" "$_expect_keys" || _extra="$_extra $_k"
    done
    if [ -z "$_missing" ] && [ -z "$_emptyval" ] && [ -z "$_extra" ]; then
      ok "child env keys are EXACTLY the allowlist: $(printf '%s' "$_keys" | tr '\n' ' ')"
    else
      bad "child env key set mismatch: missing=[$_missing ] empty=[$_emptyval ] unexpected=[$_extra ]"
    fi

    # (c) HOME/CODEX_HOME point INSIDE the fresh uid-10002 0700 trees this control verified.
    _home_v="$(printf '%s\n' "$_envtxt" | grep '^HOME=' | head -1 | cut -d= -f2-)"
    _codex_v="$(printf '%s\n' "$_envtxt" | grep '^CODEX_HOME=' | head -1 | cut -d= -f2-)"
    if [ "$_home_v" = "$_odr/home" ] && [ "$_codex_v" = "$_odr/codex" ]; then
      ok "HOME=$_home_v and CODEX_HOME=$_codex_v point into the fresh uid-10002 0700 trees"
    else
      bad "HOME/CODEX_HOME are not the fresh trees: HOME='$_home_v' (want $_odr/home) CODEX_HOME='$_codex_v' (want $_odr/codex)"
    fi

    # (d) The provider credential reached the child verbatim (== the dummy value).
    _credval="$(printf '%s\n' "$_envtxt" | grep '^FAKE_PROVIDER_API_KEY=' | head -1 | cut -d= -f2-)"
    if [ "$_credval" = "$_cred" ]; then
      ok "provider credential FAKE_PROVIDER_API_KEY delivered to child (== the dummy value)"
    else
      bad "provider credential mismatch: child got '$_credval' want '$_cred'"
    fi

    # (e) NO host/worker leak: named-forbidden keys + wildcard families must ALL be absent.
    _leak=""
    for _fk in UZI_WORKER_TOKEN UZI_WORKER_TOKEN_FILE NODE_OPTIONS M3A_ENV_CANARY; do
      printf '%s\n' "$_keys" | grep -qx "$_fk" && _leak="$_leak $_fk"
    done
    printf '%s\n' "$_keys" | grep -qi '^ANTHROPIC' && _leak="$_leak ANTHROPIC*"
    printf '%s\n' "$_keys" | grep -qE '_PAT$' && _leak="$_leak *_PAT"
    printf '%s\n' "$_keys" | grep -qE '(^|_)(GH|GITHUB|GITLAB|GITEA|FORGEJO)_TOKEN$' && _leak="$_leak forge-token"
    if [ -z "$_leak" ]; then
      ok "no host/worker leak: UZI_WORKER_TOKEN{,_FILE}, ANTHROPIC*, *_PAT, forge tokens, NODE_OPTIONS, canary all ABSENT"
    else
      bad "host/worker env LEAKED into child:$_leak"
    fi

    note "child env observed: $(printf '%s' "$_keys" | tr '\n' ' ')"
  else
    bad "control 5: stub child never dumped its env ('env-done' marker absent)"
  fi

  dispose_cli
  _clean="$("$NODE" "$EVQ" "$CLI_ERR" final clean 2>/dev/null)"
  [ "$_clean" = "true" ] && ok "control 5 clean disposal (drained, supervisor exit 0)" || bad "control 5 not clean"
}

preflight() {
  hdr "preflight (mode=$MODE uid=$(id -u) gid=$(id -g) split=${UZI_UID_SPLIT:-unset})"
  _u="$(as_runner id -u 2>/dev/null)"
  [ "$_u" = "10002" ] && ok "production setpriv drop to runner works (uid $_u)" || bad "cannot drop to runner (got '$_u')"
  as_runner mkdir -p "$DATA" 2>/dev/null || true
  [ -x "$STUB" ] && ok "stub binary present ($STUB)" || bad "stub binary missing at $STUB"
  [ -x "$SUP" ]  && ok "static supervisor present ($SUP)" || bad "supervisor missing at $SUP"
  [ -x "$TSX" ]  && ok "tsx present ($TSX)" || bad "tsx missing at $TSX"
  [ -f "$LAUNCH_CLI" ] && ok "launch-cli present ($LAUNCH_CLI)" || bad "launch-cli missing at $LAUNCH_CLI"
}

hdr "PRD 1156 M3a container controls START (mode=$MODE)"
preflight
case "$MODE" in
  main)
    control_1
    control_2a
    control_3
    control_4
    control_5
    ;;
  nnp)
    control_nnp
    ;;
  *)
    echo "unknown mode: $MODE (want 'main' or 'nnp')" >&2
    exit 2
    ;;
esac
hdr "SUMMARY mode=$MODE PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
