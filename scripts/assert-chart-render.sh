#!/bin/sh
# Assert the rendered chart is a well-formed multi-document manifest.
#
# WHY THIS EXISTS (issue #149). A Go-template comment ending `*/ -}}` immediately
# before a `---` trims the newline and GLUES the document separator onto the
# preceding value:
#
#     - name: registry-robot-secret-uzi-workers---
#
# The separator is destroyed, so two objects merge into ONE YAML document with
# duplicate keys -- and every YAML parser silently keeps the LAST one. On
# the cluster that deleted the `uzi-workers` ServiceAccount and its pull-secret
# InfisicalSecret from the manifest, which made restricted-tier hosted workers
# unprovisionable for days while ArgoCD correctly reported Synced/Healthy: it was
# in sync with what the manifest actually declared.
#
# NOTHING ELSE CATCHES IT. `helm lint` passes, `helm template` exits 0, the
# rendered text still contains `kind: ServiceAccount` at column 0 so grep finds
# it, and a server-side dry-run applies the surviving object without complaint.
# The object is not malformed -- it is ABSENT, and only a parse reveals that.
#
# The check is on the SHAPE, not on a list of object names: exactly one `kind:`
# per document. That is the merge signature regardless of which objects collide,
# so it keeps working as the chart grows.
set -eu

RENDER="${1:?usage: assert-chart-render.sh <rendered.yaml>}"

# Resolve this script's own directory so the committed canary fixture can be found
# regardless of the caller's cwd. Clear CDPATH first so `cd` cannot resolve via it
# and echo an unexpected directory (the convention assert-drain-knobs-render.sh sets).
CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)

# The glued separator itself first, because it names the exact line and the fix.
# Written without a negated bracket expression on purpose: `grep -E "[^-]---$"`
# does NOT match `foo---` under ugrep (verified 2026-07-27), which is what `grep`
# resolves to on some dev machines -- so that form would pass on a broken render.
if awk '/---$/ && $0 !~ /^---$/ && $0 !~ /^[[:space:]]*#/ && $0 !~ /^[- ]*$/ { print "  line " NR ": " $0; found=1 } END { exit !found }' "$RENDER"; then
  echo "FAIL: a document separator is glued to the end of a value (above)."
  echo "      A Go-template comment before it ends \`*/ -}}\`; the \`-}}\` eats the newline."
  echo "      Write \`*/}}\` when a \`---\` follows."
  exit 1
fi

# Then the general merge signature, which catches a collision however it arose.
awk '
  /^---$/ { doc++; kinds[doc] = 0; next }
  /^kind:/ { kinds[doc]++; if (kinds[doc] == 1) firstline[doc] = NR }
  END {
    bad = 0
    for (d in kinds) {
      if (kinds[d] > 1) {
        printf "FAIL: document %d holds %d `kind:` keys (first at line %d).\n", d, kinds[d], firstline[d]
        printf "      Two objects merged into one document, so a YAML parser keeps only the last\n"
        printf "      and the others are SILENTLY DELETED from the manifest.\n"
        bad = 1
      }
    }
    if (bad) exit 1
  }
' "$RENDER"

echo "OK: $(grep -c '^---$' "$RENDER") documents, one kind per document, no glued separators"

# ---------------------------------------------------------------------------------
# FQDN-egress completeness (PRD #808 M2).
#
# WHY THIS EXISTS. M1 single-sourced the worker's kube-native egress allow-list: the
# Antrea `-worker-egress` NetworkPolicy is the ONLY thing standing between a hosted
# worker and the open internet, so a canonical destination silently dropped from it
# (an editor removes an Allow entry, a values refactor loses one, the api SSRF
# allowlist gains a forge the policy never learns about) does not fail any render --
# `helm template` still exits 0 and the manifest is still well-formed. The worker
# just cannot reach that host at runtime, days later, with nothing red.
#
# So this check ties the two rendered artifacts together: it collects the set of
# Allow-fqdn hosts from the Antrea policy and asserts it covers BOTH a hardcoded set
# of canonical infra hosts AND every forge the api ConfigMap's FORGE_ALLOWED_BASE_URLS
# names (comma-split, host derived from each URL). The forge half is dynamic on
# purpose: it proves the egress policy tracks the api's own SSRF allowlist rather
# than a second hand-maintained copy that can drift.
#
# PARSED WITH awk, NEVER A BARE grep PATTERN. This host's `grep` is ugrep, whose
# POSIX modes mishandle negated classes and brace intervals (the well-formedness
# note above records the same trap), so the YAML is walked with awk; host membership
# is an exact string equality in awk too, so a host containing `*` (e.g.
# `*.anthropic.com`) cannot be misread as a glob.
#
# EXIT CODES (the convention assert-drain-knobs-render.sh sets):
#     2 = the instrument is broken (no crd.antrea.io policy in the render that was
#         supposed to contain one, or the committed canary did not trip the detector)
#     1 = a finding (a canonical destination is missing from the Allow-fqdn set)
#     0 = every canonical destination is covered
# `task` flattens any non-zero to its own 201.

# Disable pathname expansion: the canonical host set includes `*.anthropic.com`, and
# an unquoted `*.anthropic.com` in a `for` word list would otherwise glob against cwd.
set -f

# The canonical infra hosts every worker egress render must Allow, independent of
# the forge configuration. Kept here as the single source of the STATIC expectation.
STATIC_HOSTS="*.anthropic.com cache.nixos.org search.devbox.sh ghcr.io pkg-containers.githubusercontent.com"

