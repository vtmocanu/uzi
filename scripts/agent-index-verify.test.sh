#!/usr/bin/env bash
# Offline registry fixtures for agent-index-verify.sh: a tag that still resolves to its previous
# index (GHCR lag, a stable promoted from its RC's commit, rerun legs) is retried, not failed.
set -euo pipefail
scripts="$(cd "$(dirname "$0")" && pwd)"
script="${AIV_SCRIPT:-$scripts/agent-index-verify.sh}"  # an override lets a mutation probe run this suite
cases=0
# Wiring: publish-agent-index verifies through this script, not an inline loop.
workflow="$scripts/../.github/workflows/release.yml"
[ "$(grep -v '^[[:space:]]*#' "$workflow" | grep -cF 'scripts/agent-index-verify.sh')" -eq 1 ] \
  || { echo "FAIL: release.yml does not call agent-index-verify.sh exactly once" >&2; exit 1; }
cases=$((cases + 1)); echo 'PASS: release.yml publish-agent-index uses agent-index-verify.sh'

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export AIV_FIXTURE="$work"
export UZI_INDEX_DOCKER="$work/docker" UZI_INDEX_SLEEP="$work/sleep"
d() { printf 'sha256:%064d' "$1"; }
repo=ghcr.io/vtmocanu/uzi/agent-base
# Tag reads pop the first line of tag-<tag> until one line is left, which then repeats.
cat > "$work/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[ "$1 $2 $3" = 'buildx imagetools inspect' ]
case "$4" in
  *@sha256:*)
    [ "$5" = --raw ]
    f="$AIV_FIXTURE/index-${4##*@sha256:}.json"
    [ -f "$f" ] || { echo "no such manifest $4" >&2; exit 1; }
    cat "$f"
    # <index>.fail holds how many more raw reads print the manifest and then exit nonzero.
    if [ -f "$f.fail" ] && [ "$(cat "$f.fail")" -gt 0 ]; then
      echo "$(( $(cat "$f.fail") - 1 ))" > "$f.fail"; exit 17
    fi;;
  *)
    [ "$6" = '{{.Manifest.Digest}}' ]
    f="$AIV_FIXTURE/tag-${4##*:}"
    head -n 1 "$f"
    if [ "$(wc -l < "$f")" -gt 1 ]; then tail -n +2 "$f" > "$f.next"; mv "$f.next" "$f"; fi;;
esac
STUB
printf '#!/usr/bin/env bash\necho "$1" >> "$AIV_FIXTURE/sleeps"\n' > "$work/sleep"
chmod +x "$work/docker" "$work/sleep"
index() { # n amd64-n arm64-n
  printf '{"manifests":[{"platform":{"os":"linux","architecture":"amd64"},"digest":"%s"},{"platform":{"os":"linux","architecture":"arm64"},"digest":"%s"}]}\n' \
    "$(d "$2")" "$(d "$3")" > "$work/index-$(printf '%064d' "$1").json"
}
reset() {
  rm -f "$work"/tag-* "$work"/index-* "$work/sleeps"
  index 1 2 3   # this release's index: children 2 (amd64) and 3 (arm64)
  index 9 7 8   # the tag's previous index, from an earlier release or an earlier attempt
  d 1 > "$work/tag-0.86.0"; d 1 > "$work/tag-abc1234"
}
run() { "$script" "$repo" 0.86.0 abc1234 "$(d 2)" "$(d 3)" > "$work/out" 2> "$work/err"; }
accept() {
  run || { cat "$work/err" >&2; echo "FAIL: rejected $1" >&2; exit 1; }
  [ "$(cat "$work/out")" = "$(d 1)" ] || { echo "FAIL: $1 printed '$(cat "$work/out")'" >&2; exit 1; }
  cases=$((cases + 1)); echo "PASS: $1"
}
reject() {
  if run; then echo "FAIL: accepted $1" >&2; exit 1; fi
  [ ! -s "$work/out" ] || { echo "FAIL: $1 printed a digest" >&2; exit 1; }
  cases=$((cases + 1)); echo "PASS: rejects $1"
}

reset; accept 'both tags on the expected index'
[ ! -s "$work/sleeps" ] || { echo 'FAIL: slept on a clean read' >&2; exit 1; }
reset; printf '%s\n%s\n' "$(d 9)" "$(d 1)" > "$work/tag-abc1234"; accept ':short_sha stale once (stable promoted from its RC commit), then correct'
reset; printf '%s\n%s\n%s\n' "$(d 9)" "$(d 9)" "$(d 1)" > "$work/tag-0.86.0"; accept ':version stale twice (rerun legs), then correct'
reset; printf '\n%s\n' "$(d 1)" > "$work/tag-0.86.0"; accept 'a tag not yet visible, then correct'
reset; d 9 > "$work/tag-abc1234"; reject 'a tag stuck on its previous index'
[ "$(wc -l < "$work/sleeps" | tr -d ' ')" = 4 ] || { echo 'FAIL: did not retry with backoff' >&2; exit 1; }
reset; index 1 2 7; reject 'an index whose arm64 child is not this release'
reset; printf '{"config":{}}\n' > "$work/index-$(printf '%064d' 1).json"; reject 'a single-arch manifest'
# An inspection that fails must not pass on output it printed before failing.
idx1="$work/index-$(printf '%064d' 1).json"
reset; echo 99 > "$idx1.fail"; reject 'an inspector that prints the expected index, then exits nonzero'
reset; printf '{bad\n' >> "$idx1"; reject 'the expected index followed by malformed trailing JSON'
reset; echo 1 > "$idx1.fail"; accept 'a failed inspection, then a successful one'
reset; if "$script" "$repo" 0.86.0 abc1234 bogus "$(d 3)" > /dev/null 2>&1; then echo 'FAIL: accepted a bad digest' >&2; exit 1; fi
cases=$((cases + 1)); echo 'PASS: rejects a malformed digest argument'
echo "cases=$cases passed=$cases"
