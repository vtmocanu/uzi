#!/bin/sh
# Is one npm package's `node_modules` stale against what this branch declares?
# (PRD #103 M5 MR-C.)
#
# 🔴 A NEW devDependency FAILS LOUDLY; A VERSION BUMP FAILS SILENTLY, AND MR-C SHIPS
# SIX BUMPS. Carry-forward 12 covers the new-dependency case: `npm run` puts
# `node_modules/.bin` on PATH, so a missing binary dies with `command not found`. A
# BUMP cannot fire that -- the binary is already there -- so a checkout that skipped
# `npm ci` runs vitest 2 while the lockfile says 4, green, with nothing naming it.
#
# TWO LINES, NOT ONE, AND THE SECOND EXISTS BECAUSE THE FIRST COVERS 1 OF 6.
#
#   1. `npm ls --depth=0`   installed vs the DECLARED RANGE in package.json
#   2. a lockfile join      installed vs the RESOLVED VERSION in package-lock.json
#
# Line 1 alone was the first shipped version of this gate, and measured on the stale
# state a real `git pull` produces (`npm ci` from the pre-bump manifest, then restore
# both files, `node_modules` untouched) it walks straight past five of MR-C's six
# dependency changes:
#
#   web    declared vitest 4.1.10, installed 2.1.9   -> rc=1, names both  <- caught
#   agent  five TRANSITIVE bumps, package.json UNTOUCHED
#          npm ls --depth=0                          -> rc=0, zero findings <- missed
#
# `--depth=0` sees DIRECT dependencies, and a transitive bump has no declared range
# to compare against, so there is nothing for it to find. The same blind spot hits
# `web` on the SECURITY-RELEVANT package: a stale `postcss` reads 8.5.16 against a
# declared `^8.4.49`, which is satisfied, while the lockfile says 8.5.25 -- and that
# is the high-severity advisory fix. It is masked only because vitest reddens in the
# same run; drop the vitest bump and the security fix is silently uncovered.
#
# 🔴 THE JOIN IS ON THE INTERSECTION, AND THAT IS LOAD-BEARING RATHER THAN TIDY. A
# raw diff of the two lockfiles yields ~370 spurious rows: the COMMITTED lockfile
# lists every platform's optional binaries (`@esbuild/win32-x64`, …) while the
# INSTALLED tree holds only this platform's. Joining on keys present in both is what
# makes the result zero-false-positive. Measured: agent gives exactly 5 rows, the
# five bumps; web gives 15, including postcss.
#
# LINE 1 IS KEPT RATHER THAN REPLACED. It names the declared RANGE
# (`vitest@2.1.9 invalid: "4.1.10" from the root project`), which is the more legible
# message when it fires, and it catches a package.json edit that never reached the
# lockfile at all -- a state line 2 cannot see, because the lockfile would still agree
# with node_modules.
#
# 🔴 THE JOIN USES `node`, NOT `jq`, AND THE REASON IS THE ABSENCE BRANCH RATHER THAN
# PREFERENCE. `jq` is a brew tool; by this repo's own rule (Taskfile.yml's repo-wide
# block) a brew tool a contributor may not have gets a LOUD SKIP, and a skip is
# exit 0 -- a fail-open branch inside the one check whose entire subject is "your
# verdict is about the wrong tree". The alternative, a `UZI_*_REQUIRED` variable
# nothing sets, is the vacuous-directive class `scan:secrets` names by name. `node`
# removes the question: you cannot have a `node_modules` to be stale without it. The
# jq form below is the one the finding was measured with and is kept as the
# equivalence control -- both were run against both packages and agree row for row:
#
#   jq -r '.packages|to_entries[]|select(.key!="")|"\(.key)\t\(.value.version)"' \
#     package-lock.json | sort > lk.tsv
#   jq -r '…' node_modules/.package-lock.json | sort > nm.tsv
#   join -t$'\t' lk.tsv nm.tsv | awk -F'\t' '$2!=$3'
#
# OFFLINE, MEASURED. Neither line touches the network: with
# `npm_config_registry=http://127.0.0.1:1` line 1's output is byte-identical to the
# clean run at rc=0, and line 2 reads two files. That is what lets this check sit
# inside `task gate` while `vulncheck:*` deliberately does not.
#
# NOT A CI CHECK, and that does not violate Success Criterion 1, which is
# one-directional. CI is structurally unexposed: every npm job's `before_script` is
# `npm ci`, which reinstalls from the lockfile.
set -eu

PKG_DIR="${1:-}"

