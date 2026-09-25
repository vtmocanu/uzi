#!/bin/sh
# uzi worker container entrypoint (PRD #51 M2, mechanism A1; PRD #58 non-root start).
#
# On a ROOT start (the compose / A1 path) it runs a minimal root startup window and
# then drops to the unprivileged, credential-holding `worker` uid, retaining ONLY
# CAP_SETUID/CAP_SETGID as AMBIENT capabilities. The worker keeps those two caps for
# the run lifetime so it can later (M4) spawn the untrusted code-execution surfaces
# (SDK agent, self-improve checks, provision hooks) under the distinct, cap-less
# `runner` uid. The startup-only caps (CAP_SETPCAP/CAP_CHOWN/CAP_DAC_OVERRIDE, granted
# by the compose `cap_add`) are NOT carried across the drop: the setuid-to-non-root
# transition clears the permitted set and only the two ambient caps are re-raised, so
# CHOWN/DAC_OVERRIDE/SETPCAP are absent from the worker's permitted AND bounding sets
# afterwards (Decision 7). Verified on the built image via /proc/<pid>/status.
#
# On a NON-ROOT start (PRD #58: a hosted-k8s consumer runs this image in a PodSecurity
# RESTRICTED namespace with runAsUser: 10001 and no addable capabilities) the A1 root
# window is both unnecessary and impossible, so the entrypoint skips it and runs
# single-uid as the started user — see the branch below.
#
# Root-window hygiene (Decision 7 / audit M5): every command in the root window is an
# image-baked, root-owned binary invoked by ABSOLUTE PATH; PATH excludes the
# runner-writable volumes (/nix, /data) so root never resolves a binary from a volume;
# no untrusted env/arg is interpolated into a command; and the drop happens before
# anything resolves from a volume, regardless of volume contents.
#
# NOTE: the runtime cap set has NO CAP_FOWNER, so root can chmod a file only while it
# still owns it — hence the token block reclaims ownership to root before its chmod and
# hands it to `worker` after, and the B4 migration can otherwise only chown (not chmod)
# already-owned paths.
set -eu

# Absolute, root-owned, image-baked binaries (no /nix, no /data). Defined BEFORE the
# non-root branch so BOTH paths use absolute paths — never a volume-resolved binary,
# even here where PATH still has /nix first (a persisted /nix could carry a
# runner-planted trojan; resolving `id`/`tini` by absolute path avoids it).
ID=/usr/bin/id
TINI=/sbin/tini
SETPRIV=/bin/setpriv
CHOWN=/bin/chown
CHMOD=/bin/chmod
MKDIR=/bin/mkdir
RM=/bin/rm
FIND=/usr/bin/find

# --- PRD #58: tolerate a NON-ROOT start ---------------------------------------
# Started non-root (k8s runAsUser: 10001, PRD #58 single-uid v1)? Then there is no
# root window: the B4 volume migration is unnecessary (a fresh PVC + fsGroup, and the
# image-layer ownership /nix=worker:runner + /data=worker:worker already lets uid 10001
# write) and both the token chmod and the A1 setpriv drop need root/caps we do not have
# (an unconditional `setpriv --reuid` would EPERM -> CrashLoopBackOff). Run single-uid
# as the started user; tini stays PID 1 for clean SIGTERM. PATH is still the image's
# full worker PATH here (untouched below), so nix/devbox + the jvm JDK resolve. This
# does NOT weaken A1: the #51 uid-split containment applies on the ROOT-started
# (compose/A1) path; the non-root path is the #58 consumer's own accepted single-uid
# posture (the two-container split lands later via (C)).
# Read the uid via the ABSOLUTE id binary and validate it is a clean, non-empty number
# BEFORE branching, so BOTH paths fail CLOSED under `set -eu`: a failed or garbled `id`
# must NOT let a ROOT start slip into the non-root branch (which would run a root start
# single-uid with the token unhardened + full cap_add — asymmetric with the fail-closed
# setpriv drop below).
uid="$("$ID" -u)"
case "$uid" in
  ''|*[!0-9]*) echo "uzi-entrypoint: cannot determine uid (got '$uid'); refusing to start" >&2; exit 1 ;;
esac
if [ "$uid" != "0" ]; then
  echo "uzi-entrypoint: single-uid non-root mode (PRD #58) — no A1 uid-split on this start" >&2
  # Fail-safe against operator misconfig (audit M4 LOW): only the ROOT path below sets
  # these, but if a non-root deploy carries a stray UZI_UID_SPLIT=1 (compose env), the
  # single-uid worker would try to setpriv-wrap runner spawns → EPERM (no CAP_SETUID
  # non-root) → every spawn fails → DoS. Clear them so single-uid mode is robust to a
  # stray value. Not attacker-reachable (the runner cannot set the worker's env).
  unset UZI_UID_SPLIT UZI_RUNNER_PATH UZI_RUNNER_TMPDIR
  # PRD #120 / issue #120: pin the RUNNER PATH here too, AFTER the unset above (so a stray
  # operator value is still cleared and the value that survives is the entrypoint's own).
  #
  # Why: leaving it unset made `runnerPath()` (agent/src/runner-uid.ts) fall back to
  # `env.PATH` — and the CMD is `npm run start`, so npm's run-script PREPENDS
  # /app/node_modules/.bin, /node_modules/.bin and @npmcli/run-script/lib/node-gyp-bin to
  # the PATH the worker process actually sees. Every runner child (SDK agent, provision,
  # checks, git) then inherited a PATH on which the real npm `agent-browser` CLI shadowed
  # the crash-close + launch-config shim baked at /usr/local/bin (PRD #87), so browser
  # launches silently lost `--no-sandbox` and Chromium aborted on the setuid sandbox that
  # the PRD #51 hardening makes impossible. The ROOT/A1 path never had this: it captures
  # IMAGE_PATH below BEFORE the CMD runs and hands it over at the drop. The two modes
  # disagreeing was the defect; this makes them agree.
  #
  # `$PATH` is still the untouched image PATH at this point (the entrypoint runs BEFORE the
  # CMD), so this is exactly the value IMAGE_PATH captures on the root path.
  #
  # This does NOT weaken the #58 single-uid posture. UZI_UID_SPLIT stays unset, so
  # `uidSplitActive()` is false and every runner-uid.ts primitive remains a passthrough (no
  # setpriv wrap, no cross-uid kill) — that is what the unset above exists for, and it is
  # untouched. Nor does it widen anything: single-uid means worker == runner, and the
  # worker's own PATH already carries /nix here (only the ROOT path strips it, because only
  # there is /nix owned by a *different*, untrusted uid). It is also strictly safer than the
  # old fallback — a stray operator value used to be replaced by whatever npm produced, and
  # is now replaced by the image's own PATH.
  #
  # BEHAVIOUR DELTA worth naming: runner children lose ALL of /app/node_modules/.bin, not
  # just its agent-browser. That dir also holds tsx, tsc, tsserver, esbuild and friends, so
  # an agent on k8s that previously resolved a bare `tsc`/`tsx` from the WORKER's own
  # node_modules now gets command-not-found. That is the correct outcome — the agent should
  # never resolve the worker's toolchain — and nothing breaks: the SDK's own CLI is
  # module-resolved rather than PATH-resolved, and node/npm/npx sit at /usr/local/bin on the
  # image PATH. Stated here because it is a real change on the primary runtime.
  #
  # UZI_RUNNER_TMPDIR is deliberately NOT re-exported: controller/internal/kube/render.go
  # sets a pod-spec TMPDIR for docker workers and relies on this branch leaving it unset so
  # `runnerTmpdir()` (= UZI_RUNNER_TMPDIR || TMPDIR) returns the pod-spec value.
  export UZI_RUNNER_PATH="$PATH"
  exec "$TINI" -- "$@"