# has_antrea_policy <file> -- succeed iff the render contains a crd.antrea.io document.
has_antrea_policy() {
  awk '/^apiVersion:[[:space:]]*crd\.antrea\.io/ { f = 1 } END { exit !f }' "$1"
}

# collect_allow_fqdns <file> -- print, one per line, each `fqdn:` value that sits in
# an egress entry whose `action:` is Allow, ONLY within a crd.antrea.io document.
# Drop entries (the denyCIDRs belt) and ipBlock peers carry no fqdn and are skipped
# by construction; the action gate makes that explicit rather than incidental.
collect_allow_fqdns() {
  awk '
    /^---[[:space:]]*$/            { antrea = 0; action = ""; next }
    /^apiVersion:[[:space:]]*crd\.antrea\.io/ { antrea = 1; next }
    antrea && /^[[:space:]]*action:[[:space:]]/ { action = $2; next }
    antrea && action == "Allow" && /fqdn:/ {
      v = $0
      sub(/^.*fqdn:[[:space:]]*/, "", v)   # keep only the value after `fqdn:`
      gsub(/"/, "", v)                     # strip quotes
      gsub(/[[:space:]]/, "", v)           # strip any stray whitespace
      if (v != "") print v
    }
  ' "$1"
}

# expected_forge_hosts <file> -- read FORGE_ALLOWED_BASE_URLS from the api ConfigMap
# in the SAME render, comma-split it, and print the bare host of each URL (scheme and
# :port stripped). Absent FORGE_ALLOWED_BASE_URLS prints nothing (forge set is empty).
expected_forge_hosts() {
  awk '
    /^[[:space:]]*FORGE_ALLOWED_BASE_URLS:[[:space:]]/ {
      v = $0
      sub(/^.*FORGE_ALLOWED_BASE_URLS:[[:space:]]*/, "", v)
      gsub(/"/, "", v)
      n = split(v, urls, ",")
      for (i = 1; i <= n; i++) {
        h = urls[i]
        gsub(/[[:space:]]/, "", h)
        sub(/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//, "", h)  # strip scheme://
        sub(/\/.*$/, "", h)                          # strip /path
        sub(/:[0-9]+$/, "", h)                       # strip :port
        if (h != "") print h
      }
    }
  ' "$1"
}

# is_present <host> -- read a newline list of hosts on stdin, succeed iff one is an
# EXACT match for <host> (string equality, so `*` in a host is not a glob).
is_present() {
  awk -v h="$1" '$0 == h { f = 1 } END { exit !f }'
}

# check_completeness <file> -- assert every expected canonical host (STATIC_HOSTS plus
# each forge host derived from the render's own FORGE_ALLOWED_BASE_URLS) appears in the
# Allow-fqdn set. Prints a FAIL line naming EACH missing host; returns per the exit
# codes above. Prints to stdout so the canary self-test can inspect its output.
check_completeness() {
  _file="$1"
  if ! has_antrea_policy "$_file"; then
    echo "BROKEN: no crd.antrea.io NetworkPolicy document in the render -- the worker" >&2
    echo "        egress policy was supposed to render (workers.fqdnEgress.enabled: true)." >&2
    return 2
  fi

  _allowed=$(collect_allow_fqdns "$_file")
  _forge=$(expected_forge_hosts "$_file")
  _expected="$STATIC_HOSTS $_forge"

  _missing=""
  _count=0
  for _h in $_expected; do
    _count=$((_count + 1))
    if printf '%s\n' "$_allowed" | is_present "$_h"; then
      :
    else
      _missing="$_missing $_h"
    fi
  done

  if [ -n "$_missing" ]; then
    for _h in $_missing; do
      echo "FAIL: kube-native worker egress is missing canonical destination: $_h"
    done
    return 1
  fi

  echo "OK: kube-native worker egress covers all $_count canonical destinations (static + forge)"
  return 0
}

# --- canary self-test: prove the detector fires on a known-incomplete render --------
# Run the completeness check against the committed, deliberately-incomplete canary
# BEFORE trusting it on the real render. The canary omits exactly `cache.nixos.org`,
# so a working detector must return a finding (1) that names it. Any other outcome --
# it reports complete (0), or breaks (2) -- means the instrument cannot be trusted.
CANARY="$SCRIPT_DIR/fqdn-egress-canary.yaml"
[ -f "$CANARY" ] || { echo "BROKEN: canary fixture missing at $CANARY" >&2; exit 2; }

canary_out=$(check_completeness "$CANARY") && canary_rc=0 || canary_rc=$?
if [ "$canary_rc" -eq 1 ] && printf '%s\n' "$canary_out" | is_present "FAIL: kube-native worker egress is missing canonical destination: cache.nixos.org"; then
  echo "OK: canary self-test tripped the detector on its injected gap (cache.nixos.org)"
else
  echo "BROKEN: canary self-test did not fire as expected (rc=$canary_rc) on a" >&2
  echo "        known-incomplete render -- the FQDN completeness detector is not" >&2
  echo "        working, so a real gap would read as green. Output was:" >&2
  printf '%s\n' "$canary_out" | sed 's/^/          /' >&2
  exit 2
fi

# --- the real check on the render under test ----------------------------------------
check_completeness "$RENDER" || exit $?
