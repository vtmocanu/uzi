#!/usr/bin/env bash
# Real SQL only against containers/volumes created here; never accept an external DSN.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RESET="$ROOT/e2e/reset-recovery-tables.sh"
IMAGE="${UZI_STORE_IT_PG_IMAGE:-postgres:17}"
MODE="${RESET_CLIENT_MODE:-docker}"
case "$MODE" in
  native) command -v psql >/dev/null || { echo 'native mode requires psql; no fallback' >&2; exit 1; } ;;
  docker) ;;
  *) echo 'RESET_CLIENT_MODE must be native or docker' >&2; exit 1 ;;
esac
command -v docker >/dev/null || { echo 'Docker prerequisite missing' >&2; exit 1; }
docker info >/dev/null || { echo 'Docker daemon prerequisite failed' >&2; exit 1; }
mkdir -p "$ROOT/.uzi/scratch"
TMP="$(mktemp -d "$ROOT/.uzi/scratch/reset-live.XXXXXX")"
NAME="rrt-live-$$-$RANDOM"
VOLUME="$NAME-data"
OLD_VOLUME="$NAME-old-data"
CONTAINER_CREATED=0
VOLUME_CREATED=0
OLD_VOLUME_CREATED=0
cleanup() {
  local rc=$?
  # Only exact session-owned objects. A failed cleanup is reported, never broadened.
  if [ "$CONTAINER_CREATED" -eq 1 ]; then
    docker rm -f "$NAME" > "$TMP/cleanup.log" 2>&1 || echo "cleanup failed: $NAME (see $TMP/cleanup.log)" >&2
  fi
  if [ "$VOLUME_CREATED" -eq 1 ]; then
    docker volume rm "$VOLUME" >> "$TMP/cleanup.log" 2>&1 || echo "volume cleanup failed: $VOLUME (see $TMP/cleanup.log)" >&2
  fi
  if [ "$OLD_VOLUME_CREATED" -eq 1 ]; then
    docker volume rm "$OLD_VOLUME" >> "$TMP/cleanup.log" 2>&1 || echo "volume cleanup failed: $OLD_VOLUME (see $TMP/cleanup.log)" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT
if docker container inspect "$NAME" >/dev/null 2>&1; then
  echo "REFUSING: container already exists: $NAME" >&2
  exit 1
fi
for volume in "$VOLUME" "$OLD_VOLUME"; do
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    echo "REFUSING: volume already exists: $volume" >&2
    exit 1
  fi
done
docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE"
docker volume create "$VOLUME" >/dev/null
VOLUME_CREATED=1
docker volume create "$OLD_VOLUME" >/dev/null
OLD_VOLUME_CREATED=1
remove_owned_container() {
  [ "$CONTAINER_CREATED" -eq 1 ] || return 0
  docker rm -f "$NAME" >/dev/null
  CONTAINER_CREATED=0
}
start() {
  local volume="$1" marker="$2"
  local mounts=()
  if [ "$marker" = yes ]; then
    mounts=(--mount "type=bind,source=$ROOT/e2e/test-database-marker.sql,target=/docker-entrypoint-initdb.d/10-uzi-test-database-marker.sql,readonly")
  fi
  if docker container inspect "$NAME" >/dev/null 2>&1; then
    echo "REFUSING: container already exists: $NAME" >&2
    return 1
  fi
  docker run -d --name "$NAME" -e POSTGRES_PASSWORD=owned-test-password \
    -p 127.0.0.1::5432 --mount "type=volume,source=$volume,target=/var/lib/postgresql/data" \
    "${mounts[@]}" "$IMAGE" > "$TMP/container-id"
  CONTAINER_CREATED=1
  # Bounded 90 probes; stop on failure rather than run tests against an unready DB.
  local ready=no
  for ((i=0; i<90; i++)); do
    if docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then ready=yes; break; fi
    sleep 1
  done
  [ "$ready" = yes ] || { docker logs "$NAME"; echo 'Postgres readiness failed' >&2; return 1; }
  local port
  port="$(docker port "$NAME" 5432/tcp)"
  [[ "$port" =~ ^127\.0\.0\.1:([0-9]+)$ ]] || { echo "unexpected owned port: $port"; return 1; }
  DSN="postgres://postgres:owned-test-password@127.0.0.1:${BASH_REMATCH[1]}/postgres?sslmode=disable"
}
sql() { docker exec -i "$NAME" psql -X -U postgres -v ON_ERROR_STOP=1 -Atq; }
# Force a deterministic client branch without depending on the developer's PATH.
mkdir "$TMP/bin"
for tool in bash dirname docker; do ln -s "$(command -v "$tool")" "$TMP/bin/$tool"; done
if [ "$MODE" = native ]; then ln -s "$(command -v psql)" "$TMP/bin/psql"; fi
invoke() {
  local dsn="${1:-$DSN}"
  rc=0
  PATH="$TMP/bin" UZI_STORE_IT_PG_IMAGE="$IMAGE" bash "$RESET" --dsn "$dsn" > "$TMP/reset.out" 2>&1 || rc=$?
}
seed() {
  sql <<'SQL'
DROP TABLE IF EXISTS public.run_held_publications;
DROP TABLE IF EXISTS public.recovery_capture_chunks;
DROP TABLE IF EXISTS public.recovery_captures;
DROP TABLE IF EXISTS public.recovery_custody_holds;
DROP TABLE IF EXISTS public.custody_episode_notices;
DROP TABLE IF EXISTS public.unrelated;
CREATE TABLE public.recovery_custody_holds (id int PRIMARY KEY);
CREATE TABLE public.recovery_captures (id int PRIMARY KEY, hold_id int REFERENCES public.recovery_custody_holds ON DELETE RESTRICT);
CREATE TABLE public.recovery_capture_chunks (id int PRIMARY KEY, capture_id int REFERENCES public.recovery_captures ON DELETE CASCADE);
CREATE TABLE public.custody_episode_notices (id int PRIMARY KEY);
CREATE TABLE public.unrelated (id int PRIMARY KEY);
INSERT INTO public.recovery_custody_holds VALUES (1);
INSERT INTO public.recovery_captures VALUES (2,1);
INSERT INTO public.recovery_capture_chunks VALUES (3,2);
INSERT INTO public.custody_episode_notices VALUES (4);
INSERT INTO public.unrelated VALUES (5);
SQL
}
marker() {
  sql <<'SQL'
DROP SCHEMA IF EXISTS uzi_test_harness CASCADE;
SQL
  sql < "$ROOT/e2e/test-database-marker.sql"
}
rows() {
  sql <<'SQL'
SELECT 'chunks',id,capture_id FROM public.recovery_capture_chunks;
SELECT 'captures',id,hold_id FROM public.recovery_captures;
SELECT 'holds',id FROM public.recovery_custody_holds;
SELECT 'notices',id FROM public.custody_episode_notices;
SELECT 'unrelated',id FROM public.unrelated;
SQL
}
refuse() {
  local name="$1" diagnostic="$2" dsn="${3:-$DSN}" before after
  before="$(rows)"
  invoke "$dsn"
  after="$(rows)" # Separate connection proves rollback, rather than an aborted session snapshot.
  if [ "$rc" -eq 0 ] || ! grep -Fq "$diagnostic" "$TMP/reset.out" ||
     grep -Eq 'clearing complete|: done\.|deleted chunks=' "$TMP/reset.out" || [ "$before" != "$after" ]; then
    echo "FAIL: $name (rc=$rc; expected $diagnostic)" >&2
    cat "$TMP/reset.out" >&2
    printf 'before=%s\nafter=%s\n' "$before" "$after" >&2
    return 1
  fi
  echo "PASS: $name"
}
success() {
  invoke
  [ "$rc" -eq 0 ] && grep -Fq 'clearing complete' "$TMP/reset.out" || { cat "$TMP/reset.out"; return 1; }
  [ "$(rows)" = 'unrelated|5' ]
  [ "$(sql <<< "SELECT marker FROM uzi_test_harness.database_marker;")" = recovery-reset-v1 ]
}
# Compose rendering uses dummy required vars in an empty environment, and starts nothing.
env -i HOME="$HOME" PATH="$PATH" POSTGRES_PASSWORD=owned-dummy \
  JWT_SECRET=owned-dummy-jwt UZI_SECRET_KEY=owned-dummy-key E2E_WEB_PORT=29876 \
  E2E_RUN_DIR="$TMP" E2E_FAKE_PORT=29877 \
  docker compose --project-directory "$ROOT" -p "$NAME" \
  -f "$ROOT/docker-compose.yml" -f "$ROOT/e2e/docker-compose.e2e.yml" \
  config --format json > "$TMP/compose.json"
python3 - "$TMP/compose.json" "$ROOT" <<'PY'
import json, sys
db = json.load(open(sys.argv[1]))['services']['db']
assert not db.get('ports'), db
volumes = db['volumes']
marker = [v for v in volumes if v['target'] == '/docker-entrypoint-initdb.d/10-uzi-test-database-marker.sql']
assert len(marker) == 1 and marker[0]['type'] == 'bind' and marker[0]['read_only']
assert marker[0]['source'] == sys.argv[2] + '/e2e/test-database-marker.sql', marker
assert any(v['type'] == 'volume' and v['source'] == 'pgdata' and v['target'] == '/var/lib/postgresql/data' for v in volumes), volumes
PY
echo 'PASS: compose marker mount, pgdata retained, no db published port'
start "$VOLUME" yes
[ "$(sql <<< 'SELECT marker FROM uzi_test_harness.database_marker;')" = recovery-reset-v1 ]
echo 'PASS: fresh initdb mount automatically creates marker'
remove_owned_container
start "$VOLUME" yes
[ "$(sql <<< 'SELECT marker FROM uzi_test_harness.database_marker;')" = recovery-reset-v1 ]
echo 'PASS: initialized marker persists across restart'
seed
sql <<< 'DROP SCHEMA uzi_test_harness CASCADE;'
refuse missing-marker 'missing test database marker'
sql <<'SQL'
CREATE OR REPLACE FUNCTION public.reset_order_fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'reset-live deletion before marker probe'; END $$;
CREATE TRIGGER reset_order_fail BEFORE DELETE ON public.recovery_capture_chunks FOR EACH ROW EXECUTE FUNCTION public.reset_order_fail();
SQL
refuse marker-before-delete 'missing test database marker'
sql <<< 'DROP TRIGGER reset_order_fail ON public.recovery_capture_chunks;'
marker
sql <<< 'DELETE FROM uzi_test_harness.database_marker;'
refuse empty-marker 'expected exactly one'
sql <<< "INSERT INTO uzi_test_harness.database_marker VALUES ('wrong');"
refuse wrong-marker 'expected exactly one'
marker
sql <<< "INSERT INTO uzi_test_harness.database_marker VALUES ('wrong');"
refuse extra-marker 'expected exactly one'
sql <<< "UPDATE uzi_test_harness.database_marker SET marker='also-wrong' WHERE marker='recovery-reset-v1';"
refuse two-wrong-markers 'expected exactly one'
sql <<'SQL'
DROP TABLE uzi_test_harness.database_marker;
CREATE TABLE uzi_test_harness.database_marker (marker text);
INSERT INTO uzi_test_harness.database_marker VALUES ('recovery-reset-v1'),(NULL);
SQL
refuse nullable-extra-marker 'expected exactly one'
sql <<'SQL'
DROP TABLE uzi_test_harness.database_marker;
CREATE TABLE uzi_test_harness.database_marker (other text);
INSERT INTO uzi_test_harness.database_marker VALUES ('recovery-reset-v1');
SQL
refuse missing-column 'column "marker" does not exist'
marker
sql <<'SQL'
CREATE ROLE reset_restricted LOGIN PASSWORD 'owned-role-password';
GRANT USAGE ON SCHEMA public, uzi_test_harness TO reset_restricted;
GRANT SELECT, DELETE ON public.recovery_capture_chunks, public.recovery_captures,
 public.recovery_custody_holds, public.custody_episode_notices, public.unrelated TO reset_restricted;
SELECT has_table_privilege('reset_restricted','public.recovery_capture_chunks','SELECT,DELETE');
SQL
# Prove actual SELECT/DELETE capability on every recovery table, with rollback.
docker exec -i -e PGPASSWORD=owned-role-password "$NAME" psql -X -h 127.0.0.1 -U reset_restricted -d postgres -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
SELECT * FROM public.recovery_capture_chunks;
SELECT * FROM public.recovery_captures;
SELECT * FROM public.recovery_custody_holds;
SELECT * FROM public.custody_episode_notices;
DELETE FROM public.recovery_capture_chunks;
DELETE FROM public.recovery_captures;
DELETE FROM public.recovery_custody_holds;
DELETE FROM public.custody_episode_notices;
ROLLBACK;
SQL
restricted="${DSN/postgres:owned-test-password/reset_restricted:owned-role-password}"
refuse unreadable-marker 'permission denied for table database_marker' "$restricted"
refuse connection-failure 'password authentication failed' "${DSN/owned-test-password/wrong-password}"
success
success
echo 'PASS: valid marker clears four tables, preserves unrelated/marker, idempotent'
seed
sql <<'SQL'
CREATE OR REPLACE FUNCTION public.reset_fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'reset-live injected final notice failure'; END $$;
CREATE TRIGGER reset_fail BEFORE DELETE ON public.custody_episode_notices FOR EACH ROW EXECUTE FUNCTION public.reset_fail();
SQL
refuse atomic-final-delete 'reset-live injected final notice failure'
seed
sql <<'SQL'
CREATE TABLE public.run_held_publications (id int PRIMARY KEY, hold_id int NOT NULL REFERENCES public.recovery_custody_holds(id) ON DELETE RESTRICT);
INSERT INTO public.run_held_publications VALUES (6,1);
SQL
refuse held-publication-restrict 'run_held_publications_hold_id_fkey'
[ "$(sql <<< 'SELECT id,hold_id FROM public.run_held_publications;')" = '6|1' ]
echo 'PASS: held publication remains intact'
if [ "${RESET_MUTATIONS:-0}" = 1 ]; then
  # Source exports stay under scratch. Mutants use ONLY this script's owned DB.
  mkdir -p "$TMP/exports"
  python3 - "$RESET" "$TMP/exports" <<'PY'
from pathlib import Path
import sys
s = Path(sys.argv[1]).read_text()
start = s.index("  IF to_regclass('uzi_test_harness.database_marker')")
end = s.index('  -- run_held_publications', start)
guard = s[start:end]
mutants = {
 'bypass-guard': s[:start] + s[end:],
 'accept-wrong': s.replace("WHERE marker = 'recovery-reset-v1'", "WHERE marker IS NOT NULL"),
 'accept-extra': s.replace("(SELECT count(*) FROM uzi_test_harness.database_marker) <> 1", "(SELECT count(*) FROM uzi_test_harness.database_marker) < 1"),
 'delete-before-guard': s[:start] + "  DELETE FROM public.recovery_capture_chunks;\n" + s[start:],
}
# Break atomicity between the hold chain and the final notice deletion.
needle = "  -- Independent of the hold chain; cleared last."
mutants['break-atomicity'] = s.replace(needle, "END $$;\nDO $$\nDECLARE n_notices int := 0; n_chunks int := 0; n_captures int := 0; n_holds int := 0;\nBEGIN\n" + needle)
for name, source in mutants.items():
    export = Path(sys.argv[2]) / name / 'e2e'
    export.mkdir(parents=True)
    (export / 'reset-recovery-tables.sh').write_text(source)
PY
  original="$RESET"
  for mutation in bypass-guard accept-wrong accept-extra delete-before-guard break-atomicity; do
    RESET="$TMP/exports/$mutation/e2e/reset-recovery-tables.sh"
    seed
    marker
    case "$mutation" in
      bypass-guard|delete-before-guard)
        sql <<< 'DROP SCHEMA uzi_test_harness CASCADE;'
        if [ "$mutation" = delete-before-guard ]; then
          sql <<'SQL'