fi
echo "uzi-entrypoint: A1 uid-split active (root-started) — dropping to worker after the startup window" >&2

# --- ROOT-started (compose / A1): root startup window, then the setpriv drop ---
# The image's full runtime PATH (the nix profile bin + the JDK bin for jvm) becomes the
# RUNNER PATH (PRD #51 M4): the untrusted execution surfaces run as `runner` and need
# `/nix`, but the credential-holding worker must NOT resolve any binary from `/nix` (now
# runner-writable — a runner could plant a trojan the PAT-holding worker would run). So
# the dropped WORKER keeps only the stripped root-owned PATH (below), and the full image
# PATH is handed to the runner via UZI_RUNNER_PATH at the drop. The root window itself
# runs on the same stripped PATH.
#
# PROHIBITION (PRD #92): `/opt/uzi-toolchain/bin` (the M1 stable toolchain handle) must
# NEVER be added to the worker's stripped PATH below — it dereferences into the now
# runner-writable `/nix` store, so putting it on the worker PATH would let a runner plant
# a binary the PAT-holding worker resolves, piercing the PRD #51 M2-audit invariant. The
# baked toolchain belongs ONLY on the image PATH that becomes UZI_RUNNER_PATH (handed to
# the runner). The M3 boot preflight resolves it against UZI_RUNNER_PATH, never here.
IMAGE_PATH="${PATH}"
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

WORKER_USER=worker
WORKER_OWNER=worker:worker   # /data, /app: worker-owned (the worker's own trees)
# /nix: runner-OWNED under the A1 split (PRD #51 M4) — provisioning moves to `runner`, so
# it realizes packages as the /nix owner and the worker needs nothing from /nix. The
# worker→runner OWNER change re-triggers migrate_tree's sentinel (a new owner => a new
# sentinel name), which also fixes the group (handling the owner-keyed-guard note). On a
# #58 non-root start this whole root window is skipped, so /nix stays worker:runner from
# the image layer and the single-uid worker still provisions.
NIX_OWNER=runner:runner

# The two persisted volume roots and the join-token mount, as variables (the migration and
# token logic below reference nothing else), so the shell tests can point them at a sandbox.
DATA_DIR=/data
NIX_DIR=/nix
# busybox is the canonical absolute binary in the pinned node:24-alpine (the same base whose
# /bin/{chown,chmod,mkdir} the constants above resolve). Its `stat`/`cat` applets back the
# read-only-token posture + read checks; invoked via `busybox <applet>` so the exact applet
# symlink location does not matter.
BUSYBOX=/bin/busybox
# The pod fsGroup == the `worker` gid; the PVC-root alignment below restores it on the
# migrated mount roots so the kubelet skips its recursive fsGroup walk (PRD #1493 M2 change 5).
FSGROUP=10001

# fsGroup FINGERPRINT, captured BEFORE migrate_tree flips /nix's group: the kubelet applies
# fsGroup by setting the mount-root's group to 10001 AND the setgid bit on it; the image bakes
# /nix with `chmod -R a+rX` (never setgid), and compose has no kubelet, so a setgid /nix root
# is a reliable "this is a k8s fsGroup mount" signal. On compose it is unset and the alignment
# step is a no-op, keeping compose behaviour byte-for-byte.
NIX_HAD_FSGROUP=
[ -g "$NIX_DIR" ] && NIX_HAD_FSGROUP=1

# --- (a) B4: migrate persisted named volumes to the current uid layout --------
# agentnix (/nix) and agentdata (/data) seed from the image on first use and then
# persist their ORIGINAL ownership. When an existing install upgrades to this image the
# execution uid changed (uzi -> worker), so the volumes stay owned by the old uid and
# every write EACCESes. Re-own them here with CAP_CHOWN (held only during this root
# window). Ownership only — the finer runner-OWNED carve-out under /data (the separate
# runner clone/store, agent-home, provision) and /nix group-write for the runner are
# M3/M4 (they seed those paths and move the spawn); M2 migrates ownership so the worker,
# which still performs all work pre-split, operates.
migrate_tree() {
  # $1 = path, $2 = owner:group. One-time volume ownership migration, robust to the base
  # image's `chown -R` traversal order. The skip decision is a per-owner sentinel written
  # ONLY AFTER the chown COMPLETES — NOT the top-level owner, whose validity relied on
  # busybox chown -R being depth-first (top-level chowned LAST); a future swap to a
  # pre-order chown could leave a partial migration looking complete and be skipped. When
  # the sentinel is absent the chown runs UNCONDITIONALLY, so an interrupted migration
  # re-runs fully regardless of order (idempotent), and a later milestone that re-owns a
  # tree uses a new owner => a new sentinel name => it re-migrates. The only cost on a
  # fresh (already-correct) volume is one redundant chown on first boot. The sentinel is
  # NOT a security control. DELETING it only forces a redundant idempotent chown. FORGING
  # it (writing the sentinel before an upgrade so the migration is skipped) is the more
  # interesting direction: a pre-split, /data-owning agent could plant the next milestone's
  # sentinel so the post-upgrade chown skips, leaving the volume owned by the old uid ->
  # the worker cannot write /data -> the container fails to operate. That is an
  # AVAILABILITY DoS only (no confidentiality/integrity gain, operator-visible as a broken
  # boot), by a PRE-split attacker who already had same-uid /data write — accepted, since
  # the split's containment is about the POST-upgrade world and a clean install (down -v)
  # or a manual chown recovers it.
  path="$1"; owner="$2"
  [ -e "$path" ] || return 0
  sentinel="$path/.uzi-migrated-${owner%:*}-${owner#*:}"
  [ -f "$sentinel" ] && return 0
  echo "uzi-entrypoint: ensuring $path ownership -> $owner [one-time]" >&2
  "$CHOWN" -R "$owner" "$path"
  : > "$sentinel" 2>/dev/null && "$CHOWN" "$owner" "$sentinel" 2>/dev/null \
    || echo "uzi-entrypoint: warning: could not persist $sentinel (re-runs next boot)" >&2
}
migrate_tree "$NIX_DIR" "$NIX_OWNER"
migrate_tree "$DATA_DIR" "$WORKER_OWNER"

