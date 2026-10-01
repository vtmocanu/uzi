#!/usr/bin/env bash
# Admin-path helper for provisioning a uzi GitLab bot account. See
# docs/gitlab-bot-setup.md for the full procedure (including the non-admin
# path, which most users should use instead).
#
# Requires GitLab *instance admin* rights on the target host: creates the bot
# user via the admin Users API, mints its PAT via the admin PAT API, and adds
# it to a project as Developer (access_level=30). Safe to re-run:
#   - an existing bot user is reused, not recreated;
#   - re-running always mints a brand-new PAT, because GitLab never returns an
#     old token's value again: the old token keeps working until it expires
#     or is revoked;
#   - re-running the project-add step upgrades an existing membership to
#     Developer instead of failing on "already a member".
#
# The PAT is printed to stdout exactly once and is never written to a file;
# copy it now, GitLab will not show it again.
#
# Usage:
#   ./scripts/create-gitlab-bot.sh [--gitlab <host>] <bot-username> <project-path-or-id> [email]
#
# Env overrides:
#   GITLAB_HOSTNAME  GitLab host (default: gitlab.example.com)
#   SCOPES           PAT scope (default: api; read_api cannot write labels)
#   EXPIRES_AT       PAT expiry, YYYY-MM-DD (default: 90 days from today; your
#                    instance may enforce a shorter admin-configured max PAT
#                    lifetime, which silently clamps a longer request)
#
# Requires: jq and glab, authenticated against the selected host as an instance admin. On
# some GitLab instances an exported GITLAB_TOKEN overrides glab's stored
# credentials and 401s the admin API, so every call here runs via
# `env -u GITLAB_TOKEN glab ...` regardless of your shell's environment.
set -euo pipefail

info() { printf '==> %s\n' "$1"; }
warn() { printf '\033[33mWARN\033[0m %s\n' "$1" >&2; }
die() { printf '\033[31mERROR\033[0m %s\n' "$1" >&2; exit 1; }

usage() {
  cat <<EOF
Usage: $0 [options] <bot-username> <project-path-or-id> [email]

Options:
  --gitlab <host>  GitLab host (also --gitlab=<host>); overrides GITLAB_HOSTNAME.
                  Accepts a bare host or http(s)://host with trailing slashes.
  -h, --help      Show this help and exit.
  --              End options, allowing positional arguments starting with '-'.

Host default: GITLAB_HOSTNAME, or gitlab.example.com when unset or empty.
Requires: jq and glab authenticated as an instance admin on the selected host.
EOF
}

GITLAB_HOSTNAME="${GITLAB_HOSTNAME:-gitlab.example.com}"
positionals=()
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --gitlab)
      [ $# -ge 2 ] && [ -n "$2" ] && [[ "$2" != -* ]] \
        || die "--gitlab requires a host"
      GITLAB_HOSTNAME="$2"
      shift 2
      ;;
    --gitlab=*)
      GITLAB_HOSTNAME="${1#--gitlab=}"
      [ -n "$GITLAB_HOSTNAME" ] || die "--gitlab requires a host"
      shift
      ;;
    --) shift; positionals+=("$@"); break ;;
    -*) die "unknown option: $1 (use --help)" ;;
    *) positionals+=("$1"); shift ;;
  esac
done
[ "${#positionals[@]}" -ge 2 ] && [ "${#positionals[@]}" -le 3 ] \
  || { usage >&2; exit 1; }
GITLAB_HOSTNAME="${GITLAB_HOSTNAME#https://}"
GITLAB_HOSTNAME="${GITLAB_HOSTNAME#http://}"
while [[ "$GITLAB_HOSTNAME" == */ ]]; do GITLAB_HOSTNAME="${GITLAB_HOSTNAME%/}"; done
[ -n "$GITLAB_HOSTNAME" ] && [[ "$GITLAB_HOSTNAME" != *[/[:space:]]* ]] \
  && [[ "$GITLAB_HOSTNAME" != -* ]] || die "invalid GitLab host; use a bare host or http(s)://host"

command -v jq >/dev/null 2>&1 || die "jq is required; install jq before running this script"
command -v glab >/dev/null 2>&1 || die "glab is required; install and authenticate glab first"
SCOPES="${SCOPES:-api}"
EXPIRES_AT="${EXPIRES_AT:-$(date -v+90d +%F 2>/dev/null || date -d '+90 days' +%F)}"
DEVELOPER_ACCESS_LEVEL=30
BOT_USERNAME="${positionals[0]}"
PROJECT="${positionals[1]}"
EMAIL="${positionals[2]:-${BOT_USERNAME}@users.noreply.${GITLAB_HOSTNAME}}"
PROJECT_ENC="${PROJECT//\//%2F}"

