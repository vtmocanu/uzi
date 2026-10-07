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
# ANTREA ONLY. With workers.fqdnEgress.provider: ovn the named egress is an OVN
# EgressFirewall instead; scripts/assert-openshift-render.sh asserts that its allow set and
# deny belt EQUAL the Antrea ones for the same values, so this check's completeness and
# Codex-shape guarantees carry over to OVN without being restated here.
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
# (`*.openai.com`, `*`, `*openai*`, another port) renders fine and passes completeness,
# so this check asserts the shape directly.
#
# check_codex_egress <file> -- within the crd.antrea.io `-worker-egress` NetworkPolicy
# only (the same document scoping as collect_allow_fqdns), and over Allow entries only,
# with every fqdn lowercased and one trailing `.` stripped first:
#   - each Codex host must match exactly one Allow entry (string equality), and every
#     entry matching it must have ports of exactly one `protocol: TCP` + `port: 443`;
#   - any other Allow fqdn that equals or ends with `openai.com` or `chatgpt.com`
#     (literal string comparison) is a finding;
#   - any other Allow fqdn containing `*` is read as a glob (`*` = any run of
#     characters, anchored at both ends) and is a finding if it matches a host in
#     CODEX_PROBES, so `*`, `*.com` and `*openai*` fail while `*.anthropic.com` passes.
# Forge-derived Allows that are not OpenAI/ChatGPT hosts are ignored. One FAIL line per
# finding on stdout; returns 1 on a finding, 2 when the -worker-egress document is
# absent, 0 (with an OK line) otherwise.
CODEX_HOSTS="api.openai.com chatgpt.com auth.openai.com"
CODEX_PROBES="api.openai.com chatgpt.com auth.openai.com platform.openai.com x.openai.com x.chatgpt.com"