# --- (a2) PRD #51 M4: runner-owned /data subtree carve-out ((b) ownership model) --
# Under (b) separate-runner-clone the RUNNER clone store + the SDK/provision HOMEs are
# runner-owned trees (the agent checks out + commits there as uid `runner`), while the
# WORKER bare cache repos/ stays worker-only (its config/hooks/refs are the B2 code-exec
# surface — the runner must never write it). migrate_tree above set ALL of /data to
# worker:worker, so own these subtree ROOTS worker:runner + setgid/group-write + STICKY
# (3775) so children inherit group `runner` and the runner (a `runner`-group member) can
# create its per-run dirs under them, while the sticky bit stops a `runner`-group member
# (incl. uid 10003) that does NOT own a top-level entry from renaming/unlinking/replacing it
# (PRD #1493 M2 change 4 / D7 — closes the provider-root swap for the ancestors the
# entrypoint creates); the worker runs umask 002 (main.ts) so those worker-created per-run
# dirs are group-`runner`-writable. repos/ is deliberately NOT in this list.
#
# RESTART-SAFE + resume guard (two independent fixes, same block — reviewer flag B +
# tester e2e crash-on-restart):
#   * chmod ONLY while root still OWNS the dir (a fresh dir this boot, `[ -O ]` = owned by
#     the effective uid = root here). The runtime cap set has NO CAP_FOWNER (deliberate),
#     so root CANNOT chmod a dir it already handed to `worker` on a prior boot — an
#     UNCONDITIONAL chmod EPERMs there, and `set -eu` turns that into a DETERMINISTIC
#     crash on every restart/recreate over the persisted agentdata volume. The setgid +
#     group-write is set once, when the dir is first created (root-owned); it need not be
#     re-applied. (chown does NOT clear a directory's setgid on Linux, so it persists.)
#   * NON-RECURSIVE chown (runs every boot — cheap, CAP_CHOWN needs no FOWNER): a
#     `chown -R worker:runner` would re-own the runner's OWN per-run resume state
#     (agent-home/<runId> a requeued run resumes from) runner->worker on every restart.
#     Owning only the ROOTS leaves runner-owned content untouched; a fresh volume's roots
#     are empty, and an upgrade's stale content was re-owned by migrate_tree /data above.
RUNNER_TREE_OWNER=worker:runner
require_real_carveout_root() {
  if [ -L "$1" ]; then
    echo "uzi-entrypoint: refusing to start: $1 is a symlink" >&2
    exit 1
  fi
}
for d in runner agent-home provision; do
  "$MKDIR" -p "$DATA_DIR/$d"
  # SYMLINK-ROOT GUARD (PRD #1493 M2 rework, BLOCKING): a legacy single-uid /data was
  # attacker-writable, so a carve-out ROOT itself may be a planted symlink (e.g.
  # `agent-home -> repos`). `mkdir -p` on an existing symlink-to-dir SUCCEEDS without
  # replacing the link, so this guard comes AFTER the mkdir to catch the pre-existing link.
  # Fail closed: skipping it would leave the symlink for the runtime's lexical path joins to
  # follow after the privilege drop. A legitimate carve-out root is always a real directory
  # directly under /data. Without this check the chmod/chown below would also dereference the
  # link and could re-own the worker-only repos/ cache to the runner identity.
  require_real_carveout_root "$DATA_DIR/$d"
  # 🔴 SC3067 IS A TRUE PORTABILITY STATEMENT AND A FALSE BUG REPORT AGAINST THIS
  # IMAGE, AND IT STOPS BEING FALSE THE MOMENT THE SHEBANG OR THE BASE IMAGE MOVES.
  # This file is `#!/bin/sh` and both worker Dockerfiles ship it on the same
  # digest-pinned node:24-alpine with an exec-form ENTRYPOINT
  # (controller/internal/kube/render.go sets no `Command:` ON THE WORKER CONTAINER,
  # so k8s uses the image's own ENTRYPOINT. That file has five `Command:` fields on
  # OTHER containers, so the unqualified version of this sentence invited a reader
  # to grep, find them, and doubt the paragraph). There /bin/sh is busybox ash
  # 1.37.0, which implements `-O` with correct semantics -- measured 2026-08-03,
  # with a control proving the probe could have detected an unsupported operator
  # (`[ -Q dir ]` -> rc=2 "unknown operand").
  #
  # WHY THIS IS A PER-INSTANCE DISABLE AND NOT A `.shellcheckrc` LINE OR A
  # `# shellcheck shell=busybox` HEADER (PRD #103 M5, ruled): on a shell where `-O`
  # is undefined, `[ -O x ]` is false, the `&&` short-circuits, THE CHMOD IS SKIPPED,
  # and `set -eu` does not fire -- POSIX exempts the left-hand side of an AND-list
  # from errexit. So the failure mode of changing the interpreter is a permission
  # that is never applied. That warning belongs at the three call sites where
  # someone would need it; an rc file would blanket every tracked script and a
  # whole-file dialect declaration would go on silently asserting busybox after the
  # shebang changed.
  #
  # TWO BOUNDS ON THAT CLAIM, because this comment is the entire recorded
  # justification for the ruling and an overstated one is worth less than a narrow
  # one (both re-measured 2026-08-03 in Debian's dash, which is /bin/sh there):
  #
  #   * IT IS NOT LITERALLY SILENT. An unsupported operator writes one line to
  #     stderr -- `[: -Q: unexpected operator`, 39 bytes -- which for THIS file
  #     means it lands in the pod's logs. What is silent is the EXIT STATUS: the
  #     same run printed `SURVIVED` at rc=0 under `sh -eu`, so nothing fails and
  #     nothing retries. A line in a log nobody greps is not a gate.
  #   * THE HYPOTHETICAL SHELL IS NONE OF THE OBVIOUS ONES. dash implements `-O`,
  #     and so do bash, ksh and busybox ash -- measured, dash and bash both return
  #     0 on `[ -O /tmp ]`. So this guards against a future interpreter that is not
  #     any shell currently plausible here. That is a bound on the scenario, not a
  #     refutation: the comment is explicitly conditional on the shebang or base
  #     image moving, and the cost of being wrong is a permission mode that is
  #     never applied on a tree nobody is looking at.
  # shellcheck disable=SC3067
  [ -O "$DATA_DIR/$d" ] && "$CHMOD" 3775 "$DATA_DIR/$d"   # only on a fresh (root-owned) dir
  "$CHOWN" "$RUNNER_TREE_OWNER" "$DATA_DIR/$d"
