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
# Homebrew itself runs on its vendored portable ruby (4.0.6 here), which parses it
# fine. So the guard reports 2 -- the instrument is broken -- rather than 1, and
# says where to get a newer ruby. A gate that is red on a correct file for everyone
# is a gate people learn to ignore, and the first "fix" anyone reaches for is
# rewriting the formula to 2.6-compatible syntax, which would be a real regression
# in a file that ships to a public tap.
#
# EXIT CODES (the convention fmt-check:api / lint:api / deadcode-gate.sh set):
#     2 = the instrument is broken   1 = the formula does not parse   0 = clean
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

if ! command -v ruby >/dev/null 2>&1; then
  echo "lint-formula: no ruby on PATH." >&2
  echo "  Install one >= $MIN_RUBY. On macOS the system ruby is too old anyway (see below)." >&2
  exit 2
fi

# The comparison is done BY RUBY, not by parsing RUBY_VERSION in shell: Gem::Version
# is present in every ruby this could run under (verified on 2.6.10 and 4.0.6) and
# compares segment-wise, so it gets 2.6.10 < 3.1 right where a lexicographic or
# float-ish shell comparison does not.
if ! ruby -e 'exit(Gem::Version.new(RUBY_VERSION) >= Gem::Version.new(ARGV[0]) ? 0 : 1)' "$MIN_RUBY"; then
  RUBY_V="$(ruby -e 'print RUBY_VERSION' 2>/dev/null || echo '<unknown>')"
  echo "lint-formula: ruby $RUBY_V is older than $MIN_RUBY; refusing to run the check." >&2
  echo "  This is an INSTRUMENT failure (exit 2), NOT a finding. $FORMULA uses the" >&2
  echo "  Ruby >= 3.1 shorthand hash value (\`ldflags:\`), which a 2.x parser rejects" >&2
  echo "  with a syntax error on a file that is correct -- and the tempting fix for" >&2
  echo "  that red is to downgrade the formula, which would be a real regression in" >&2
  echo "  a file that ships to a public tap." >&2
  echo "  macOS /usr/bin/ruby is 2.6.10. Homebrew's own vendored ruby parses it fine:" >&2
  echo "    PATH=\"\$(brew --prefix)/Library/Homebrew/vendor/portable-ruby/current/bin:\$PATH\" \\" >&2
  echo "      task lint:formula" >&2
  exit 2
fi

rc=0
ruby -c "$FORMULA" || rc=$?

if [ "$rc" -ne 0 ]; then
  echo "lint-formula: $FORMULA does not parse (ruby -c exited $rc)." >&2
  echo "  Both guards above passed, so this is the file and not the interpreter." >&2
  exit 1
fi

RUBY_V="$(ruby -e 'print RUBY_VERSION')"
echo "lint-formula: $FORMULA parses (ruby $RUBY_V, syntax only -- see this script's header)"
