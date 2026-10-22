#!/bin/sh
set -eu
# 1. Image gate, before any write: the image must carry the runner-shared entrypoint.
M=/etc/uzi-runner-layout
if [ -L "$M" ] || [ ! -f "$M" ] || [ "$(cat "$M")" != "runner-shared-v1" ]; then
  echo "uzi-runner-init: worker image lacks runner-shared layout support (want runner-shared-v1); refusing; upgrade the worker image or roll back the controller" >&2
  exit 1
fi
R=/data-seed/runner-shared
T=/data-seed/runner-shared/.tmp
fail() { echo "uzi-runner-init: $1" >&2; exit 1; }
# check_dir refuses a symlink or a non-directory at the path. A missing path is fine.
check_dir() {
  if [ -L "$1" ]; then fail "$1 is a symlink; refusing to follow it (remove it from the data volume)"; fi
  if [ -e "$1" ] && [ ! -d "$1" ]; then fail "$1 exists and is not a directory (remove it from the data volume)"; fi
}
# ensure_dir: mkdir WITHOUT -p, so a final-component symlink cannot be followed. EEXIST
# is tolerated only if the path is then a real directory.
ensure_dir() {
  check_dir "$1"
  mkdir "$1" 2>/dev/null || true
  if [ -L "$1" ] || [ ! -d "$1" ]; then fail "$1 is not a real directory after mkdir"; fi
}
# 2. Create and validate the subPath entry, then .tmp beneath it.
ensure_dir "$R"
ensure_dir "$T"
# 3. Ownership. Root (uid-split) can chown; the single-uid container cannot, so there it
#    fails closed unless both entries are already owned by the current uid and R is sticky.
if [ "$(id -u)" = 0 ]; then
  # chown to root, chmod, THEN chown to the final owner: this container has no CAP_FSETID
  #    and root is not in group runner, so a chmod after chown to worker:runner would have
  #    the kernel clear S_ISGID (3775 -> 1775). Same order as the worker entrypoint.
  chown -h 0:0 "$R"
  chmod 3775 "$R"
  chown -h worker:runner "$R"
  chown -h 0:0 "$T"
  chmod 0755 "$T"
  chown -h worker:worker "$T"
else
  chmod 3775 "$R" "$T" 2>/dev/null || true
  # shellcheck disable=SC3065,SC3067 # busybox ash (the worker image shell) implements -k and -O
  # Positive form: a shell lacking -O/-k errors (non-zero), which lands in the else branch.
  if [ -O "$R" ] && [ -O "$T" ] && [ -k "$R" ]; then
    :
  else
    fail "$R and $T must be owned by uid $(id -u) with $R sticky, and could not be made so; see ADR-3153 for the operator remedy"
  fi
fi
# 4. Final re-validation.
check_dir "$R"
check_dir "$T"
[ -d "$R" ] && [ -d "$T" ] || fail "$R or $T is not a directory"
echo "uzi-runner-init: runner-shared ready ($R, $T)"