done

# --- (a2b) PRD #1493 M2: one-time, ownership-aware migration of a POPULATED legacy
# single-uid /data volume, so uid 10002 (runner) can use it after first split enablement ----
# migrate_tree "$DATA_DIR" above left every legacy descendant worker:worker, and the carve-out
# `[ -O ]`-guarded chmod is SKIPPED on a legacy (worker-owned, not root-owned) parent, so
# without this the three carve-out trees stay unusable by the runner (uid 10002). This is
# ONE-TIME and sentinel-gated like migrate_tree; it is NEVER a blanket `chown -R "$DATA_DIR"`
# (which would clobber the mixed agent-home map), NEVER purges working state (a retained clone
# or resume state may be the only copy of unpublished work), and NEVER runs per-boot (a
# per-boot recursive chown would clobber a requeued run's resume state). Only `chown`/`chgrp`
# (CAP_CHOWN) touch descendants — never `chmod` (the root window has no CAP_FOWNER, so it
# cannot chmod a path it does not own); the two dirs that STAY worker-owned (the run HOME root
# + codex-data root) are chgrp'd to `runner` while the executor's ensureCodexSharedDirectory
# re-asserts their exact 2770 mode on the next resume. The EXACT ownership map (change 3):
#
#   * carve-out parents "$DATA_DIR"/{runner,agent-home,provision}: reclaim to root (no
#     CAP_FOWNER), then worker:runner + setgid + group-write + STICKY == 3775 (the legacy
#     counterpart of the fresh-dir carve-out above).
#   * "$DATA_DIR"/runner (plain-lane runner working clones): PRESERVED byte-for-byte, re-owned
#     one-time to the `runner` identity (uid 10002) that adopts them on resume. NEVER purged.
#   * "$DATA_DIR"/agent-home per-run HOMEs (Codex AND Claude SDK):
#       - the run HOME root + its codex-data root STAY worker:runner (owner worker, gid
#         runner) so ensureCodexSharedDirectory still validates them on resume.
#       - codex-session-store (worker-private resume state, 0700/0600) is NEVER touched — it is
#         deliberately worker-private; exposing it to group `runner` would let uid 10003 read
#         or tamper with resume state.
#       - every OTHER descendant — the Claude SDK .claude tree / .claude.json / history / todos
#         / shell snapshots AND the provider-owned codex-data/epoch-N trees — is re-owned to
#         the `runner` identity, which runs the SDK/provider under the split and must read AND
#         update this state or every resumed run on a migrated volume fails on its own HOME.
#   * "$DATA_DIR"/agent-home's OWN top-level dot entries (.cache, .local, .claude, .claude.json,
#     ...) are NOT per-run HOMEs: agent-home is also the shared HOME of runner-uid processes
#     (provisioning's devbox/nix, the chat SDK CLI), so each is re-owned wholesale to runner
#     (issue #1696). Every worker-created agent-home entry (run ids, codex-advice-*, uzi-judge-/
#     uzi-review-/uzi-summary- mkdtemps, tombstones) is non-dot, so the run-HOME map above only
#     ever needs the non-dot glob.
#   * "$DATA_DIR"/provision (shared provisioning state, written by the runner): re-owned to runner.
LEGACY_SENTINEL="$DATA_DIR/.uzi-legacy-split-migrated"
if [ ! -f "$LEGACY_SENTINEL" ]; then
  echo "uzi-entrypoint: one-time ownership-aware migration of legacy $DATA_DIR [PRD #1493 M2]" >&2
  for d in runner agent-home provision; do
    require_real_carveout_root "$DATA_DIR/$d"
    if [ -d "$DATA_DIR/$d" ]; then
      "$CHOWN" 0:0 "$DATA_DIR/$d"                       # reclaim (no CAP_FOWNER at runtime)
      "$CHMOD" 3775 "$DATA_DIR/$d"                      # setgid + group-write + STICKY (change 4 / D7)
      "$CHOWN" "$RUNNER_TREE_OWNER" "$DATA_DIR/$d"
    fi
  done
  # SYMLINK GIVE-AWAY DEFENSE (PRD #1493 M2 audit, BLOCKING): a legacy single-uid /data was
  # writable by the untrusted agent uid, so it could plant a symlink either AT a carve-out ROOT
  # itself (e.g. `agent-home -> repos`, or `runner -> ../repos`) or as a DESCENDANT of one (e.g.
  # `agent-home/evil -> ../repos`, or a run HOME's `codex-data -> ../../repos`). Either lets a
  # `chmod`/`chown`/`chown -R` dereference onto — or a glob descend through — an unrelated tree,
  # notably the worker-only bare-repo cache repos/ (the B2 code-exec surface): re-owning repos/ to
  # `runner` lets the untrusted identity plant a git hook the PAT-holding worker later executes,
  # defeating the uid split. The kernel ALWAYS resolves mid-path components regardless of busybox
  # chown's no-dereference default, so a symlinked ROOT poisons every op keyed on it and a symlinked
  # DESCENDANT poisons its sub-walk. TWO GUARD LAYERS close the whole class:
  #   (a) ROOTS — every place a carve-out root {runner,agent-home,provision} is chmod'd, chown'd or
  #       used as a glob/mid-path prefix calls require_real_carveout_root first. A symlink aborts
  #       startup before the sentinel or privilege drop, so no migration op or later runtime path
  #       can follow it. A legitimate carve-out root is always a real directory directly under /data.
  #   (b) DESCENDANTS — every descendant loop below skips a symlink entry outright (`[ -L ]`) and only
  #       ever descends REAL directories. A legitimate carve-out child / per-run HOME / epoch is
  #       always a real path; a symlink there is never something the migration needs to re-own.
  #
  # KNOWN, ACCEPTED, NARROW RESIDUAL (no nlink filter INSIDE the recursive walks): `[ -L ]` cannot
  # catch a HARDLINK. A legacy-planted hardlink from inside a migrated tree to an existing
  # repos/<repo> FILE would transfer that single inode's ownership to `runner` via `chown -R` (files
  # only, same-fs, the target must pre-exist). This is far narrower than the symlink class (no
  # directory trees, no new inodes), and a general defense is impractical: git LEGITIMATELY hardlinks
  # pack/object files, so an `nlink > 1` skip would break real clones. Documented and accepted.
  # The shared HOME's dot loops (below, and the (a2c) repair) DO filter their own TOP-LEVEL entry:
  # only a real directory or a regular file with a link count of exactly 1 is re-owned, so for the
  # shared HOME this residual stays scoped to hardlinks NESTED inside its recursively re-owned trees.
  #
  # "$DATA_DIR"/runner + /provision: re-own retained content to `runner`, preserving it. Only the
  # PARENT's CHILDREN are re-owned (the parents stay worker:runner, set just above).
  for tree in runner provision; do
    require_real_carveout_root "$DATA_DIR/$tree"
    if [ -d "$DATA_DIR/$tree" ]; then
      for c in "$DATA_DIR/$tree"/* "$DATA_DIR/$tree"/.[!.]* "$DATA_DIR/$tree"/..?*; do
        [ -e "$c" ] || continue
        [ -L "$c" ] && continue                          # never dereference an attacker-planted symlink into a chown -R
        "$CHOWN" -R runner:runner "$c"
      done
    fi
  done
  # "$DATA_DIR"/agent-home: the mixed per-subtree map above.
  require_real_carveout_root "$DATA_DIR/agent-home"
  if [ -d "$DATA_DIR/agent-home" ]; then
    # Per-run HOMEs are NON-DOT only: a dot entry here is the shared runner HOME's own state
    # (handled by the loop below), never a run HOME (issue #1696).
    for home in "$DATA_DIR"/agent-home/*; do
      [ -d "$home" ] || continue
      [ -L "$home" ] && continue                        # a legit per-run HOME is always a REAL dir; skip a planted symlink so it can never become a mid-path prefix
      "$CHOWN" worker:runner "$home"                    # run HOME / advice-parent root STAYS worker-owned, gid runner
      if [ -d "$home/codex-data" ] && [ ! -L "$home/codex-data" ]; then   # only descend a REAL codex-data, never a planted symlink
        "$CHOWN" worker:runner "$home/codex-data"       # codex-data root STAYS worker:runner
        for epoch in "$home"/codex-data/* "$home"/codex-data/.[!.]* "$home"/codex-data/..?*; do
          [ -e "$epoch" ] || continue
          [ -L "$epoch" ] && continue                   # skip a planted epoch symlink before the recursive chown
          "$CHOWN" -R runner:runner "$epoch"            # provider-owned per-epoch trees -> runner
        done
      fi
      for entry in "$home"/* "$home"/.[!.]* "$home"/..?*; do
        [ -e "$entry" ] || continue
        [ -L "$entry" ] && continue                     # skip a planted symlink so the recursive chown never dereferences it
        case "${entry##*/}" in
          codex-data|codex-session-store) continue ;;   # codex-data handled above; store is worker-private
        esac
        "$CHOWN" -R runner:runner "$entry"              # Claude SDK state + advice provider roots -> runner
      done
    done
    # The shared HOME's own dot entries (issue #1696): the legacy single-uid volume was written
    # entirely by the agent uid, and post-split only runner-uid children (provisioning's
    # devbox/nix, the chat SDK CLI) use this HOME's dot state, so each dot entry (dir OR file,
    # e.g. .claude.json) is re-owned wholesale to runner. A symlink entry is skipped (`[ -L ]`
    # first; a dangling one also fails `[ -e ]`). A symlink NESTED inside a dot dir is not
    # followed by the recursive chown on the current image: its /bin/chown is BusyBox v1.37.0
    # on the digest-pinned node:24-alpine base (no coreutils), and a BusyBox v1.37.0 `chown -R`
    # measured 2026-09-25 re-owned a nested dir- or file-symlink itself and left its target
    # untouched. That is a measurement of this binary, not a claim about chown in general.
    # A top-level HARDLINK (a regular file with nlink > 1, e.g. one planted to a repos/<r>.git
    # file) is skipped too, as is anything that is neither a directory nor a regular file (fifo,
    # socket, device): only a real dir or a single-link regular file is re-owned. A failed stat
    # counts as "skip".
    for dot in "$DATA_DIR"/agent-home/.[!.]* "$DATA_DIR"/agent-home/..?*; do
      [ -L "$dot" ] && continue                         # never hand a planted symlink to a chown -R
      if [ ! -d "$dot" ]; then
        [ -f "$dot" ] || continue                       # not a dir or regular file (or absent): skip
        nlink=$("$BUSYBOX" stat -c %h "$dot" 2>/dev/null) || continue
        [ "$nlink" = 1 ] || continue                    # a hardlinked file: never re-own the shared inode
      fi
      "$CHOWN" -R runner:runner "$dot"
    done
  fi
  : > "$LEGACY_SENTINEL" 2>/dev/null && "$CHOWN" "$WORKER_OWNER" "$LEGACY_SENTINEL" 2>/dev/null \
    || echo "uzi-entrypoint: warning: could not persist $LEGACY_SENTINEL (re-runs next boot)" >&2
