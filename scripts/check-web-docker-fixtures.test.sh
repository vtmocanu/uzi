#!/usr/bin/env bash
# Hermetic exit-code contract for scripts/check-web-docker-fixtures.sh: throwaway git repos,
# no network. Each case writes web sources and the two Dockerfiles, then asserts the check's
# status. Prints a `cases=N passed=N` tally and fails below its case floor, so a gutted run
# cannot read green.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FLOOR=12
cases=0
passed=0

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

# repo <name> <copied fixture names...>: a git repo with the check and both Dockerfiles
# copying exactly the named fixtures; prints its path.
repo() {
  local d="$TMP/$1"
  shift
  mkdir -p "$d/scripts" "$d/web/src/lib"
  cp "$ROOT/scripts/check-web-docker-fixtures.sh" "$d/scripts/"
  local df name
  for df in Dockerfile Dockerfile.mock; do
    printf 'FROM scratch\nCOPY web/ /app/web/\n' > "$d/web/$df"
    for name in "$@"; do
      printf 'COPY fixtures/%s/ /app/fixtures/%s/\n' "$name" "$name" >> "$d/web/$df"
    done
    printf 'RUN npm run build\n' >> "$d/web/$df"
  done
  git -C "$d" init -q
  printf '%s' "$d"
}

# src <repo> <file> <content>: write and stage a web source file.
src() {
  printf '%s\n' "$3" > "$1/web/src/$2"
  git -C "$1" add -A
}

# expect <want-status> <label> <repo>: run the check in the repo and compare its status.
expect() {
  local want=$1 label=$2 dir=$3 got=0
  cases=$((cases + 1))
  (cd "$dir" && ./scripts/check-web-docker-fixtures.sh) > "$TMP/out" 2>&1 || got=$?
  if [ "$got" -eq "$want" ]; then
    passed=$((passed + 1))
  else
    echo "FAIL: $label: want exit $want, got $got" >&2
    sed 's/^/    /' "$TMP/out" >&2
  fi
}

d=$(repo single-line)
src "$d" lib/a.test.ts 'import parity from "../../../fixtures/run-progress/parity.json";'
expect 1 "the #2602 omission: uncopied single-line static import" "$d"

d=$(repo single-line-copied run-progress)
src "$d" lib/a.test.ts 'import parity from "../../../fixtures/run-progress/parity.json";'
expect 0 "copied single-line static import" "$d"

d=$(repo multiline-static)
src "$d" lib/a.test.ts $'import probe from\n  "../../../fixtures/probe/case.json";'
expect 1 "uncopied static import with the specifier on the next line" "$d"

d=$(repo multiline-static-copied probe)
src "$d" lib/a.test.ts $'import probe from\n  "../../../fixtures/probe/case.json";'
expect 0 "copied static import with the specifier on the next line" "$d"

d=$(repo multiline-dynamic)
src "$d" lib/a.test.ts $'const m = await import(\n  \x27../../../fixtures/dyn/case.json\x27\n);'
expect 1 "uncopied dynamic import split across lines" "$d"

d=$(repo multiline-dynamic-copied dyn)
src "$d" lib/a.test.ts $'const m = await import(\n  \x27../../../fixtures/dyn/case.json\x27\n);'
expect 0 "copied dynamic import split across lines" "$d"

d=$(repo comments)
src "$d" lib/a.test.ts $'import x from /* block */\n  // line\n  "../../../fixtures/commented/case.json";'
expect 1 "uncopied import with comments between keyword and specifier" "$d"

d=$(repo raw run-progress)
src "$d" lib/a.test.ts 'import parity from "../../../fixtures/run-progress/parity.json";'
src "$d" b.browser.test.tsx 'import flow from "../../fixtures/pr-diagram/flow.md?raw";'
expect 0 "a Vite ?raw import needs no COPY" "$d"

d=$(repo runtime-read run-progress)
src "$d" lib/a.test.ts 'import parity from "../../../fixtures/run-progress/parity.json";'
src "$d" lib/b.test.ts 'const url = new URL("../../../fixtures/termsafe/corpus.json", import.meta.url);'
expect 0 "a runtime read is not a module import" "$d"

d=$(repo one-dockerfile-missing run-progress)
drop_line() { grep -v -F "$2" "$1" > "$1.new"; mv "$1.new" "$1"; }
drop_line "$d/web/Dockerfile.mock" 'fixtures/run-progress'
src "$d" lib/a.test.ts 'import parity from "../../../fixtures/run-progress/parity.json";'
expect 1 "copied in web/Dockerfile but not web/Dockerfile.mock" "$d"

d=$(repo wrong-destination)
for df in Dockerfile Dockerfile.mock; do
  printf 'FROM scratch\nCOPY fixtures/run-progress/ /tmp/run-progress/\nRUN npm run build\n' > "$d/web/$df"
done
src "$d" lib/a.test.ts 'import parity from "../../../fixtures/run-progress/parity.json";'
expect 1 "a COPY to a destination other than /app/fixtures/<name>" "$d"

d=$(repo after-build)
for df in Dockerfile Dockerfile.mock; do
  printf 'FROM scratch\nRUN npm run build\nCOPY fixtures/run-progress/ /app/fixtures/run-progress/\n' > "$d/web/$df"
done
src "$d" lib/a.test.ts 'import parity from "../../../fixtures/run-progress/parity.json";'
expect 1 "a COPY placed after RUN npm run build" "$d"

d=$(repo no-imports)
src "$d" lib/a.ts 'export const x = 1;'
expect 2 "no fixture import at all is an instrument failure" "$d"

echo "check-web-docker-fixtures.test: cases=$cases passed=$passed"
if [ "$passed" -ne "$cases" ] || [ "$cases" -lt "$FLOOR" ]; then
  echo "check-web-docker-fixtures.test: FAILED (floor $FLOOR)" >&2
  exit 1
fi