if [ -z "$PKG_DIR" ]; then
  echo "usage: scripts/deps-check-gate.sh <package-dir>" >&2
  echo "  e.g. scripts/deps-check-gate.sh web" >&2
  exit 2
fi

if [ ! -f "$PKG_DIR/package.json" ]; then
  echo "deps-check-gate: no package.json in: $PKG_DIR" >&2
  exit 2
fi

# A MISSING node_modules IS AN INSTRUMENT FAILURE, NOT A CLEAN TREE. Without this,
# the join below reads no installed lockfile, produces zero rows, and reports clean
# over a package with nothing installed at all -- the empty-result-set-looks-clean
# shape this repo has now met at gofmt, deadcode and govulncheck. (`npm ls` also
# fails closed here, but that is a property of npm rather than of this recipe.)
if [ ! -d "$PKG_DIR/node_modules" ]; then
  echo "deps-check-gate: $PKG_DIR/node_modules does not exist." >&2
  echo "  INSTRUMENT FAILURE, NOT A CLEAN TREE. Run 'cd $PKG_DIR && npm ci" >&2
  echo "  --ignore-scripts' first. In agent/ the --ignore-scripts is required:" >&2
  echo "  agent-browser's postinstall rewrites /opt/homebrew/bin/agent-browser" >&2
  echo "  host-wide for every other session on this machine." >&2
  exit 2
fi

if [ ! -f "$PKG_DIR/node_modules/.package-lock.json" ]; then
  echo "deps-check-gate: $PKG_DIR/node_modules/.package-lock.json is missing." >&2
  echo "  INSTRUMENT FAILURE. npm writes that file on every install; without it the" >&2
  echo "  lockfile join has nothing to compare and would report clean." >&2
  exit 2
fi

# No `-t` on mktemp: it is not portable, and only CI could show it (f0e3c438).
TMP="$(mktemp -d)"
# shellcheck disable=SC2064  # expand TMP now: the trap must survive its unset.
trap "rm -rf '$TMP'" EXIT INT TERM

status=0

# ---- line 1: installed vs the DECLARED RANGE ------------------------------------
# Delegates to package.json's `deps-check` script, as Taskfile.yml's header requires
# of every npm target, so the flag lives where a rewrite cannot drop it silently.
#
# 🔴 RC FIRST, INTO A FILE. Do not pipe and read the pipeline's status, and do not
# feed a shell builtin's multi-line output into an early-exiting reader: measured in
# this repo, `printf '%s' "$BIG" | grep -q` returns 141 on SIGPIPE while the pattern
# is present, and it blocked a release.
rc1=0
(cd "$PKG_DIR" && npm run deps-check) >"$TMP/out" 2>&1 || rc1=$?
cat "$TMP/out"
if [ "$rc1" -ne 0 ]; then
  echo "deps-check-gate: $PKG_DIR -- node_modules does not satisfy package.json." >&2
  status=1
fi

# ---- line 2: installed vs the RESOLVED lockfile version -------------------------
skew="$(node -e '
const fs = require("fs");
const dir = process.argv[1];
const read = (p) => JSON.parse(fs.readFileSync(p, "utf8")).packages || {};
const lock = read(dir + "/package-lock.json");
const inst = read(dir + "/node_modules/.package-lock.json");
const rows = [];
for (const [k, v] of Object.entries(lock)) {
  if (k === "" || !v.version) continue;
  const i = inst[k];
  // INTERSECTION ONLY. A key absent from the installed tree is almost always
  // another platform s optional binary, not a stale package.
  if (!i || !i.version) continue;
  if (i.version !== v.version) rows.push(k + "  lockfile=" + v.version + "  installed=" + i.version);
}
process.stdout.write(rows.sort().join("\n"));
' "$PKG_DIR")" || {
  echo "deps-check-gate: could not read $PKG_DIR's lockfiles. INSTRUMENT FAILURE." >&2
  exit 2
}

if [ -n "$skew" ]; then
  echo "deps-check-gate: $PKG_DIR -- node_modules does not match package-lock.json:"
  echo "$skew" | sed -e 's/^/  /'
  echo "  These are RESOLVED versions, so transitive packages appear here and cannot"
  echo "  appear above: a transitive bump has no declared range for npm ls to check."
  echo "  Run 'cd $PKG_DIR && npm ci --ignore-scripts'."
  status=1
fi

if [ "$status" -eq 0 ]; then
  echo "deps-check-gate: $PKG_DIR up to date (package.json ranges satisfied, lockfile matched)"
fi

exit "$status"
