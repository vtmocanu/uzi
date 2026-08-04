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

# THE DELEGATED SCRIPT MUST BE AN `npm ls` INVOCATION, ASSERTED RATHER THAN
# ASSUMED. Line 1 delegates to `scripts.deps-check` so that script's flags cannot
# be dropped silently (Taskfile.yml's header), but the failure CLASSIFIER further
# down reads `npm ls --depth=0 --json`'s structured `problems` array -- which is
# only a valid discriminator if the thing that failed was npm ls. Asserting it here
# makes the assumption checkable instead of implicit, and matches
# scripts/npm-audit-gate.sh, which asserts its two flags against `scripts.audit`
# for the same reason.
DEPS_SCRIPT="$(node -e '
const fs = require("fs");
const pkg = JSON.parse(fs.readFileSync(process.argv[1] + "/package.json", "utf8"));
const s = (pkg.scripts || {})["deps-check"];
if (typeof s !== "string") { process.stderr.write("no scripts.deps-check"); process.exit(3); }
process.stdout.write(s);
' "$PKG_DIR")" || {
  echo "deps-check-gate: $PKG_DIR/package.json has no \`deps-check\` script." >&2
  echo "  INSTRUMENT FAILURE. This gate delegates to that script and classifies its" >&2
  echo "  failures with npm ls's own structured output; with no script there is" >&2
  echo "  nothing to delegate to." >&2
  exit 2
}
case "$DEPS_SCRIPT" in
  *"npm ls"*) ;;
  *)
    echo "deps-check-gate: $PKG_DIR/package.json's deps-check script does not run \`npm ls\`." >&2
    echo "  Its deps-check script is: $DEPS_SCRIPT" >&2
    echo "  REFUSED. This gate classifies a non-zero exit by reading" >&2
    echo "  \`npm ls --depth=0 --json\`'s problems array, which is only a valid" >&2
    echo "  discriminator for an npm ls failure." >&2
    exit 2
    ;;
esac

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
  # 🔴 THE FINDINGS BRANCH NEEDS A POSITIVE OBSERVATION, NOT MERELY A NON-ZERO RC.
  # `npm run` returns 1 for "npm ls found a dependency problem" AND for every way
  # npm can fail to run at all -- a package.json with no `deps-check` script
  # (`Missing script: "deps-check"`), npm absent from PATH, an npm crash. Mapping
  # all of those to status=1 announces "node_modules does not satisfy
  # package.json", which is a verdict about a tree this script never inspected.
  # Same shape as scripts/npm-audit-gate.sh's network branch, and the same fix:
  # require the output to carry npm ls's own finding vocabulary before calling it
  # a finding, and exit 2 otherwise. It fails CLOSED either way -- what this
  # protects is the 2/1/0 convention that makes a red READABLE, which
  # scripts/govulncheck-gate.sh's header exists to preserve.
  #
  # 🔴 THE DISCRIMINATOR IS A STRUCTURED READ, NOT A SUBSTRING OF FREE TEXT, AND
  # THE SUBSTRING VERSION OF THIS GUARD WAS WRONG IN BOTH DIRECTIONS.
  #
  # The first version of this branch grepped the delegated script's combined output
  # for npm ls's finding vocabulary (`invalid:`, `missing:`, `extraneous`,
  # `UNMET DEPENDENCY`, `ELSPROBLEMS`). Measured by the auditor: a deps-check script
  # of `echo "cannot run: node_modules missing: reinstall" >&2; exit 127` was
  # classified as a FINDING at exit 1 -- the exact inversion this branch exists to
  # fix, reached through text instead of through an exit code.
  #
  # Stripping npm's `> ` script-echo lines was the obvious repair and it is NOT
  # sufficient, which is worth recording because it looks sufficient: the token is
  # in what the script PRINTS, not only in npm's echo of its body. Re-measured after
  # that repair, the same fixture still returned 1. No refinement of a substring test
  # reaches a difference the substring cannot see.
  #
  # So: ask npm directly, in a form that carries structure. `npm ls --depth=0 --json`
  # emits a top-level `problems` array -- ABSENT on a clean tree, and holding the
  # finding text when there is one (verified both ways against agent/ on npm 11.17).
  # A script that merely prints the word "missing:" cannot populate it.
  #
  # THE ASSUMPTION IS ASSERTED RATHER THAN ASSUMED. This classifier only means
  # anything if the delegated script is an `npm ls` invocation, so that is checked
  # above; the delegation still owns WHAT RUNS (so the script's flags cannot be
  # dropped silently, per Taskfile.yml's header), and this owns only HOW THE FAILURE
  # IS CLASSIFIED.
  problems=0
  if (cd "$PKG_DIR" && npm ls --depth=0 --json) >"$TMP/ls.json" 2>/dev/null; then :; fi
  problems="$(node -e '
    const fs = require("fs");
    try {
      const j = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
      process.stdout.write(String((j.problems || []).length));
    } catch (_) { process.stdout.write("-1"); }
  ' "$TMP/ls.json")"
  if [ "$problems" = "-1" ]; then
    echo "deps-check-gate: could not parse 'npm ls --depth=0 --json' in $PKG_DIR." >&2
    echo "  INSTRUMENT FAILURE. Without it this script cannot tell a dependency" >&2
    echo "  finding from npm failing to run at all." >&2
    exit 2
  fi
  if [ "$problems" -gt 0 ]; then
    echo "deps-check-gate: $PKG_DIR -- node_modules does not satisfy package.json." >&2
    status=1
  else
    echo "deps-check-gate: 'npm run deps-check' in $PKG_DIR exited $rc1 while" >&2
    echo "  'npm ls --depth=0 --json' reports ZERO problems. INSTRUMENT FAILURE," >&2
    echo "  NOT A FINDING. Its output is above. The usual causes are a package.json" >&2
    echo "  with no deps-check script, npm missing from PATH, or a deps-check script" >&2
    echo "  that fails for a reason unrelated to the dependency tree." >&2
    exit 2
  fi