glab_api() { env -u GITLAB_TOKEN glab api --hostname "$GITLAB_HOSTNAME" "$@"; }

json_value() {
  # Exactly one JSON document, with a caller-specific shape. Suppress parser
  # diagnostics: even malformed responses may contain a freshly minted PAT.
  printf '%s' "$1" | jq -ers '
    def user_id:
      if type == "object" and (.id | type == "number") then
        .id | if . > 0 and floor == . then . else error("invalid id") end
      else error("invalid user") end;
    if length == 1 then .[0] | ('"$2"') else error("expected one response") end
  ' 2>/dev/null || die "$3"
}

info "checking glab auth against $GITLAB_HOSTNAME"
env -u GITLAB_TOKEN glab auth status --hostname "$GITLAB_HOSTNAME" >/dev/null 2>&1 \
  || die "not authenticated against $GITLAB_HOSTNAME as an admin; run: glab auth login --hostname $GITLAB_HOSTNAME"

info "looking up existing user $BOT_USERNAME"
existing="$(glab_api "users?username=${BOT_USERNAME}")"
bot_id="$(json_value "$existing" '
  if type != "array" then error("expected user array")
  elif length == 0 then ""
  elif length == 1 then .[0] | user_id
  else error("ambiguous user lookup") end
' "user lookup failed: invalid JSON or user response")"

if [ -n "$bot_id" ]; then
  info "bot user $BOT_USERNAME already exists (id $bot_id): reusing"
else
  info "creating bot user $BOT_USERNAME <$EMAIL>"
  created="$(glab_api users -X POST \
    --raw-field "username=${BOT_USERNAME}" \
    --raw-field "name=${BOT_USERNAME}" \
    --raw-field "email=${EMAIL}" \
    --field "skip_confirmation=true" \
    --field "force_random_password=true")"
  bot_id="$(json_value "$created" 'user_id' "user creation failed: invalid JSON or user response")"
  info "created bot user $BOT_USERNAME (id $bot_id)"
fi

info "minting a PAT (scope: $SCOPES, expires: $EXPIRES_AT)"
# glab debug HTTP logging dumps response bodies to stderr, including the PAT.
# Suppress that stream as well as keeping the captured response out of errors.
pat_json="$(jq -n --arg scope "$SCOPES" --arg expires "$EXPIRES_AT" \
  '{name: "uzi-bot", scopes: [$scope], expires_at: $expires}' \
  | glab_api "users/${bot_id}/personal_access_tokens" -X POST \
    -H "Content-Type: application/json" --input - 2>/dev/null)" \
  || die "PAT creation request failed; check admin rights, scope and expiry"
token="$(json_value "$pat_json" '
  if type == "object" and (.token | type == "string") then
    .token | if length > 0 and (explode | all(. >= 32 and . != 127)) then .
    else error("invalid token") end
  else error("invalid PAT response") end
' "PAT creation failed: invalid JSON or token response; a token may have been minted, check GitLab before retrying")"

info "checking membership on $PROJECT"
if glab_api "projects/${PROJECT_ENC}/members/${bot_id}" >/dev/null 2>&1; then
  info "already a member: ensuring Developer access"
  glab_api "projects/${PROJECT_ENC}/members/${bot_id}" -X PUT \
    --field "access_level=${DEVELOPER_ACCESS_LEVEL}" >/dev/null
else
  info "adding $BOT_USERNAME to $PROJECT as Developer"
  glab_api "projects/${PROJECT_ENC}/members" -X POST \
    --raw-field "user_id=${bot_id}" \
    --field "access_level=${DEVELOPER_ACCESS_LEVEL}" >/dev/null
fi

echo
echo "Bot ready: ${BOT_USERNAME} (id ${bot_id}) is Developer on ${PROJECT}"
echo
printf '\033[33mSAVE THIS NOW. GitLab will not show it again:\033[0m\n'
printf '  %s\n' "$token"
echo
echo "Paste it into uzi: Settings -> Forge -> Base URL https://${GITLAB_HOSTNAME} -> Token."
