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
STATIC_HOSTS="*.anthropic.com api.openai.com chatgpt.com auth.openai.com cache.nixos.org search.devbox.sh api.github.com ghcr.io pkg-containers.githubusercontent.com"

# has_worker_egress_policy <file> -- succeed iff the render contains the crd.antrea.io
# `-worker-egress` NetworkPolicy specifically. Keyed on that document's metadata name
# (not merely on the crd.antrea.io apiVersion), so an unrelated Antrea policy in the
# same render does not satisfy this check -- the completeness guard is about ONE policy.
has_worker_egress_policy() {
  awk '
    /^---[[:space:]]*$/                       { antrea = 0; next }
    /^apiVersion:[[:space:]]*crd\.antrea\.io/ { antrea = 1; next }
    antrea && /^[[:space:]]+name:[[:space:]].*-worker-egress[[:space:]]*$/ { f = 1 }
    END { exit !f }
  ' "$1"
}

# collect_allow_fqdns <file> -- print, one per line, each `fqdn:` value that sits in
# an egress entry whose `action:` is Allow, ONLY within the crd.antrea.io
# `-worker-egress` NetworkPolicy document. Scoping to that ONE document (via its
# metadata name) is load-bearing: pooling Allow-fqdns across every Antrea policy in
# the render would let a host absent from THIS policy but present in an unrelated one
# read as covered -- a false green in the gate whose whole job is to prevent one. The
# metadata `name:` line (2-space indent, no leading `- `) precedes `spec.egress`, so
# the `we` flag is set before any fqdn is seen. Drop entries (the denyCIDRs belt) and
# ipBlock peers carry no fqdn and are skipped by construction; the action gate makes
# that explicit rather than incidental.
collect_allow_fqdns() {
  awk '
    /^---[[:space:]]*$/                       { antrea = 0; we = 0; action = ""; next }
    /^apiVersion:[[:space:]]*crd\.antrea\.io/ { antrea = 1; next }
    antrea && /^[[:space:]]+name:[[:space:]].*-worker-egress[[:space:]]*$/ { we = 1; next }
    antrea && we && /^[[:space:]]*action:[[:space:]]/ { action = $2; next }
    antrea && we && action == "Allow" && /fqdn:/ {
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
        sub(/[?#].*$/, "", h)                        # strip query/fragment
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
  if ! has_worker_egress_policy "$_file"; then
    echo "BROKEN: no crd.antrea.io -worker-egress NetworkPolicy document in the render --" >&2
    echo "        it was supposed to render (workers.fqdnEgress.enabled: true)." >&2
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

# ---------------------------------------------------------------------------------
# Codex egress SHAPE (PRD #1106 D12, issue #1623).
#
# Completeness above only proves each Codex host is PRESENT somewhere in the Allow set.
# D12 is narrower than that: exactly api.openai.com, chatgpt.com and auth.openai.com,
# each on TCP 443 only, and NO wildcard or further OpenAI/ChatGPT host. A widening
# (`*.openai.com`, `*.chatgpt.com`, another port) renders fine and passes completeness,
# so this check asserts the shape directly.
#
# check_codex_egress <file> -- within the crd.antrea.io `-worker-egress` NetworkPolicy
# only (the same document scoping as collect_allow_fqdns), for each Codex host require
# exactly one Allow egress entry whose fqdn equals it (string equality) and whose ports
# are exactly one `protocol: TCP` + `port: 443`. Also flag any other Allow fqdn that
# equals or ends with `openai.com` or `chatgpt.com`, compared as literal strings, so a
# `*` is never a glob. Forge-derived Allows that are not OpenAI/ChatGPT hosts are
# ignored. One FAIL line per finding on stdout; returns 1 on a finding, 2 when the
# -worker-egress document is absent, 0 (with an OK line) otherwise.
CODEX_HOSTS="api.openai.com chatgpt.com auth.openai.com"

check_codex_egress() {
  _file="$1"
  if ! has_worker_egress_policy "$_file"; then
    echo "BROKEN: no crd.antrea.io -worker-egress NetworkPolicy document in $_file --" >&2
    echo "        the Codex egress shape check has nothing to inspect." >&2
    return 2
  fi
  awk -v hosts="$CODEX_HOSTS" '
    # Value after the first `key:`, quotes and whitespace stripped.
    function val(line,   v) {
      v = line
      sub(/^[^:]*:[[:space:]]*/, "", v)
      gsub(/"/, "", v)
      gsub(/[[:space:]]/, "", v)
      return v
    }
    # Close the current ports item into the entry signature, e.g. `TCP/443`.
    function closeitem(   one) {
      if (item) {
        one = proto "/" port (extra ? "+other" : "")
        sig = (sig == "" ? one : sig "," one)
      }
      item = 0; proto = ""; port = ""; extra = 0
    }
    # Record each fqdn of the finished entry, if it was an Allow.
    function flush(   i) {
      closeitem()
      if (inentry && action == "Allow")
        for (i = 1; i <= nf; i++) { n++; F[n] = fq[i]; S[n] = (sig == "" ? "none" : sig) }
      inentry = 0; nf = 0; sig = ""; action = ""; inports = 0
    }
    function endswith(s, suf) {
      return length(s) >= length(suf) && substr(s, length(s) - length(suf) + 1) == suf
    }
    /^---[[:space:]]*$/                       { if (we) flush(); antrea = 0; we = 0; eg = 0; next }
    /^apiVersion:[[:space:]]*crd\.antrea\.io/ { antrea = 1; next }
    antrea && !we && /^[[:space:]]+name:[[:space:]].*-worker-egress[[:space:]]*$/ { we = 1; next }
    !we { next }
    /^[[:space:]]*(#.*)?$/                    { next }   # comments and blank lines carry no keys
    /^[[:space:]]*egress:[[:space:]]*$/       { eg = 1; next }
    !eg { next }
    /^[[:space:]]*-[[:space:]]+name:/         { flush(); inentry = 1; next }
    /^[[:space:]]*action:/                    { action = val($0); next }
    /^[[:space:]]*to:[[:space:]]*$/           { closeitem(); inports = 0; next }
    /^[[:space:]]*ports:/                     { closeitem(); inports = 1; next }
    /^[[:space:]]*(-[[:space:]]+)?fqdn:/      { fq[++nf] = val($0); next }
    inports && /^[[:space:]]*-[[:space:]]/    { closeitem(); item = 1 }
    inports && item {
      k = $0
      sub(/^[[:space:]]*(-[[:space:]]+)?/, "", k)
      sub(/:.*$/, "", k)
      if (k == "protocol") proto = val($0)
      else if (k == "port") port = val($0)
      else extra = 1
    }
    END {
      if (we) flush()
      bad = 0
      nh = split(hosts, H, " ")
      for (j = 1; j <= nh; j++) { want[H[j]] = 1; c = 0; got = ""
        for (i = 1; i <= n; i++) if (F[i] == H[j]) { c++; got = S[i] }
        if (c == 0) {
          printf "FAIL: Codex worker egress %s: no Allow entry\n", H[j]; bad = 1
        } else if (c > 1) {
          printf "FAIL: Codex worker egress %s: %d Allow entries, want exactly one\n", H[j], c; bad = 1
        } else if (got != "TCP/443") {
          printf "FAIL: Codex worker egress %s: ports are %s, want exactly TCP/443\n", H[j], got; bad = 1
        }
      }
      for (i = 1; i <= n; i++) {
        f = F[i]
        if (f in want) continue
        if (endswith(f, "openai.com") || endswith(f, "chatgpt.com")) {
          printf "FAIL: Codex worker egress %s: unexpected OpenAI/ChatGPT Allow (only the exact api.openai.com, chatgpt.com and auth.openai.com are permitted)\n", f
          bad = 1
        }
      }
      if (bad) exit 1
      printf "OK: Codex worker egress is exactly %s, each on TCP/443 only\n", hosts
    }
  ' "$_file"
}

# --- canary self-test: prove the detector fires on a known-incomplete render --------
# Run the completeness check against the committed, deliberately-incomplete canary
# BEFORE trusting it on the real render. The canary omits TWO hosts, one per detector
# half: `cache.nixos.org` (a STATIC_HOSTS entry) and the forge host `gitlab.example.com`
# (which its ConfigMap names in FORGE_ALLOWED_BASE_URLS, exercising the forge-derivation
# path). A working detector must return a finding (1) naming BOTH. Any other outcome --
# complete (0), broken (2), or only one named -- means the instrument cannot be trusted
# (a regression in EITHER the static or the forge half would be caught here).
CANARY="$SCRIPT_DIR/fqdn-egress-canary.yaml"
[ -f "$CANARY" ] || { echo "BROKEN: canary fixture missing at $CANARY" >&2; exit 2; }

canary_out=$(check_completeness "$CANARY") && canary_rc=0 || canary_rc=$?
if [ "$canary_rc" -eq 1 ] \
   && printf '%s\n' "$canary_out" | is_present "FAIL: kube-native worker egress is missing canonical destination: cache.nixos.org" \
   && printf '%s\n' "$canary_out" | is_present "FAIL: kube-native worker egress is missing canonical destination: gitlab.example.com"; then
  echo "OK: canary self-test tripped both detector halves (static cache.nixos.org + forge gitlab.example.com)"
else
  echo "BROKEN: canary self-test did not fire as expected (rc=$canary_rc) on a" >&2
  echo "        known-incomplete render -- the FQDN completeness detector is not" >&2
  echo "        working, so a real gap would read as green. Output was:" >&2
  printf '%s\n' "$canary_out" | sed 's/^/          /' >&2
  exit 2
fi

# --- Codex canary self-test: prove the shape check fires ---------------------------
# The committed canary is deliberately wrong in three ways, one per finding class:
# chatgpt.com is on port 80 (wrong ports), an extra `*.openai.com` Allow (a widening)
# and NO auth.openai.com (missing). api.openai.com on TCP 443 is correct and must NOT
# be named. A working check returns 1 with all three exact FAIL lines; anything else
# means the instrument is broken.
CODEX_CANARY="$SCRIPT_DIR/codex-egress-canary.yaml"
[ -f "$CODEX_CANARY" ] || { echo "BROKEN: Codex canary fixture missing at $CODEX_CANARY" >&2; exit 2; }

codex_out=$(check_codex_egress "$CODEX_CANARY") && codex_rc=0 || codex_rc=$?
if [ "$codex_rc" -eq 1 ] \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress chatgpt.com: ports are TCP/80, want exactly TCP/443" \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress *.openai.com: unexpected OpenAI/ChatGPT Allow (only the exact api.openai.com, chatgpt.com and auth.openai.com are permitted)" \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress auth.openai.com: no Allow entry" \
   && ! printf '%s\n' "$codex_out" | grep -F -q "api.openai.com:"; then
  echo "OK: Codex canary self-test named all three findings (chatgpt.com port, *.openai.com widening, auth.openai.com missing)"
else
  echo "BROKEN: Codex canary self-test did not fire as expected (rc=$codex_rc) on a" >&2
  echo "        known-wrong render -- the Codex egress shape check is not working," >&2
  echo "        so a widened or missing Codex rule would read as green. Output was:" >&2
  printf '%s\n' "$codex_out" | sed 's/^/          /' >&2
  exit 2
fi

# --- the real checks on the render under test ---------------------------------------
check_completeness "$RENDER" || exit $?
check_codex_egress "$RENDER" || exit $?

# --- chart defaults: default-off, and the chart's OWN allowFQDNs --------------------
# The render under test (CI: deploy/values/ci-render.yaml) REPLACES allowFQDNs, so it
# proves nothing about deploy/chart/values.yaml's defaults. Render the chart twice more
# from its defaults: (i) with fqdnEgress left at its default, which must emit NO
# -worker-egress policy (off by default, fail-closed on clusters without Antrea); and
# (ii) with fqdnEgress enabled (plus the forge.allowedBaseURLs its guard requires),
# which must pass both completeness and the Codex shape check. Rendered from an
# offline copy with the `dependencies:` block stripped (the assert-drain-knobs-render.sh
# approach), so no subchart fetch is needed.
if ! command -v helm >/dev/null 2>&1; then
  echo "SKIP: helm not on PATH -- chart-default and default-off checks did not run"
  exit 0
fi

CHART_DIR="$SCRIPT_DIR/../deploy/chart"
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

STRIPPED="$WORK/chart"
cp -R "$CHART_DIR" "$STRIPPED"
rm -f "$STRIPPED/Chart.lock"
awk '
  BEGIN { skip = 0 }
  /^dependencies:/ { skip = 1; next }        # drop the dependencies: list ...
  skip && /^[^[:space:]-]/ { skip = 0 }      # ... until the next top-level key
  skip { next }
  { print }
' "$CHART_DIR/Chart.yaml" > "$STRIPPED/Chart.yaml"

DEFAULT_OFF="$WORK/default-off.yaml"
if ! helm template uzi "$STRIPPED" \
     --set workers.enabled=true --set api.tls.enabled=true > "$DEFAULT_OFF" 2> "$WORK/err"; then
  echo "BROKEN: helm template of the chart defaults failed:" >&2
  sed 's/^/          /' "$WORK/err" >&2
  exit 2
fi
if has_worker_egress_policy "$DEFAULT_OFF"; then
  echo "FAIL: the chart defaults render a -worker-egress policy; workers.fqdnEgress must default off"
  exit 1
fi
echo "OK: default-off -- the chart defaults render no -worker-egress policy"

DEFAULT_ON="$WORK/default-on.yaml"
if ! helm template uzi "$STRIPPED" \
     --set workers.enabled=true --set api.tls.enabled=true \
     --set workers.fqdnEgress.enabled=true \
     --set 'forge.allowedBaseURLs={https://gitlab.example.com}' > "$DEFAULT_ON" 2> "$WORK/err"; then
  echo "BROKEN: helm template of the chart defaults with fqdnEgress enabled failed:" >&2
  sed 's/^/          /' "$WORK/err" >&2
  exit 2
fi
_rc=0
check_completeness "$DEFAULT_ON" || _rc=$?
[ "$_rc" -eq 0 ] || { echo "FAIL: chart-default allowFQDNs (deploy/chart/values.yaml) failed completeness"; exit "$_rc"; }
check_codex_egress "$DEFAULT_ON" || _rc=$?
[ "$_rc" -eq 0 ] || { echo "FAIL: chart-default allowFQDNs (deploy/chart/values.yaml) failed the Codex egress shape check"; exit "$_rc"; }
echo "OK: chart defaults -- deploy/chart/values.yaml allowFQDNs pass completeness and the Codex shape check"
