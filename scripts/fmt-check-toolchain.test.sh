#!/usr/bin/env bash
# Hermetic contract for Taskfile.yml's fmt-check:api and fmt-check:controller: both must run
# the gofmt of the toolchain the module selects (`go env GOROOT`, which follows go.mod's
# `toolchain` line), never whichever gofmt is first on PATH. A PATH gofmt older than that
# toolchain lists files the module's own gofmt accepts, so the old bare `gofmt -l .` reddened
# gate:api on an untouched tree and skipped every later slot. Fake `go` and `gofmt` on PATH,
# no Go toolchain or network. Prints a `cases=N passed=N` tally and fails below its case
# floor, so a gutted run cannot read green.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FLOOR=8
cases=0
passed=0

mkdir -p "$TMP/bin" "$TMP/goroot/bin"
# A PATH gofmt that always reports drift: the stale-toolchain false positive.
printf '#!/bin/sh\necho stale_path_gofmt.go\n' > "$TMP/bin/gofmt"
# A fake `go`: `go env GOROOT` prints the fake GOROOT, or fails when GO_ENV_FAIL is set.
cat > "$TMP/bin/go" <<STUB
#!/bin/sh
[ "\$1 \$2" = "env GOROOT" ] || { echo "fake go: unexpected args: \$*" >&2; exit 97; }
[ -z "\${GO_ENV_FAIL:-}" ] || { echo "fake go: env failed" >&2; exit 1; }
echo "$TMP/goroot"
STUB
chmod +x "$TMP/bin/gofmt" "$TMP/bin/go"

# goroot_gofmt <mode>: clean = lists nothing; drift = lists a file; parse = gofmt's
# unparseable-file exit 2.
goroot_gofmt() {
  case "$1" in
    clean) printf '#!/bin/sh\nexit 0\n' ;;
    drift) printf '#!/bin/sh\necho module_drift.go\n' ;;
    parse) printf '#!/bin/sh\necho "x.go:1:1: expected package" >&2\nexit 2\n' ;;
  esac > "$TMP/goroot/bin/gofmt"
  chmod +x "$TMP/goroot/bin/gofmt"
}

# expect <target> <want: 0|1|2> <must-contain or ""> <label>. Task reports its own rc (201)
# for any failed command, so the command's 1 vs 2 is read from Task's `exit status N` line.
expect() {
  cases=$((cases + 1))
  local rc=0
  (cd "$ROOT" && PATH="$TMP/bin:$PATH" task "$1") > "$TMP/out" 2>&1 || rc=$?
  local ok=1
  if [ "$2" = 0 ]; then
    [ "$rc" -eq 0 ] || ok=0
  else
    [ "$rc" -ne 0 ] || ok=0
    grep -q -E "exit status $2\$" "$TMP/out" || ok=0
  fi
  if [ -n "$3" ]; then grep -q -F "$3" "$TMP/out" || ok=0; fi
  if [ "$ok" = 1 ]; then
    passed=$((passed + 1))
  else
    echo "FAIL: $1: $4: want $2, got rc=$rc" >&2
    sed 's/^/    /' "$TMP/out" >&2
  fi
}

for t in fmt-check:api fmt-check:controller; do
  goroot_gofmt clean
  expect "$t" 0 "" "PATH gofmt reports drift, module toolchain gofmt is clean: green"
  goroot_gofmt drift
  expect "$t" 1 module_drift.go "module toolchain gofmt reports drift: red, naming the file"
  goroot_gofmt parse
  expect "$t" 2 "" "module toolchain gofmt cannot parse a file: instrument red (exit 2)"
  goroot_gofmt clean
  GO_ENV_FAIL=1 expect "$t" 2 "" "go env GOROOT fails: instrument red (exit 2), never green"
done

echo "cases=$cases passed=$passed"
[ "$cases" -ge "$FLOOR" ] || { echo "FAIL: ran $cases cases, floor is $FLOOR" >&2; exit 1; }
[ "$passed" -eq "$cases" ]
