#!/bin/sh
# Syntax-check the Homebrew formula (PRD #103 M5, PRD #103 Open Question 6).
#
# usage: scripts/lint-formula.sh <formula-path>
#        e.g. scripts/lint-formula.sh Formula/uzi-cli.rb
#
# A SCRIPT, NOT AN INLINE `cmds:` LINE -- see scripts/lint-shell.sh for the reason.
#
# 🔴 WHY THIS FILE IS WORTH A GATE AT ALL, since it is "just a formula": the
# tag-only `publish_brew` job is in `*publish_needs` and copies Formula/uzi-cli.rb
# VERBATIM into vtmocanu/homebrew-tap on every `v*` tag. A syntax error here is
# discovered by a teammate running `brew install`, after the release is out. The
# formula is also the SOURCE OF TRUTH for the tap copy, which is fully generated
# from it, so there is no second place the mistake gets caught.
#
# 🔴 THREE THINGS THIS CHECK CANNOT DO, stated here because each is a plausible
# reading of "the formula is linted" and none of them is true:
#
#   1. IT IS SYNTAX-ONLY. `ruby -c` parses; it does not evaluate. A broken Homebrew
#      DSL -- a misspelled `depends_on`, a `url` scheme brew cannot fetch, a `test do`
#      that could never pass -- is perfectly valid Ruby and passes here. `brew audit`
#      is the tool that sees those, and it needs a Homebrew installation, which no
#      image in this pipeline has.
#   2. IT VALIDATES THE PRE-SED SOURCE, NOT THE SHIPPED ARTIFACT. `publish_brew`
#      `sed -i`s the `tag:` line to the release tag before pushing to the tap, so
#      the file this script parses and the file consumers install from are not the
#      same bytes. The sed is a single quoted-string substitution and cannot plausibly
#      break the parse, but "the formula is syntax-checked in CI" is a claim about
#      this file only.
#   3. IT CANNOT DISTINGUISH A BAD FILE FROM A MISSING ONE. Measured 2026-08-03:
#      `ruby -c` on a nonexistent path also exits 1, with "No such file or directory
#      -- x (LoadError)". That is why the existence assertion below is a SEPARATE
#      step and not left to the tool.
#
# 🔴 AND THE VERSION GUARD IS NOT DEFENSIVE PADDING -- WITHOUT IT THIS TARGET IS RED
# FOR EVERY CONTRIBUTOR ON A STOCK MAC, AND THE TEMPTING FIX IS TO BREAK THE FORMULA.
# Measured 2026-08-03: /usr/bin/ruby on macOS 15 is 2.6.10 and `ruby -c` on this
# formula exits 1 with
#
#     Formula/uzi-cli.rb:49: syntax error, unexpected ')'
#     ...gs(output: bin/"uzi", ldflags:), "./cmd/uzi"
#
# The construct is the Ruby >= 3.1 SHORTHAND HASH VALUE (`ldflags:` meaning
# `ldflags: ldflags`). The formula is correct; the interpreter is nine years old.
# A gate that is red on a correct file for everyone is a gate people learn to
# ignore, and the first "fix" anyone reaches for is rewriting the formula to
# 2.6-compatible syntax, which would be a real regression in a file that ships to a
# public tap.
#
# 🔴 BUT AN `exit 2` THAT IS CORRECT ABOUT THE INSTRUMENT IS STILL A PERMANENT RED
# FOR A WHOLE POPULATION, AND THAT IS WHY THIS SCRIPT RESOLVES AN INTERPRETER AND
# THEN SKIPS RATHER THAN FAILING. The first version of this file did exactly what
# the frozen design said -- assert >= 3.1, exit 2 -- and measured the consequence:
# `task gate:repo` was rc=201 under /usr/bin/ruby and rc=0 only under a modern one.
# Stock macOS IS 2.6.10, so a contributor with no other ruby could not get a green
# `task gate` AT ALL, and PRD #103 Decision 2 is explicit that a gate people cannot
# run is a gate that stops being run. Ruled 2026-08-03 (#6):
#
#     1. `ruby` on PATH, if it is >= 3.1;
#     2. else Homebrew's vendored portable-ruby, if present -- this repo SHIPS a
#        Homebrew formula, so anyone editing Formula/uzi-cli.rb almost certainly has
#        brew, and brew always vendors a modern ruby for its own use;
#     3. else print a loud SKIP naming what to install, and exit 0.
#
# 🔴 STEP 3 IS FAIL-OPEN, SO CI MUST NOT BE ALLOWED TO TAKE IT. Setting
# UZI_LINT_FORMULA_REQUIRED=1 turns the skip into `exit 2`, and `lint:repo` sets it.
# CI runs Debian trixie's ruby 3.3, so a skip THERE means the image changed under
# us -- exactly the "instrument broken" case.
#
# THIS IS NOT A NEW PATTERN. It is the shape the `*LiveDB` tests already use in this
# repo: self-skip without UZI_TEST_DATABASE_URL locally, required and asserted in
# CI. Including the part CLAUDE.md is emphatic about, which is the whole reason the
# skip branch below prints a banner instead of nothing: A SKIPPED RUN AND A PASSING
# RUN MUST NOT LOOK ALIKE IN THE OUTPUT. `PASS=0 SKIP=n` with rc=0 is how this repo
# has already shipped a green that ran no assertions.
#
# EXIT CODES (the convention fmt-check:api / lint:api / deadcode-gate.sh set):
#     2 = the instrument is broken   1 = the formula does not parse   0 = clean
# with the one documented widening above: 0 ALSO means "skipped, loudly, locally".
set -eu

