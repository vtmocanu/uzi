#!/bin/sh
# Hermetic command/exit-code tests: npm is a fixture server, never the registry.
set -eu
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
GATE="$ROOT/scripts/npm-audit-gate.sh"
regression_only=0
if [ "$#" -ne 0 ]; then
  [ "$#" -eq 2 ] && [ "$1" = "--regression-gate" ] || exit 2
  GATE="$2"
  regression_only=1
fi
TMP="$(mktemp -d)"
# shellcheck disable=SC2064
trap "rm -rf '$TMP'" EXIT INT TERM
today="$(node -p 'new Date().toISOString().slice(0,10)')"
yesterday="$(node -p 'new Date(Date.now()-86400000).toISOString().slice(0,10)')"

setup() {
  name="$1"
  scenario="$2"
  CASE_DIR="$TMP/$name"
  mkdir -p "$CASE_DIR/scripts" "$CASE_DIR/bin" "$CASE_DIR/web"
  cp "$GATE" "$CASE_DIR/scripts/npm-audit-gate.sh"
  cp "$ROOT/scripts/npm-audit-exceptions.mjs" "$CASE_DIR/scripts/"
  cp "$ROOT/scripts/fixtures/npm-audit/web.json" "$CASE_DIR/audit.json"
  cp "$ROOT/scripts/fixtures/npm-audit/web-lock.json" "$CASE_DIR/web/package-lock.json"
  cp "$ROOT/scripts/fixtures/npm-audit/web-package.json" "$CASE_DIR/web/package.json"
  printf 'component\tghsa\tpackage\trange\tmax_severity\trecheck_by\tissue\nweb\tGHSA-fc5q-39xc-vjpf\tpicomatch\t<=4.0.7\thigh\t%s\t#4435\n' "$today" >"$CASE_DIR/scripts/npm-audit-exceptions.tsv"
  : >"$CASE_DIR/calls"
  cat >"$CASE_DIR/bin/npm" <<'STUB'
#!/bin/sh
set -eu
if [ "$1" = run ]; then
  echo text >>"$AUDIT_CALLS"
  case "$SCENARIO" in
    clean|stale) echo 'found 0 vulnerabilities'; exit 0 ;;
    below) echo '# npm audit report'; echo '1 moderate severity vulnerability'; exit 0 ;;
    network) echo 'npm error audit endpoint returned an error ECONNREFUSED'; exit 1 ;;
    no-report) echo undefined; exit 1 ;;
    text-bad-status) echo '# npm audit report'; exit 3 ;;
    *) echo '# npm audit report'; echo '10 high severity vulnerabilities'; exit 1 ;;
  esac
fi
[ "$1" = audit ] && [ "$2" = --json ] && [ "$3" = --include=dev ] && [ "$#" -eq 3 ] || exit 9
echo json >>"$AUDIT_CALLS"
case "$SCENARIO" in
  bad-json) echo '{'; exit 1 ;;
  json-status) cat "$AUDIT_JSON"; exit 3 ;;
  json-network) echo ECONNREFUSED >&2; echo '{"error":{"code":"ECONNREFUSED"}}'; exit 1 ;;
  json-retry)
    count="$(awk '$0=="json"{n++} END{print n+0}' "$AUDIT_CALLS")"
    if [ "$count" -eq 1 ]; then
      echo ECONNREFUSED >&2
      echo '{"error":{"code":"ECONNREFUSED"}}'
      exit 1
    fi
    ;;
esac
cat "$AUDIT_JSON"
exit 1
STUB
  chmod +x "$CASE_DIR/bin/npm"
}
check() {
  expected="$1"
  package_dir=web
  if [ "$#" -ge 2 ]; then package_dir="$2"; fi
  actual=0
  (cd "$CASE_DIR" && PATH="$CASE_DIR/bin:$PATH" SCENARIO="$scenario" AUDIT_CALLS="$CASE_DIR/calls" \
    AUDIT_JSON="$CASE_DIR/audit.json" UZI_NPM_AUDIT_ATTEMPTS=2 UZI_NPM_AUDIT_BACKOFF_SECONDS=0 \
    "$CASE_DIR/scripts/npm-audit-gate.sh" "$package_dir" high) >"$CASE_DIR/out" 2>&1 || actual=$?
  if [ "$actual" -ne "$expected" ]; then
    cat "$CASE_DIR/out" >&2
    echo "FAIL $name: exit $actual, expected $expected" >&2
    exit 1
  fi
  echo "PASS $name (exit $actual)"
}
has() { grep -F "$1" "$CASE_DIR/out" >/dev/null || { cat "$CASE_DIR/out" >&2; exit 1; }; }
no_json() { if grep -F json "$CASE_DIR/calls" >/dev/null; then echo "unexpected JSON call: $name" >&2; exit 1; fi; }
no_rows() { printf 'component\tghsa\tpackage\trange\tmax_severity\trecheck_by\tissue\n' >"$CASE_DIR/scripts/npm-audit-exceptions.tsv"; }