fi

# --- (a2c) issue #1696: one-time repair of the shared provisioning HOME's dot-entry ownership ----
# /data/agent-home is also the HOME of runner-uid processes (provisioning's devbox/nix via
# runnerCommand, the chat SDK CLI), but the legacy walk above used to treat its dot entries
# (.cache, .local, .claude) as per-run HOMEs: their ROOT inode was left worker:runner in a private
# legacy mode (no CAP_FOWNER to chmod it) and a top-level dot FILE such as .claude.json was skipped
# and left worker:worker, so the runner could not use them. Affected volumes already carry
# LEGACY_SENTINEL, so a fix inside that block never runs for them; this separate sentinel does.
# The old walk already chown -R'd every non-symlink CHILD of each dot dir to runner, so only the
# top-level inodes are damaged. A post-split runner can write agent-home (worker:runner, group
# writable) and so could plant a hardlink there to a worker-owned file (e.g. a repos/<r>.git
# config) before this repair runs. The repair is NON-recursive, which keeps any NESTED hardlink out
# of reach, and a TOP-LEVEL hardlink is excluded by the filter: only a real directory or a regular
# file with a link count of exactly 1 is re-owned (symlinks, hardlinked files, fifos, sockets and
# devices are skipped; a failed stat counts as "skip"). A per-entry loop (not a fixed dir list)
# also covers .claude.json. It runs once on a fresh volume too (idempotent there).
# Chown only, never chmod (no CAP_FOWNER). A runner-owned 0700 .claude is unreadable to the worker's
# resume probe, which fails open (sdk-session.ts: "leaving the resume as-is"), as on a fresh volume.
PROVISION_HOME_SENTINEL="$DATA_DIR/.uzi-provision-home-repaired"
if [ ! -f "$PROVISION_HOME_SENTINEL" ]; then
  echo "uzi-entrypoint: one-time repair of shared provisioning HOME dot-entry ownership [issue #1696]" >&2
  require_real_carveout_root "$DATA_DIR/agent-home"
  if [ -d "$DATA_DIR/agent-home" ]; then
    for dot in "$DATA_DIR"/agent-home/.[!.]* "$DATA_DIR"/agent-home/..?*; do
      [ -L "$dot" ] && continue                         # never re-own through a planted symlink
      if [ ! -d "$dot" ]; then
        [ -f "$dot" ] || continue                       # not a dir or regular file (or absent): skip
        nlink=$("$BUSYBOX" stat -c %h "$dot" 2>/dev/null) || continue
        [ "$nlink" = 1 ] || continue                    # a hardlinked file: never re-own the shared inode
      fi
      "$CHOWN" runner:runner "$dot"                     # top-level inode only, NON-recursive
    done
  fi
  : > "$PROVISION_HOME_SENTINEL" 2>/dev/null && "$CHOWN" "$WORKER_OWNER" "$PROVISION_HOME_SENTINEL" 2>/dev/null \
    || echo "uzi-entrypoint: warning: could not persist $PROVISION_HOME_SENTINEL (re-runs next boot)" >&2
