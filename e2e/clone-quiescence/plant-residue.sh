#!/bin/sh
# Issue #1783 M3: the ROOT step of the clone-quiescence fixture, run by run.sh as the container's
# entry command BEFORE the real root-start entrypoint drops root -> worker. It plants the incident's
# shape: a `root:root drwxr-sr-x` directory AT a canonical runner clone path, holding `agent/src`
# and `gate-log.loop`, which the worker uid can neither delete nor move to another parent. Then it
# execs the unmodified entrypoint with its arguments, so the fixture still runs as the worker uid
# under the real worker/runner/runner-cmd split.
#
# Why here and not in the fixture: the fixture runs AFTER the drop, as the worker uid, which cannot
# create a root-owned directory. Why not under /data: /data is the entrypoint's volume, and its
# root window re-owns what is there (migrate_tree sets all of /data worker:worker, and the one-time
# legacy migration chowns every child of /data/runner to runner), which would erase the root
# ownership under test. /tmp/cq-residue is outside everything the entrypoint touches. run.sh mounts
# it as its own tmpfs, like /data: measured on the image's overlayfs /tmp, root's chown of a
# directory dropped its setgid bit (the worker's `<slug>` then read back 775, not 2775) and /tmp's
# setgid group leaked into the planted residue; on tmpfs both behave as the entrypoint's /data does.
#
# The layout mirrors production ownership (see agent/templates/entrypoint.sh and git.ts
# seedRunnerClone): the data dir worker-owned; the runner root `worker:runner` 3775 (setgid +
# sticky); the repo dir `<slug>` `worker:runner` 2775 (setgid, NOT sticky), as the worker's own
# mkdir makes it. The runtime cap set has no CAP_FOWNER, so every chmod runs while root still owns
# the path, BEFORE its chown (on tmpfs, as on /data, that chown keeps a directory's setgid bit).
#
# The canonical path must equal what GitCache derives for the fixture's origin `/tmp/cq-origin`
# (`<data>/runner/tmp+cq-origin/issue-42`); fixture.test.ts asserts that.
set -eu

ID=/usr/bin/id
MKDIR=/bin/mkdir
CHOWN=/bin/chown
CHMOD=/bin/chmod
TOUCH=/bin/touch

if [ "$("$ID" -u)" != "0" ]; then
  echo "plant-residue: must start as root (run it through run.sh)" >&2
  exit 1
fi

DATA=/tmp/cq-residue
RUNNER="$DATA/runner"
SLUG="$RUNNER/tmp+cq-origin"
CANON="$SLUG/issue-42"

if [ ! -d "$DATA" ] || [ -L "$DATA" ]; then
  echo "plant-residue: $DATA must be a real directory (run.sh mounts it as a tmpfs)" >&2
  exit 1
fi
"$MKDIR" -p "$CANON/agent/src"
"$TOUCH" "$CANON/gate-log.loop"
# The residue itself: root:root drwxr-sr-x, exactly the incident's shape (root still owns it, so
# this chmod needs no CAP_FOWNER).
"$CHOWN" -R 0:0 "$CANON"
"$CHMOD" 2755 "$CANON"
# The repo dir: worker-owned 2775, not sticky.
"$CHMOD" 2775 "$SLUG"
"$CHOWN" worker:runner "$SLUG"
# The runner root: worker:runner 3775, as the entrypoint makes /data/runner.
"$CHMOD" 3775 "$RUNNER"
"$CHOWN" worker:runner "$RUNNER"
# The data dir: the worker's (it creates repos/ there).
"$CHMOD" 0755 "$DATA"
"$CHOWN" worker:worker "$DATA"

exec /usr/local/sbin/uzi-entrypoint "$@"
