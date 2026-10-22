#!/bin/sh
# Explicit fixture-only #3305 workaround; keep the actual entrypoint and caps.
set -eu
[ -L /opt/uzi-toolchain ] || { echo "ERROR: expected toolchain symlink" >&2; exit 1; }
[ -x /usr/local/lib/uxlab-go/bin/go ] && [ -d /usr/local/lib/uxlab-go/src ] || exit 1
rm -- /opt/uzi-toolchain
ln -s /usr/local/lib/uxlab-go /opt/uzi-toolchain
exec /usr/local/sbin/uzi-entrypoint "$@"
