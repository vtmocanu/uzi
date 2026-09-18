#!/usr/bin/env bash
# Hermetic regression: GitHub GraphQL partial data with errors must fail closed.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=review-threads.sh
. "$HERE/review-threads.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
[ "${1:-}" = api ] && [ "${2:-}" = graphql ] || { echo "unexpected gh call: $*" >&2; exit 1; }
if [ "$MODE" = partial ]; then
  echo '{"errors":[{"message":"author resolver failed"}],"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"databaseId":1,"author":null,"body":"hidden finding","path":"x.go","line":4,"originalLine":4}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}'
else
  echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}'
fi
STUB
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH"

MODE="good"; export MODE
[ "$(fetch_review_threads test/repo 42)" = '[]' ] || fail "valid response was not returned"
MODE="partial"; export MODE
if fetch_review_threads test/repo 42 > "$WORK/partial.out"; then
  fail "GraphQL partial data with errors was accepted: $(cat "$WORK/partial.out")"
fi

echo "PASS review-threads: GraphQL errors fail closed"
