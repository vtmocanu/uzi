#!/usr/bin/env bash
# End-to-end smoke test for the uzi auth API. Exercises the full journey plus a
# concurrent first-registration race check.
#
# Expects a FRESH stack (empty users table):
#   docker compose down -v && docker compose up -d --build && ./scripts/smoke.sh
#
# Override the base URL with BASE=... (defaults to the nginx origin).
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:8080}"
PASSWORD="correct-horse-battery-staple"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

# csrf_from JAR — extract the readable CSRF token from a curl cookie jar.
csrf_from() { awk '$6=="uzi_csrf"{print $7}' "$1"; }

echo "==> waiting for $BASE/api/health"
for i in $(seq 1 60); do
  if curl -fsS "$BASE/api/health" >/dev/null 2>&1; then break; fi
  [ "$i" = 60 ] && fail "health never came up"
  sleep 1
done
pass "health is up"

# ---------------------------------------------------------------------------
echo "==> concurrent first-registration race (expect exactly one admin)"
pids=()
for n in 1 2 3 4 5; do
  ( curl -fsS -X POST "$BASE/api/auth/register" \
      -H 'Content-Type: application/json' \
      -d "{\"email\":\"race${n}@uzi.test\",\"password\":\"${PASSWORD}\",\"display_name\":\"Race ${n}\"}" \
      >"$WORK/race${n}.json" 2>/dev/null ) &
  pids+=($!)
done
for p in "${pids[@]}"; do wait "$p" || true; done

admins=0
admin_email=""
regular_email=""
for n in 1 2 3 4 5; do
  body="$(cat "$WORK/race${n}.json" 2>/dev/null || true)"
  email="race${n}@uzi.test"
  if printf '%s' "$body" | grep -q '"is_admin":true'; then
    admins=$((admins + 1))
    admin_email="$email"
  elif printf '%s' "$body" | grep -q '"is_admin":false'; then
    regular_email="$email"
  fi
done
[ "$admins" -eq 1 ] || fail "expected exactly 1 admin from the race, got $admins (need a fresh DB: docker compose down -v)"
pass "exactly one admin elected: $admin_email"
[ -n "$regular_email" ] || fail "no regular user produced by the race"

# ---------------------------------------------------------------------------
echo "==> admin login + /me"
admin_jar="$WORK/admin.jar"
curl -fsS -c "$admin_jar" -X POST "$BASE/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${admin_email}\",\"password\":\"${PASSWORD}\"}" >/dev/null
me="$(curl -fsS -b "$admin_jar" "$BASE/api/auth/me")"
printf '%s' "$me" | grep -q '"is_admin":true' || fail "/me did not report admin"
pass "admin logged in, /me confirms admin"

echo "==> admin lists users (expect 5)"
users="$(curl -fsS -b "$admin_jar" "$BASE/api/admin/users")"
count="$(printf '%s' "$users" | grep -o '"email":' | wc -l | tr -d ' ')"
[ "$count" -eq 5 ] || fail "expected 5 users, got $count"
pass "admin sees 5 users"

# ---------------------------------------------------------------------------
echo "==> regular user login"
user_jar="$WORK/user.jar"
curl -fsS -c "$user_jar" -X POST "$BASE/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${regular_email}\",\"password\":\"${PASSWORD}\"}" >/dev/null
curl -fsS -b "$user_jar" "$BASE/api/auth/me" >/dev/null
pass "regular user logged in, session live"

# find the regular user's id
regular_id="$(printf '%s' "$users" \
  | tr '}' '\n' | grep "\"email\":\"${regular_email}\"" \
  | grep -o '"id":"[^"]*"' | head -1 | sed 's/.*:"//;s/"//')"
[ -n "$regular_id" ] || fail "could not resolve regular user id"

echo "==> non-admin is forbidden from admin endpoints"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$user_jar" "$BASE/api/admin/users")"
[ "$code" = 403 ] || fail "expected 403 for non-admin on /admin/users, got $code"
pass "non-admin blocked from admin API (403)"

# ---------------------------------------------------------------------------
echo "==> admin deactivates the regular user (revocation)"
csrf="$(csrf_from "$admin_jar")"
[ -n "$csrf" ] || fail "no CSRF token in admin jar"
curl -fsS -b "$admin_jar" -X PATCH "$BASE/api/admin/users/${regular_id}" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: ${csrf}" \
  -d '{"is_active":false}' >/dev/null
pass "deactivation request accepted"

echo "==> deactivated user's live session is killed"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$user_jar" "$BASE/api/auth/me")"
[ "$code" = 401 ] || fail "expected 401 for revoked session, got $code"
pass "revoked session returns 401"

echo "==> deactivated user cannot log in"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${regular_email}\",\"password\":\"${PASSWORD}\"}")"
[ "$code" = 403 ] || fail "expected 403 on deactivated login, got $code"
pass "deactivated login blocked (403)"

echo "==> CSRF is enforced (PATCH without token is rejected)"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$admin_jar" -X PATCH \
  "$BASE/api/admin/users/${regular_id}" \
  -H 'Content-Type: application/json' -d '{"is_active":true}')"
[ "$code" = 403 ] || fail "expected 403 for missing CSRF, got $code"
pass "missing CSRF rejected (403)"

echo "==> wrong password yields generic 401"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${admin_email}\",\"password\":\"wrong-password-here\"}")"
[ "$code" = 401 ] || fail "expected 401 for wrong password, got $code"
pass "wrong password returns 401"

echo "==> admin logout revokes admin session"
csrf="$(csrf_from "$admin_jar")"
curl -fsS -b "$admin_jar" -X POST "$BASE/api/auth/logout" \
  -H "X-CSRF-Token: ${csrf}" >/dev/null
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$admin_jar" "$BASE/api/auth/me")"
[ "$code" = 401 ] || fail "expected 401 after logout, got $code"
pass "post-logout session returns 401"

echo
printf '\033[32mAll smoke checks passed.\033[0m\n'
