#!/bin/sh
# Hermetic host-selection tests for scripts/create-gitlab-bot.sh (issue #2010).
#
# The script used to read its GitLab host from HOSTNAME, which bash sets to the machine's
# own hostname at startup, so the gitlab.example.com default never applied. Each case runs
# the script with a FAKE `glab` on PATH that records the --hostname it was given and fails
# `auth status`, so the script dies at its first network step and nothing else runs.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SCRIPT="$ROOT/scripts/create-gitlab-bot.sh"
# Portable template form (check:mktemp-portability): a full path with 6 X's, no -t.
TMP="$(mktemp -d "${TMPDIR:-/tmp}/uzi-gitlab-bot-test.XXXXXX")"
# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$TMP/bin"
cat > "$TMP/bin/glab" <<'EOF'
#!/bin/sh
while [ $# -gt 0 ]; do
  if [ "$1" = "--hostname" ]; then printf '%s\n' "$2" >> "$GLAB_HOST_LOG"; break; fi
  shift
done
exit 1
EOF
chmod +x "$TMP/bin/glab"

cases=0
passed=0

# check NAME EXPECTED_HOST [VAR=VALUE ...]: runs the script under env with the given
# overrides and asserts the first --hostname glab saw.
check() {
  name="$1"; want="$2"; shift 2
  cases=$((cases + 1))
  log="$TMP/$cases.log"
  : > "$log"
  rc=0
  env -u GITLAB_HOSTNAME -u HOSTNAME GLAB_HOST_LOG="$log" PATH="$TMP/bin:$PATH" "$@" \
    "$SCRIPT" bot group/project > "$TMP/$cases.out" 2>&1 || rc=$?
  got="$(head -1 "$log")"
  if [ "$rc" -ne 0 ] && [ "$got" = "$want" ]; then
    passed=$((passed + 1))
    echo "ok   $name"
  else
    echo "FAIL $name: want host '$want', got '$got' (rc=$rc)"
    sed 's/^/     /' "$TMP/$cases.out"
  fi
}

# An exported HOSTNAME must not affect GitLab host selection.
check "default host ignores HOSTNAME" gitlab.example.com HOSTNAME=decoy.invalid
check "default host with no HOSTNAME" gitlab.example.com
check "GITLAB_HOSTNAME overrides" gitlab.test.invalid GITLAB_HOSTNAME=gitlab.test.invalid HOSTNAME=decoy.invalid

echo "$passed/$cases passed"
# Tally guard: a gutted case list must not read green.
[ "$cases" -eq 3 ] && [ "$passed" -eq "$cases" ]
