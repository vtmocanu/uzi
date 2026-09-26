#!/bin/sh
# Assert the chart's OpenShift/OKD knobs render what docs/openshift.md promises, and that
# a default install renders none of it.
#
# usage: scripts/assert-openshift-render.sh [chart-dir]
#
# WHY THIS EXISTS. Every knob here is off by default and targets a cluster no CI job
# runs (OpenShift admission, OVN-Kubernetes, a Gateway API implementation). A template
# edit that dropped a rule, reordered the EgressFirewall, or lost a fail-guard would still
# render and lint clean, and the first signal would be a hosted worker that cannot start
# or cannot reach its forge on someone's cluster. So each property below is asserted on
# the parsed render, including the guards that must REFUSE to render.
#
# OFFLINE, AND NON-VACUOUS. `helm template` against a temp copy of the chart with the
# CNPG subchart dependency stripped (postgres is off in every render here), so no OCI
# pull is needed. A query that finds nothing where an object must exist is a BROKEN
# INSTRUMENT (exit 2), never a pass.
#
# EXIT CODES (the assert-drain-knobs-render.sh convention):
#     2 = the instrument is broken (helm/yq/chart missing, or an expected object absent)
#     1 = a property does not hold
#     0 = every property holds
set -eu

CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
command -v yq >/dev/null 2>&1 || { echo "BROKEN: yq (mikefarah v4) not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

CHART="$WORK/chart"
cp -R "$CHART_DIR" "$CHART"
rm -f "$CHART/Chart.lock"
rm -rf "$CHART/charts"
awk '
  BEGIN { skip = 0 }
  /^dependencies:/ { skip = 1; next }
  skip && /^[^[:space:]-]/ { skip = 0 }
  skip { next }
  { print }
' "$CHART_DIR/Chart.yaml" > "$CHART/Chart.yaml"

# Values shared by every worker-enabled render. A generic forge plus the TLS pairing the
# chart requires whenever workers are on.
cat > "$WORK/workers.yaml" <<'EOF'
forge:
  allowedBaseURLs: [https://git.example.com:8443, https://github.com]
api:
  tls:
    enabled: true
workers:
  enabled: true
  docker:
    enabled: true
    networkPolicy:
      enabled: true
      podCIDR: 10.128.0.0/14
      serviceCIDR: 172.30.0.0/16
      linkLocalCIDR: 169.254.0.0/16
EOF

fail=0
ok() { echo "OK: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

# render <out> [helm args...]: render or exit 2 (a render that must succeed and did not).
render() {
  _out="$1"; shift
  if ! helm template uzi "$CHART" --namespace uzi "$@" > "$_out" 2> "$_out.err"; then
    echo "BROKEN: a render that must succeed failed: helm template $*" >&2
    cat "$_out.err" >&2
    exit 2
  fi
}

# refuse <needle> [helm args...]: the render must FAIL, with a message containing needle.
refuse() {
  _needle="$1"; shift
  if helm template uzi "$CHART" --namespace uzi "$@" > /dev/null 2> "$WORK/refuse.err"; then
    bad "render succeeded but must refuse ($_needle): helm template $*"
  elif grep -F -q -- "$_needle" "$WORK/refuse.err"; then
    ok "refuses to render: $_needle"
  else
    bad "render failed, but not with the expected guard ($_needle):"
    cat "$WORK/refuse.err" >&2
  fi
}

# q <file> <yq expression>: evaluate across every document.
q() { yq ea "$2" "$1"; }

# count <file> <kind> [name]: number of rendered objects of that kind (and name).
count() {
  if [ $# -eq 3 ]; then
    q "$1" "[select(.kind == \"$2\" and .metadata.name == \"$3\")] | length"
  else
    q "$1" "[select(.kind == \"$2\")] | length"
  fi
}

# --- (a) defaults render none of it ------------------------------------------------------
render "$WORK/default.yaml" -f "$WORK/workers.yaml" --set workers.fqdnEgress.enabled=true
for k in HTTPRoute EgressFirewall SecurityContextConstraints; do
  [ "$(count "$WORK/default.yaml" "$k")" = 0 ] && ok "default render has no $k" || bad "default render carries a $k"
done
[ "$(count "$WORK/default.yaml" NetworkPolicy uzi-worker-egress)" = 1 ] || { echo "BROKEN: default render lost the Antrea worker-egress policy" >&2; exit 2; }
ok "default provider is antrea (crd.antrea.io worker-egress present)"
n=$(q "$WORK/default.yaml" '[select(.kind == "Namespace") | .metadata.labels | select(has("security.openshift.io/scc.podSecurityLabelSync"))] | length')
[ "$n" = 0 ] && ok "default namespaces carry no OpenShift label-sync opt-out" || bad "default namespaces carry the OpenShift label-sync opt-out"
ports=$(q "$WORK/default.yaml" '[select(.kind == "NetworkPolicy" and .apiVersion == "networking.k8s.io/v1") | .spec.egress[]? | select(.to[]?.namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kube-system") | .ports[].port] | unique | join(",")')
[ -n "$ports" ] || { echo "BROKEN: no DNS egress rule found in the default worker policies" >&2; exit 2; }
[ "$ports" = 53 ] && ok "default DNS egress port is 53" || bad "default DNS egress ports are '$ports', expected 53"

# --- (b) HTTPRoute ----------------------------------------------------------------------
refuse "web.httpRoute.enabled and web.ingress.enabled are both true" \
  --set web.httpRoute.enabled=true --set 'web.httpRoute.parentRefs[0].name=shared'
refuse "web.httpRoute.parentRefs is empty" \
  --set web.httpRoute.enabled=true --set web.ingress.enabled=false
render "$WORK/route.yaml" --set web.httpRoute.enabled=true --set web.ingress.enabled=false \
  --set 'web.httpRoute.parentRefs[0].name=shared' --set 'web.httpRoute.parentRefs[0].namespace=gateway-system' \
  --set 'web.httpRoute.hostnames[0]=uzi.example.com'
[ "$(count "$WORK/route.yaml" HTTPRoute uzi-web)" = 1 ] || { echo "BROKEN: HTTPRoute uzi-web absent" >&2; exit 2; }
[ "$(count "$WORK/route.yaml" Ingress)" = 0 ] && ok "HTTPRoute render has no Ingress" || bad "HTTPRoute render still carries an Ingress"
got=$(q "$WORK/route.yaml" 'select(.kind == "HTTPRoute") | [.spec.hostnames[0], .spec.parentRefs[0].name, .spec.parentRefs[0].namespace, .spec.rules[0].matches[0].path.type, .spec.rules[0].matches[0].path.value, .spec.rules[0].backendRefs[0].name, .spec.rules[0].backendRefs[0].port] | join(" ")')
want="uzi.example.com shared gateway-system PathPrefix / uzi-web 80"
[ "$got" = "$want" ] && ok "HTTPRoute routes uzi.example.com / to uzi-web:80 via gateway-system/shared" || bad "HTTPRoute renders '$got', expected '$want'"

# --- (c) nulled pod ids are omitted, the rest of the posture stays -----------------------
cat > "$WORK/nullids.yaml" <<'EOF'
api_podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
web_podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
controller_podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
database:
  simple:
    podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
EOF
render "$WORK/nullids.yaml.out" -f "$WORK/workers.yaml" -f "$WORK/nullids.yaml"
for d in uzi-api uzi-web uzi-controller uzi-postgres; do
  sc=$(q "$WORK/nullids.yaml.out" "select((.kind == \"Deployment\" or .kind == \"StatefulSet\") and .metadata.name == \"$d\") | .spec.template.spec.securityContext")
  [ -n "$sc" ] && [ "$sc" != null ] || { echo "BROKEN: no pod securityContext rendered for $d" >&2; exit 2; }
  pinned=$(printf '%s\n' "$sc" | yq '[has("runAsUser"), has("runAsGroup"), has("fsGroup")] | any')
  keep=$(printf '%s\n' "$sc" | yq '.runAsNonRoot == true and .seccompProfile.type == "RuntimeDefault"')
  [ "$pinned" = false ] && [ "$keep" = true ] && ok "$d omits nulled ids, keeps runAsNonRoot + seccomp" || bad "$d securityContext is: $sc"
done

# --- (d) configurable DNS ports -----------------------------------------------------------
render "$WORK/dns.yaml" -f "$WORK/workers.yaml" --set 'workers.networkPolicy.dns.ports={5353}' \
  --set workers.networkPolicy.dns.namespace=openshift-dns
got=$(q "$WORK/dns.yaml" '[select(.kind == "NetworkPolicy" and .apiVersion == "networking.k8s.io/v1") | .spec.egress[]? | select(.to[]?.namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "openshift-dns") | .ports[] | .protocol + "/" + (.port | tostring)] | sort | join(",")')
[ -n "$got" ] || { echo "BROKEN: no openshift-dns egress rule rendered" >&2; exit 2; }
[ "$got" = "TCP/5353,TCP/5353,UDP/5353,UDP/5353" ] && ok "both worker policies allow DNS on UDP+TCP 5353 only" || bad "DNS egress renders '$got'"

# --- (e) OVN provider ---------------------------------------------------------------------
refuse "is not supported" -f "$WORK/workers.yaml" --set workers.fqdnEgress.enabled=true --set workers.fqdnEgress.provider=calico
refuse "workers.fqdnEgress.ovn.exceptCIDRs is empty" -f "$WORK/workers.yaml" --set workers.fqdnEgress.enabled=true --set workers.fqdnEgress.provider=ovn
render "$WORK/ovn.yaml" -f "$WORK/workers.yaml" --set workers.fqdnEgress.enabled=true --set workers.fqdnEgress.provider=ovn \
  --set 'workers.fqdnEgress.ovn.exceptCIDRs={10.128.0.0/14,172.30.0.0/16,192.0.2.0/24,169.254.0.0/16}'
[ "$(count "$WORK/ovn.yaml" EgressFirewall default)" = 1 ] || { echo "BROKEN: EgressFirewall default absent under provider ovn" >&2; exit 2; }
[ "$(q "$WORK/ovn.yaml" '[select(.apiVersion == "crd.antrea.io/v1beta1")] | length')" = 0 ] && ok "provider ovn renders no Antrea policy" || bad "provider ovn still renders an Antrea policy"
ns=$(q "$WORK/ovn.yaml" 'select(.kind == "EgressFirewall") | .metadata.namespace')
[ "$ns" = uzi-workers ] && ok "EgressFirewall lives in the kube-native worker namespace" || bad "EgressFirewall namespace is '$ns'"
first=$(q "$WORK/ovn.yaml" 'select(.kind == "EgressFirewall") | .spec.egress[0] | .type + " " + .to.cidrSelector')
last=$(q "$WORK/ovn.yaml" 'select(.kind == "EgressFirewall") | .spec.egress[-1] | .type + " " + .to.cidrSelector')
[ "$first" = "Deny 169.254.169.254/32" ] && ok "EgressFirewall starts with the deny belt" || bad "EgressFirewall first rule is '$first'"
[ "$last" = "Deny 0.0.0.0/0" ] && ok "EgressFirewall ends with Deny 0.0.0.0/0" || bad "EgressFirewall last rule is '$last'"
forge=$(q "$WORK/ovn.yaml" 'select(.kind == "EgressFirewall") | [.spec.egress[] | select(.to.dnsName == "git.example.com" or .to.dnsName == "github.com") | .to.dnsName + ":" + (.ports[0].port | tostring)] | join(",")')
[ "$forge" = "git.example.com:8443,github.com:443" ] && ok "derived forge hosts allowed on their own ports" || bad "derived forge allows render '$forge'"
wild=$(q "$WORK/ovn.yaml" 'select(.kind == "EgressFirewall") | [.spec.egress[] | select(.to.dnsName == "*.anthropic.com")] | length')
[ "$wild" = 1 ] && ok "allowFQDNs carried into the EgressFirewall" || bad "allowFQDNs entry *.anthropic.com missing"
# Provider PARITY: the OVN allow set (dnsName + ports) must equal the Antrea allow set
# (fqdn + ports) for the same values. assert-chart-render.sh proves the Antrea set is
# complete and correctly shaped (canonical hosts, forge hosts, the exact Codex trio on
# 443); equality carries every one of those properties over to OVN without restating them.
antrea_set=$(q "$WORK/default.yaml" '[select(.apiVersion == "crd.antrea.io/v1beta1" and .metadata.name == "uzi-worker-egress") | .spec.egress[] | select(.action == "Allow") | .to[0].fqdn + ":" + ([.ports[] | .protocol + "/" + (.port | tostring)] | join("+"))] | sort | join(",")')
ovn_set=$(q "$WORK/ovn.yaml" '[select(.kind == "EgressFirewall") | .spec.egress[] | select(.type == "Allow") | .to.dnsName + ":" + ([.ports[] | .protocol + "/" + (.port | tostring)] | join("+"))] | sort | join(",")')
[ -n "$antrea_set" ] || { echo "BROKEN: empty Antrea allow set" >&2; exit 2; }
[ "$antrea_set" = "$ovn_set" ] && ok "OVN allow set equals the Antrea allow set ($(printf '%s' "$ovn_set" | tr ',' '\n' | wc -l | tr -d ' ') entries)" || bad "provider allow sets diverge: antrea='$antrea_set' ovn='$ovn_set'"
antrea_deny=$(q "$WORK/default.yaml" '[select(.apiVersion == "crd.antrea.io/v1beta1" and .metadata.name == "uzi-worker-egress") | .spec.egress[] | select(.action == "Drop") | .to[0].ipBlock.cidr] | join(",")')
ovn_deny=$(q "$WORK/ovn.yaml" '[select(.kind == "EgressFirewall") | .spec.egress[:-1][] | select(.type == "Deny") | .to.cidrSelector] | join(",")')
[ "$antrea_deny" = "$ovn_deny" ] && ok "OVN deny belt equals the Antrea deny belt, in order" || bad "deny belts diverge: antrea='$antrea_deny' ovn='$ovn_deny'"
exc=$(q "$WORK/ovn.yaml" 'select(.kind == "NetworkPolicy" and .metadata.name == "uzi-worker-external-egress") | .spec.egress[0].to[0].ipBlock | .cidr + " except " + (.except | join(","))')
[ "$exc" = "0.0.0.0/0 except 10.128.0.0/14,172.30.0.0/16,192.0.2.0/24,169.254.0.0/16" ] && ok "external-egress NetworkPolicy excludes the in-cluster and host ranges" || bad "external-egress NetworkPolicy renders '$exc'"

# --- (f) openshift.enabled -----------------------------------------------------------------
render "$WORK/os.yaml" -f "$WORK/workers.yaml" --set openshift.enabled=true
[ "$(count "$WORK/os.yaml" SecurityContextConstraints uzi-worker)" = 1 ] || { echo "BROKEN: SCC uzi-worker absent" >&2; exit 2; }
got=$(q "$WORK/os.yaml" 'select(.kind == "SecurityContextConstraints") | [.runAsUser.type, (.runAsUser.uidRangeMin | tostring), (.allowedCapabilities | length | tostring), .fsGroup.type, (.fsGroup.ranges[0].min | tostring), (.allowPrivilegedContainer | tostring), (.allowPrivilegeEscalation | tostring)] | join(" ")')
[ "$got" = "MustRunAsRange 10001 0 MustRunAs 10001 false false" ] && ok "single-uid SCC pins uid/fsGroup 10001, adds no capability" || bad "single-uid SCC renders '$got'"
render "$WORK/os-split.yaml" -f "$WORK/workers.yaml" --set openshift.enabled=true --set workers.uidSplit.enabled=true
got=$(q "$WORK/os-split.yaml" 'select(.kind == "SecurityContextConstraints") | .runAsUser.type + " " + (.allowedCapabilities | sort | join(","))')
[ "$got" = "RunAsAny CHOWN,DAC_OVERRIDE,FOWNER,SETGID,SETPCAP,SETUID" ] && ok "uid-split SCC admits root with exactly the rendered capabilities" || bad "uid-split SCC renders '$got'"
got=$(q "$WORK/os.yaml" '[select(.kind == "Role" and (.metadata.name == "uzi-worker-scc" or .metadata.name == "uzi-worker-docker-scc")) | .metadata.namespace + "=" + .rules[0].resourceNames[0] + "/" + .rules[0].verbs[0]] | sort | join(",")')
[ "$got" = "uzi-workers-docker=privileged/use,uzi-workers=uzi-worker/use" ] && ok "SCC use grants: docker tier -> privileged, kube-native -> uzi-worker" || bad "SCC Roles render '$got'"
got=$(q "$WORK/os.yaml" '[select(.kind == "RoleBinding" and (.metadata.name == "uzi-worker-scc" or .metadata.name == "uzi-worker-docker-scc")) | .subjects[0].namespace + ":" + .subjects[0].name] | sort | join(",")')
[ "$got" = "uzi-workers-docker:uzi-hosted-worker,uzi-workers:uzi-hosted-worker" ] && ok "SCC grants bind only the worker ServiceAccounts" || bad "SCC RoleBindings render '$got'"
n=$(q "$WORK/os.yaml" '[select(.kind == "Namespace") | select(.metadata.labels."security.openshift.io/scc.podSecurityLabelSync" == "false")] | length')
[ "$n" = 2 ] && ok "both worker namespaces opt out of OpenShift label sync" || bad "label-sync opt-out on $n namespaces, expected 2"
rg_anyuid=$(q "$WORK/os.yaml" '[select(.kind == "Role") | .rules[]?.resourceNames[]? | select(. == "anyuid")] | length')
[ "$rg_anyuid" = 0 ] && ok "no anyuid grant" || bad "a Role grants anyuid"

if [ "$fail" -ne 0 ]; then
  echo "FAIL: the OpenShift/OKD chart knobs do not render as documented (docs/openshift.md)" >&2
  exit 1
fi
echo "OK: every OpenShift/OKD render property holds"
