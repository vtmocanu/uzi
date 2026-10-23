#!/bin/sh
# Explicit fixture-only #3305 workaround; keep the actual entrypoint and caps.
set -eu
[ -L /opt/uzi-toolchain ] || { echo "ERROR: expected toolchain symlink" >&2; exit 1; }
[ -x /usr/local/lib/uxlab-go/bin/go ] && [ -d /usr/local/lib/uxlab-go/src ] || exit 1
# Retain the image profile's tools and font data; override only Go/gofmt.
original="$(readlink -f /opt/uzi-toolchain)"
overlay=/opt/uxlab-toolchain
mkdir -m 755 "$overlay"
"$original/bin/cp" -R -P -s --no-preserve=mode,ownership "$original/." "$overlay/"
rm -- "$overlay/bin/go" "$overlay/bin/gofmt"
ln -s /usr/local/lib/uxlab-go/bin/go "$overlay/bin/go"
ln -s /usr/local/lib/uxlab-go/bin/gofmt "$overlay/bin/gofmt"
printf 'GO_FIXTURE_ORIGINAL_PROFILE=%s\n' "$original"
rm -- /opt/uzi-toolchain
/bin/ln -s "$overlay" /opt/uzi-toolchain
exec /usr/local/sbin/uzi-entrypoint "$@"