fi

# --- (a3) PRD #51 M3 / 5-bis: distinct per-uid TMPDIR on 0700 trees -------------
# git/npm/node scratch writes would otherwise share a sticky /tmp (symlink races +
# exposure of any worker temp write across the uid boundary). Give the worker and the
# runner each a private 0700 tmp. The worker's is exported as TMPDIR below; the runner's
# is exported as UZI_RUNNER_TMPDIR (the runner env builders put it on the agent/checks/
# provision children — runner-uid.ts). Its 0700/runner mode is owner-only, so the worker
# (a `runner`-GROUP member) still cannot read it. The mode is re-asserted every boot by
# reclaiming ownership to root FIRST and only then chmod'ing: /tmp is the container's
# writable layer (NOT a named volume), so a `docker restart` reuses it and a prior boot
# may have left the dir worker/runner-owned. Rather than guard the chmod on root-ownership,
# we `chown 0:0` each dir back to root (CAP_CHOWN, which the root startup window has; no
# CAP_FOWNER needed) and then chmod — so an unconditional chmod can never EPERM here.
#
# DOCKER-LANE TMPDIR (PRD #1493 M2 change 2 / coupling #2): the k8s docker lane pre-sets an
# ambient TMPDIR to the DinD-shared run workdir (render_dind.go: /data/runner) so a
# `docker run -v <src>` bind source staged under $TMPDIR resolves in the daemon's filesystem.
# When that ambient TMPDIR is present, derive BOTH private per-uid tmpdirs BENEATH it (still
# one per uid, still 0700, still worker-/runner-owned) so bind sources stay inside the shared
# workdir. With no ambient TMPDIR (compose, and plain-lane k8s), keep /tmp/uzi-worker +
# /tmp/uzi-runner EXACTLY as before.
if [ -n "${TMPDIR:-}" ]; then
  WORKER_TMPDIR="$TMPDIR/uzi-worker"
  RUNNER_TMPDIR="$TMPDIR/uzi-runner"
else
  WORKER_TMPDIR=/tmp/uzi-worker
  RUNNER_TMPDIR=/tmp/uzi-runner
fi
# HARDEN ADOPTION (PRD #1493 rework / CWE-732 CodeRabbit): TMPDIR is an ambient, possibly
# PERSISTENT shared volume on the docker lane (render_dind.go pre-sets it to /data/runner),
# so an untrusted principal can pre-create uzi-worker/uzi-runner. `mkdir -p` NO-OPS on an
# existing dir (it keeps that dir's mode AND contents), and a planted SYMLINK at that name
# would make the chmod/chown below DEREFERENCE onto its target. A persistent docker-lane
# parent is sticky (3775), and after the first boot these entries are worker/runner-owned;
# root deliberately lacks CAP_FOWNER, so it cannot unlink either entry through that parent.
# Reclaim each existing entry itself with CAP_CHOWN before removing it. `-h` is load-bearing:
# a planted symlink is re-owned rather than dereferenced, then `rm -rf` unlinks the link itself.
# A real tree may itself contain sticky directories with runtime-owned files. Reclaim only its
# directories, then clear their modes to 0700 before removal; this gives root ownership of every
# deletion parent without chowning files (which could be hardlinks to state outside this scratch
# tree). find does not follow symlinks by default, and -xdev bounds traversal to one filesystem.
# These paths hold only disposable per-uid scratch (git/npm/node temp); no resumed-run state
# lives here (that is under /data/agent-home + the clone), so a boot-time wipe is safe.
for tmpdir in "$WORKER_TMPDIR" "$RUNNER_TMPDIR"; do
  if [ -e "$tmpdir" ] || [ -L "$tmpdir" ]; then
    "$CHOWN" -h 0:0 "$tmpdir"
    if [ -d "$tmpdir" ] && [ ! -L "$tmpdir" ]; then
      "$FIND" "$tmpdir" -xdev -type d -exec "$CHOWN" 0:0 '{}' +
      "$FIND" "$tmpdir" -xdev -type d -exec "$CHMOD" 0700 '{}' +
    fi
  fi
