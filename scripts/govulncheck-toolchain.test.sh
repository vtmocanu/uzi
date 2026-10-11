#!/usr/bin/env bash
# Offline contract for govulncheck-gate.sh. A fake older PATH go selects distinct
# module versions, installs a fake analyzer, and simulates its child package loader.
# Override with GOVULNCHECK_GATE_WRAPPER=<path> or the first argument to test an
# exported base wrapper without changing tracked files. No Go or network is used.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
WRAPPER="${1:-${GOVULNCHECK_GATE_WRAPPER:-$ROOT/scripts/govulncheck-gate.sh}}"
# Resolve a relative override before the cases change directory.
case "$WRAPPER" in /*) ;; *) WRAPPER="$PWD/$WRAPPER" ;; esac
mkdir -p "$ROOT/.uzi/scratch"
TMP="$(mktemp -d "$ROOT/.uzi/scratch/govulncheck-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
FLOOR=32
cases=0
passed=0
TOOL=example.com/tools/govulncheck@v0.0.1
# Deliberately different from each other and from the repo's current toolchain.
API_VERSION=go1.91.2
CONTROLLER_VERSION=go1.92.3
mkdir -p "$TMP/bin"
REAL_MKTEMP="$(command -v mktemp)"
# Model BSD's bare -d behavior even on GNU hosts; explicit templates still use
# the real mktemp. Keep the escaped scratch inside the case so cleanup is bounded.
cat > "$TMP/bin/mktemp" <<'STUB'
#!/bin/sh
set -eu
if [ "$#" -eq 1 ] && [ "$1" = -d ]; then
  exec "$REAL_MKTEMP" -d "$CASE_DIR/escaped.XXXXXX"
fi
exec "$REAL_MKTEMP" "$@"
STUB
chmod +x "$TMP/bin/mktemp"

cat > "$TMP/bin/go" <<'STUB'
#!/bin/sh
set -eu
case "$*" in
  "env GOFLAGS")
    if [ "$FAKE_MODE" = persisted-tags ]; then
      echo '-tags=persisted'
    elif [ -n "${GOFLAGS:-}" ]; then
      echo "$GOFLAGS"
    elif [ "${GOENV:-off}" != off ]; then
      cat "$GOENV"
    fi
    ;;
  "env GOVERSION")
    pwd > "$CASE_DIR/selection"
    echo selection-stderr >&2
    case "$PWD" in
      "$MODULE_ROOT/api") version="$API_VERSION" ;;
      "$MODULE_ROOT/controller") version="$CONTROLLER_VERSION" ;;
      *) version=go1.20.0 ;;
    esac
    case "$FAKE_MODE" in
      selection-fail) echo "$version"; exit 7 ;;
      selection-empty) exit 0 ;;
    esac
    echo "$version"
    ;;
  "install "*)
    printf '%s\n' "toolchain=${GOTOOLCHAIN:-unset}" \
      "os=${GOOS:-darwin}" "arch=${GOARCH:-arm64}" \
      "cgo=${CGO_ENABLED:-1}" "goflags=${GOFLAGS:-}" \
      "gobin=$GOBIN" > "$CASE_DIR/install"
    printf '[%s]\n' "$@" > "$CASE_DIR/install-args"
    case "$FAKE_MODE" in
      install-fail) echo install-stderr >&2; exit 1 ;;
      download-fail) echo toolchain-download-stderr >&2; exit 9 ;;
      missing) exit 0 ;;
    esac
    mkdir -p "$GOBIN"
    cp "$ANALYZER" "$GOBIN/govulncheck"
    [ "$FAKE_MODE" = nonexecutable ] || chmod +x "$GOBIN/govulncheck"
    ;;
  "list ./...")
    # A govulncheck built with a newer Go still invokes PATH go for packages.
    # Record that child's inherited toolchain separately from the install.
    printf '%s\n' "toolchain=${GOTOOLCHAIN:-unset}" \
      "os=${GOOS:-darwin}" "arch=${GOARCH:-arm64}" \
      "cgo=${CGO_ENABLED:-1}" > "$CASE_DIR/child"
    ;;
  *) echo "fake older go: unexpected arguments: $*" >&2; exit 97 ;;
esac
STUB

cat > "$TMP/analyzer" <<'STUB'
#!/bin/sh
set -eu
pwd > "$CASE_DIR/analysis"
printf '[%s]\n' "$@" > "$CASE_DIR/analysis-args"
printf '%s\n' "toolchain=${GOTOOLCHAIN:-unset}" \
  "os=${GOOS:-darwin}" "arch=${GOARCH:-arm64}" \
  "cgo=${CGO_ENABLED:-1}" "goflags=${GOFLAGS:-}" > "$CASE_DIR/analysis-env"
go list ./...
echo analysis-stdout
echo analysis-stderr >&2
exit "$ANALYSIS_EXIT"
STUB
chmod +x "$TMP/bin/go"

# Each case runs once and independently; an assertion failure does not skip the
# remaining finite matrix. Fake commands do no fetching, waiting, or retries.
check() {
  local label="$1"
  shift
  if ! "$@"; then
    echo "FAIL: $module/$mode: $label" >&2
    ok=0
  fi
}

contains() { grep -q -F -x -- "$2" "$1"; }

expect() {
  local module="$1" mode="$2" want="$3" analysis_exit="${4:-0}"
  local dir rc=0 ok=1 goflags='' driver='' goenv=off selected stage
  cases=$((cases + 1))
  dir="$TMP/case-$cases"
  mkdir -p "$dir/tmp"
  case "$module" in
    api) selected="$API_VERSION" ;;
    controller) selected="$CONTROLLER_VERSION" ;;
  esac
  case "$mode" in
    driver) driver=untrusted-driver ;;
    env-tags) goflags='-tags=hidden' ;;
    goenv-tags) goenv="$dir/goenv"; echo '-tags hidden' > "$goenv" ;;
    buildvcs) goflags='-buildvcs=false' ;;
  esac
  (cd "$ROOT" && env -i PATH="$TMP/bin:$PATH" TMPDIR="$dir/tmp" \
    REAL_MKTEMP="$REAL_MKTEMP" MODULE_ROOT="$ROOT" API_VERSION="$API_VERSION" \
    CONTROLLER_VERSION="$CONTROLLER_VERSION" CASE_DIR="$dir" \
    ANALYZER="$TMP/analyzer" FAKE_MODE="$mode" ANALYSIS_EXIT="$analysis_exit" \
    GOTOOLCHAIN=auto GOFLAGS="$goflags" GOENV="$goenv" GOPACKAGESDRIVER="$driver" \
    sh "$WRAPPER" "$module" "$TOOL") > "$dir/out" 2> "$dir/err" || rc=$?
  check "exit: want $want, got $rc" test "$rc" -eq "$want"

  case "$mode" in
    driver|env-tags|goenv-tags|persisted-tags)
      check 'guard diagnostic' grep -q -F Refused. "$dir/err"
      for stage in selection install analysis child; do
        check "guard prevented $stage" test ! -e "$dir/$stage"
      done
      ;;
    *)
      check 'module-local version selection' contains "$dir/selection" "$ROOT/$module"
      case "$mode" in
        selection-fail|selection-empty)
          check 'selection stderr forwarded' contains "$dir/err" selection-stderr
          check 'selection failure diagnostic' grep -q -F GOVERSION "$dir/err"
          check 'selection prevented install' test ! -e "$dir/install"
          ;;
        *)
          check 'install toolchain' contains "$dir/install" "toolchain=$selected"
          check 'host-native install OS' contains "$dir/install" os=darwin
          check 'host-native install architecture' contains "$dir/install" arch=arm64
          check 'install cgo not overridden' contains "$dir/install" cgo=1
          check 'install GOFLAGS preserved' contains "$dir/install" "goflags=$goflags"
          printf '[install]\n[%s]\n' "$TOOL" > "$dir/want-install-args"
          check 'exact install arguments' diff -u "$dir/want-install-args" "$dir/install-args"
          local gobin
          gobin="$(sed -n 's/^gobin=//p' "$dir/install")"
          check 'temporary GOBIN inside case scratch' test "${gobin#"$dir/tmp/"}" != "$gobin"
          check 'temporary GOBIN parent cleaned' test ! -e "${gobin%/bin}"
          case "$mode" in
            install-fail) check 'install stderr forwarded' contains "$dir/err" install-stderr ;;
            download-fail) check 'download stderr forwarded' contains "$dir/err" toolchain-download-stderr ;;
            missing|nonexecutable) check 'missing executable diagnostic' grep -q -F 'produced no' "$dir/err" ;;
            *)
              check 'analysis module directory' contains "$dir/analysis" "$ROOT/$module"
              for stage in analysis-env child; do
                check "$stage toolchain" contains "$dir/$stage" "toolchain=$selected"
                check "$stage target OS" contains "$dir/$stage" os=linux
                check "$stage target architecture" contains "$dir/$stage" arch=amd64
                check "$stage cgo" contains "$dir/$stage" cgo=0
              done
              check 'analysis GOFLAGS preserved' contains "$dir/analysis-env" "goflags=$goflags"
              printf '[-test=false]\n[-show]\n[verbose]\n[./...]\n' > "$dir/want-analysis-args"
              check 'exact analysis arguments' diff -u "$dir/want-analysis-args" "$dir/analysis-args"
              check 'analysis stdout forwarded' contains "$dir/out" analysis-stdout
              check 'analysis stderr forwarded' contains "$dir/err" analysis-stderr
              check 'stderr not merged into stdout' test "$(grep -c -F analysis-stderr "$dir/out")" -eq 0
              check 'stdout not merged into stderr' test "$(grep -c -F analysis-stdout "$dir/err")" -eq 0
              ;;
          esac
          ;;
      esac
      case "$mode" in
        selection-fail|selection-empty|install-fail|download-fail|missing|nonexecutable)
          check 'failure prevented analysis' test ! -e "$dir/analysis"
          check 'failure prevented child package loading' test ! -e "$dir/child"
          ;;
      esac
      ;;
  esac
  check 'all wrapper temporary files cleaned' test -z "$(ls -A "$dir/tmp")"
  if [ "$ok" -eq 1 ]; then
    passed=$((passed + 1))
  else
    printf '  wrapper stdout:\n' >&2
    cat "$dir/out" >&2
    printf '  wrapper stderr:\n' >&2
    cat "$dir/err" >&2
  fi
}

for module in api controller; do
  expect "$module" clean 0 0
  expect "$module" findings 1 3
  expect "$module" loader-error 2 1
  expect "$module" usage-error 2 2
  expect "$module" unexpected-error 2 42
  expect "$module" selection-fail 2
  expect "$module" selection-empty 2
  expect "$module" install-fail 2
  expect "$module" download-fail 2
  expect "$module" missing 2
  expect "$module" nonexecutable 2
  expect "$module" driver 2
  expect "$module" env-tags 2
  expect "$module" goenv-tags 2
  expect "$module" persisted-tags 2
  expect "$module" buildvcs 0
done

echo "cases=$cases passed=$passed"
[ "$cases" -ge "$FLOOR" ] || { echo "FAIL: ran $cases cases, floor is $FLOOR" >&2; exit 1; }
[ "$passed" -eq "$cases" ]
