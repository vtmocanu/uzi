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
#     old token's value again — the old token keeps working until it expires
#     or is revoked;
#   - re-running the project-add step upgrades an existing membership to
#     Developer instead of failing on "already a member".
#
# The PAT is printed to stdout exactly once and is never written to a file —
# copy it now, GitLab will not show it again.
#
# Usage:
#   ./scripts/create-gitlab-bot.sh <bot-username> <project-path-or-id> [email]
#
# Env overrides:
#   HOSTNAME     GitLab host (default: gitlab.example.com)
#   SCOPES       PAT scope (default: api — read_api cannot write labels)
#   EXPIRES_AT   PAT expiry, YYYY-MM-DD (default: 90 days from today; your
#                instance may enforce a shorter admin-configured max PAT
#                lifetime, which silently clamps a longer request)
#
# Requires: glab, authenticated against HOSTNAME as an instance admin. On
# gitlab.example.com an exported GITLAB_TOKEN overrides glab's stored
# credentials and 401s the admin API, so every call here runs via
# `env -u GITLAB_TOKEN glab ...` regardless of your shell's environment.
set -euo pipefail

HOSTNAME="${HOSTNAME:-gitlab.example.com}"
SCOPES="${SCOPES:-api}"
EXPIRES_AT="${EXPIRES_AT:-$(date -v+90d +%F 2>/dev/null || date -d '+90 days' +%F)}"
DEVELOPER_ACCESS_LEVEL=30

usage() {
  echo "Usage: $0 <bot-username> <project-path-or-id> [email]" >&2
  exit 1
}
[ $# -ge 2 ] || usage
BOT_USERNAME="$1"
PROJECT="$2"
EMAIL="${3:-${BOT_USERNAME}@users.noreply.${HOSTNAME}}"
PROJECT_ENC="${PROJECT//\//%2F}"

info() { printf '==> %s\n' "$1"; }
warn() { printf '\033[33mWARN\033[0m %s\n' "$1" >&2; }
die() { printf '\033[31mERROR\033[0m %s\n' "$1" >&2; exit 1; }

glab_api() { env -u GITLAB_TOKEN glab api --hostname "$HOSTNAME" "$@"; }

json_field() {
  # json_field <compact-json> <key> — extracts a bare (unquoted) numeric or
  # string value for a top-level key from glab's compact JSON output. Good
  # enough for the single-purpose extractions below; not a general parser.
  printf '%s' "$1" | grep -o "\"$2\":[0-9]*" | head -1 | grep -o '[0-9]*$' \
    || printf '%s' "$1" | grep -o "\"$2\":\"[^\"]*\"" | head -1 | sed "s/.*\"$2\":\"//;s/\"\$//"
}

info "checking glab auth against $HOSTNAME"
env -u GITLAB_TOKEN glab auth status --hostname "$HOSTNAME" >/dev/null 2>&1 \
  || die "not authenticated against $HOSTNAME as an admin; run: glab auth login --hostname $HOSTNAME"

info "looking up existing user $BOT_USERNAME"
existing="$(glab_api "users?username=${BOT_USERNAME}")"
bot_id="$(json_field "$existing" id)"

if [ -n "$bot_id" ]; then
  info "bot user $BOT_USERNAME already exists (id $bot_id) — reusing"
else
  info "creating bot user $BOT_USERNAME <$EMAIL>"
  created="$(glab_api users -X POST \
    --raw-field "username=${BOT_USERNAME}" \
    --raw-field "name=${BOT_USERNAME}" \
    --raw-field "email=${EMAIL}" \
    --field "skip_confirmation=true" \
    --field "force_random_password=true")"
  bot_id="$(json_field "$created" id)"
  [ -n "$bot_id" ] || die "user creation failed: $created"
  info "created bot user $BOT_USERNAME (id $bot_id)"
fi

info "minting a PAT (scope: $SCOPES, expires: $EXPIRES_AT)"
pat_json="$(printf '{"name":"uzi-bot","scopes":["%s"],"expires_at":"%s"}' "$SCOPES" "$EXPIRES_AT" \
  | glab_api "users/${bot_id}/personal_access_tokens" -X POST --input -)"
token="$(json_field "$pat_json" token)"
[ -n "$token" ] || die "PAT creation failed: $pat_json"

info "checking membership on $PROJECT"
if glab_api "projects/${PROJECT_ENC}/members/${bot_id}" >/dev/null 2>&1; then
  info "already a member — ensuring Developer access"
  glab_api "projects/${PROJECT_ENC}/members/${bot_id}" -X PUT \
    --field "access_level=${DEVELOPER_ACCESS_LEVEL}" >/dev/null
else
  info "adding $BOT_USERNAME to $PROJECT as Developer"
  glab_api "projects/${PROJECT_ENC}/members" -X POST \
    --raw-field "user_id=${bot_id}" \
    --field "access_level=${DEVELOPER_ACCESS_LEVEL}" >/dev/null
fi

echo
echo "Bot ready: ${BOT_USERNAME} (id ${bot_id}) — Developer on ${PROJECT}"
echo
printf '\033[33mSAVE THIS NOW — GitLab will not show it again:\033[0m\n'
printf '  %s\n' "$token"
echo
echo "Paste it into uzi: Settings -> Forge -> Base URL https://${HOSTNAME} -> Token."
