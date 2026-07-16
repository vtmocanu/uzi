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
# still owns it (chmod-before-chown for the token) and can otherwise only chown (not
# chmod) already-owned paths during the B4 migration.
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
STAT=/bin/stat

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
if [ "$("$ID" -u)" != "0" ]; then
  exec "$TINI" -- "$@"
fi

# --- ROOT-started (compose / A1): root startup window, then the setpriv drop ---
# The image's runtime PATH (per template: the nix profile bin, and the JDK bin for jvm)
# is handed to the dropped worker; the root window itself runs on a constrained PATH
# containing only root-owned system dirs.
WORKER_PATH="${PATH}"
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

WORKER_USER=worker
WORKER_OWNER=worker:worker   # /data, /app: worker-owned (worker does all work pre-split)
NIX_OWNER=worker:runner      # /nix: worker-owned, group `runner` — forward hook for M4

# --- (a) B4: migrate persisted named volumes to the current uid layout --------
# agentnix (/nix) and agentdata (/data) seed from the image on first use and then
# persist their ORIGINAL ownership. When an existing install upgrades to this image the
# execution uid changed (uzi -> worker), so the volumes stay owned by the old uid and
# every write EACCESes. Re-own them here with CAP_CHOWN (held only during this root
# window). Guarded by the top-level owner so the one-time recursive migration does not
# run on every boot: a fresh volume already seeds with the correct build-time ownership,
# and a previously-migrated volume is a no-op. Ownership only — the finer runner-OWNED
# carve-out under /data (the separate runner clone/store, agent-home, provision) and
# /nix group-write for the runner are M3/M4 (they seed those paths and move the spawn);
# M2 migrates ownership so the worker, which still performs all work pre-split, operates.
migrate_tree() {
  # $1 = path, $2 = owner:group (owner half is the expected uid to guard on)
  path="$1"; owner="$2"
  [ -e "$path" ] || return 0
  want_user="${owner%%:*}"
  cur_user="$("$STAT" -c '%U' "$path" 2>/dev/null || echo '?')"
  if [ "$cur_user" != "$want_user" ]; then
    echo "uzi-entrypoint: migrating $path ownership ($cur_user -> $owner) [one-time]" >&2
    "$CHOWN" -R "$owner" "$path"
  fi
}
migrate_tree /nix "$NIX_OWNER"
migrate_tree /data "$WORKER_OWNER"

# --- (b) token: force 0400 worker on the join-token secret ---------------------
# Compose delivers the env-sourced `worker_token` secret 0444 root:root (world-readable
# — the runner uid could read it), and an env-sourced secret's uid/gid/mode are
# unreliable (audit L2), so enforce it here rather than trusting compose. chmod BEFORE
# chown: the runtime cap set has no CAP_FOWNER, so root can chmod the file only while
# root still owns it; 0400 carries no setuid/setgid bit for chown to strip, so the final
# mode is 0400 worker:worker. The e2e overlay delivers its token via a different
# (writable) path and is left untouched here.
TOKEN=/run/secrets/worker_token
if [ -e "$TOKEN" ]; then
  "$CHMOD" 0400 "$TOKEN"
  "$CHOWN" "$WORKER_OWNER" "$TOKEN"
fi

# --- (c) drop root -> worker, keeping ONLY setuid/setgid (ambient) -------------
# --init-groups picks up worker's supplementary membership of group `runner` (from
# /etc/group) so the worker can access runner-group trees. tini stays PID 1 (now as
# `worker`) to reap and forward SIGTERM, preserving clean shutdown. The CMD
# (npm run start) arrives as "$@" and is passed as argv (no shell re-parse).
export PATH="$WORKER_PATH"
exec "$SETPRIV" \
  --reuid "$WORKER_USER" --regid "$WORKER_USER" --init-groups \
  --bounding-set -all,+setuid,+setgid \
  --inh-caps -all,+setuid,+setgid \
  --ambient-caps -all,+setuid,+setgid \
  -- "$TINI" -- "$@"
