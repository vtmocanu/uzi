---
title: OpenShift and OKD
order: 60
audience: operator
---

# OpenShift and OKD

The Helm chart targets a plain Kubernetes cluster by default: ingress-nginx for the web
frontend, fixed pod ids, kube-dns, and Antrea for the hosted workers' named egress.
OpenShift and OKD differ on each of these. The knobs below adapt the chart. All of them are
off by default, so an existing install renders exactly as before.

The example values use placeholder names and upstream default network ranges. Replace
them with your cluster's.

## Exposing the web frontend through a Gateway

If your cluster exposes apps through a shared Gateway API `Gateway`, render an `HTTPRoute`
instead of the nginx `Ingress`:

```yaml
web:
  ingress:
    enabled: false
  httpRoute:
    enabled: true
    hostnames: [uzi.example.com]
    parentRefs:
      - name: shared
        namespace: gateway-system
```

- **One or the other:** `web.httpRoute` and `web.ingress` are mutually exclusive, and the
  render fails if both are enabled.
- **A Gateway is required:** it also fails if `parentRefs` is empty, because a route
  attached to no Gateway serves nothing.
- **TLS:** it terminates wherever your Gateway, or the router in front of it, terminates it.
- **WebSocket timeout:** `/api/ws` is a long-lived WebSocket. Raise the idle timeout on
  whatever sits in front of the Gateway. The web client reconnects and replays missed
  messages after a cut, but a short timeout causes needless churn.

## Letting OpenShift assign pod ids

The default `restricted-v2` SCC assigns every pod a UID and fsGroup from its namespace's
range, and it rejects a pod that pins its own. Null the pinned ids and the rendered pods
omit them:

```yaml
api_podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
web_podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
controller_podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
# simple database mode only:
database:
  simple:
    podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
```

`runAsNonRoot`, the `RuntimeDefault` seccomp profile and the dropped capabilities stay in
place. With the ids nulled, the api, web and controller need nothing beyond
`restricted-v2`:

- **api and controller** run from a distroless image with a read-only root filesystem and
  an `emptyDir` for `/tmp`. The controller reads its mounted token and CA through the
  fsGroup that OpenShift assigns.
- **web** runs from `nginx-unprivileged`, whose writable paths are group-owned by gid 0.
  That is the group an arbitrary OpenShift UID runs with.

## Worker DNS

The worker NetworkPolicies allow DNS to the pods named by `workers.networkPolicy.dns`.
A NetworkPolicy matches the destination pod's port, not the Service's. OpenShift's DNS pods
listen on 5353 behind a port-53 Service:

```yaml
workers:
  networkPolicy:
    dns:
      namespace: openshift-dns
      podSelector:
        dns.operator.openshift.io/daemonset-dns: default
      ports: [5353]
```

Each listed port is allowed for both UDP and TCP, in both worker namespaces.

## Named egress on OVN-Kubernetes

`workers.fqdnEgress` restricts kube-native workers to named destinations:

- the forge hosts derived from `forge.allowedBaseURLs`;
- the `allowFQDNs` list;
- behind a `denyCIDRs` belt.

The default provider is `antrea`. On OVN-Kubernetes, use `ovn`:

```yaml
workers:
  fqdnEgress:
    enabled: true
    provider: ovn
    ovn:
      # pod network, service network, node network, link-local
      exceptCIDRs: [10.128.0.0/14, 172.30.0.0/16, 192.0.2.0/24, 169.254.0.0/16]
    denyCIDRs:
      - name: drop-cloud-metadata
        cidr: 169.254.169.254/32
      - name: drop-kube-apiserver
        cidr: 172.30.0.1/32
```

OVN has no allow rule that bypasses a NetworkPolicy the way an Antrea `Allow` does: an
`EgressFirewall` only filters traffic a NetworkPolicy already admitted. So `ovn` renders two
objects in the kube-native worker namespace, and they work only together:

