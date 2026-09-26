---
title: OpenShift and OKD
order: 60
audience: operator
---

# OpenShift and OKD

The Helm chart targets a plain Kubernetes cluster by default: ingress-nginx for the web
frontend, fixed pod ids, kube-dns, Antrea for the hosted workers' named egress, and a
join-token Secret mounted at `/run/secrets`.
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
        k8s-app: null
      ports: [5353]
```

`k8s-app: null` is required. Helm deep-merges maps, so without it the chart default
`k8s-app: kube-dns` stays in the selector next to the OpenShift label. No OpenShift DNS pod
carries both labels, so the policy then matches nothing and every worker DNS lookup times
out.

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

## Where the worker's join token mounts

A hosted worker reads its join token and the api's CA from a Secret the controller mounts,
by default at `/run/secrets`. On OpenShift that path is taken: CRI-O's default mounts file
injects its own `/run/secrets` (subscription data and the ServiceAccount directory) into
every container, and it shadows a volume mounted there. The worker then finds no token and
exits at startup. Move the mount:

```yaml
workers:
  secretMountPath: /run/uzi-secrets
```

The value must be one lower-case directory directly under `/run`. The controller also
refuses to start if it overlaps another worker mount (for example `/run/dind`). With
`openshift.enabled` the render fails until it is set.

The worker IMAGE must carry the same change, and uzi enforces that: `workers.image.tag`
must be `0.85.0-rc.2` or newer (semver precedence, release candidates included, so
`0.85.0-rc.1` is too old and `0.85.0` and `0.86.0-rc.1` qualify). The tag is pinned
separately from the chart, so a chart upgrade alone does not move it. An older worker
image would still start with a relocated Secret, because it reads `UZI_WORKER_TOKEN_FILE`,
but its guardrails deny only `/run/secrets`, so agent commands naming the new directory
would not be screened. Requiring `secretMountPath` does not prevent that pairing, so both
the chart render and the controller at startup refuse it.

A tag that is not a semver version (an image digest, `dev`) cannot be compared and is
refused too. If you know that image carries the change, set
`workers.secretMountPathAllowUnversionedImage: true`. It never accepts an older semver tag.

The worker follows the new path through `UZI_WORKER_TOKEN_FILE`: its entrypoint checks the
token's ownership and mode there, as it does at `/run/secrets`. Its guardrails deny agent
commands that name the Secret directory, including the Secret volume's `..data` aliases.
The built-in `/run/secrets` deny stays in place.

## Admission for hosted workers

```yaml
openshift:
  enabled: true
```

This also requires `workers.secretMountPath` (see above).

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
- the guards that must refuse to render, including `openshift.enabled` without
  `workers.secretMountPath`;
- that the documented DNS selector contains only the OpenShift label;
- that the defaults render none of it.

It cannot prove admission or packet behaviour. Verify those on your cluster:
- start a hosted worker in each tier;
- confirm it resolves names, reaches its forge, and cannot reach a denied range.
