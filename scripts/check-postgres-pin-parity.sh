#!/usr/bin/env bash
# Guard: CI tests the Postgres the stack ships.
#
#   check-postgres-pin-parity.sh [compose ci store-it deploy-compose chart m3b-server m3b-lifecycle]
#     defaults are the corresponding repository files below.
#
# 1. ci.yml's UZI_CI_PG_IMAGE (the single LiveDB job, test-codex-refresh-lostreply-e2e) pins the same tag AND digest as
#    both compose files, the chart simple-mode default and both packaged-proof
#    defaults. CI pulls through mirror.gcr.io; normalize that repository alias to
#    the same tag+digest. Renovate groups all six pins (#2622, #4275).
# 2. Every floating `postgres:<tag>` reference in ci.yml and run-store-it.sh (any
#    UZI_STORE_IT_PG_IMAGE or docker-pull-fallback.sh argument, the script's
#    default) uses the pinned tag. Renovate does not manage floating tags, so a major
#    bump of the pins must carry them by hand; this makes that bump's PR red until it does.
#
# Exit: 0 = consistent; 1 = a pin or floating tag differs; 2 = a pin is missing,
# duplicated or malformed (instrument broken: a reworded image line must not read as
# parity).
# awk, not grep: the patterns need a negated class, which this host's ugrep mishandles
# in -E mode and BSD grep cannot do in -P mode (root CLAUDE.md, "grep on this host").
set -euo pipefail

compose=${1:-docker-compose.yml}
ci=${2:-.github/workflows/ci.yml}
storeit=${3:-e2e/run-store-it.sh}
deploy_compose=${4:-deploy/compose/compose.yaml}
chart=${5:-deploy/chart/values.yaml}
m3b_server=${6:-e2e/codex-m3b/testserver.sh}
m3b_lifecycle=${7:-e2e/codex-m3b/run-lifecycle.sh}