done
"$RM" -rf "$WORKER_TMPDIR" "$RUNNER_TMPDIR"
"$MKDIR" -p "$WORKER_TMPDIR" "$RUNNER_TMPDIR"
# Then RECLAIM to root and re-assert 0700 UNCONDITIONALLY (no `[ -O ]` guard): root reclaims
# via CAP_CHOWN (the root startup window has CHOWN, no FOWNER), so the chmod always succeeds
# even when a prior boot left the dir worker/runner-owned -- the SAME restart-safe reclaim the
# token block and the legacy-migration roots use. No `if`-wrapper (unlike the token's EROFS
# read-only Secret mount): these tmpdirs are ALWAYS a writable mount, never read-only.
"$CHOWN" 0:0 "$WORKER_TMPDIR"; "$CHMOD" 0700 "$WORKER_TMPDIR"; "$CHOWN" "$WORKER_OWNER" "$WORKER_TMPDIR"
# runner:runner 0700 -- owner-only, so even the worker (a `runner`-GROUP member) cannot reach
# it (0700 grants the group nothing); true per-uid isolation.
"$CHOWN" 0:0 "$RUNNER_TMPDIR"; "$CHMOD" 0700 "$RUNNER_TMPDIR"; "$CHOWN" runner:runner "$RUNNER_TMPDIR"

# --- (a4) issue #1598: the Codex command-cache root, runner-cmd 0700 ---------------
# The supervisor's uid-10003 cache holder creates ONE random per-run dir under this root
# (GOMODCACHE, GOCACHE and the npm cache for every Codex command in the run), and its uid-10003
# reaper deletes stale ones. Owning the root runner-cmd (10003:10003) 0700 means only uid 10003
# can create, traverse or delete in it: worker 10001 and runner 10002 get nothing (the group is
# runner-cmd's own primary group, which neither joins). A storage boundary, not a trust
# boundary. Compose uses the image-baked root-owned dir (writable layer); k8s mounts a
# worker-only emptyDir here (controller/internal/kube/render.go), which already exists.
#
# ROOT ONLY, NOT RECURSIVE: this re-owns the root dir alone and never touches its contents;
# existing per-run dirs from a previous boot are the reaper's to remove, not ours.
# SYMLINK GUARD: a symlink at the path is refused (before and after `mkdir -p`, which succeeds
# on a link to a dir), so nothing below is ever chowned or chmod'ed THROUGH a link. `chown -h`
# is belt-and-braces on top of that. RECLAIM FIRST: the root window has no CAP_FOWNER, so a dir
# a prior boot left 10003-owned cannot be chmod'ed until root owns it again (same restart-safe
# reclaim -> chmod -> hand-over as the (a3) tmpdirs and the token).
# Unreachable on the non-root (#58) start: that branch exec'd above, so no uid split, no cache
# root.
CODEX_CMD_CACHE_DIR=/var/cache/uzi-codex-cmd
CODEX_CMD_CACHE_OWNER=10003:10003   # runner-cmd:runner-cmd
require_real_carveout_root "$CODEX_CMD_CACHE_DIR"
"$MKDIR" -p "$CODEX_CMD_CACHE_DIR"
require_real_carveout_root "$CODEX_CMD_CACHE_DIR"
"$CHOWN" -h 0:0 "$CODEX_CMD_CACHE_DIR"
"$CHMOD" 0700 "$CODEX_CMD_CACHE_DIR"
"$CHOWN" -h "$CODEX_CMD_CACHE_OWNER" "$CODEX_CMD_CACHE_DIR"