setup real-fixture findings
check 0
has 'EXCEPTED GHSA-fc5q-39xc-vjpf picomatch'
if grep -F 'clean at' "$CASE_DIR/out" >/dev/null; then exit 1; fi
[ "$regression_only" -eq 0 ] || exit 0

for scenario_name in clean below findings; do
  setup "no-rows-$scenario_name" "$scenario_name"
  no_rows
  case "$scenario_name" in findings) check 1 ;; *) check 0 ;; esac
  no_json
done
setup stale-on-clean stale
printf '%s\n' '{"auditReportVersion":2,"vulnerabilities":{}}' >"$CASE_DIR/audit.json"
check 1
has 'unused exception'
setup stale-on-below below
node -e 'const fs=require("node:fs"),r=JSON.parse(fs.readFileSync(process.argv[1]));for(const v of Object.values(r.vulnerabilities)){v.severity="moderate";v.via=v.via.map(x=>typeof x==="object"?{...x,severity:"moderate"}:x)}process.stdout.write(JSON.stringify(r))' "$ROOT/scripts/fixtures/npm-audit/web.json" >"$CASE_DIR/audit.json"
check 1
has 'unused exception'
setup expired findings
printf 'component\tghsa\tpackage\trange\tmax_severity\trecheck_by\tissue\nweb\tGHSA-fc5q-39xc-vjpf\tpicomatch\t<=4.0.7\thigh\t%s\t#4435\n' "$yesterday" >"$CASE_DIR/scripts/npm-audit-exceptions.tsv"
check 1
has 'expired exception'
setup prod findings
node -e 'const fs=require("node:fs"),r=JSON.parse(fs.readFileSync(process.argv[1]));r.packages["node_modules/picomatch"].dev=false;process.stdout.write(JSON.stringify(r))' "$ROOT/scripts/fixtures/npm-audit/web-lock.json" >"$CASE_DIR/web/package-lock.json"
check 1
has 'production occurrence'
setup manifest-lock-mismatch findings
node -e 'const fs=require("node:fs"),r=JSON.parse(fs.readFileSync(process.argv[1]));r.dependencies.knip=r.devDependencies.knip;delete r.devDependencies.knip;process.stdout.write(JSON.stringify(r))' "$CASE_DIR/web/package.json" >"$CASE_DIR/changed-package.json"
mv "$CASE_DIR/changed-package.json" "$CASE_DIR/web/package.json"
check 2
has 'manifest and lockfile disagree; regenerate the lockfile'
setup missing-lock-root findings
node -e 'const fs=require("node:fs"),r=JSON.parse(fs.readFileSync(process.argv[1]));delete r.packages[""];process.stdout.write(JSON.stringify(r))' "$CASE_DIR/web/package-lock.json" >"$CASE_DIR/changed-lock.json"
mv "$CASE_DIR/changed-lock.json" "$CASE_DIR/web/package-lock.json"
check 2
has 'manifest and lockfile disagree; regenerate the lockfile'
for scenario_name in network no-report text-bad-status bad-json json-status json-network; do
  setup "$scenario_name" "$scenario_name"
  check 2
done
setup json-retry json-retry
check 0
[ "$(awk '$0=="json"{n++} END{print n+0}' "$CASE_DIR/calls")" -eq 2 ]
setup missing-flags findings
printf '%s\n' '{"scripts":{"audit":"npm audit --audit-level=high"}}' >"$CASE_DIR/web/package.json"
check 2
has 'does not carry --include=dev'
setup script-decoy findings
printf '%s\n' '{"scripts":{"audit":"echo npm audit --audit-level=high --include=dev"}}' >"$CASE_DIR/web/package.json"
check 2
has 'require the exact npm audit command'
setup duplicate-policy findings
tail -1 "$CASE_DIR/scripts/npm-audit-exceptions.tsv" >>"$CASE_DIR/scripts/npm-audit-exceptions.tsv"
check 2
has 'duplicate or conflicting'
setup malformed-policy findings
printf 'not a policy header\n' >"$CASE_DIR/scripts/npm-audit-exceptions.tsv"
check 2
has 'invalid exceptions header'
[ ! -s "$CASE_DIR/calls" ]
setup missing-node findings
printf '#!/bin/sh\nexit 127\n' >"$CASE_DIR/bin/node"
chmod +x "$CASE_DIR/bin/node"
check 2
has 'invalid exception policy'
[ ! -s "$CASE_DIR/calls" ]
setup symlink-component findings
ln -s "$CASE_DIR" "$TMP/linked"
check 0 "$TMP/linked/web"
has 'EXCEPTED'
setup outside-component findings
mkdir -p "$TMP/outside"
cp "$CASE_DIR/web/package.json" "$TMP/outside/package.json"
check 2 "$TMP/outside"
has 'package outside repository'
# Neither an environment variable nor extra CLI arguments can replace the policy or UTC date.
setup no-overrides findings
if node "$CASE_DIR/scripts/npm-audit-exceptions.mjs" has-rows "$CASE_DIR/web" --today 1970-01-01 >"$CASE_DIR/cli-out" 2>&1; then exit 1; fi
grep -F 'invalid evaluator arguments' "$CASE_DIR/cli-out" >/dev/null
echo 'PASS no policy/clock CLI overrides'
