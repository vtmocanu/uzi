# ADR-1906: Web research runs in a no-internet lane and reads the web only through a fetcher

**Status**: Accepted (PRD #1906 M1-M7 implemented; M8, job integration, and M9, live k8s acceptance, are not done: no run is bound to a site list yet and no lane measurement has been taken on a real cluster)
**Date**: 2026-09-29
**Issue**: [vtmocanu/uzi#1906](https://github.com/vtmocanu/uzi/issues/1906)
**PRD**: [prds/1906-official-sources-web-research.md](../prds/1906-official-sources-web-research.md)
**Related**: [ADR-0285](0285-worker-egress-tier-trust-model.md) (the two-tier egress model this lane sits beside), [#1651](https://github.com/vtmocanu/uzi/issues/1651) (the allowlist is IP-level, not host-level)

## Decision (summary)

A research run that must read **official sources only** runs in a third worker
lane with no internet, and reads web content only through `uzi-fetcher`, a
separate service that checks every URL against the run's admin-managed site list
and logs every attempt.

- **A separate no-internet lane.** A third hosted-worker namespace
  (`workers.isolatedLane.namespace`, default `uzi-workers-isolated`) at Pod
  Security `restricted`. Its default-deny `NetworkPolicy` admits DNS, the api's
  worker port, `uzi-fetcher`, and one model host by exact name. It honours
  neither `workers.networkPolicy.allowWebService` nor
  `workers.networkPolicy.extraEgress`, and is not gated on
  `workers.networkPolicy.enabled`.
- **The fetcher is the only web path.** `uzi-fetcher` is its own Deployment
  (`api/cmd/fetcher`, built into the api image, no new image), so untrusted
  content is never parsed in the process that holds the api's secrets. It
  fetches per URL, not per address: https on 443 only, no userinfo, host on the
  run's site list at every redirect hop, every resolved address public (checked
  again on the exact address dialed), no proxy, `Accept-Encoding: identity`,
  bytes counted after decoding. Its own `NetworkPolicy` excludes the cluster's
  pod, service and node ranges and the private ranges, so an internal
  destination is refused twice.
- **A per-run credential and a fail-closed source log.** The api mints a
  per-run fetch credential at claim (only its sha256 is stored), revokes it when
  the run leaves the running state, and snapshots the site list onto the run at
  claim. The fetcher forwards the credential, and the api takes the run from it,
  never from a run id the fetcher supplies. The api reserves bytes, a file slot
  and a concurrency slot atomically before each fetch and writes one
  `run_fetches` row per attempt; the fetcher returns content only after the log
  write is acknowledged.
- **A fixed tool set.** A lane run gets `Read`, `Write`, `Edit`, `Grep`, `Glob`
  and one in-process fetch tool, and nothing else (no `Bash`, `WebFetch`,
  `WebSearch`, subagents, forge or memory tools). The agent compares the SDK's
  effective tool list to that set at start and fails the run if it differs.
- **Lane-only claim routing.** A profile-bound run can be claimed only by a
  worker the api provisioned into the lane, and a lane worker claims only
  profile-bound runs. The clause sits in `ClaimRun`, outside
  `fn_worker_can_claim`, `required_capabilities`, the clear-requirements action
  and the capability-aware kill-switch, keyed on a server-set
  `workers.isolated_lane` column plus the `isolated_fetch_v1` protocol
  capability. A lane worker reaches only an allowlist of `/api/worker` routes.
- **`api.anthropic.com` only.** The lane admits its model host by exact name,
  never a wildcard, and no OpenAI host. The lane runs Claude only.

## Context

External products need vetted web research, and the requirement is hard: only
official sources. uzi could not enforce that:

1. Worker egress is IP-level (ADR-0285, #1651). Official documentation often
   sits on shared hosting (CDNs, edge networks), so allowing its addresses opens
   TCP 443 to every other site on them.
2. The docker tier is open by design, and the standard tier's list is
   namespace-wide, so no per-run list is expressible.
3. Claude's built-in `WebSearch` runs server-side; no worker network rule can
   restrict or log it.
4. `Bash` is a general network client, so restricting only the named web tools is
   not a boundary.

Name-level egress control is not available from the CNIs uzi runs on (OVN-Kubernetes
`EgressFirewall` `dnsName` is IP-level; Cilium would replace the CNI; a service
mesh brings a whole mesh and handles per-run lists poorly). A fetch service is
the only design that gives a per-URL check and an exact log.

## The decisions

### Remove the general client, then put a check in front of the one path left
The network lane is a backstop, not the whole boundary. DNS recursion and the
model API stay reachable from it, and both need code execution in the pod to
abuse. The real boundary is the tool set: the lane removes the general network
client, and the fixed tool set keeps the agent from getting one back. The two
are load-bearing together.

### The fetcher is a separate Deployment from the same image
It parses untrusted internet content, and the api holds `UZI_SECRET_KEY`, forge
PATs and model tokens. Same repo, chart and release, so no new image and no
release workflow change; a different `command`. Worker to fetcher and fetcher to
api are both TLS with no plain-HTTP mode, because both hops carry credentials.

### Per-run state lives in the api, not the fetcher
Per-replica counters do not add up across fetcher replicas, so totals, file
counts and concurrency are enforced by an atomic reservation in the api. The
fetcher holds only its own service token; the api stores only its sha256.

### Placement is exclusive and server-owned
Reusing capability matching would leave the escape hatches that exist for good
reasons (the kill-switch, clearing a requirement, self-reported capabilities).
The lane clause follows the precedent of the Codex and completion-interlock
clauses instead. The lane marker is written by one statement, from the
provisioner's own decision; no registration, heartbeat or worker input reaches
it. A worker-side purpose check (`laneMismatch`) and the orphan reaper's lane
arms are defence in depth behind the claim clause.

### Only the exact model host
`api.anthropic.com` on TCP 443, through the same Antrea or OVN provider as the
standard tier. On 2026-09-29 its addresses were registered to Anthropic, PBC,
not a shared CDN. That is a point-in-time observation. The FQDN rule is enforced
per resolved address, so if the host later moves to shared hosting, other hosts
on those addresses become reachable at the network layer. That opens no content
path by itself, since the agent has no client to use it. When PRD #50 lands, the
lane is meant to drop direct model egress.

## Consequences

- The lane is **inert until PRD #1908 (M8)**: nothing sets a run's site list, so
  no run is placed in it. Turning it on renders the namespace, the fetcher and
  the policies and nothing else changes. Live acceptance (M9) is
  maintainer-owned and not done; the network guarantees are what the chart and
  code are built to enforce, not yet a measurement (record the tier and cluster
  type with every measurement, ADR-0285).
- **Residuals, recorded and not closed:**
  - DNS is a channel (a lane pod must resolve names).
  - The model API is a channel (the same class the standard tier accepts for
    `*.anthropic.com`).
  - The model-host ownership check is point-in-time and must be re-checked.
  - A multi-publisher host admitted by an explicit override admits every
    publisher on it; the fetcher checks the host, not the path.
  - Content from an approved host is not vetted; the lane restricts where it
    comes from.
  - Enforcement depends on the CNI; a CNI that enforces no `NetworkPolicy` (KinD's
    default) enforces no lane.
- Operationally: a rotated fetcher CA reaches only lane workers created after a
  controller restart; a fetcher URL change is a pod-spec change that rolls lane
  pods; turning the lane off leaves existing lane objects in place for the
  operator to remove; the `generated` fetcher token churns under Argo CD, which
  renders with `helm template` (use `existing`). See
  [docs/isolated-research-lane.md](../docs/isolated-research-lane.md).
- The chart adds a required-value surface (`clusterCIDRs`, an FQDN provider,
  api TLS, workers enabled) that fails the render when missing, so a half-enabled
  lane cannot ship.

### Invariants a future change must not break

1. **No path from a lane pod to web content except `uzi-fetcher`.** The lane
   policy must not gain `allowWebService`, `extraEgress`, an `ipBlock`, a
   wildcard model host, or a fourth in-cluster peer. `scripts/assert-chart-render.sh`
   guards the shape; keep it failing when the policy grows.
2. **The tool set is fixed and checked against what the SDK reports**, not what
   was requested. A new SDK tool that `tools` does not govern must fail the run.
3. **The fetch credential never enters the SDK child's environment** and never
   outlives the claim it was minted for.
4. **The api takes the run from the credential**, never from a run id the fetcher
   supplies, and a fetch whose log write fails returns no content.
5. **Placement is two-way and outside the capability switches.** A profile-bound
   run is claimed only by a server-marked lane worker advertising
   `isolated_fetch_v1`, and a lane worker claims nothing else. The marker is
   never set from a worker's own input.
6. **Every lane-worker route is denied unless listed.** A new `/api/worker` route
   is unavailable to a lane worker until it is added to the allowlist on purpose,
   and none that return third-party content (memory, forge, other runs' traces)
   is added.
7. **The site list is a claim-time snapshot.** An admin edit changes runs not yet
   claimed, and a short introspection cache may delay revocation but never widen
   a list.
8. **Every resolved address must be public, and the dialed address is re-checked.**
   The fetcher's address check and its `NetworkPolicy` are two layers; do not
   remove one because the other exists.
9. **No lane object is rendered when the lane is off**, and existing tiers'
   egress is unchanged by turning it on.
