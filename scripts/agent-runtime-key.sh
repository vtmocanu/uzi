#!/usr/bin/env bash
# agent-runtime-key.sh -- print the INPUT KEY of an agent template's runtime base (#1720).
#
# Usage: scripts/agent-runtime-key.sh <template> <platform>     (run from the repo root)
#   e.g. scripts/agent-runtime-key.sh base linux/amd64   ->   v1-<32 hex>
#
# release.yml publishes each template's `runtime` stage (agent/templates/<t>/Dockerfile,
# everything above `FROM runtime AS release`) as ghcr.io/vtmocanu/uzi/agent-runtime-<t>:<key>
# and builds only when that tag is absent. The key must therefore change whenever ANY input
# of the runtime stage changes, or a release silently reuses a stale base (ADR-1720). It
# covers, NUL-framed:
#   * a schema version, the template, the platform;
#   * the runtime stage's Dockerfile text verbatim (parent digest, ARG defaults, RUN lines);
#   * the template's Dockerfile.dockerignore text;
#   * every COPY source as `<path> <mode> <type> <git object id at HEAD>` (a tree id covers
#     every file, mode and symlink target beneath it; the mode covers the entry itself).
# It is an input identity, not a reproducibility guarantee: steps that resolve packages at
# build time can produce different bytes for the same key.
#
# FAIL-CLOSED (exit 2): anything this parser cannot account for is refused rather than
# silently left out of the key -- ADD, COPY --from / JSON / heredoc / globs / variables /
# escapes / continuations, RUN --mount, a second FROM, a global ARG, an ARG without a
# default (other than TARGETARCH/TARGETPLATFORM), an unknown platform, or an input that
# differs from HEAD (staged, unstaged, untracked or ignored files under a COPY source).
# Extend the parser when the Dockerfile needs a new form; never widen the refusal.
set -euo pipefail

KEY_SCHEMA=v1
die() { echo "agent-runtime-key: $*" >&2; exit 2; }

[ "$#" -eq 2 ] || die "usage: $0 <template> <platform>"
template="$1"
platform="$2"
case "$template" in ''|*[!a-z0-9-]*) die "bad template name '$template'";; esac
case "$platform" in linux/amd64|linux/arm64) ;; *) die "unsupported platform '$platform'";; esac

dockerfile="agent/templates/${template}/Dockerfile"
ignorefile="agent/templates/${template}/Dockerfile.dockerignore"
[ -f "$dockerfile" ] || die "missing $dockerfile"
[ -f "$ignorefile" ] || die "missing $ignorefile"

# The runtime stage: every line before the release marker.
marker_line="$(grep -n -x 'FROM runtime AS release' "$dockerfile" | cut -d: -f1 || true)"
[ -n "$marker_line" ] || die "$dockerfile has no 'FROM runtime AS release' line"
[ "$(printf '%s\n' "$marker_line" | wc -l | tr -d ' ')" -eq 1 ] || die "$dockerfile has more than one release marker"
stage="$(head -n "$((marker_line - 1))" "$dockerfile")"

# Logical instructions: join backslash continuations so a flag or source on a continued
# line cannot hide from the checks below. Comments and blank lines are skipped for parsing
# (they stay in the key via the verbatim stage text).
logical="$(printf '%s\n' "$stage" | awk '
  /^[[:space:]]*#/ && cur == "" { next }
  { line = $0
    if (line ~ /\\[[:space:]]*$/) { sub(/\\[[:space:]]*$/, "", line); cur = cur line " "; next }
    cur = cur line
    if (cur ~ /[^[:space:]]/) print cur
    cur = "" }
  END { if (cur != "") print cur }')"