1. **A NetworkPolicy that opens external addresses only:** `0.0.0.0/0` except
   `ovn.exceptCIDRs`. Lateral traffic to pods, services, nodes and link-local stays
   denied. The render fails if `exceptCIDRs` is empty.
2. **The namespace's `EgressFirewall` (always named `default`):** it lists, in order:
   - a `Deny` for each `denyCIDRs` entry;
   - an `Allow` by `dnsName` for each `allowFQDNs` entry and each derived forge host,
     on their ports;
   - a final `Deny 0.0.0.0/0`.

**Wildcards need a cluster feature, not just a version.** OVN enforces a wildcard
`dnsName` such as `*.anthropic.com` only when the DNSNameResolver feature is enabled
(the `dnsnameresolvers.network.openshift.io` CRD exists). OpenShift ships that as a
Technology Preview feature gate, off by default. Without it, the API server still
accepts the object, but the wildcard rule matches nothing. So the render refuses any
`*` entry in `allowFQDNs` under `provider: ovn` unless you opt in:

- **Feature off (the default):** replace each wildcard with the exact hosts you need,
  for example:

  ```yaml
  workers:
    fqdnEgress:
      allowFQDNs:
        - name: allow-anthropic
          fqdn: api.anthropic.com
          ports: [443]
        # ...the rest of the list, restated (Helm replaces lists, it does not merge them)
  ```

- **Feature on:** set `workers.fqdnEgress.ovn.allowWildcards: true`.

Either way, verify at runtime that a worker reaches each destination. An accepted
object is not proof of enforcement.

**Name resolution:** OVN resolves each `dnsName` itself and refreshes it on the record's
TTL. A name whose addresses rotate faster than its TTL can briefly miss.

The allow set and deny belt are the same as the Antrea policy's for the same values. The
chart's render checks assert that equality.

The docker tier is unaffected: it keeps its own NetworkPolicy, which allows the internet
and excludes the in-cluster ranges you configure under `workers.docker.networkPolicy`.

## Admission for hosted workers

```yaml
openshift:
  enabled: true
```

This adds, for the hosted-worker namespaces only:

- **Label-sync opt-out:** `security.openshift.io/scc.podSecurityLabelSync: "false"` on
  both worker namespaces, so OpenShift does not rewrite the chart's PodSecurity labels.
- **Kube-native tier:** a minimal custom SCC named `<release>-worker`, plus a namespaced
  Role and RoleBinding granting the worker ServiceAccount `use` on it. It admits exactly
  what the controller renders:
  - every container must drop `ALL` (`requiredDropCapabilities`);
  - uid and fsGroup 10001, no added capabilities, no privilege escalation, no host
    access, the namespace's SELinux label, the `RuntimeDefault` seccomp profile, and
    `secret`, `persistentVolumeClaim` and `emptyDir` volumes only;
  - with `workers.uidSplit.enabled`, root start with exactly `SETUID`, `SETGID`, `SETPCAP`,
    `CHOWN`, `DAC_OVERRIDE` and `FOWNER`. The worker and its nix-seed init container drop
    everything else.
- **Docker tier:** a namespaced Role and RoleBinding granting the docker worker
  ServiceAccount `use` on the built-in `privileged` SCC. Its DinD sidecar is privileged in
  both the rootless and non-rootless postures.

These are admission grants, checked when a pod is created. The worker ServiceAccounts still
hold no API permissions and their pods mount no token. `anyuid` is not granted: its
`allowedCapabilities` is empty, so it would not admit the uid-split containers anyway.

The only new cluster-scoped object is the custom SCC. Installing it needs cluster-admin
rights, like the chart's PriorityClasses.

## What the render checks cover

`task render:openshift-check` runs in CI's chart job. It renders each knob offline and
asserts:
- the resulting objects, including the full custom SCC in both postures;
- the guards that must refuse to render;
- that the defaults render none of it.

It cannot prove admission or packet behaviour. Verify those on your cluster:
- start a hosted worker in each tier;
- confirm it resolves names, reaches its forge, and cannot reach a denied range.