fi

# ---- line 2: installed vs the RESOLVED lockfile version -------------------------
skew="$(node -e '
const fs = require("fs");
const dir = process.argv[1];
// 🔴 NO `|| {}` FALLBACK ON `packages`. An installed lockfile without that key
// (lockfileVersion 1) yielded ZERO ROWS and read as up-to-date -- the
// empty-result-set-looks-clean shape this file already guards against forty
// lines up, reintroduced by a defensive default. Latent under npm 11, which
// writes v3, and that is exactly the kind of latency that outlives the npm
// version somebody checked. Fail closed instead.
const read = (p, what) => {
  const j = JSON.parse(fs.readFileSync(p, "utf8"));
  if (!j.packages || typeof j.packages !== "object") {
    throw new Error(what + " has no `packages` map (lockfileVersion " + j.lockfileVersion + "?)");
  }
  return j.packages;
};
const lock = read(dir + "/package-lock.json", "package-lock.json");
const inst = read(dir + "/node_modules/.package-lock.json", "node_modules/.package-lock.json");
// A lockfile entry that is optional, or platform-gated by os/cpu/libc, is
// legitimately absent from THIS machine s tree. Everything else is not.
const platformGated = (v) => Boolean(v.optional || v.os || v.cpu || v.libc);
const skewRows = [], missingRows = [];
for (const [k, v] of Object.entries(lock)) {
  if (k === "" || !v.version) continue;
  const i = inst[k];
  if (!i || !i.version) {
    // ABSENCE, which the intersection-only join was blind to. This is the state a
    // `git pull` adding a TRANSITIVE dependency produces: the branch lockfile
    // declares it, node_modules has never seen it, `npm ls` has no declared range
    // to check it against, and require() fails at runtime while the gate says
    // up to date. Suppressed only for platform-gated entries, which is what the
    // intersection was really for -- a raw diff gives ~370 spurious rows.
    if (!platformGated(v)) missingRows.push(k + "  lockfile=" + v.version + "  installed=(absent)");
    continue;
  }
  if (i.version !== v.version) skewRows.push(k + "  lockfile=" + v.version + "  installed=" + i.version);
}
process.stdout.write(skewRows.sort().concat(missingRows.sort()).join("\n"));
' "$PKG_DIR")" || {
  echo "deps-check-gate: could not read $PKG_DIR's lockfiles. INSTRUMENT FAILURE." >&2
  exit 2
}

if [ -n "$skew" ]; then
  echo "deps-check-gate: $PKG_DIR -- node_modules does not match package-lock.json:"
  echo "$skew" | sed -e 's/^/  /'
  echo "  These are RESOLVED versions, so transitive packages appear here and cannot"
  echo "  appear above: a transitive bump has no declared range for npm ls to check."
  echo "  An '(absent)' row is a package the lockfile declares and the tree has never"
  echo "  installed; platform-gated entries (optional/os/cpu/libc) are excluded."
  echo "  Run 'cd $PKG_DIR && npm ci --ignore-scripts'."
  status=1
fi

if [ "$status" -eq 0 ]; then
  echo "deps-check-gate: $PKG_DIR up to date (package.json ranges satisfied, lockfile matched)"
fi

exit "$status"
