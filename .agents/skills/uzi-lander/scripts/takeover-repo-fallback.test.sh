#!/usr/bin/env bash
# Hermetic regression (#2489): when --repo is absent and `gh repo view` fails, takeover.sh
# falls back to a github.com origin remote (https://github.com/O/R or git@github.com:O/R,
# optional .git); any other origin keeps "cannot infer --repo" and exit 3. An explicit
# --repo wins and consults neither. Every case exits 3 later anyway (the stubbed `gh pr view`
# fails), so the REPO= line, not the exit code, tells a resolved repo from a refused one.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/takeover.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
# gh cannot reach GitHub: every call fails, `repo view` included.
echo "$*" >> "$GH_LOG"
exit 1
STUB
cat > "$WORK/bin/git" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$GIT_LOG"
if [ "$*" = "remote get-url origin" ]; then
  [ -n "${ORIGIN_URL:-}" ] || { echo "error: No such remote 'origin'" >&2; exit 2; }
  echo "$ORIGIN_URL"; exit 0
fi
exit 1
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/git" "$WORK/bin/uzi"

# run <origin-url|""> [takeover args...]: sets OUT, ERR, RC; fresh gh/git logs per case.
run() {
  local origin="$1"; shift
  : > "$WORK/gh.log"; : > "$WORK/git.log"
  RC=0
  ORIGIN_URL="$origin" GH_LOG="$WORK/gh.log" GIT_LOG="$WORK/git.log" PATH="$WORK/bin:$PATH" \
    bash "$SCRIPT" 42 --no-claim "$@" > "$WORK/out" 2> "$WORK/err" || RC=$?
  OUT=$(cat "$WORK/out"); ERR=$(cat "$WORK/err")
}

for url in https://github.com/own/rep https://github.com/own/rep.git \
  git@github.com:own/rep git@github.com:own/rep.git; do
  run "$url"
  printf '%s\n' "$OUT" | grep -qxF 'REPO=own/rep' || fail "$url: no REPO=own/rep: $OUT / $ERR"
  grep -qF 'repo view' "$WORK/gh.log" || fail "$url: gh repo view was not tried first"
  if printf '%s\n' "$ERR" | grep -qF 'cannot infer --repo'; then fail "$url: refused: $ERR"; fi
done

for url in "" https://gitlab.com/own/rep.git https://github.com/own/rep/extra \
  ssh://git@github.com/own/rep.git https://github.com/own/rep/ https://github.com/own/.git \
  git@github.com:own/rep/ http://github.com/own/rep; do
  run "$url"
  [ "$RC" -eq 3 ] || fail "'$url': rc=$RC, want 3"
  printf '%s\n' "$ERR" | grep -qF 'cannot infer --repo' || fail "'$url': no 'cannot infer --repo': $ERR"
  if printf '%s\n' "$OUT" | grep -q '^REPO='; then fail "'$url': resolved anyway: $OUT"; fi
done

run https://github.com/own/rep --repo given/one
printf '%s\n' "$OUT" | grep -qxF 'REPO=given/one' || fail "explicit --repo not used: $OUT / $ERR"
if grep -qF 'repo view' "$WORK/gh.log"; then fail "explicit --repo still consulted gh repo view"; fi
if grep -qF 'remote get-url' "$WORK/git.log"; then fail "explicit --repo still read the origin remote"; fi
grep -qF 'pr view 42 --repo given/one' "$WORK/gh.log" || fail "gh pr view did not use the explicit repo: $(cat "$WORK/gh.log")"

echo "takeover-repo-fallback: all cases passed"