# --- (b) token: force 0400 worker on the join-token secret ---------------------
# Compose delivers the env-sourced `worker_token` secret 0444 root:root (world-readable
# — the runner uid could read it), and an env-sourced secret's uid/gid/mode are
# unreliable (audit L2), so enforce it here rather than trusting compose. chmod BEFORE
# chown: the runtime cap set has no CAP_FOWNER, so root can chmod the file only while
# root still owns it; 0400 carries no setuid/setgid bit for chown to strip, so the final
# mode is 0400 worker:worker. The e2e stack (PRD #51 M5) delivers its token via THIS
# same Docker-secret path (UZI_WORKER_TOKEN_FILE=/run/secrets/worker_token, sourced from
# the minted UZI_WORKER_TOKEN), so this hardening covers it too — that is exactly what
# makes the e2e's runner-uid read-denial assertion non-vacuous.
#
# RESTART-SAFE: RECLAIM ownership to root FIRST, then chmod, then hand it over. A
# `docker compose restart` (as opposed to a recreate) keeps the container AND its secret
# mount, so the second boot finds the file already owned by `worker` from the first —
# and with no CAP_FOWNER root cannot chmod a file it does not own. Going straight to
# chmod EPERMs there, `set -eu` aborts the ENTRYPOINT, and the worker never starts at
# all: no heartbeat, so the api sweeps the worker stale and requeues every run it was
# holding. CAP_CHOWN needs no FOWNER, so taking the file back is always permitted and
# the chmod that follows is too. Measured in this image with the compose cap set, on a
# worker-owned file: bare `chmod` prints "Operation not permitted" and exits 1, while
# reclaim-chmod-hand-over exits 0 and leaves 0400 worker:worker.
#
# Reclaiming rather than skipping the chmod when root is not the owner is deliberate:
# the mode is a security control and every boot must re-assert it. Measured on a token
# left 0444 worker:worker by a previous boot, an ownership-guarded chmod leaves it 0444
# while this sequence restores 0400. The transient root ownership never widens anything
# (0400 carries no setuid/setgid bit for the final chown to strip).
#
# READ-ONLY KUBE SECRET MOUNT (PRD #1493 M2 change 1 / coupling #4): on k8s the join token
# is a Secret volume the kubelet presents READ-ONLY as uid=0 gid=10001 mode=0440. The
# reclaim `chown 0:0` there fails with EROFS; under `set -eu` an unconditional chown aborts
# the entrypoint and the pod crash-loops. So the reclaim is run in an `if` (whose failure is
# exempt from errexit): on SUCCESS (a writable compose secret) keep today's exact
# reclaim -> chmod 0400 -> hand-over sequence and result; on FAILURE do NOT abort but
# POSITIVELY verify the exact kube posture (uid=0 gid=10001 mode=0440) AND that `worker` can
# actually read it, failing CLOSED (exit non-zero) on anything else. gid 10001 is the
# `worker` primary group and 0440 is group-readable, so a token in that posture is readable
# by the dropped worker and unreadable by `runner`/`runner-cmd` (whose --init-groups drops
# the fsGroup) — the same containment the compose 0400 worker:worker gives.
TOKEN=/run/secrets/worker_token
if [ -e "$TOKEN" ] || [ -L "$TOKEN" ]; then
  if "$CHOWN" 0:0 "$TOKEN" 2>/dev/null; then
    "$CHMOD" 0400 "$TOKEN"
    "$CHOWN" "$WORKER_OWNER" "$TOKEN"
  else
    # Dereference the projected-Secret symlink chain: `stat -L` reads the TARGET's posture
    # (0 10001 440), not the atomic-writer symlink's own 0777 lstat. A dangling link or a
    # stat failure makes the dereference fail -> empty posture -> fail closed below.
    token_posture="$("$BUSYBOX" stat -L -c '%u %g %a' "$TOKEN" 2>/dev/null || true)"
    case "$token_posture" in
      "0 10001 440"|"0 10001 0440") token_posture_ok=1 ;;
      *) token_posture_ok= ;;
    esac
    if [ -n "$token_posture_ok" ] \
      && "$SETPRIV" --reuid "$WORKER_USER" --regid "$WORKER_USER" --init-groups \
           -- "$BUSYBOX" cat "$TOKEN" >/dev/null 2>&1; then
      echo "uzi-entrypoint: join token is a read-only kube Secret (uid=0 gid=10001 mode=0440), worker-readable — keeping as mounted" >&2
    else
      echo "uzi-entrypoint: join token at $TOKEN cannot be re-owned and is NOT the expected read-only kube posture (uid=0 gid=10001 mode=0440, worker-readable); refusing to start (posture: '$token_posture')" >&2
      exit 1
    fi
  fi
fi

# --- (b2) PRD #1493 M2 change 5: restore the kubelet fsGroup alignment on migrated roots ---
# The kubelet's FSGroupChangeOnRootMismatch skips its recursive fsGroup walk only while a
# mounted volume's ROOT directory already matches the pod fsGroup (group 10001) with the
# setgid bit set. migrate_tree's `chown -R "$NIX_OWNER" "$NIX_DIR"` flips the /nix mount-root's
# group to `runner` (10002), so without this the kubelet re-walks the whole nix store on EVERY
# start. Restore each migrated root to group 10001 + setgid + group-rwx WITHOUT touching
# content ownership (a NON-recursive chmod/chown of the ROOT dir only; the owner is preserved).
# Reclaim to root first because the root window has no CAP_FOWNER (it cannot chmod a dir it does
# not own). Gated on the fsGroup fingerprint captured BEFORE migrate_tree, so this is a strict
# NO-OP on compose (whose volume roots never carry the setgid bit) — compose behaviour is
# byte-for-byte preserved.
align_pvc_root() {
  # $1 = mounted volume root; $2 = owner to restore (owner preserved; only group + mode change).
  root="$1"; owner="$2"
  [ -d "$root" ] || return 0
  "$CHOWN" 0:0 "$root"                 # reclaim (CAP_CHOWN; the root window has no CAP_FOWNER)
  "$CHMOD" 2775 "$root"                # setgid + group-rwx == what OnRootMismatch expects
  "$CHOWN" "$owner:$FSGROUP" "$root"   # restore owner, set group to the pod fsGroup (10001)
}
if [ -n "$NIX_HAD_FSGROUP" ]; then
  align_pvc_root "$NIX_DIR" runner
  align_pvc_root "$DATA_DIR" "$WORKER_USER"
fi

# --- (c) drop root -> worker, keeping ONLY setuid/setgid (ambient) -------------
# --init-groups picks up worker's supplementary membership of group `runner` (from
# /etc/group) so the worker can access runner-group trees. tini stays PID 1 (now as
# `worker`) to reap and forward SIGTERM, preserving clean shutdown. The CMD
# (npm run start) arrives as "$@" and is passed as argv (no shell re-parse).
#
# The dropped worker's env activates the PRD #51 M4 uid split for the worker process:
#   - PATH stays the STRIPPED root-owned set (set above, still exported) — NOT the image
#     PATH — so no worker-side exec ever resolves from the runner-writable /nix (M2-audit
#     MEDIUM). The worker's own tools (git/node/npm/setpriv/tini) are all in these dirs.
#   - UZI_UID_SPLIT=1 tells runner-uid.ts to setpriv-wrap every untrusted spawn as `runner`
#     and to reap runner groups via a setpriv-to-runner kill. Its ABSENCE (a #58 non-root
#     start, which never reaches this line) = single-uid, no split.
#   - UZI_RUNNER_PATH = the full image PATH (nix + JDK) the runner env builders put on the
#     agent/checks/provision children; UZI_RUNNER_TMPDIR = the runner's private 0700 tmp.
#   - TMPDIR = the worker's own private 0700 tmp (5-bis), inherited by its git children.
export TMPDIR="$WORKER_TMPDIR"
export UZI_UID_SPLIT=1
export UZI_RUNNER_PATH="$IMAGE_PATH"
export UZI_RUNNER_TMPDIR="$RUNNER_TMPDIR"
exec "$SETPRIV" \
  --reuid "$WORKER_USER" --regid "$WORKER_USER" --init-groups \
  --bounding-set -all,+setuid,+setgid \
  --inh-caps -all,+setuid,+setgid \
  --ambient-caps -all,+setuid,+setgid \
  -- "$TINI" -- "$@"