# The minimum is a property of the FORMULA's syntax, not a policy: bump it only
# when the formula starts using a construct that needs a newer parser.
MIN_RUBY=3.1

FORMULA="${1:-}"

if [ -z "$FORMULA" ]; then
  echo "usage: scripts/lint-formula.sh <formula-path>" >&2
  echo "  e.g. scripts/lint-formula.sh Formula/uzi-cli.rb" >&2
  exit 2
fi

# Run from the repo root so a relative formula path means the same thing from any
# caller's directory.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "lint-formula: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || exit 2

# SEPARATE FROM THE PARSE, for reason 3 in the header: `ruby -c` gives 1 for both a
# missing file and a broken one, so without this a deleted formula would be reported
# as a syntax error and send the reader to a file that is not there.
if [ ! -f "$FORMULA" ]; then
  echo "lint-formula: no such formula: $FORMULA" >&2
  echo "  A missing formula must not read as a syntax error -- ruby -c exits 1 for" >&2
  echo "  both. publish_brew copies this file into the tap on every v* tag, so its" >&2
  echo "  absence is a release-integrity problem, not a lint result." >&2
  exit 2
fi

# The comparison is done BY RUBY, not by parsing RUBY_VERSION in shell: Gem::Version
# is present in every ruby this could run under (verified on 2.6.10, 3.3.8 and
# 4.0.6) and compares SEGMENT-WISE, so it gets 2.6.10 < 3.1 right where a
# lexicographic or float-ish shell comparison does not. Used in a CONDITION context
# everywhere below, so errexit never pre-empts the answer.
ruby_at_least() {
  [ -n "${1:-}" ] || return 1
  command -v "$1" >/dev/null 2>&1 || return 1
  "$1" -e 'exit(Gem::Version.new(RUBY_VERSION) >= Gem::Version.new(ARGV[0]) ? 0 : 1)' \
    "$MIN_RUBY" 2>/dev/null
}

RUBY=""
RUBY_SOURCE=""

# 1. Whatever `ruby` means on this PATH. In CI that is Debian's 3.3; on a stock mac
#    it is 2.6.10 and fails the test.
if ruby_at_least ruby; then
  RUBY=ruby
  RUBY_SOURCE="PATH"
