#!/bin/sh
# Hermetic bot-provisioning tests (issues #2010 and #2016), fake glab, no network.
# TEST_SCRIPT allows the same assertions to run against an unfixed script.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SCRIPT="${TEST_SCRIPT:-$ROOT/scripts/create-gitlab-bot.sh}"
BASH_BIN="$(command -v bash)"
ENV_BIN="$(command -v env)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/uzi-gitlab-bot-test.XXXXXX")"
# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$TMP/bin" "$TMP/no-jq"
ln -s "$ENV_BIN" "$TMP/no-jq/env"
cat > "$TMP/bin/glab" <<'FAKE'
#!/bin/sh
set -eu
# Every call, including auth, must ignore an exported GITLAB_TOKEN.
[ "${GITLAB_TOKEN+x}" != x ] || exit 98
printf 'glab' >> "$GLAB_LOG"
for arg in "$@"; do printf '|%s' "$arg" >> "$GLAB_LOG"; done
printf '\n' >> "$GLAB_LOG"
if [ "$1" = auth ]; then
  [ "$GLAB_SCENARIO" != auth-fail ]
  exit "$?"
fi
[ "$1" = api ] && [ "$2" = --hostname ] || exit 97
shift 3
endpoint="$1"
shift
case "$endpoint" in
  users\?username=*)
    case "$GLAB_SCENARIO" in
      new|created-malformed|created-shape) printf '[]' ;;
      nested) printf '[{"created_by":{"id":99},"id":123}]' ;;
      lookup-malformed) printf '{broken' ;;
      lookup-shape) printf '{"id":123}' ;;
      lookup-id) printf '[{"id":"123"}]' ;;
      lookup-missing-id) printf '[{}]' ;;
      lookup-multiple) printf '[{"id":123},{"id":124}]' ;;
      lookup-stream) printf '[{"id":123}]\n[{"id":123}]' ;;
      *) printf '[{"id":123}]' ;;
    esac
    ;;
  users)
    case "$GLAB_SCENARIO" in
      created-malformed) printf '{broken' ;;
      created-shape) printf '{"id":null}' ;;
      *) printf '{"created_by":{"id":99},"id":123}' ;;
    esac
    ;;
  users/*/personal_access_tokens)
    cat > "$GLAB_BODY"
    case "$GLAB_SCENARIO" in
      pat-malformed) printf '{"token":"FAKE-BOT-PAT-2016",' ;;
      pat-shape) printf '{"token":{},"secret":"FAKE-BOT-PAT-2016"}' ;;
      pat-empty) printf '{"token":"","secret":"FAKE-BOT-PAT-2016"}' ;;
      pat-control) printf '{"token":"FAKE-BOT-PAT-2016\\n"}' ;;
      pat-http-error)
        printf '{"token":"FAKE-BOT-PAT-2016"}' >&2
        exit 1
        ;;
      *) printf '{"token":"FAKE-BOT-PAT-2016"}' ;;
    esac
    ;;
  projects/*/members/123)
    if [ "${1:-}" = -X ]; then
      [ "$GLAB_SCENARIO" = existing-member ] && [ "$2" = PUT ]
    else
      [ "$GLAB_SCENARIO" = existing-member ]
    fi
    ;;
  projects/*/members)
    [ "$1" = -X ] && [ "$2" = POST ]
    ;;
  *) exit 96 ;;
esac
FAKE
chmod +x "$TMP/bin/glab"
cases=0
passed=0
CASE_HOST=
CASE_PATH="$TMP/bin:$PATH"
run() {
  name="$1"; scenario="$2"; shift 2
  cases=$((cases + 1))
  log="$TMP/$cases.log"
  out="$TMP/$cases.out"
  err="$TMP/$cases.err"
  body="$TMP/$cases.body"
  : > "$log"
  rc=0
  "$ENV_BIN" GITLAB_HOSTNAME="$CASE_HOST" GITLAB_TOKEN=FAKE-ADMIN-ENV \
    GLAB_LOG="$log" GLAB_BODY="$body" GLAB_SCENARIO="$scenario" \
    EXPIRES_AT=2030-01-01 PATH="$CASE_PATH" \
    "$BASH_BIN" "$SCRIPT" "$@" > "$out" 2> "$err" || rc=$?
}
verify() {
  if "$@"; then
    passed=$((passed + 1))
    echo "ok   $name"
  else
    echo "FAIL $name (rc=$rc)"
    cat "$out" "$err" "$log"
  fi
}
success() {
  [ "$rc" -eq 0 ] && grep -F -q 'Bot ready:' "$out" \
    && [ "$(grep -F -c 'FAKE-BOT-PAT-2016' "$out")" -eq 1 ] \
    && ! grep -F -q 'FAKE-BOT-PAT-2016' "$err"
}
host_is() {
  [ "$rc" -ne 0 ] && grep -F -q "|--hostname|$1" "$log"
}
safe_error() {
  [ "$rc" -ne 0 ] && grep -F -q "$1" "$err" \
    && ! grep -F -q 'FAKE-BOT-PAT-2016' "$out" "$err" \
    && ! grep -F -q 'projects/' "$log"
}
no_call_error() {
  [ "$rc" -ne 0 ] && [ ! -s "$log" ] && grep -F -q "$1" "$err"
}
help_ok() {
  [ "$rc" -eq 0 ] && [ ! -s "$log" ] && grep -F -q 'Usage:' "$out"
}
created_ok() {
  success && grep -F -q 'glab|api|--hostname|gitlab.example.com|users|-X|POST' "$log" \
    && grep -F -q '|user_id=123|--field|access_level=30' "$log"
}
id_ok() {
  grep -F -q 'users/123/personal_access_tokens' "$log" \
    && ! grep -F -q 'users/99/personal_access_tokens' "$log"
}
header_ok() {
  grep -F 'personal_access_tokens' "$log" | grep -F -q '|-H|Content-Type: application/json|'
}
membership_ok() {
  success && grep -F -q 'projects/group%2Fproject/members/123|-X|PUT|--field|access_level=30' "$log"
}
dash_ok() {
  success && grep -F -q 'users?username=-bot' "$log"
}
escaped_body_ok() {
  success && jq -e '.scopes == ["api\"quoted"] and .expires_at == "2030-01-01"' "$body" >/dev/null
}

# Retain the original host-selection regressions, including bash's own HOSTNAME.
unset HOSTNAME
run 'default host with no HOSTNAME' auth-fail bot group/project
verify host_is gitlab.example.com
HOSTNAME=decoy.invalid; export HOSTNAME
run 'default host ignores HOSTNAME' auth-fail bot group/project
verify host_is gitlab.example.com
CASE_HOST=gitlab.test.invalid
run 'GITLAB_HOSTNAME overrides' auth-fail bot group/project
verify host_is gitlab.test.invalid
CASE_HOST=
run 'new user completes creation and Developer membership' new bot group/project
verify created_ok
run 'string PAT printed exactly once on stdout' existing bot group/project
verify success
run 'nested created_by.id does not select the wrong user' nested bot group/project
verify id_ok
run 'PAT POST carries Content-Type application/json' existing bot group/project
verify header_ok
run 'existing membership upgraded to Developer' existing-member bot group/project
verify membership_ok
run 'malformed lookup fails clearly' lookup-malformed bot group/project
verify safe_error 'user lookup failed'
run 'object lookup rejected' lookup-shape bot group/project
verify safe_error 'user lookup failed'
run 'string user id rejected' lookup-id bot group/project
verify safe_error 'user lookup failed'
run 'missing user id rejected' lookup-missing-id bot group/project
verify safe_error 'user lookup failed'
run 'ambiguous lookup rejected' lookup-multiple bot group/project
verify safe_error 'user lookup failed'
run 'multiple JSON documents rejected' lookup-stream bot group/project
verify safe_error 'user lookup failed'
run 'malformed created user fails clearly' created-malformed bot group/project
verify safe_error 'user creation failed'
run 'invalid created user shape fails clearly' created-shape bot group/project
verify safe_error 'user creation failed'
run 'malformed PAT response never leaks token' pat-malformed bot group/project
verify safe_error 'PAT creation failed'
run 'invalid PAT shape never leaks token' pat-shape bot group/project
verify safe_error 'PAT creation failed'
run 'empty PAT never leaks response' pat-empty bot group/project
verify safe_error 'PAT creation failed'
run 'control characters in PAT rejected without leak' pat-control bot group/project
verify safe_error 'PAT creation failed'
run 'glab PAT HTTP error never leaks token' pat-http-error bot group/project
verify safe_error 'PAT creation request failed'
CASE_HOST=env.example.com
run 'separate option wins over env and strips HTTPS URL' auth-fail --gitlab https://option.example.com/// bot group/project
verify host_is option.example.com
run 'equals option wins over env and strips HTTP URL' auth-fail --gitlab=http://equals.example.com/ bot group/project
verify host_is equals.example.com
CASE_HOST=https://env.example.com/
run 'environment URL normalized' auth-fail bot group/project
verify host_is env.example.com
CASE_HOST=
run 'short help exits successfully before auth' existing -h
verify help_ok
run 'long help exits successfully before auth' existing --help
verify help_ok
run 'unknown option rejected before auth' existing --unknown bot group/project
verify no_call_error 'unknown option'
run 'missing option value rejected' existing --gitlab
verify no_call_error 'requires a host'
run 'option cannot consume another option' existing --gitlab --help bot group/project
verify no_call_error 'requires a host'
run 'empty equals option rejected' existing --gitlab= bot group/project
verify no_call_error 'requires a host'
run 'URL without host rejected' existing --gitlab https:/// bot group/project
verify no_call_error 'invalid GitLab host'
run 'host with path rejected' existing --gitlab example.com/path bot group/project
verify no_call_error 'invalid GitLab host'
run 'too many positional arguments rejected' existing bot group/project email extra
verify no_call_error 'Usage:'
run 'double dash permits leading hyphen username' existing -- -bot group/project
verify dash_ok
SCOPES='api"quoted' run 'PAT body JSON escapes environment overrides' existing bot group/project
verify escaped_body_ok
CASE_PATH="$TMP/no-jq"
run 'missing jq fails before auth with clear diagnostic' existing bot group/project
verify no_call_error 'jq is required'

echo "$passed/$cases passed"
# Fixed tally: removing cases must fail the gate.
[ "$cases" -eq 36 ] && [ "$passed" -eq "$cases" ]