check_codex_egress() {
  _file="$1"
  if ! has_worker_egress_policy "$_file"; then
    echo "BROKEN: no crd.antrea.io -worker-egress NetworkPolicy document in $_file --" >&2
    echo "        the Codex egress shape check has nothing to inspect." >&2
    return 2
  fi
  awk -v hosts="$CODEX_HOSTS" -v probes="$CODEX_PROBES" '
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
    # Anchored regex for a wildcard fqdn: `*` matches any run, everything else literal.
    function globre(s,   i, ch, r) {
      r = "^"
      for (i = 1; i <= length(s); i++) {
        ch = substr(s, i, 1)
        if (ch == "*") r = r ".*"
        else if (ch ~ /[a-z0-9-]/) r = r ch
        else if (ch == "\\" || ch == "^" || ch == "]") r = r "\\" ch
        else r = r "[" ch "]"
      }
      return r "$"
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
    /^[[:space:]]*(-[[:space:]]+)?fqdn:/      { v = tolower(val($0)); sub(/\.$/, "", v); fq[++nf] = v; next }
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
      np = split(probes, P, " ")
      for (j = 1; j <= nh; j++) { want[H[j]] = 1; c = 0
        for (i = 1; i <= n; i++) if (F[i] == H[j]) c++
        if (c == 0) {
          printf "FAIL: Codex worker egress %s: no Allow entry\n", H[j]; bad = 1
        } else if (c > 1) {
          printf "FAIL: Codex worker egress %s: %d Allow entries, want exactly one\n", H[j], c; bad = 1
        }
        for (i = 1; i <= n; i++) if (F[i] == H[j] && S[i] != "TCP/443") {
          printf "FAIL: Codex worker egress %s: ports are %s, want exactly TCP/443\n", H[j], S[i]; bad = 1
        }
      }
      for (i = 1; i <= n; i++) {
        f = F[i]
        if (f in want) continue
        if (endswith(f, "openai.com") || endswith(f, "chatgpt.com")) {
          printf "FAIL: Codex worker egress %s: unexpected OpenAI/ChatGPT Allow (only the exact api.openai.com, chatgpt.com and auth.openai.com are permitted)\n", f
          bad = 1
        } else if (index(f, "*")) {
          re = globre(f)
          for (p = 1; p <= np; p++) if (P[p] ~ re) {
            printf "FAIL: Codex worker egress %s: wildcard Allow covers %s (only the exact api.openai.com, chatgpt.com and auth.openai.com are permitted)\n", f, P[p]
            bad = 1
            break
          }
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
# The committed canary plants one defect per detector path (its header lists them):
# a wrong port, a duplicate (written upper-case with a trailing dot), a suffix
# widening, a widening that only matches once normalized, a catch-all `*` glob and a
# missing host. It also carries entries that must NOT be reported: a correct
# api.openai.com, `*.anthropic.com`, a Drop entry (Allow-only filter) and an Allow in
# a second Antrea policy (document scoping). A working check returns 1 with EXACTLY
# the six FAIL lines below and none of the must-not-report hosts; anything else means
# the instrument is broken.
CODEX_CANARY="$SCRIPT_DIR/codex-egress-canary.yaml"
[ -f "$CODEX_CANARY" ] || { echo "BROKEN: Codex canary fixture missing at $CODEX_CANARY" >&2; exit 2; }

CODEX_ONLY="(only the exact api.openai.com, chatgpt.com and auth.openai.com are permitted)"
codex_out=$(check_codex_egress "$CODEX_CANARY") && codex_rc=0 || codex_rc=$?
codex_ok=0
if [ "$codex_rc" -eq 1 ] \
   && [ "$(printf '%s\n' "$codex_out" | grep -c '^FAIL: ')" -eq 6 ] \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress chatgpt.com: ports are TCP/80, want exactly TCP/443" \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress chatgpt.com: 2 Allow entries, want exactly one" \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress *.openai.com: unexpected OpenAI/ChatGPT Allow $CODEX_ONLY" \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress *.chatgpt.com: unexpected OpenAI/ChatGPT Allow $CODEX_ONLY" \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress *: wildcard Allow covers api.openai.com $CODEX_ONLY" \
   && printf '%s\n' "$codex_out" | is_present "FAIL: Codex worker egress auth.openai.com: no Allow entry"; then
  codex_ok=1
  for _absent in "api.openai.com:" "*.anthropic.com" "evil.chatgpt.com" "platform.openai.com"; do
    if printf '%s\n' "$codex_out" | grep -F -q -- "$_absent"; then codex_ok=0; fi
  done
fi
if [ "$codex_ok" -eq 1 ]; then
  echo "OK: Codex canary self-test named exactly its six findings (port, duplicate, two widenings, catch-all glob, missing host) and none of the out-of-scope entries"
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

# ---------------------------------------------------------------------------------
# The isolated lane (PRD #1906 M6).
#
# WHY THIS EXISTS. The lane's guarantee is "no content except through uzi-fetcher", and
# every way to break it renders clean: a fourth egress rule, the allowWebService or
# extraEgress knob copied in from worker-networkpolicy.yaml, a wildcard model host, an
# ipBlock on the lane, or a fetcher policy that forgets a cluster range and so lets the
# fetcher reach an internal address. So the CI render (deploy/values/ci-render.yaml turns
# the lane on) is checked for:
#   (a) the lane Namespace enforces Pod Security `restricted`;
#   (b) the lane NetworkPolicy selects every pod, denies all ingress, and has EXACTLY three
#       egress rules: the restricted tier's DNS rule, its api rule, and the same api rule
#       re-aimed at the fetcher's component and container port. No ipBlock, no web peer,
#       nothing appended (so neither allowWebService, on in ci-render, nor extraEgress);
#   (c) the lane's Antrea policy: every rule names a peer; its Allow rules are the floor's
#       three in-cluster rules (signature-equal) plus exactly one fqdn, api.anthropic.com,
#       on TCP/443; its ipBlock-only Drop belt holds 169.254.169.254/32, fd00:ec2::254/128,
#       every configured cluster range and every addrpolicy.go range (bar IPv4-mapped),
#       and precedes the fqdn Allow; it has no spec.ingress;
#   (d) uzi-fetcher's policy admits ingress from the lane's worker pods (and the api's
#       probe CIDRs) on the fetcher port only, every rule with a `from` peer; its egress
#       rules all name a peer and are DNS, the api, and TCP/443 to 0.0.0.0/0 and 2000::/3
#       whose except lists hold every range api/internal/fetcher/addrpolicy.go refuses
#       (read from that file; IPv6 ones only when inside 2000::/3) AND every CIDR the
#       fetcher itself is told to refuse (UZI_FETCHER_BLOCKED_CIDRS); and by address
#       arithmetic 169.254.169.254, fd00:ec2::254 and every one of those ranges fall
#       outside every allowed ipBlock while two public addresses fall inside;
#   (e) the api admits the lane's worker pods and the fetcher pods on its worker port;
#   (f) the controller and the api carry the lane's env;
#   (g) the lane namespace holds exactly the lane's two policy objects (by apiVersion,
#       kind and name) and the render has no cluster-scoped network policy: policies are
#       additive, so any other one could open what (b)-(c) close;
#   (h) wiring: the controller's fetcher URL host is on the fetcher certificate; the
#       fetcher's env names are ones api/internal/fetcher/config.go reads (or GOMEMLIMIT),
#       the required ones present, GOMEMLIMIT a byte count within 80-100% of the
#       container's memory limit; the token-hash and token-file Secret keys exist; and every file-path
#       env resolves through a volumeMount to the right key of the right Secret.
#
# PARSED, NOT GREPPED. `flatten` turns the block-style YAML helm emits into one
# `doc<TAB>path<TAB>value` line per scalar (path like
# `spec.egress[0].to[0].podSelector.matchLabels.app.kubernetes.io/component`), in POSIX
# awk like the rest of this script. Two policies are compared by their rule SIGNATURES:
# the sorted `relative-path=value` lines under one rule.
#
# EXIT CODES as above: 2 = a lane object the CI render must carry is absent, or the
# address-arithmetic self-test failed; 1 = a property does not hold.

flatten() {
  awk '
    function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    function unq(s) {
      s = trim(s)
      if (length(s) >= 2 && ((substr(s, 1, 1) == "\"" && substr(s, length(s), 1) == "\"") || (substr(s, 1, 1) == "\047" && substr(s, length(s), 1) == "\047")))
        s = substr(s, 2, length(s) - 2)
      return s
    }
    function emit(p, v) { print doc "\t" p "\t" unq(v) }
    function reset() { sp = 0; P[0] = ""; I[0] = 0; L[0] = 0; N[0] = 0; K[0] = -1 }
    function join(a, b) { return a == "" ? b : a "." b }
    # A key with no value opens a container whose shape the NEXT line decides.
    function pend(p, ind) { sp++; P[sp] = p; I[sp] = -1; L[sp] = 0; N[sp] = 0; K[sp] = ind }
    function handle(ind, body,   isitem, rest, key, v, ip) {
      isitem = (body ~ /^-( |$)/)
      if (I[sp] == -1) {
        if (ind > K[sp] || (isitem && ind == K[sp])) { I[sp] = ind; L[sp] = isitem }
        else { emit(P[sp], ""); sp-- }
      }
      while (sp > 0 && (ind < I[sp] || (ind == I[sp] && L[sp] && !isitem))) sp--
      if (isitem && L[sp]) {
        ip = P[sp] "[" N[sp] "]"; N[sp]++
        match(body, /^-[ ]*/); rest = substr(body, RLENGTH + 1)
        if (rest == "") { pend(ip, ind); return }
        if (rest ~ /^[^ "\047][^:]*:( |$)/) {
          sp++; P[sp] = ip; I[sp] = ind + RLENGTH; L[sp] = 0; N[sp] = 0; K[sp] = ind
          handle(ind + RLENGTH, rest)
          return
        }
        emit(ip, rest)
        return
      }
      if (!match(body, /:( |$)/)) return
      key = substr(body, 1, RSTART - 1)
      v = trim(substr(body, RSTART + 1))
      if (v == "") pend(join(P[sp], key), ind)
      else emit(join(P[sp], key), v)
    }
    BEGIN { doc = 1; reset() }
    /^---[[:space:]]*$/ { if (I[sp] == -1) emit(P[sp], ""); doc++; reset(); next }
    /^[[:space:]]*(#.*)?$/ { next }
    { match($0, /^ */); handle(RLENGTH, substr($0, RLENGTH + 1)) }
  ' "$1"
}

# docs_of <flat> <kind> <name> [apiVersion]: the doc numbers of matching objects.
docs_of() {
  awk -F '\t' -v k="$2" -v n="$3" -v a="${4:-}" '
    $2 == "kind" && $3 == k { K[$1] = 1 }
    $2 == "metadata.name" && $3 == n { N[$1] = 1 }
    $2 == "apiVersion" { A[$1] = $3 }
    END { for (d in K) if ((d in N) && (a == "" || A[d] == a)) print d }
  ' "$1"
}

# field <flat> <doc> <path>: the scalar at an exact path.
field() { awk -F '\t' -v d="$2" -v p="$3" '$1 == d && $2 == p { print $3; exit }' "$1"; }

# count_items <flat> <doc> <list-path>: how many items a list at that path holds.
count_items() {
  awk -F '\t' -v d="$2" -v p="$3" '
    $1 == d && index($2, p "[") == 1 { r = substr($2, length(p) + 2); sub(/].*$/, "", r); S[r] = 1 }
    END { n = 0; for (i in S) n++; print n }
  ' "$1"
}

# sig <flat> <doc> <prefix>: the rule signature, sorted `relpath=value` lines joined by `;`.
sig() {
  awk -F '\t' -v d="$2" -v p="$3." '$1 == d && index($2, p) == 1 { print substr($2, length(p) + 1) "=" $3 }' "$1" \
    | LC_ALL=C sort | tr '\n' ';'
}

# values_under <flat> <doc> <prefix> <leaf>: every value whose path starts with prefix and
# ends with `.leaf` (or `leaf[N]` for a scalar list), one per line.
values_under() {
  awk -F '\t' -v d="$2" -v p="$3" -v l="$4" '
    $1 != d || index($2, p) != 1 { next }
    { t = $2; sub(/\[[0-9]+\]$/, "", t) }
    length(t) >= length(l) && substr(t, length(t) - length(l) + 1) == l { print $3 }
  ' "$1"
}

# in_list <word> <list>: exact membership in a whitespace-separated list.
# shellcheck disable=SC2086 # the list is split on purpose; pathname expansion is off (set -f)
in_list() { printf '%s\n' $2 | awk -v w="$1" '$0 == w { f = 1 } END { exit !f }'; }

# cidr_allowed <addr> <rules>: succeed iff addr (v4 or v6) falls inside some rule's cidr
# and outside all of that rule's excepts. <rules> is one line per ipBlock:
# `cidr except1 except2 ...`. Pure POSIX awk: v4 as one number, v6 as eight hextets.
cidr_allowed() {
  printf '%s\n' "$2" | awk -v addr="$1" '
    function hex(s,   i, c, n) {
      n = 0; s = tolower(s)
      for (i = 1; i <= length(s); i++) { c = index("0123456789abcdef", substr(s, i, 1)); if (!c) return -1; n = n * 16 + c - 1 }
      return n
    }
    # parse6 <a> <out>: eight hextets into out[1..8]; returns 0 when malformed.
    function parse6(a, out,   h, t, nh, nt, i, dc, x) {
      dc = index(a, "::")
      if (dc) { nh = (dc > 1) ? split(substr(a, 1, dc - 1), h, ":") : 0; x = substr(a, dc + 2); nt = (x != "") ? split(x, t, ":") : 0 }
      else { nh = split(a, h, ":"); nt = 0; if (nh != 8) return 0 }
      if (nh + nt > 8) return 0
      for (i = 1; i <= 8; i++) out[i] = 0
      for (i = 1; i <= nh; i++) { out[i] = hex(h[i]); if (out[i] < 0 || h[i] == "") return 0 }
      for (i = 1; i <= nt; i++) { out[8 - nt + i] = hex(t[i]); if (out[8 - nt + i] < 0 || t[i] == "") return 0 }
      return 1
    }
    function parse4(a,   o, n, i, v) {
      n = split(a, o, "."); if (n != 4) return -1
      v = 0; for (i = 1; i <= 4; i++) { if (o[i] !~ /^[0-9]+$/ || o[i] + 0 > 255) return -1; v = v * 256 + o[i] }
      return v
    }
    # inside <cidr>: whether the probe address lies in cidr (same family only).
    function inside(c,   p, net, bits, n6, full, rem, i, d) {
      p = index(c, "/"); if (!p) return 0
      net = substr(c, 1, p - 1); bits = substr(c, p + 1) + 0
      if (v6 != (index(net, ":") > 0)) return 0
      if (!v6) { n = parse4(net); if (n < 0) { bad = 1; return 0 }; d = 2 ^ (32 - bits); return int(A4 / d) == int(n / d) }
      if (!parse6(net, n6)) { bad = 1; return 0 }
      full = int(bits / 16); rem = bits % 16
      for (i = 1; i <= full; i++) if (A6[i] != n6[i]) return 0
      if (rem) { d = 2 ^ (16 - rem); if (int(A6[full + 1] / d) != int(n6[full + 1] / d)) return 0 }
      return 1
    }
    BEGIN {
      v6 = (index(addr, ":") > 0)
      if (v6) { if (!parse6(addr, A6)) bad = 1 } else { A4 = parse4(addr); if (A4 < 0) bad = 1 }
    }
    NF >= 1 {
      if (!inside($1)) next
      hit = 1
      for (i = 2; i <= NF; i++) if (inside($i)) hit = 0
      if (hit) allowed = 1
    }
    END { if (bad) exit 3; exit !allowed }
  '
}

# Self-test of the arithmetic before it is trusted: a wrong answer here is BROKEN (2).
_rules="0.0.0.0/0 10.0.0.0/8 169.254.0.0/16 192.0.2.128/25
::/0 fc00::/7 fe80::/10 2001:db8:ab00::/40"
for _probe in "8.8.8.8 0" "10.1.2.3 1" "169.254.169.254 1" "192.0.2.127 0" "192.0.2.128 1" \
              "2606:4700::1111 0" "fd00:ec2::254 1" "fe80::1 1" "2001:db8:abff::1 1" "2001:db8:ac00::1 0" "::1 0"; do
  # shellcheck disable=SC2086 # split the "address want" pair on purpose
  set -- $_probe
  _rc=0; cidr_allowed "$1" "$_rules" || _rc=$?
  _want_blocked="$2"
  if { [ "$_want_blocked" = 1 ] && [ "$_rc" -ne 1 ]; } || { [ "$_want_blocked" = 0 ] && [ "$_rc" -ne 0 ]; }; then
    echo "BROKEN: the ipBlock arithmetic self-test got rc=$_rc for $1 (want blocked=$_want_blocked)" >&2
    exit 2
  fi
done
echo "OK: ipBlock arithmetic self-test (v4 and v6, in and out of cidr and except)"

check_isolated_lane() {
  _render="$1"
  _flat=$(mktemp)
  flatten "$_render" > "$_flat"
  _bad=0
  _fail() { echo "FAIL: isolated lane: $1"; _bad=1; }
  _one() { # _one <what> <docs>: exactly one doc, else BROKEN
    _n=$(printf '%s\n' "$2" | awk 'NF { n++ } END { print n + 0 }')
    if [ "$_n" -ne 1 ]; then echo "BROKEN: isolated lane: expected exactly one $1 in the render, found $_n" >&2; rm -f "$_flat"; return 2; fi
  }

  _ns_doc=$(awk -F '\t' '$2 == "kind" && $3 == "Namespace" { K[$1] = 1 } $2 == "metadata.labels.app.kubernetes.io/component" && $3 == "worker-isolated" { C[$1] = 1 } END { for (d in K) if (d in C) print d }' "$_flat")
  _one "lane Namespace" "$_ns_doc" || return 2
  _lane_ns=$(field "$_flat" "$_ns_doc" metadata.name)
  _np=$(docs_of "$_flat" NetworkPolicy uzi-worker-isolated-default-deny networking.k8s.io/v1); _one "lane NetworkPolicy" "$_np" || return 2
  _ref=$(docs_of "$_flat" NetworkPolicy uzi-worker-default-deny networking.k8s.io/v1); _one "restricted worker NetworkPolicy" "$_ref" || return 2
  _fnp=$(docs_of "$_flat" NetworkPolicy uzi-fetcher networking.k8s.io/v1); _one "fetcher NetworkPolicy" "$_fnp" || return 2
  _fdep=$(docs_of "$_flat" Deployment uzi-fetcher); _one "fetcher Deployment" "$_fdep" || return 2
  _anp=$(docs_of "$_flat" NetworkPolicy uzi-api networking.k8s.io/v1); _one "api NetworkPolicy" "$_anp" || return 2
  _adep=$(docs_of "$_flat" Deployment uzi-api); _one "api Deployment" "$_adep" || return 2
  _cdep=$(docs_of "$_flat" Deployment uzi-controller); _one "controller Deployment" "$_cdep" || return 2
  _model=$(docs_of "$_flat" NetworkPolicy uzi-worker-isolated-model-egress crd.antrea.io/v1beta1); _one "lane Antrea model-egress policy" "$_model" || return 2

  _fport=$(field "$_flat" "$_fdep" "spec.template.spec.containers[0].ports[0].containerPort")
  _aport=$(awk -F '\t' -v d="$_adep" '$1 == d && $2 ~ /^spec\.template\.spec\.containers\[0\]\.ports\[[0-9]+\]\.name$/ && $3 == "https" { p = $2; sub(/name$/, "containerPort", p); want = p } $1 == d && $2 == want { print $3; exit }' "$_flat")
  [ -n "$_fport" ] && [ -n "$_aport" ] || { echo "BROKEN: isolated lane: no fetcher or api https container port in the render" >&2; rm -f "$_flat"; return 2; }

  # (a) Pod Security.
  [ "$(field "$_flat" "$_ns_doc" 'metadata.labels.pod-security.kubernetes.io/enforce')" = restricted ] \
    || _fail "namespace $_lane_ns does not enforce Pod Security restricted"

  # (b) the lane floor.
  [ "$(field "$_flat" "$_np" metadata.namespace)" = "$_lane_ns" ] || _fail "the default-deny policy is not in $_lane_ns"
  [ "$(field "$_flat" "$_np" spec.podSelector)" = "{}" ] || _fail "the default-deny policy does not select every pod (podSelector is not {})"
  [ "$(field "$_flat" "$_np" spec.ingress)" = "[]" ] || _fail "the default-deny policy admits ingress"
  _types=$(values_under "$_flat" "$_np" spec.policyTypes policyTypes | LC_ALL=C sort | tr '\n' ' ')
  [ "$_types" = "Egress Ingress " ] || _fail "policyTypes are '$_types', want Egress and Ingress"
  _n=$(count_items "$_flat" "$_np" spec.egress)
  [ "$_n" = 3 ] || _fail "the lane policy has $_n egress rules, want exactly 3 (DNS, api, fetcher): an appended rule (allowWebService, extraEgress) is a path around the fetcher"
  if awk -F '\t' -v d="$_np" '$1 == d && index($2, "spec.egress") == 1 && ($2 ~ /ipBlock/ || $3 == "web") { f = 1 } END { exit !f }' "$_flat"; then
    _fail "the lane policy carries an ipBlock or a web peer"
  fi
  _dns=$(sig "$_flat" "$_ref" "spec.egress[0]")
  _api=$(sig "$_flat" "$_ref" "spec.egress[1]")
  case "$_api" in *"app.kubernetes.io/component=api;"*) ;; *) echo "BROKEN: isolated lane: the restricted policy's egress[1] is not its api rule: $_api" >&2; rm -f "$_flat"; return 2 ;; esac
  _fet=$(printf '%s' "$_api" | sed "s#app.kubernetes.io/component=api;#app.kubernetes.io/component=fetcher;#; s#ports\\[0\\]\\.port=$_aport;#ports[0].port=$_fport;#")
  _want=$(printf '%s\n%s\n%s\n' "$_dns" "$_api" "$_fet" | LC_ALL=C sort)
  _got=$(i=0; while [ "$i" -lt "$_n" ]; do sig "$_flat" "$_np" "spec.egress[$i]"; echo; i=$((i + 1)); done | LC_ALL=C sort)
  [ "$_got" = "$_want" ] || _fail "the lane policy's egress rules are not exactly {restricted DNS rule, restricted api rule, the api rule aimed at the fetcher on $_fport}:
--- want
$_want
--- got
$_got"

  # (c) the model host. The Antrea policy's rules, in order: Allow rules for the floor's
  # three in-cluster peers (signature-equal to the floor's, so they admit nothing it does
  # not), a Drop belt, then the one FQDN Allow. Every rule names a peer (a rule without
  # `to` matches every destination), every Drop is ipBlock-only with no port narrowing,
  # the belt holds the metadata addresses, every configured cluster range and every range
  # api/internal/fetcher/addrpolicy.go refuses (so every uzi.isolatedLaneExceptV4 entry, the
  # set the OVN branch excepts, and the non-global IPv6 ranges), and no Drop follows the
  # FQDN Allow (a DNS answer for the model host must not reach a dropped address through
  # it). The policy carries no ingress: an Antrea Allow ingress rule would jump past the
  # floor's `ingress: []` and admit whatever it names into every lane pod.
  _policy_go="$SCRIPT_DIR/../api/internal/fetcher/addrpolicy.go"
  _fixed=$(awk '/^var blockedPrefixes = mustPrefixes\(/ { f = 1; next } f && /^\)/ { f = 0 } f && match($0, /"[^"]+"/) { print substr($0, RSTART + 1, RLENGTH - 2) }' "$_policy_go" 2>/dev/null | tr '\n' ' ') || true
  if [ "$(printf '%s' "$_fixed" | wc -w)" -lt 20 ]; then
    echo "BROKEN: isolated lane: could not read blockedPrefixes from $_policy_go (got: $_fixed)" >&2; rm -f "$_flat"; return 2
  fi
  _blocked=$(awk -F '\t' -v d="$_fdep" '$1 == d && $3 == "UZI_FETCHER_BLOCKED_CIDRS" { p = $2; sub(/name$/, "value", p); want = p } $1 == d && $2 == want { print $3; exit }' "$_flat" | tr ',' ' ')
  [ -n "$_blocked" ] || _fail "the fetcher Deployment carries no UZI_FETCHER_BLOCKED_CIDRS"
  [ "$(field "$_flat" "$_model" metadata.namespace)" = "$_lane_ns" ] || _fail "the model-egress policy is not in $_lane_ns"
  if awk -F '\t' -v d="$_model" '$1 == d && index($2, "spec.ingress") == 1 { f = 1 } END { exit !f }' "$_flat"; then
    _fail "the lane's Antrea policy has a spec.ingress: it is egress-only, and an Antrea ingress Allow skips the floor's ingress: []"
  fi
  _mn=$(count_items "$_flat" "$_model" spec.egress)
  _in_allow=""; _fqdn_at=""; _fqdn_n=0; _last_drop=-1; _drops=" "
  i=0
  while [ "$i" -lt "$_mn" ]; do
    _core=$(sig "$_flat" "$_model" "spec.egress[$i]" | tr ';' '\n' | awk 'NF && !/^(name|action)=/' | tr '\n' ';')
    _act=$(field "$_flat" "$_model" "spec.egress[$i].action")
    if ! printf '%s' "$_core" | tr ';' '\n' | grep -q '^to\['; then
      _fail "the lane's Antrea rule $i ($_act) has no \`to\` peer, so it matches every destination: $_core"
    fi
    case "$_act" in
      Drop)
        _last_drop=$i
        _extra=$(printf '%s' "$_core" | tr ';' '\n' | awk 'NF && !/^to\[[0-9]+\]\.ipBlock\.cidr=/')
        [ -z "$_extra" ] || _fail "the lane's Antrea Drop rule $i is not ipBlock-only (a port or selector narrows the belt): $_core"
        _drops="$_drops$(values_under "$_flat" "$_model" "spec.egress[$i].to" ipBlock.cidr | tr '\n' ' ')" ;;
      Allow)
        if [ -n "$(values_under "$_flat" "$_model" "spec.egress[$i].to" fqdn)" ]; then
          _fqdn_at=$i; _fqdn_n=$((_fqdn_n + 1))
          [ "$_core" = "ports[0].port=443;ports[0].protocol=TCP;to[0].fqdn=api.anthropic.com;" ] \
            || _fail "the lane's Antrea FQDN rule $i must allow exactly api.anthropic.com on TCP/443: $_core"
        else
          _in_allow="$_in_allow$_core
"
        fi ;;
      *) _fail "the lane's Antrea rule $i has action '$_act', want Allow or Drop" ;;
    esac
    i=$((i + 1))
  done
  [ "$_fqdn_n" = 1 ] || _fail "the lane's Antrea policy has $_fqdn_n FQDN Allow rules, want exactly 1 (api.anthropic.com)"
  [ "$_last_drop" -lt "${_fqdn_at:-0}" ] || _fail "the lane's Antrea Drop rule $_last_drop follows the FQDN Allow (rule ${_fqdn_at:-none}), so the model host's DNS answer can open a dropped address"
  _in_sorted=$(printf '%s' "$_in_allow" | awk NF | LC_ALL=C sort)
  [ "$_in_sorted" = "$_got" ] || _fail "the lane's Antrea in-cluster Allow rules are not exactly the floor's three (an Antrea Allow skips the floor, so any difference widens the lane):
--- want (the floor's)
$_got
--- got
$_in_sorted"
  for _c in 169.254.169.254/32 fd00:ec2::254/128 $_blocked; do
    in_list "$_c" "$_drops" || _fail "the lane's Antrea drop belt does not drop $_c"
  done
  # Every addrpolicy.go range but ::ffff:0:0/96 (IPv4-mapped: a socket dialling one sends an
  # IPv4 packet, which the IPv4 entries drop; see uzi.isolatedLaneAntreaDropCIDRs).
  for _c in $_fixed; do
    [ "$_c" = "::ffff:0:0/96" ] && continue
    in_list "$_c" "$_drops" || _fail "the lane's Antrea drop belt does not drop $_c, which uzi-fetcher refuses (addrpolicy.go; the OVN branch excepts every IPv4 one via uzi.isolatedLaneExceptV4)"
  done

  # (d) the fetcher's own policy.
  _probe=$(values_under "$_flat" "$_anp" spec.ingress ipBlock.cidr | tr '\n' ' ')
  _in=$(awk -F '\t' -v d="$_fnp" '
    $1 != d || index($2, "spec.ingress[") != 1 { next }
    { r = $2; sub(/^spec\.ingress\[/, "", r); sub(/].*$/, "", r); R[r] = 1 }
    $2 ~ /\.ports\[[0-9]+\]\.(protocol|port)$/ { P[r] = P[r] $3 "/" }
    $2 ~ /\.from\[[0-9]+\]\./ {
      f = $2; sub(/^spec\.ingress\[[0-9]+\]\.from\[/, "", f); sub(/].*$/, "", f)
      t = $2; sub(/^spec\.ingress\[[0-9]+\]\.from\[[0-9]+\]\./, "", t)
      k = r SUBSEP f; E[k] = E[k] t "=" $3 ";"; F[k] = r; H[r] = 1
    }
    END {
      for (k in F) print "peer " E[k]
      for (r in R) { print "ports " P[r]; if (!(r in H)) print "nopeer " r }
    }
  ' "$_flat")
  _lane_peer="namespaceSelector.matchLabels.kubernetes.io/metadata.name=$_lane_ns;podSelector.matchLabels.app.kubernetes.io/name=uzi-hosted-worker;"
  _lane_seen=0
  printf '%s\n' "$_in" | while IFS= read -r _l; do
    case "$_l" in
      "ports TCP/$_fport/") ;;
      "ports "*) echo "FAIL: isolated lane: a fetcher ingress rule's ports are '${_l#ports }', want exactly TCP/$_fport" ;;
      "peer $_lane_peer") ;;
      "nopeer "*) echo "FAIL: isolated lane: fetcher ingress rule ${_l#nopeer } has no \`from\` peer, so it admits every source" ;;
      "peer ipBlock.cidr="*";") _c=${_l#peer ipBlock.cidr=}; _c=${_c%;}
        in_list "$_c" "$_probe" || echo "FAIL: isolated lane: fetcher ingress admits ipBlock $_c, which is not one of the api's probe CIDRs ($_probe)" ;;
      *) echo "FAIL: isolated lane: fetcher ingress admits an unexpected peer: ${_l#peer }" ;;
    esac
  done > "$_flat.in"
  if [ -s "$_flat.in" ]; then cat "$_flat.in"; _bad=1; fi
  case "$_in" in *"peer $_lane_peer"*) _lane_seen=1 ;; esac
  [ "$_lane_seen" = 1 ] || _fail "fetcher ingress has no rule for the lane's worker pods ($_lane_peer)"

  _ne=$(count_items "$_flat" "$_fnp" spec.egress)
  _dns_seen=0; _api_seen=0; _ip_seen=0
  _api_want=$( { sig "$_flat" "$_fnp" spec.podSelector | tr ';' '\n' | awk 'NF { print "to[0].podSelector." $0 }' | sed 's#component=fetcher$#component=api#'
                printf 'ports[0].port=%s\nports[0].protocol=TCP\n' "$_aport"; } | LC_ALL=C sort | tr '\n' ';')
  _rules=""
  i=0
  while [ "$i" -lt "$_ne" ]; do
    _s=$(sig "$_flat" "$_fnp" "spec.egress[$i]")
    if ! printf '%s' "$_s" | tr ';' '\n' | grep -q '^to\['; then
      _fail "fetcher egress rule $i has no \`to\` peer, so it admits every destination: $_s"
    elif [ "$_s" = "$_dns" ]; then _dns_seen=1
    elif [ "$_s" = "$_api_want" ]; then _api_seen=1
    else
      _pp=$(printf '%s' "$_s" | tr ';' '\n' | awk -F= '/^ports\[/ { print }' | tr '\n' ';')
      _peers=$(printf '%s' "$_s" | tr ';' '\n' | awk '/^to\[/ { t = $0; sub(/^to\[[0-9]+\]\./, "", t); if (t !~ /^ipBlock\.(cidr|except\[[0-9]+\])=/) print "x" }')
      if [ -n "$_peers" ] || [ "$_pp" != "ports[0].port=443;ports[0].protocol=TCP;" ]; then
        _fail "fetcher egress rule $i is neither DNS, the api, nor an ipBlock-only TCP/443 rule: $_s"
      else
        _ip_seen=1
        _rules="$_rules$(awk -F '\t' -v d="$_fnp" -v p="spec.egress[$i].to[" '
          $1 != d || index($2, p) != 1 { next }
          { t = substr($2, length(p) + 1); b = t; sub(/].*$/, "", b); sub(/^[0-9]+\]\./, "", t) }
          t == "ipBlock.cidr" { C[b] = $3 } t ~ /^ipBlock\.except\[/ { X[b] = X[b] " " $3 }
          END { for (b in C) print C[b] X[b] }
        ' "$_flat")
"
      fi
    fi
    i=$((i + 1))
  done
  [ "$_dns_seen" = 1 ] || _fail "fetcher egress has no rule equal to the restricted tier's DNS rule"
  [ "$_api_seen" = 1 ] || _fail "fetcher egress has no rule for the api on TCP/$_aport (want: $_api_want)"
  [ "$_ip_seen" = 1 ] || _fail "fetcher egress has no internet (ipBlock, TCP/443) rule"
  _cidrs=$(printf '%s' "$_rules" | awk 'NF { print $1 }' | LC_ALL=C sort | tr '\n' ' ')
  [ "$_cidrs" = "0.0.0.0/0 2000::/3 " ] || _fail "fetcher internet ipBlocks are '$_cidrs', want exactly 0.0.0.0/0 and 2000::/3 (the fetcher's own check admits IPv6 only in 2000::/3)"
  _x4=$(printf '%s' "$_rules" | awk '$1 == "0.0.0.0/0" { for (i = 2; i <= NF; i++) print $i }' | tr '\n' ' ')
  _x6=$(printf '%s' "$_rules" | awk '$1 == "2000::/3" { for (i = 2; i <= NF; i++) print $i }' | tr '\n' ' ')
  # The fixed ranges come from the fetcher's own address policy, so the chart cannot drift
  # from the code: every IPv4 range, and every IPv6 range inside 2000::/3, must be an
  # except by name; every range (IPv6 outside 2000::/3 included) must fall outside the
  # allow by arithmetic. The configured cluster ranges get the same treatment.
  for _c in $_fixed $_blocked; do
    _a=${_c%/*}
    case "$_c" in
      *:*) _rc=0; cidr_allowed "$_a" "2000::/3" || _rc=$?
           case "$_rc" in
             0) in_list "$_c" "$_x6" || _fail "fetcher egress 2000::/3 does not except $_c (addrpolicy.go or clusterCIDRs)" ;;
             1) ;;
             *) echo "BROKEN: isolated lane: cannot evaluate $_c against 2000::/3" >&2; rm -f "$_flat" "$_flat.in"; return 2 ;;
           esac ;;
      *) in_list "$_c" "$_x4" || _fail "fetcher egress 0.0.0.0/0 does not except $_c (addrpolicy.go or clusterCIDRs)" ;;
    esac
  done
  for _a in 169.254.169.254 fd00:ec2::254 $(for _c in $_fixed $_blocked; do printf '%s ' "${_c%/*}"; done); do
    _rc=0; cidr_allowed "$_a" "$_rules" || _rc=$?
    case "$_rc" in
      0) _fail "fetcher egress admits $_a through an ipBlock" ;;
      1) ;;
      *) echo "BROKEN: isolated lane: cannot evaluate $_a against the fetcher's ipBlocks" >&2; rm -f "$_flat" "$_flat.in"; return 2 ;;
    esac
  done
  for _a in 93.184.215.14 2606:4700:4700::1111; do
    cidr_allowed "$_a" "$_rules" || _fail "fetcher egress does not admit the public address $_a (the internet rule is vacuous)"
  done

  # (e) the api's ingress.
  _api_in=$(awk -F '\t' -v d="$_anp" '
    $1 != d || index($2, "spec.ingress[") != 1 { next }
    { r = $2; sub(/^spec\.ingress\[/, "", r); sub(/].*$/, "", r); t = $2; sub(/^spec\.ingress\[[0-9]+\]\./, "", t); S[r] = S[r] t "=" $3 ";" }
    END { for (r in S) print S[r] }
  ' "$_flat")
  _want_lane="from[0].namespaceSelector.matchLabels.kubernetes.io/metadata.name=$_lane_ns;from[0].podSelector.matchLabels.app.kubernetes.io/name=uzi-hosted-worker;ports[0].protocol=TCP;ports[0].port=$_aport;"
  printf '%s\n' "$_api_in" | awk -v w="$_want_lane" '$0 == w { f = 1 } END { exit !f }' \
    || _fail "the api NetworkPolicy has no ingress rule for $_lane_ns's worker pods on TCP/$_aport alone"
  printf '%s\n' "$_api_in" | awk -v p="$_aport" '
    index($0, "from[0].podSelector.matchLabels.app.kubernetes.io/component=fetcher;") && $0 !~ /namespaceSelector/ && $0 !~ /from\[1\]/ && index($0, "ports[0].protocol=TCP;ports[0].port=" p ";") && $0 !~ /ports\[1\]/ { f = 1 }
    END { exit !f }' || _fail "the api NetworkPolicy has no ingress rule for the fetcher pods on TCP/$_aport alone"

  # (f) env.
  _env() { awk -F '\t' -v d="$1" -v n="$2" '$1 == d && $3 == n && $2 ~ /\.env\[[0-9]+\]\.name$/ { p = $2; sub(/name$/, "value", p); want = p; v = p; sub(/value$/, "valueFrom.secretKeyRef.key", v); wantk = v } $1 == d && ($2 == want || $2 == wantk) { print $3; exit }' "$_flat"; }
  [ "$(_env "$_cdep" UZI_WORKER_ISOLATED_NAMESPACE)" = "$_lane_ns" ] || _fail "the controller's UZI_WORKER_ISOLATED_NAMESPACE is not $_lane_ns"
  case "$(_env "$_cdep" UZI_WORKER_ISOLATED_FETCHER_URL)" in "https://uzi-fetcher."*":$_fport") ;; *) _fail "the controller's UZI_WORKER_ISOLATED_FETCHER_URL is not https://uzi-fetcher.<ns>...:$_fport" ;; esac
  [ -n "$(_env "$_cdep" UZI_WORKER_ISOLATED_FETCHER_CA_FILE)" ] || _fail "the controller carries no UZI_WORKER_ISOLATED_FETCHER_CA_FILE"
  [ -n "$(_env "$_adep" UZI_FETCHER_TOKEN_SHA256)" ] || _fail "the api carries no UZI_FETCHER_TOKEN_SHA256"

  # (g) the lane namespace holds exactly the lane's two policy objects, and the render
  # carries no cluster-scoped network policy. A second policy there is additive (an
  # `egress: [{}]` NetworkPolicy or an Antrea Allow 0.0.0.0/0 opens the lane whatever the
  # floor says), so the inventory is by apiVersion, kind and name. A "policy object" is a
  # kind ending in Policy or Firewall, or anything in a network-policy API group.
  _inv=$(awk -F '\t' -v ns="$_lane_ns" '
    $2 == "kind" { K[$1] = $3 } $2 == "apiVersion" { A[$1] = $3 } $2 == "metadata.name" { N[$1] = $3 } $2 == "metadata.namespace" { S[$1] = $3 }
    END {
      for (d in K) {
        g = A[d]; if (index(g, "/")) sub(/\/[^\/]*$/, "", g); else g = ""
        pol = (K[d] ~ /(Policy|Firewall)$/ || g ~ /^(networking\.k8s\.io|policy\.networking\.k8s\.io|crd\.antrea\.io|k8s\.ovn\.org|cilium\.io|crd\.projectcalico\.org|projectcalico\.org)$/)
        clus = (K[d] ~ /^(ClusterNetworkPolicy|AdminNetworkPolicy|BaselineAdminNetworkPolicy|GlobalNetworkPolicy|CiliumClusterwideNetworkPolicy)$/)
        if (clus) print A[d] " " K[d] " " N[d] " (cluster-scoped)"
        else if (pol && S[d] == ns) print A[d] " " K[d] " " N[d]
      }
    }' "$_flat" | LC_ALL=C sort)
  _inv_want="crd.antrea.io/v1beta1 NetworkPolicy uzi-worker-isolated-model-egress
networking.k8s.io/v1 NetworkPolicy uzi-worker-isolated-default-deny"
  [ "$_inv" = "$_inv_want" ] || _fail "the policy objects in $_lane_ns (plus any cluster-scoped network policy) are not exactly the lane's two:
--- want
$_inv_want
--- got
$_inv"

  # (h) wiring: each name one component writes must be one the other reads.
  #   (i) the controller's fetcher URL names a host on the fetcher's certificate and the
  #       fetcher Service's port;
  #  (ii) the fetcher's env names are all ones api/internal/fetcher/config.go reads (a
  #       typo is silently ignored there), with the required ones present, no envFrom;
  # (iii) the Secret keys the api (token hash) and the fetcher (token file) reference exist
  #       in the Secret the chart renders (or one a rendered InfisicalSecret manages);
  #  (iv) every file-path env (api CA, serving pair, token, and the controller's fetcher CA)
  #       resolves through a volumeMount to the right key of the right Secret.
  _cert=$(docs_of "$_flat" Certificate uzi-fetcher-tls); _one "fetcher Certificate" "$_cert" || return 2
  _svc=$(docs_of "$_flat" Service uzi-fetcher); _one "fetcher Service" "$_svc" || return 2
  _url=$(_env "$_cdep" UZI_WORKER_ISOLATED_FETCHER_URL)
  _host=${_url#https://}; _host=${_host%%/*}; _uport=${_host##*:}; _host=${_host%:*}
  _dnsn=$(values_under "$_flat" "$_cert" spec.dnsNames dnsNames | tr '\n' ' ')
  in_list "$_host" "$_dnsn" || _fail "the controller's fetcher URL host $_host is not among the fetcher Certificate's dnsNames ($_dnsn): lane workers would fail TLS name verification"
  [ "$_uport" = "$(field "$_flat" "$_svc" 'spec.ports[0].port')" ] || _fail "the controller's fetcher URL port $_uport is not the fetcher Service's port"

  _config_go="$SCRIPT_DIR/../api/internal/fetcher/config.go"
  _known=$(grep -o 'get("UZI_[A-Z0-9_]*")' "$_config_go" 2>/dev/null | sed 's/^get("//; s/")$//' | LC_ALL=C sort -u | tr '\n' ' ') || true
  if [ "$(printf '%s' "$_known" | wc -w)" -lt 8 ]; then
    echo "BROKEN: isolated lane: could not read the env names $_config_go reads (got: $_known)" >&2; rm -f "$_flat" "$_flat.in"; return 2
  fi
  _fenv=$(awk -F '\t' -v d="$_fdep" '$1 == d && $2 ~ /^spec\.template\.spec\.containers\[0\]\.env\[[0-9]+\]\.name$/ { print $3 }' "$_flat")
  for _n in $_fenv; do
    # GOMEMLIMIT is the Go runtime's, not config.go's.
    in_list "$_n" "$_known GOMEMLIMIT" || _fail "the fetcher's env $_n is not read by api/internal/fetcher/config.go (it reads: $_known)"
  done
  for _n in UZI_FETCHER_TLS_CERT UZI_FETCHER_TLS_KEY UZI_API_URL UZI_FETCHER_TOKEN_FILE UZI_API_CA_FILE UZI_FETCHER_BLOCKED_CIDRS; do
    in_list "$_n" "$_fenv" || _fail "the fetcher's env lacks $_n"
  done
  _dup=$(printf '%s\n' "$_fenv" | LC_ALL=C sort | uniq -d | tr '\n' ' ')
  [ -z "$_dup" ] || _fail "the fetcher's env sets $_dup more than once (the last one wins)"
  if awk -F '\t' -v d="$_fdep" '$1 == d && index($2, "spec.template.spec.containers[0].envFrom") == 1 { f = 1 } END { exit !f }' "$_flat"; then
    _fail "the fetcher's container has envFrom, which injects env names this check cannot see"
  fi
  case "$(_env "$_fdep" UZI_API_URL)" in https://*) ;; *) _fail "the fetcher's UZI_API_URL is not https (the fetcher refuses to start)" ;; esac
  # GOMEMLIMIT: a plain byte count below the container's memory limit, so the Go GC holds the
  # heap under it rather than GOGC=100 growing it past the limit into an OOM kill.
  _gml=$(_env "$_fdep" GOMEMLIMIT)
  _mlim=$(field "$_flat" "$_fdep" 'spec.template.spec.containers[0].resources.limits.memory')
  _mlim_b=$(printf '%s\n' "$_mlim" | awk '
    match($0, /^[0-9]+/) { n = substr($0, 1, RLENGTH); u = substr($0, RLENGTH + 1)
      f["" ] = 1; f["k"] = 1e3; f["M"] = 1e6; f["G"] = 1e9; f["T"] = 1e12
      f["Ki"] = 1024; f["Mi"] = 1048576; f["Gi"] = 1073741824; f["Ti"] = 1099511627776
      if (u in f) printf "%.0f\n", n * f[u] }')
  if [ -z "$_mlim_b" ]; then
    _fail "the fetcher container has no parseable memory limit ('$_mlim'), so nothing bounds GOMEMLIMIT"
  else
    case "$_gml" in
      ''|*[!0-9]*) _fail "the fetcher's GOMEMLIMIT is '$_gml', want a plain byte count below its memory limit ($_mlim)" ;;
      *) awk -v g="$_gml" -v l="$_mlim_b" 'BEGIN { exit !(g > 0 && g < l && g >= l * 0.8) }' \
           || _fail "the fetcher's GOMEMLIMIT $_gml is not within 80-100% of its memory limit $_mlim ($_mlim_b bytes)" ;;
    esac
  fi

  # _skey <secret> <key>: 0 = a rendered Secret carries the key, or a rendered cert-manager
  # Certificate writes that Secret and the key is one it writes (tls.crt, tls.key, ca.crt);
  # 1 = neither holds; 3 = not rendered, but a rendered InfisicalSecret manages it (its
  # keys are external); 2 = nothing in the render produces the Secret.
  _skey() {
    _sd=$(docs_of "$_flat" Secret "$1" | head -n 1)
    if [ -n "$_sd" ]; then
      awk -F '\t' -v d="$_sd" -v k="$2" '$1 == d && ($2 == "data." k || $2 == "stringData." k) { f = 1 } END { exit !f }' "$_flat" && return 0
      return 1
    fi
    if awk -F '\t' -v n="$1" '$2 == "kind" && $3 == "Certificate" { C[$1] = 1 } $2 == "spec.secretName" && $3 == n { S[$1] = 1 } END { for (d in S) if (d in C) f = 1; exit !f }' "$_flat"; then
      case "$2" in tls.crt|tls.key|ca.crt) return 0 ;; *) return 1 ;; esac
    fi
    awk -F '\t' -v n="$1" '$2 == "spec.managedSecretReference.secretName" && $3 == n { f = 1 } END { exit !f }' "$_flat" && return 3
    return 2
  }
  # _refcheck <what> <secret> <key>
  _refcheck() {
    _rc=0; _skey "$2" "$3" || _rc=$?
    case "$_rc" in
      0|3) ;;
      1) _fail "$1 references key $3 of Secret $2, which the rendered Secret does not carry" ;;
      *) _fail "$1 references Secret $2, which the chart neither renders nor manages through an InfisicalSecret" ;;
    esac
  }
  # _mounted <doc> <file>: "<secretName> <key>" of the Secret projection that puts <file>
  # into the doc's first container; empty when no volumeMount + secret volume does.
  _mounted() {
    awk -F '\t' -v d="$1" -v f="$2" '
      BEGIN { dir = f; sub(/\/[^\/]*$/, "", dir); base = f; sub(/^.*\//, "", base) }
      $1 != d { next }
      $2 ~ /^spec\.template\.spec\.containers\[0\]\.volumeMounts\[[0-9]+\]\.mountPath$/ && $3 == dir { m = $2; sub(/mountPath$/, "name", m); MP[m] = 1 }
      $2 ~ /^spec\.template\.spec\.containers\[0\]\.volumeMounts\[[0-9]+\]\.name$/ { MN[$2] = $3 }
      $2 ~ /^spec\.template\.spec\.volumes\[[0-9]+\]\.name$/ { v = $2; sub(/\.name$/, "", v); VN[$3] = v }
      $2 ~ /^spec\.template\.spec\.volumes\[[0-9]+\]\.secret\.secretName$/ { v = $2; sub(/\.secret\.secretName$/, "", v); SN[v] = $3 }
      $2 ~ /^spec\.template\.spec\.volumes\[[0-9]+\]\.secret\.items\[[0-9]+\]\.(key|path)$/ {
        v = $2; sub(/\.secret\.items.*$/, "", v); it = $2; sub(/\.(key|path)$/, "", it); HI[v] = 1; IV[it] = v
        if ($2 ~ /\.key$/) IK[it] = $3; else IP[it] = $3
      }
      END {
        for (m in MP) mv = MN[m]
        if (mv == "" || !(mv in VN)) exit
        v = VN[mv]; if (!(v in SN)) exit
        if (!(v in HI)) { print SN[v] " " base; exit }
        for (it in IP) if (IV[it] == v && IP[it] == base) { print SN[v] " " IK[it]; exit }
      }' "$_flat"
  }
  _certsecret=$(field "$_flat" "$_cert" spec.secretName)
  _apicert=$(docs_of "$_flat" Certificate uzi-api-tls | head -n 1)
  _apisecret=$([ -n "$_apicert" ] && field "$_flat" "$_apicert" spec.secretName)
  _hash_name=$(awk -F '\t' -v d="$_adep" '$1 == d && $3 == "UZI_FETCHER_TOKEN_SHA256" && $2 ~ /\.env\[[0-9]+\]\.name$/ { p = $2; sub(/name$/, "valueFrom.secretKeyRef.name", p); want = p } $1 == d && $2 == want { print $3; exit }' "$_flat")
  _hash_key=$(_env "$_adep" UZI_FETCHER_TOKEN_SHA256)
  [ -n "$_hash_name" ] && [ -n "$_hash_key" ] || _fail "the api's UZI_FETCHER_TOKEN_SHA256 is not a secretKeyRef with a name and a key"
  [ -z "$_hash_name" ] || _refcheck "the api's UZI_FETCHER_TOKEN_SHA256" "$_hash_name" "$_hash_key"
  # _filecheck <what> <doc> <env> <want-secret (empty = any)> <want-key (empty = any)>
  _filecheck() {
    _path=$(_env "$2" "$3")
    _m=$(_mounted "$2" "$_path")
    if [ -z "$_m" ]; then _fail "$1 $3=$_path is not a file any Secret volumeMount provides"; return 0; fi
    _ms=${_m% *}; _mk=${_m#* }
    [ -z "$4" ] || [ "$_ms" = "$4" ] || _fail "$1 $3=$_path comes from Secret $_ms, want $4"
    [ -z "$5" ] || [ "$_mk" = "$5" ] || _fail "$1 $3=$_path is key $_mk of Secret $_ms, want key $5"
    _refcheck "$1 $3" "$_ms" "$_mk"
  }
  _filecheck "the fetcher's" "$_fdep" UZI_API_CA_FILE "$_apisecret" ca.crt
  _filecheck "the fetcher's" "$_fdep" UZI_FETCHER_TLS_CERT "$_certsecret" tls.crt
  _filecheck "the fetcher's" "$_fdep" UZI_FETCHER_TLS_KEY "$_certsecret" tls.key
  _filecheck "the fetcher's" "$_fdep" UZI_FETCHER_TOKEN_FILE "$_hash_name" ""
  _filecheck "the controller's" "$_cdep" UZI_WORKER_ISOLATED_FETCHER_CA_FILE "$_certsecret" ca.crt

  rm -f "$_flat" "$_flat.in"
  if [ "$_bad" -ne 0 ]; then return 1; fi
  echo "OK: isolated lane -- $_lane_ns is restricted and default-deny with egress exactly {DNS, api, fetcher} and holds no other policy object; its Antrea policy re-allows only those three, carries no ingress, drops the metadata addresses, every cluster range and every range the fetcher refuses, then allows api.anthropic.com TCP/443; the fetcher's internet rule excludes every addrpolicy.go and configured range and every rule names a peer; the api admits the lane and the fetcher; the fetcher URL, env names, Secret keys and mounted files line up"
  return 0
}

check_isolated_lane "$RENDER" || exit $?

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
trap 'rm -rf "$WORK"' EXIT
trap 'exit 2' INT TERM

STRIPPED="$WORK/chart"
cp -R "$CHART_DIR" "$STRIPPED"
rm -f "$STRIPPED/Chart.lock"
# Drop any built subchart archives (CI runs `helm dependency build` first), so the
# defaults render the same offline chart everywhere, without the CNPG subchart.
rm -rf "$STRIPPED/charts"
awk '
  BEGIN { skip = 0 }
  /^dependencies:/ { skip = 1; next }        # drop the dependencies: list ...
  skip && /^[^[:space:]-]/ { skip = 0 }      # ... until the next top-level key
  skip { next }
  { print }
' "$CHART_DIR/Chart.yaml" > "$STRIPPED/Chart.yaml"

# The existing api.config path carries the requeue budget as a quoted string and
# injects it into the API through the SAME ConfigMap. Three independent offline
# renders cover the API default budget, a raised budget and disabled requeue.
# Render failures stop the loop; it has exactly three attempts, without retries.
for budget in 3 5 0; do
  REQUEUE_RENDER="$WORK/requeue-$budget.yaml"
  if ! helm template uzi "$STRIPPED" \
       --show-only templates/api-configmap.yaml \
       --show-only templates/api-deployment.yaml \
       --set api.tls.enabled=true \
       --set-string "api.config.RUN_MAX_REQUEUES=$budget" \
       > "$REQUEUE_RENDER" 2> "$WORK/err"; then
    echo "BROKEN: helm template of the API with RUN_MAX_REQUEUES=$budget failed:" >&2
    cat "$WORK/err" >&2
    exit 2
  fi
  awk -v budget="$budget" '
    /^---$/ { doc++; section = ""; api_container = 0; env_from = 0; ref = 0; next }
    /^[^[:space:]#]/ { section = $0 }
    /^apiVersion:/ { versions[doc]++; version[doc] = $0 }
    /^kind:/ { kinds[doc]++; kind[doc] = $0 }
    section == "metadata:" && /^  name:/ { names[doc]++; name[doc] = $0 }
    section == "data:" && /^  RUN_MAX_REQUEUES:/ {
      budgets[doc]++; value[doc] = $0
    }
    /^        - name:/ { api_container = ($0 == "        - name: api"); env_from = 0; ref = 0 }
    api_container && /^          [^[:space:]]/ {
      env_from = ($0 == "          envFrom:"); ref = 0
    }
    api_container && env_from && /^            - / {
      ref = ($0 == "            - configMapRef:")
      if (ref) refs[doc]++
    }
    api_container && env_from && ref && /^                name:/ {
      ref_names[doc]++; ref_name[doc] = $0
    }
    END {
      for (d = 0; d <= doc; d++) {
        if (!kinds[d]) continue
        if (kinds[d] != 1 || versions[d] != 1 || names[d] != 1) {
          print "BROKEN: API render must contain valid, unambiguous object identities"; exit 2
        }
        if (kind[d] == "kind: ConfigMap") {
          maps++
          if (version[d] != "apiVersion: v1" || name[d] != "  name: uzi-api-config") {
            print "FAIL: expected the exact API ConfigMap identity uzi-api-config"; exit 1
          }
          if (budgets[d] != 1 || value[d] != "  RUN_MAX_REQUEUES: \"" budget "\"") {
            print "FAIL: API ConfigMap must contain exactly one quoted RUN_MAX_REQUEUES=" budget; exit 1
          }
        } else if (kind[d] == "kind: Deployment") {
          deployments++
          if (version[d] != "apiVersion: apps/v1" || name[d] != "  name: uzi-api") {
            print "FAIL: expected the exact API Deployment identity uzi-api"; exit 1
          }
          if (refs[d] != 1 || ref_names[d] != 1 || ref_name[d] != "                name: uzi-api-config") {
            print "FAIL: API container envFrom must reference the SAME ConfigMap uzi-api-config"; exit 1
          }
        } else {
          print "BROKEN: unexpected object in the focused API render"; exit 2
        }
      }
      if (maps != 1 || deployments != 1) {
        print "BROKEN: expected exactly one API ConfigMap and one API Deployment"; exit 2
      }
    }
  ' "$REQUEUE_RENDER" || exit $?
done
echo "OK: RUN_MAX_REQUEUES budgets 3/5/0 are quoted exactly in the API ConfigMap and consumed by the API Deployment envFrom"

DEFAULT_OFF="$WORK/default-off.yaml"
# Issue #1982: the hosted-worker pin also tells healthsvc to expect a controller.
# Render only the API Deployment so an unrelated object's env cannot satisfy the
# check. Every render must succeed and contain a Deployment, including the negative
# case: missing/broken output must never count as proof the pin was omitted.
for hosting in default false true; do
  HOSTED_RENDER="$WORK/hosted-$hosting.yaml"
  set -- --set api.tls.enabled=true --set workers.image.tag=0.0.0-regression
  if [ "$hosting" != default ]; then
    set -- "$@" --set "workers.enabled=$hosting"
  fi
  if ! helm template uzi "$STRIPPED" --show-only templates/api-deployment.yaml \
       "$@" > "$HOSTED_RENDER" 2> "$WORK/err"; then
    echo "BROKEN: helm template of the API with workers.enabled=$hosting failed:" >&2
    cat "$WORK/err" >&2
    exit 2
  fi
  awk -v hosting="$hosting" '
    /^kind: Deployment$/ { deployment++ }
    /- name: HOSTED_WORKER_VERSION$/ { pins++; want_value = 1; next }
    want_value && /^[[:space:]]*value:/ { value = $2; want_value = 0 }
    END {
      if (deployment != 1) {
        print "BROKEN: expected exactly one API Deployment"; exit 2
      }
      if (hosting != "true" && pins != 0) {
        print "FAIL: HOSTED_WORKER_VERSION must be absent when hosting is disabled"; exit 1
      }
      if (hosting == "true" && (pins != 1 || value != "\"0.0.0-regression\"")) {
        print "FAIL: enabled hosting must emit exactly one HOSTED_WORKER_VERSION with the pinned tag"; exit 1
      }
    }
  ' "$HOSTED_RENDER" || exit $?
done
echo "OK: HOSTED_WORKER_VERSION is absent by default and with hosting disabled, and carries the pinned tag with hosting enabled"

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