from_count=0
seen_from=0
sources=()
while IFS= read -r ins; do
  kw="$(printf '%s' "$ins" | awk '{print toupper($1)}')"
  case "$kw" in
    FROM)
      from_count=$((from_count + 1))
      seen_from=1
      printf '%s' "$ins" | grep -Eq '^FROM[[:space:]]+[^[:space:]$]+@sha256:[0-9a-f]{64}[[:space:]]+AS[[:space:]]+runtime[[:space:]]*$' \
        || die "runtime stage must start 'FROM <image>@sha256:<digest> AS runtime', got: $ins"
      ;;
    ARG)
      [ "$seen_from" -eq 1 ] || die "global ARG before FROM is not supported: $ins"
      spec="$(printf '%s' "$ins" | awk '{print $2}')"
      case "$spec" in
        TARGETARCH|TARGETPLATFORM) ;;
        *=*) ;;
        *) die "ARG without a default would take an unkeyed build arg: $ins";;
      esac
      ;;
    ADD) die "ADD is not supported in the runtime stage: $ins";;
    ONBUILD) die "ONBUILD is not supported in the runtime stage: $ins";;
    RUN)
      printf '%s' "$ins" | grep -q -- '--mount' && die "RUN --mount is not supported in the runtime stage: $ins"
      ;;
    COPY)
      case "$ins" in *'$'*|*'['*|*'*'*|*'?'*|*'<<'*|*\\*) die "unsupported COPY form: $ins";; esac
      read -r -a toks <<< "$ins"
      args=()
      for tok in "${toks[@]:1}"; do
        case "$tok" in
          --chmod=*|--chown=*|--link|--link=*) ;;
          --*) die "unsupported COPY flag '$tok': $ins";;
          *) args+=("$tok");;
        esac
      done
      [ "${#args[@]}" -ge 2 ] || die "COPY needs a source and a destination: $ins"
      for src in "${args[@]:0:${#args[@]}-1}"; do
        # Canonical repo-relative paths only, naming ONE tree entry: `.`/`./` (the whole
        # context) or a `./`, `//`, `/.` spelling would make `git ls-tree` list several
        # entries or none, and the key would silently cover less than the COPY does.
        src="${src%/}"
        case "$src" in
          ''|.|./*|/*|..|../*|*/..|*/../*|*/.|*/./*|*//*) die "COPY source must be a canonical repo-relative path naming one file or directory: $ins";;
        esac
        sources+=("$src")
      done
      ;;
  esac
done <<< "$logical"
[ "$from_count" -eq 1 ] || die "runtime stage must have exactly one FROM (found $from_count)"

# Every input must equal HEAD: the key describes a commit, and release.yml builds a clean
# checkout. Staged, unstaged, untracked and ignored-but-present files all count.
inputs=("$dockerfile" "$ignorefile")
if [ "${#sources[@]}" -gt 0 ]; then
  while IFS= read -r s; do inputs+=("$s"); done < <(printf '%s\n' "${sources[@]}" | LC_ALL=C sort -u)
fi
dirty="$(git status --porcelain --ignored --untracked-files=all -- "${inputs[@]}")"
[ -z "$dirty" ] || die "inputs differ from HEAD:
$dirty"

oids=()
for p in "${inputs[@]:2}"; do
  # `<mode> <type> <oid>`: the blob/tree id alone omits a file's own mode, which lives in
  # the parent tree (a chmod +x would not change the key).
  entry="$(git ls-tree --full-tree HEAD -- "$p")"
  [ -n "$entry" ] || die "COPY source '$p' is not tracked at HEAD"
  # Exactly one entry, for exactly this path: never a listing of a directory's children.
  [ "$(printf '%s\n' "$entry" | wc -l | tr -d ' ')" -eq 1 ] && [ "${entry#*$'\t'}" = "$p" ] \
    || die "COPY source '$p' does not resolve to a single tree entry"
  oid="${entry%%$'\t'*}"
  oids+=("$oid")
done

material() {
  printf '%s\0' "$KEY_SCHEMA" "$template" "$platform"
  printf 'stage\0%s\0' "$stage"
  printf 'dockerignore\0%s\0' "$(cat "$ignorefile")"
  local i
  for i in "${!oids[@]}"; do
    printf 'src\0%s\0%s\0' "${inputs[$((i + 2))]}" "${oids[$i]}"
  done
}

sum="$(material | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-32)"
printf '%s-%s\n' "$KEY_SCHEMA" "$sum"