else
  # 2. Homebrew's vendored portable-ruby. brew keeps a modern ruby for its own use
  #    and exposes it at a stable `current` symlink; this repo ships a Homebrew
  #    formula, so a contributor editing that formula almost certainly has brew.
  #    Not on PATH by design, which is why it has to be looked for by path.
  if command -v brew >/dev/null 2>&1; then
    BREW_PREFIX="$(brew --prefix 2>/dev/null || true)"
    if [ -n "$BREW_PREFIX" ]; then
      CANDIDATE="$BREW_PREFIX/Library/Homebrew/vendor/portable-ruby/current/bin/ruby"
      if ruby_at_least "$CANDIDATE"; then
        RUBY="$CANDIDATE"
        RUBY_SOURCE="Homebrew's vendored portable-ruby"
      fi
    fi
  fi
fi

if [ -z "$RUBY" ]; then
  FOUND="none on PATH"
  if command -v ruby >/dev/null 2>&1; then
    FOUND="ruby $(ruby -e 'print RUBY_VERSION' 2>/dev/null || echo '<unparseable>') on PATH"
  fi

  # 🔴 REQUIRED MODE: CI MUST NOT TAKE THE SKIP. `lint:repo` sets this, and its
  # image ships ruby 3.3 -- so reaching here in CI means the image changed under us,
  # which is the "instrument broken" case rather than a contributor's laptop.
  if [ "${UZI_LINT_FORMULA_REQUIRED:-}" = "1" ]; then
    echo "lint-formula: no Ruby >= $MIN_RUBY resolved, and UZI_LINT_FORMULA_REQUIRED=1." >&2
    echo "  Found: $FOUND. This is an INSTRUMENT failure (exit 2), not a finding." >&2
    echo "  In CI this means the job image no longer ships the ruby it is supposed to." >&2
    echo "  The skip this replaces is fail-open and exists for contributor laptops;" >&2
    echo "  CI must never be allowed to take it, which is what this variable enforces." >&2
    exit 2
  fi

  # A SKIPPED RUN AND A PASSING RUN MUST NOT LOOK ALIKE. Hence the banner: the
  # passing line below reads "parses (ruby X)" and says nothing about skipping.
  echo "lint-formula: ================================================================"
  echo "lint-formula: SKIPPED -- $FORMULA WAS NOT CHECKED."
  echo "lint-formula: No Ruby >= $MIN_RUBY is available ($FOUND, and no Homebrew"
  echo "lint-formula: portable-ruby either). macOS ships 2.6.10, which rejects this"
  echo "lint-formula: formula's Ruby >= 3.1 shorthand hash value (\`ldflags:\`) with a"
  echo "lint-formula: syntax error ON A FILE THAT IS CORRECT -- so running it would"
  echo "lint-formula: report a defect that is not there, and the tempting fix would be"
  echo "lint-formula: to downgrade a formula that ships to a public tap."
  echo "lint-formula:"
  echo "lint-formula: This is FAIL-OPEN and deliberate: it is the same shape the"
  echo "lint-formula: *LiveDB tests use (self-skip locally, required in CI). CI runs"
  echo "lint-formula: this with UZI_LINT_FORMULA_REQUIRED=1, so the formula IS checked"
  echo "lint-formula: on every MR regardless of what you have locally."
  echo "lint-formula:"
  echo "lint-formula: To check it here: \`brew install ruby\`, or any rbenv/asdf/mise"
  echo "lint-formula: ruby >= $MIN_RUBY on PATH."
  echo "lint-formula: ================================================================"
  exit 0
fi

# Read the version BEFORE the parse, so the failure message below can name the
# interpreter. Under `set -u` a variable assigned only on the success path would be
# unbound in exactly the branch that needs it.
RUBY_V_OK="$("$RUBY" -e 'print RUBY_VERSION')" || exit 2

rc=0
"$RUBY" -c "$FORMULA" || rc=$?

if [ "$rc" -ne 0 ]; then
  echo "lint-formula: $FORMULA does not parse (ruby -c exited $rc)." >&2
  echo "  The interpreter is ruby $RUBY_V_OK (>= $MIN_RUBY, from $RUBY_SOURCE), so" >&2
  echo "  this is the FILE and not the parser." >&2
  exit 1
fi

echo "lint-formula: $FORMULA parses (ruby $RUBY_V_OK from $RUBY_SOURCE, syntax only -- see this script's header)"