valid_pin() {
  [[ $1 =~ ^[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]
}

# Read only the default database.simple.image block, not unrelated chart images.
# Count every path component so missing/duplicated keys cannot look like parity.
chart_pin() {
  local out
  out=$(awk '
    /^database:/ { db++; in_db=1; in_simple=0; in_image=0; next }
    /^[^ #]/ { in_db=0 }
    in_db && /^  simple:/ { simple++; in_simple=1; in_image=0; next }
    in_db && /^  [^ ]/ { in_simple=0 }
    in_simple && /^    image:/ { image++; in_image=1; next }
    in_simple && /^    [^ ]/ { in_image=0 }
    in_image && /^      repository:/ { repos++; repo=$0; sub(/^      repository: */, "", repo) }
    in_image && /^      tag:/ {
      tags++; tag=$0; sub(/^      tag: */, "", tag)
      if (substr(tag,1,1)!="\"" || substr(tag,length(tag))!="\"") exit 1
      tag=substr(tag,2,length(tag)-2)
    }
    END {
      if (db!=1 || simple!=1 || image!=1 || repos!=1 || tags!=1 || repo!="postgres") exit 1
      print tag
    }' "$1") || return 1
  valid_pin "$out" || return 1
  printf '%s\n' "$out"
}

# These fixed assignments retain CDR_M3B_PG_IMAGE overrides. A renamed variable,
# absent default, malformed digest or duplicate assignment is an instrument error.
shell_pin() {
  local out
  out=$(awk -v key="$2" '
    { line=$0; sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line) }
    index(line,key "=")==1 {
      n++; prefix=key "=\"${CDR_M3B_PG_IMAGE:-postgres:"
      if (index(line,prefix)!=1 || substr(line,length(line)-1)!="}\"") exit 1
      value=substr(line,length(prefix)+1,length(line)-length(prefix)-2)
    }
    END { if (n!=1) exit 1; print value }' "$1") || return 1
  valid_pin "$out" || return 1
  printf '%s\n' "$out"
}

# Prints "<tag>@sha256:<hex>" from the compose image or dedicated CI env key.
# Exactly one candidate may exist and it must be a full tag + 64-hex digest pin.
# A malformed or duplicated CI key must fail even when its repository is wrong.
pin() {
  local file=$1 repo=$2 key=${3:-image} out
  out=$(awk -v repo="$repo" -v key="$key" '
    { line = $0; sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line) }
    index(line, key ":") == 1 {
      ref = substr(line, length(key) + 2); sub(/^[ \t]+/, "", ref)
      if (key != "image" || index(ref, repo ":") == 1) {
        n++; if (index(ref, repo ":") != 1) exit 1
        v = substr(ref, length(repo) + 2)
      }
    }
    END {
      if (n != 1) exit 1
      if (v !~ /^[^@ \t]+@sha256:[0-9a-f]+$/) exit 1
      if (length(v) - index(v, "@sha256:") - 7 != 64) exit 1
      print v
    }' "$file") || {
    echo "check-postgres-pin-parity: expected exactly one '$repo:<tag>@sha256:<64 hex>' $key line in $file" >&2
    return 1
  }
  printf '%s\n' "$out"
}

compose_pin=$(pin "$compose" 'postgres') || exit 2
ci_pin=$(pin "$ci" 'mirror.gcr.io/library/postgres' UZI_CI_PG_IMAGE) || exit 2

if [ "$compose_pin" != "$ci_pin" ]; then
  echo "check-postgres-pin-parity: postgres pins differ:" >&2
  echo "  $compose: postgres:$compose_pin" >&2
  echo "  $ci: mirror.gcr.io/library/postgres:$ci_pin" >&2
  echo "Bump both to the same tag and digest (renovate.json groups them as \"postgres\")." >&2
  exit 1
fi

same_pin() {
  if [ "$compose_pin" != "$2" ]; then
    echo "check-postgres-pin-parity: $1 has $2; expected $compose_pin" >&2
    return 1
  fi
}

deploy_pin=$(pin "$deploy_compose" postgres) || exit 2
chart_default=$(chart_pin "$chart") || {
  echo "check-postgres-pin-parity: expected one pinned database.simple.image in $chart" >&2
  exit 2
}
server_default=$(shell_pin "$m3b_server" CDR_M3B_PG_IMAGE) || {
  echo "check-postgres-pin-parity: expected one pinned CDR_M3B_PG_IMAGE default in $m3b_server" >&2
  exit 2
}
lifecycle_default=$(shell_pin "$m3b_lifecycle" PG_IMAGE) || {
  echo "check-postgres-pin-parity: expected one pinned PG_IMAGE default in $m3b_lifecycle" >&2
  exit 2
}
same_pin "$deploy_compose" "$deploy_pin"
same_pin "$chart" "$chart_default"
same_pin "$m3b_server" "$server_default"
same_pin "$m3b_lifecycle" "$lifecycle_default"

tag=${compose_pin%%@*}
# Floating references: `postgres:<tag>` starting a name, i.e. at line start or after a
# space, `/`, quote, `=`, `(` or `:`, or after the `:-` of a shell default
# (`${X:-postgres:17}`). So `mirror.gcr.io/library/postgres:` counts and
# `my-postgres:` does not.
drift=$(awk -v want="$tag" '
  {
    s = $0; off = 0
    while (match(s, /postgres:[A-Za-z0-9._-]+/)) {
      abs = off + RSTART
      pre = (abs > 1) ? substr($0, abs - 1, 1) : ""
      pre2 = (abs > 2) ? substr($0, abs - 2, 1) : ""
      named = (abs == 1) || pre ~ /[ \t\/"'"'"'=(:]/ || (pre == "-" && pre2 == ":")
      t = substr(s, RSTART + 9, RLENGTH - 9)
      if (named && t != want) printf "%s:%d: postgres:%s\n", FILENAME, FNR, t
      off += RSTART + RLENGTH - 1
      s = substr(s, RSTART + RLENGTH)
    }
  }' "$ci" "$storeit")
if [ -n "$drift" ]; then
  echo "check-postgres-pin-parity: floating postgres references differ from the pinned tag $tag:" >&2
  printf '  %s\n' "$drift" >&2
  echo "Renovate does not manage floating tags; update them with the pin." >&2
  exit 1
fi
echo "check-postgres-pin-parity: OK ($compose_pin)"
