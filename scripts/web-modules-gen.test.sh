#!/usr/bin/env bash
# Hermetic test of scripts/web-modules-gen.py, the compose generator for the runtime web
# modules (PRD #4686). No docker, no network; needs bash, python3, jq and yq.
#
# What it pins:
#   - every accept/reject case of the shared upstream list (scripts/fixtures/web-modules/
#     upstreams.txt, the file the chart's render driver also reads);
#   - every chart values fixture (scripts/fixtures/web-modules/values-*.yaml): its
#     web.modules is read with yq into the generator's JSON input, so the fixture set is
#     literally shared. Valid ones are accepted, invalid ones are rejected with the chart's
#     message (modules[<i>] in place of web.modules[<i>]). Two fixtures are chart-only and are
#     skipped by name: values-no-resolver and values-bad-resolver test web.nginx.resolver,
#     which the generator does not have (compose's resolver is fixed at 127.0.0.11). A dotless
#     host is not expanded to <host>.<ns>.svc.cluster.local either, because compose resolves a
#     service name as it is;
#   - nothing is written on invalid input: the output directory is not created, and an
#     existing one is left as it was;
#   - modes: directory 0755 and both files 0644 under a restrictive umask;
#   - the rename order, observed through os.replace: locations.conf first, then modules.json,
#     each from a temporary file in the same directory, and modules.json absent at the first
#     rename (a first generation into an empty directory);
#   - the projection: exactly id, version, page, label, icon, navGroup, adminOnly (a boolean,
#     false by default), in input order, with no upstream and no unknown field;
#   - the emitted block equals scripts/fixtures/web-modules/location.example.conf with the
#     resolver 127.0.0.11 and the upstream filled in, byte for byte.
#
# Exit: 0 all passed, 1 a check failed, 2 the instrument is broken.
set -euo pipefail

CDPATH=''
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd)
GEN="$ROOT/scripts/web-modules-gen.py"
FIX="$ROOT/scripts/fixtures/web-modules"

broken() { echo "BROKEN: $*" >&2; exit 2; }
for t in python3 jq yq; do command -v "$t" >/dev/null 2>&1 || broken "$t is not on PATH"; done
[ -x "$GEN" ] || broken "$GEN is missing or not executable"
[ -f "$FIX/upstreams.txt" ] && [ -f "$FIX/location.example.conf" ] || broken "shared fixtures missing under $FIX"

W=$(mktemp -d)
cleanup() { rm -rf "$W"; }
trap cleanup EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $*" >&2; }

# mode_of <path>: octal mode, GNU or BSD stat.
mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# run_gen <input.json> <outdir>: sets RC and ERR (stderr text); stdout goes to the work dir.
run_gen() {
  RC=0
  python3 -I "$GEN" "$1" "$2" > "$W/gen.out" 2> "$W/gen.err" || RC=$?
  ERR=$(cat "$W/gen.err")
}

# mod_json <id> <upstream>: a one-module list.
mod_json() {
  jq -n --arg id "$1" --arg up "$2" \
    '[{id: $id, version: "1.0.0", page: "Page", label: "Label", icon: "package", navGroup: "modules", upstream: $up}]'
}