CREATE OR REPLACE FUNCTION public.reset_order_fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'reset-live deletion before marker probe'; END $$;
CREATE TRIGGER reset_order_fail BEFORE DELETE ON public.recovery_capture_chunks FOR EACH ROW EXECUTE FUNCTION public.reset_order_fail();
SQL
        fi
        name=missing-marker; diagnostic='missing test database marker' ;;
      accept-wrong)
        sql <<< "UPDATE uzi_test_harness.database_marker SET marker='wrong';"
        name=wrong-marker; diagnostic='expected exactly one' ;;
      accept-extra)
        sql <<< "INSERT INTO uzi_test_harness.database_marker VALUES ('wrong');"
        name=extra-marker; diagnostic='expected exactly one' ;;
      break-atomicity)
        sql <<'SQL'
CREATE TRIGGER reset_fail BEFORE DELETE ON public.custody_episode_notices FOR EACH ROW EXECUTE FUNCTION public.reset_fail();
SQL
        name=atomic-final-delete; diagnostic='reset-live injected final notice failure' ;;
    esac
    if refuse "$name" "$diagnostic" > "$TMP/mutation-$mutation.log" 2>&1; then
      echo "FAIL: mutation survived: $mutation" >&2; exit 1
    fi
    grep -Fq "FAIL: $name" "$TMP/mutation-$mutation.log"
    # Prove a behavioral failure, not a syntax/build error in the mutant.
    case "$mutation" in
      bypass-guard|accept-wrong|accept-extra)
        grep -Fq '(rc=0;' "$TMP/mutation-$mutation.log"
        grep -Fq 'after=unrelated|5' "$TMP/mutation-$mutation.log" ;;
      delete-before-guard)
        grep -Fq 'ERROR:  reset-live deletion before marker probe' "$TMP/mutation-$mutation.log" ;;
      break-atomicity)
        grep -Fq 'ERROR:  reset-live injected final notice failure' "$TMP/mutation-$mutation.log"
        grep -Fq 'after=notices|4' "$TMP/mutation-$mutation.log" ;;
    esac
    echo "PASS: mutation killed: $mutation by $name ($TMP/mutation-$mutation.log)"
  done
  RESET="$original"
  marker
fi
# Marked but no recovery tables is a valid no-op.
seed
sql <<'SQL'
DROP TABLE public.recovery_capture_chunks, public.recovery_captures, public.recovery_custody_holds, public.custody_episode_notices;
SQL
invoke
[ "$rc" -eq 0 ] && grep -Fq 'deleted chunks=0 captures=0 holds=0 episode_notices=0' "$TMP/reset.out"
echo 'PASS: marked missing recovery schema no-op'
# Reusing an initialized unmarked volume must NOT execute the added initdb mount.
remove_owned_container
start "$OLD_VOLUME" no
[ "$(sql <<< "SELECT to_regclass('uzi_test_harness.database_marker') IS NULL;")" = t ]
remove_owned_container
start "$OLD_VOLUME" yes
[ "$(sql <<< "SELECT to_regclass('uzi_test_harness.database_marker') IS NULL;")" = t ]
seed
refuse retained-unmarked-volume 'missing test database marker'
echo "PASS: live reset contract ($MODE); logs $TMP"
