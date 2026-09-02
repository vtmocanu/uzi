# shellcheck shell=bash
# phase:    autopilot-carry-item
# title:    carry-item: concurrent cross-key settings PUT — the FOR UPDATE serialization rejects the equal-label race
# critical: no
# lane:     gitlab
# executor: any
# race-sensitive: yes
# requires: -
# provides: JAR2
# handoff:  -
# mutates:  uzi_label/autopilot_label (raced by the concurrent PUTs, restored to defaults below); registers user2 + forge connection CONN2
# restores: uzi_label/autopilot_label->defaults (admin settings PUT after the race)
# --- autopilot #4: carry-item e2e (settings race + username collision) -------
# CROSS-PHASE DEPENDENCY (documented, deliberately NOT a `requires:`): the 409
# collision below (the user2 forge-connection PUT) relies on 29-autopilot-lifecycle having mapped owner-alice.
# That is DB state (a hardcoded literal 'owner-alice', not a round-tripped shell var),
# so `requires:` — which only validates shell vars / env lines — cannot express it.
# JAR2 IS provided downstream (34-vault reads it with `-b "$JAR2"`), so it is declared
# in `provides:` and round-trips; user2/CONN2 stay in-phase (no later phase reads them).
say "carry-item: concurrent cross-key settings PUT — the FOR UPDATE serialization rejects the equal-label race"
# Two concurrent single-key PUTs that each pass the cache precheck but together would
# land uzi_label == autopilot_label (PRD #764). Exactly one commits; the other is
# rejected (400), whether by the in-tx FOR UPDATE cross-key check (true race) or the
# cache precheck (if it lost the race). Same admin session, two concurrent requests = two txns.
( apiput_code /api/admin/settings '{"settings":{"uzi_label":"SHARED"}}'        > "$RUNROOT/race.a" ) &
( apiput_code /api/admin/settings '{"settings":{"autopilot_label":"SHARED"}}' > "$RUNROOT/race.b" ) &
wait
CA="$(cat "$RUNROOT/race.a")"; CB="$(cat "$RUNROOT/race.b")"
ok=0; bad=0
for c in "$CA" "$CB"; do
  case "$c" in 200) ok=$((ok+1));; 400) bad=$((bad+1));; *) fail "unexpected settings PUT status: $c";; esac
done
{ [ "$ok" = 1 ] && [ "$bad" = 1 ]; } \
  || fail "concurrent cross-key PUT: expected one 200 + one 400, got $CA and $CB"
pass "concurrent cross-key settings PUT: exactly one accepted, one rejected (got $CA / $CB)"
# Restore the defaults so nothing downstream sees a half-applied label swap.
apiput /api/admin/settings '{"settings":{"uzi_label":"uzi","autopilot_label":"autopilot"}}' >/dev/null

say "carry-item: human_username collision — a second user claiming the same forge username on the same host is 409"
JAR2="$RUNROOT/u2.jar"
# Open registration issues a session (first user is the seeded admin, so this one is
# a normal user). The cookie jar carries both the session and the CSRF token.
curl -fsS -c "$JAR2" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d '{"email":"user2@uzi.e2e","password":"e2e-user2-password-000000","display_name":"User Two"}' >/dev/null
u2csrf="$(awk '$6=="uzi_csrf"{print $7}' "$JAR2")"
CONN2="$(curl -fsS -b "$JAR2" -X POST "$BASE/api/forge/connections" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $u2csrf" \
  -d "{\"forge_type\":\"gitlab\",\"base_url\":\"https://forge-fake.e2e\",\"token\":\"$DUMMY_FORGE_PAT\"}" \
  | jq -r '.connection.id // empty')"
[ -n "$CONN2" ] || fail "user2 could not connect to the fake forge"
# The admin already mapped owner-alice above; user2 claiming it on the same host must 409.
COLLIDE="$(curl -sS -o /dev/null -w '%{http_code}' -b "$JAR2" -X PUT "$BASE/api/forge/connections/$CONN2" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $u2csrf" \
  -d '{"human_username":"owner-alice"}')"
[ "$COLLIDE" = 409 ] || fail "second user mapping owner-alice should be 409, got $COLLIDE"
pass "human_username collision on the same host is rejected (409)"