# --- the shared upstream list -----------------------------------------------------------------
n_accept=0
n_reject=0
GRAMMAR='is invalid: it must be [http://]HOST:PORT'
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    ''|'#'*) continue ;;
    accept\ \<*\>) kind=accept ;;
    reject\ \<*\>) kind=reject ;;
    *) broken "upstreams.txt: unreadable line: $line" ;;
  esac
  v=${line#*<}
  v=${v%>}
  mod_json m "$v" > "$W/up.json"
  out="$W/up-out"
  rm -rf "$out"
  run_gen "$W/up.json" "$out"
  if [ "$kind" = accept ]; then
    n_accept=$((n_accept + 1))
    want=${v#http://}
    if [ "$RC" -eq 0 ] && grep -qxF "    set \$uzi_module_m $want;" "$out/locations.conf"; then
      ok
    else
      bad "accept <$v>: exit $RC, $ERR"
    fi
  else
    n_reject=$((n_reject + 1))
    if [ "$RC" -eq 1 ] && [[ $ERR == *"modules[0] (m): upstream"*"$GRAMMAR"* ]] && [ ! -e "$out" ]; then
      ok
    else
      bad "reject <$v>: exit $RC, output dir present=$([ -e "$out" ] && echo yes || echo no), $ERR"
    fi
  fi
done < "$FIX/upstreams.txt"
[ "$n_accept" -ge 4 ] && [ "$n_reject" -ge 20 ] || broken "upstreams.txt yielded only $n_accept accept and $n_reject reject cases"

# --- the chart values fixtures -----------------------------------------------------------------
n_valid=0
n_invalid=0
n_skipped=0
# expect_valid <suffix> / expect_invalid <suffix> <message fragment, chart wording>
NAMED=()
fixture_json() { yq -o=json '.web.modules // []' "$FIX/values-$1.yaml" > "$W/fx.json" || broken "yq could not read values-$1.yaml"; }
expect_valid() {
  NAMED+=("$1")
  n_valid=$((n_valid + 1))
  fixture_json "$1"
  rm -rf "$W/fx-out"
  run_gen "$W/fx.json" "$W/fx-out"
  if [ "$RC" -eq 0 ] && [ -f "$W/fx-out/modules.json" ] && [ -f "$W/fx-out/locations.conf" ]; then ok; else bad "values-$1 should be accepted: exit $RC, $ERR"; fi
}
expect_invalid() {
  NAMED+=("$1")
  n_invalid=$((n_invalid + 1))
  fixture_json "$1"
  want=${2//web.modules[/modules[}
  rm -rf "$W/fx-out"
  run_gen "$W/fx.json" "$W/fx-out"
  if [ "$RC" -eq 1 ] && [[ $ERR == *"$want"* ]] && [ ! -e "$W/fx-out" ]; then
    ok
  else
    bad "values-$1 should be rejected with <$want>: exit $RC, output dir present=$([ -e "$W/fx-out" ] && echo yes || echo no), $ERR"
  fi
}
# expect_not_list <suffix>: the chart refuses a non-list web.modules; the generator is given the
# same value untouched (no `// []`, which would turn false into "no modules") and must refuse it.
expect_not_list() {
  NAMED+=("$1")
  n_invalid=$((n_invalid + 1))
  yq -o=json '.web.modules' "$FIX/values-$1.yaml" > "$W/fx.json" || broken "yq could not read values-$1.yaml"
  rm -rf "$W/fx-out"
  run_gen "$W/fx.json" "$W/fx-out"
  if [ "$RC" -eq 1 ] && [[ $ERR == *"must be a JSON array"* ]] && [ ! -e "$W/fx-out" ]; then
    ok
  else
    bad "values-$1 (web.modules not a list) should be rejected: exit $RC, $ERR"
  fi
}
expect_valid zero
expect_valid one
expect_valid two
expect_invalid bad-id 'id "Bad" is malformed'
expect_invalid bad-id-long 'is malformed'
expect_invalid dup-id 'web.modules[1]: duplicate id "same" (web.modules[0] already uses it)'
for f in version page label icon navGroup upstream; do
  expect_invalid "missing-$f" "missing required field \"$f\""
done
expect_invalid version-number 'missing required field "version"'
expect_invalid label-41 'label must be 1 to 40 characters'
expect_invalid label-41-multibyte 'label must be 1 to 40 characters'
expect_invalid label-control 'label contains control or invisible formatting characters'
expect_invalid label-bidi 'label contains control or invisible formatting characters'
expect_invalid label-newline 'label contains control or invisible formatting characters'
expect_invalid version-invisible 'version contains control or invisible formatting characters'
expect_invalid page-invisible 'page contains control or invisible formatting characters'
expect_invalid label-bom 'label contains control or invisible formatting characters'
expect_invalid id-number 'id must be a string'
expect_invalid unknown-icon 'unknown icon "rocket"'
expect_invalid unknown-navgroup 'unknown navGroup "sidebar"'
expect_invalid adminonly-string 'adminOnly must be a boolean'
expect_invalid module-not-map 'web.modules[0] must be a map'
for f in empty-map empty-string false zero; do
  expect_not_list "modules-$f"
done
# Chart-only: web.nginx.resolver has no counterpart in compose.
SKIPPED="no-resolver bad-resolver"
for f in $SKIPPED; do
  [ -f "$FIX/values-$f.yaml" ] || broken "the skipped fixture values-$f.yaml does not exist"
  n_skipped=$((n_skipped + 1))
done
# Every fixture must be named above or deliberately skipped: compare the NAMES, so a rename or a
# new file cannot go unread.
printf '%s\n' "${NAMED[@]}" $SKIPPED | sort > "$W/fx.want"
find "$FIX" -maxdepth 1 -name 'values-*.yaml' | sed -e 's#.*/values-##' -e 's#\.yaml$##' | sort > "$W/fx.have"
cmp -s "$W/fx.want" "$W/fx.have" || broken "values fixtures and the names in this test differ: $(diff "$W/fx.want" "$W/fx.have" | tr '\n' '|')"

# --- input shape ------------------------------------------------------------------------------
printf '{"id": "x"}' > "$W/obj.json"
run_gen "$W/obj.json" "$W/shape-out"
if [ "$RC" -eq 1 ] && [[ $ERR == *"must be a JSON array"* ]] && [ ! -e "$W/shape-out" ]; then ok; else bad "a JSON object input: exit $RC, $ERR"; fi
printf '[' > "$W/trunc.json"
run_gen "$W/trunc.json" "$W/shape-out"
if [ "$RC" -eq 2 ] && [ ! -e "$W/shape-out" ]; then ok; else bad "malformed JSON: exit $RC, $ERR"; fi
run_gen "$W/does-not-exist.json" "$W/shape-out"
if [ "$RC" -eq 2 ] && [ ! -e "$W/shape-out" ]; then ok; else bad "missing input file: exit $RC, $ERR"; fi
RC=0
python3 -I "$GEN" > /dev/null 2>&1 || RC=$?
if [ "$RC" -eq 2 ]; then ok; else bad "no arguments: exit $RC, want 2"; fi
python3 -I "$GEN" --help > "$W/help.txt" 2>&1 || bad "--help exited non-zero"
if grep -q 'OPERATOR RULES' "$W/help.txt" && grep -q 'BIND-MOUNT WINDOW' "$W/help.txt"; then ok; else bad "--help does not state the operator rules and the bind-mount window"; fi

# --- a lone surrogate is invalid input: no traceback, nothing written ----------------------------
printf '[{"id":"s","version":"1","page":"P","label":"\\ud800","icon":"package","navGroup":"modules","upstream":"svc:80"}]' > "$W/sur.json"
run_gen "$W/sur.json" "$W/sur-out"
if [ "$RC" -eq 1 ] && [[ $ERR != *Traceback* ]] && [[ $ERR == *"web-modules-gen:"* ]] && [ ! -e "$W/sur-out" ]; then ok; else bad "a lone surrogate label: exit $RC, output dir present=$([ -e "$W/sur-out" ] && echo yes || echo no), $ERR"; fi

# --- nothing is written on invalid input, into an existing directory too ---------------------
mkdir "$W/keep"
printf 'old locations\n' > "$W/keep/locations.conf"
printf '[]\n' > "$W/keep/modules.json"
before=$(cd "$W/keep" && find . -type f -print0 | sort -z | xargs -0 cksum | cksum)
jq -n '[{id: "good", version: "1", page: "P", label: "L", icon: "package", navGroup: "modules", upstream: "svc:80"},
        {id: "Bad", version: "1", page: "P", label: "L", icon: "package", navGroup: "modules", upstream: "svc:80"}]' > "$W/second-bad.json"
run_gen "$W/second-bad.json" "$W/keep"
after=$(cd "$W/keep" && find . -type f -print0 | sort -z | xargs -0 cksum | cksum)
if [ "$RC" -eq 1 ] && [ "$before" = "$after" ] && [ "$(find "$W/keep" -type f | wc -l)" -eq 2 ]; then
  ok
else
  bad "an invalid second module changed the existing directory (exit $RC, $ERR)"
fi

# --- first generation into a directory that does not exist, modes under a hostile umask -------
mod_json example example-module:8080 > "$W/one.json"
(
  umask 077
  run_gen "$W/one.json" "$W/deep/a/b"
  [ "$RC" -eq 0 ] || { echo "FAIL: first generation into a nested missing directory: exit $RC, $ERR" >&2; exit 1; }
) || fail=$((fail + 1))
if [ -f "$W/deep/a/b/locations.conf" ] && [ -f "$W/deep/a/b/modules.json" ]; then ok; else bad "first generation into a missing directory wrote no files"; fi
[ "$(mode_of "$W/deep/a/b")" = 755 ] && ok || bad "output directory mode is $(mode_of "$W/deep/a/b"), want 755"
for f in locations.conf modules.json; do
  [ "$(mode_of "$W/deep/a/b/$f")" = 644 ] && ok || bad "$f mode is $(mode_of "$W/deep/a/b/$f"), want 644"
done
[ "$(find "$W/deep/a/b" -mindepth 1 | wc -l)" -eq 2 ] && ok || bad "temporary files were left in the output directory: $(ls -A "$W/deep/a/b" | tr '\n' ' ')"
# An existing 0700 directory is made readable for the web container's uid.
mkdir -m 700 "$W/closed"
run_gen "$W/one.json" "$W/closed"
[ "$RC" -eq 0 ] && [ "$(mode_of "$W/closed")" = 755 ] && ok || bad "an existing 0700 directory was not made 0755 (exit $RC)"
# Regenerating over existing files replaces them.
mod_json other other-module:9090 > "$W/two.json"
run_gen "$W/two.json" "$W/closed"
if [ "$RC" -eq 0 ] && grep -q 'other-module:9090' "$W/closed/locations.conf" && ! grep -q 'example-module' "$W/closed/locations.conf" \
  && [ "$(jq -r '.[0].id' "$W/closed/modules.json")" = other ]; then ok; else bad "regeneration did not replace both files (exit $RC)"; fi

# --- rename order, observed through os.replace --------------------------------------------------
cat > "$W/order.py" <<'PYEOF'
import importlib.util
import json
import os
import stat
import sys

sys.dont_write_bytecode = True
gen_path, input_path, outdir, report = sys.argv[1:5]
spec = importlib.util.spec_from_file_location("web_modules_gen", gen_path)
gen = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gen)

calls = []
real_replace = os.replace


def recording_replace(src, dst, *a, **kw):
    calls.append(
        {
            "src_dir": os.path.dirname(src),
            "src": os.path.basename(src),
            "dst": os.path.basename(dst),
            "dst_dir": os.path.dirname(dst),
            "mode": stat.S_IMODE(os.stat(src).st_mode),
            "locations_present": os.path.exists(os.path.join(outdir, "locations.conf")),
            "json_present": os.path.exists(os.path.join(outdir, "modules.json")),
        }
    )
    return real_replace(src, dst, *a, **kw)


os.replace = recording_replace
rc = gen.main(["web-modules-gen.py", input_path, outdir])
with open(report, "w") as f:
    json.dump({"rc": rc, "calls": calls}, f)
PYEOF
mkdir "$W/order-out"
python3 -I "$W/order.py" "$GEN" "$W/one.json" "$W/order-out" "$W/order.json" >/dev/null 2> "$W/order.err" || broken "order harness failed: $(cat "$W/order.err")"
order_ok=$(jq -r --arg d "$W/order-out" '
  .rc == 0 and (.calls | length) == 2
  and .calls[0].dst == "locations.conf" and .calls[1].dst == "modules.json"
  and (.calls | all(.src_dir == $d and .dst_dir == $d and .mode == 420 and (.src | endswith(".tmp")) and (.src != .dst)))
  and (.calls[0].json_present | not)
  and .calls[1].locations_present' "$W/order.json" || true)
if [ "$order_ok" = true ]; then ok; else bad "rename order / temp-file contract: $(cat "$W/order.json")"; fi

# --- the projection -----------------------------------------------------------------------------
jq -n '[
  {id: "alpha", version: "1.2.3", page: "Alpha page", label: "Alpha", icon: "factory", navGroup: "factory", adminOnly: true, upstream: "http://alpha.tools:9000", secret: "do-not-leak", extra: {a: 1}},
  {id: "beta-two", version: "0.0.1", page: "Beta", label: "Beta", icon: "terminal", navGroup: "work", upstream: "10.0.0.1:8080"}
]' > "$W/proj.json"
run_gen "$W/proj.json" "$W/proj-out"
[ "$RC" -eq 0 ] || broken "projection input was rejected: $ERR"
want_json='[{"id":"alpha","version":"1.2.3","page":"Alpha page","label":"Alpha","icon":"factory","navGroup":"factory","adminOnly":true},{"id":"beta-two","version":"0.0.1","page":"Beta","label":"Beta","icon":"terminal","navGroup":"work","adminOnly":false}]'
[ "$(jq -c . "$W/proj-out/modules.json")" = "$want_json" ] && ok || bad "projection is $(jq -c . "$W/proj-out/modules.json")"
# Key order is part of the contract (a stable, reviewable file).
[ "$(jq -c '.[0] | keys_unsorted' "$W/proj-out/modules.json")" = '["id","version","page","label","icon","navGroup","adminOnly"]' ] && ok || bad "projection key order changed"
if grep -q 'upstream\|do-not-leak\|alpha.tools\|10.0.0.1' "$W/proj-out/modules.json"; then bad "modules.json leaks the upstream or an unknown field"; else ok; fi
[ "$(jq '.[1].adminOnly | type' "$W/proj-out/modules.json")" = '"boolean"' ] && ok || bad "adminOnly is not a boolean when omitted"

# --- the emitted blocks equal the shared template ----------------------------------------------
# golden <locations.conf> <id> <var-suffix> <upstream>
golden() {
  sed -n '/^location /,$p' "$FIX/location.example.conf" \
    | sed -e "s#/modules/example/#/modules/$2/#" -e "s#uzi_module_example#uzi_module_$3#g" \
        -e "s#@UZI_RESOLVER@#127.0.0.11#" -e "s#@UZI_UPSTREAM@#$4#" > "$W/golden.want"
  if grep -q '@UZI_' "$W/golden.want" || { [ "$2" != example ] && grep -q 'example' "$W/golden.want"; }; then broken "the substituted location template still has a placeholder or the example id"; fi
  awk -v want="location ^~ /modules/$2/ {" '$0 == want { on = 1 } on { print } on && $0 == "}" { exit }' "$1" > "$W/golden.got"
  if [ -s "$W/golden.got" ] && cmp -s "$W/golden.got" "$W/golden.want"; then
    ok
  else
    bad "the block for $2 differs from location.example.conf: $(diff "$W/golden.want" "$W/golden.got" | head -n 6 | tr '\n' '|')"
  fi
}
jq -n '[
  {id: "example", version: "1", page: "P", label: "E", icon: "package", navGroup: "modules", upstream: "modup:8080"},
  {id: "ghost-dns", version: "1", page: "P", label: "G", icon: "package", navGroup: "modules", upstream: "http://ghost.invalid:8080"},
  {id: "cc", version: "1", page: "P", label: "C", icon: "package", navGroup: "modules", upstream: "10.1.2.3:9"}
]' > "$W/gold.json"
run_gen "$W/gold.json" "$W/gold-out"
[ "$RC" -eq 0 ] || broken "golden input was rejected: $ERR"
golden "$W/gold-out/locations.conf" example example modup:8080
golden "$W/gold-out/locations.conf" ghost-dns ghost_dns ghost.invalid:8080
golden "$W/gold-out/locations.conf" cc cc 10.1.2.3:9
[ "$(grep -c '^location ' "$W/gold-out/locations.conf")" -eq 3 ] && ok || bad "locations.conf does not hold exactly three location blocks"
# The file the image ships for no modules is a comment; the generator's empty output is too.
printf '[]' > "$W/empty.json"
run_gen "$W/empty.json" "$W/empty-out"
if [ "$RC" -eq 0 ] && [ "$(wc -l < "$W/empty-out/locations.conf")" -eq 1 ] && grep -q '^#' "$W/empty-out/locations.conf" \
  && ! grep -q '^location ' "$W/empty-out/locations.conf" && [ "$(jq -c . "$W/empty-out/modules.json")" = '[]' ]; then ok; else bad "the empty list did not produce a comment-only locations.conf and []"; fi

echo "web-modules-gen: $pass passed, $fail failed ($n_accept accept + $n_reject reject upstream cases, $n_valid valid + $n_invalid invalid fixtures, $n_skipped chart-only skipped)"
[ "$fail" -eq 0 ] || exit 1
