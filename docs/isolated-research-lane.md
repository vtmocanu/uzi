---
title: Isolated research lane
order: 66
audience: operator
---

# Isolated research lane

The isolated research lane is a worker namespace with no internet, plus
`uzi-fetcher`, the one service through which a run in that lane reads web
content. It exists so an official-sources research run ([PRD
#1906](../prds/1906-official-sources-web-research.md)) can read a vendor's
documentation and datasheets from hosts on an admin-approved [site
list](egress-profiles.md) and nowhere else, with every attempt logged. The
design rationale is in [ADR-1906](../adr/1906-isolated-fetch-lane.md).

**Nothing uses the lane yet.** A run is placed in the lane only when it is bound
to a site list, and no way to bind a run exists in this release: that arrives
with job creation in [PRD #1908](../prds/1908-repo-less-jobs-api.md) (this PRD's
milestone M8). Turning the lane on renders the namespace, the fetcher and the
policies, and the api starts to provision lane workers only for bound runs, so
until then the lane is idle. Live acceptance on a real cluster (this PRD's
milestone M9) has not been done: the network guarantees below are what the
chart and code are built to enforce, not something a maintainer has yet
measured on a running cluster.

## What the lane is

- **A third worker namespace** (default `uzi-workers-isolated`) beside the
  standard and docker tiers, at Pod Security `restricted`. Its NetworkPolicy is
  default-deny in both directions. A lane pod may reach exactly cluster DNS, the
  api's worker port, `uzi-fetcher`, and one model host by exact name
  (`api.anthropic.com`, TCP 443). It does not inherit
  `workers.networkPolicy.allowWebService` or `workers.networkPolicy.extraEgress`.
- **`uzi-fetcher`**, a Deployment in the release namespace that runs `/fetcher`
  from the api image (no new image). It checks each URL against the run's site
  list, fetches it, and reports every attempt to the api, which logs it. It is a
  separate Deployment because it parses untrusted internet content and the api
  holds every secret.
- **A per-run fetch credential** (prefix `uzf_`). The api mints it when a bound
  run is claimed, stores only its sha256, and revokes it when the run leaves the
  running state. It stays in the worker's node process; the model's child
  process never sees it.
- **A fixed tool set.** A lane run gets `Read`, `Write`, `Edit`, `Grep`, `Glob`
  and one in-process fetch tool (`fetch_url`), and nothing else: no `Bash`, no
  `WebFetch` or `WebSearch`, no subagents, no forge or memory tools. The agent
  checks the effective tool list the SDK reports at start and fails the run if
  it differs. The tool saves each download under `sources/<sha256>` in the run
  workspace and returns its path, final URL, content type, size and hash. (The
  result tool that PRD #1908 adds joins the set with M8; it is not offered yet.)
- **Exclusive placement.** Only a lane worker can claim a bound run, and a lane
  worker claims only bound runs. The api provisions lane workers itself, one per
  bound run (they are run-bound and removed with the run), and marks them in the
  database; a worker cannot mark itself. This holds whatever the capability
  scheduling switches or a cleared requirement say.
- **A narrowed worker API.** A lane worker may call only the worker routes its
  agent needs (register, heartbeat, claim, state and message reports, the
  message-gap read, and the steering inputs). Every other worker route, agent
  memory and the forge routes included, answers `403` for it. The claim carries
  no forge credential, and the lane runs Claude only.

## Enabling the lane

The lane is off by default and off renders nothing: no namespace, no fetcher, no
policy, no controller setting, no api rule. Set `workers.isolatedLane.enabled:
true` in the Helm values. Every knob is documented in the comments of
`deploy/chart/values.yaml` under `workers.isolatedLane`.

The chart refuses to render (`helm template` fails) unless:

| Prerequisite | Why |
|---|---|
| `workers.enabled: true` | The lane is a hosted-worker namespace and needs the [worker controller](hosted-workers.md). |
| `workers.fqdnEgress.enabled: true` | The model host is admitted through the same FQDN provider the standard tier uses (`workers.fqdnEgress.provider`: Antrea or OVN-Kubernetes). Without it a lane pod cannot reach its model. |
| `api.tls.enabled: true` | The fetcher dials the api over https only; its service token and every run credential cross that hop. `workers.allowPlaintextAPI` does not apply to it. |
| `workers.isolatedLane.clusterCIDRs.pod`, `.service` and `.node`, each non-empty | The fetcher's NetworkPolicy allows the internet minus these ranges and the fetcher refuses them itself, so an internal address is blocked twice. IPv4 and IPv6 ranges both go here. |
| A namespace distinct from `workers.namespace`, `workers.docker.namespace` and the release namespace | The lane's default-deny policy selects every pod in it. |

Also needed, and not checked by the chart:

- **Ephemeral (run-bound) workers must be enabled** on the instance (the
  `ephemeral_workers_enabled` admin setting; see [Hosted
  workers](hosted-workers.md)). Lane workers are provisioned by the same
  provisioner. The lane trigger skips a user's opt-in, but not this switch or the
  per-user ephemeral cap.
- **`workers.isolatedLane.modelHost`** is one exact host name, `api.anthropic.com`
  by default; a `*` fails the render. No OpenAI host is admitted in the lane.
  Change it only from a real failure.
- **Under `provider: ovn`** the lane's model-egress pair (a NetworkPolicy plus an
  `EgressFirewall`) renders only where the cluster serves `k8s.ovn.org/v1`
  `EgressFirewall`. Elsewhere it is omitted, and the lane cannot reach its model:
  it fails closed, not open.
- **The fetcher's TLS certificate** defaults to `<fullname>-fetcher-tls`, issued
  by the same issuer as the api's. With `api.tls.certManager.enabled: false`, name a
  pre-created Secret in `workers.isolatedLane.fetcher.tls.secretName` holding
  `tls.crt`, `tls.key` and `ca.crt`. Its `ca.crt` is the one trust anchor lane
  workers get for the fetcher.
- **The fetcher's service token** (`workers.isolatedLane.fetcher.token.source`):
  `generated` (default) has the chart create a random token and store its hash
  for the api. `existing` reads the token and its sha256 from the two keys named
  in `tokenSecretKey` and `tokenHashSecretKey` of the
  `infisical.app.managedSecretName` Secret. Use `existing` for a GitOps install;
  see [Operations](#operations). The api's hash of this token must differ from
  the controller's.
- **Fetcher sizing.** `workers.isolatedLane.fetcher.resources` defaults to a 1Gi
  memory limit, sized for the default `maxInflight` of 16 and 25 MiB per file
  (worst case about `maxInflight x 2 x maxFileBytes` in buffered bodies). The
  chart sets `GOMEMLIMIT` to 90% of the memory limit, so size the limit so that
  `0.9 x limit >= maxInflight x 2 x maxFileBytes + ~100 MiB`. If you
  raise `maxInflight` or `maxFileBytes`, raise the limit with them. The chart
  leaves both empty by default, meaning the fetcher's own defaults (25 MiB per
  file, 60 s per fetch, 16 concurrent fetches).

The chart passes the controller its three lane settings together
(`UZI_WORKER_ISOLATED_NAMESPACE`, `UZI_WORKER_ISOLATED_FETCHER_URL`,
`UZI_WORKER_ISOLATED_FETCHER_CA_FILE`). They are all or none. The controller
refuses to start if only some are set, if the lane namespace equals the standard
or docker namespace, or if it equals the controller's own namespace or that
cannot be read. With the lane unset, a lane worker in the desired list is
skipped, never rendered elsewhere.

## Site lists

The hosts a lane run may read come from its site list. Site lists are managed on
**Admin → Site lists** (the only place to write them) and read with `uzi admin
egress-profile list|show`; see [Egress profiles](egress-profiles.md) for entry
rules, wildcard and public-suffix checks, and the multi-publisher override. A
run takes a snapshot of its list when it is claimed, so editing a list changes
only runs that have not been claimed yet.

Official files often live on a different host than the main site (a CDN or a
downloads host), so a list usually needs tuning. The source log shows every
refusal.

## What the fetcher guarantees

On every request, and on every redirect hop:

- **Request shape is fixed.** The agent supplies a URL and nothing else. The
  fetcher performs a `GET`; there is no way to set a method, header or body.
- **HTTPS on port 443 only.** No other scheme or port, no user information in
  the URL, no IP-address host, and a URL over 2048 bytes is refused. At most 5
  redirects are followed, each re-checked as a fresh request.
- **The host must be on the run's site list.** The host is normalized the way
  list entries are, and the normalized name is what is resolved, sent as SNI and
  `Host`, and verified on the certificate. A redirect to an off-list host is
  refused as `redirect_off_list`.
- **Every resolved address must be public.** All answers must pass; private,
  loopback, link-local (the metadata address included), CGNAT, multicast,
  documentation, NAT64, 6to4 and Teredo ranges and the cluster's pod, service
  and node ranges are refused, and IPv6 is admitted only inside `2000::/3`. The
  connection is made to the checked address itself, so a DNS answer that changes
  between check and connect cannot swap it.
- **Bytes are counted after any decoding.** The fetcher asks for
  `Accept-Encoding: identity`, refuses any other `Content-Encoding`, ignores
  proxy environment variables, and reads through a cap of the per-file limit
  plus one byte.
- **Per-run caps are the api's.** Admission is an atomic reservation against the
  run's totals, so parallel fetches cannot overshoot a total. The limits are the
  [research fetch caps](admin-settings.md#research-fetch-caps): 25 MiB per file,
  200 MiB and 100 files per run, 4 concurrent fetches, and 500 requests
  (`fetch_max_run_attempts`), which count refusals too. A refused admission
  returns `admission_refused` with the reason (`run_bytes`, `run_files`,
  `concurrency` or `attempts`).
- **Every attempt is logged, or it did not happen.** The fetcher reports each
  admitted attempt, allowed or refused, and returns content only after the api
  acknowledged the log write. If the api cannot be reached, the fetch is refused
  (`control_unavailable` or `log_failed`).
- **The credential dies with the claim.** It is revoked when the run leaves the
  running state (any park, requeue or terminal status), and a re-claim issues a
  new one, so a token from an earlier claim no longer works. The run is always
  taken from the credential, never from anything the fetcher sends.

### Reading the source log

A run's owner reads its source log with `uzi run fetches <run-id>` (see the
[CLI reference](cli.md)) or `GET /api/runs/{id}/fetches`: one row per attempt,
oldest first, with the verdict (`allowed` or `refused`), the reason code, the
HTTP status, bytes, content type, the requested URL, the final URL after
redirects, and (in `--json`) the file's sha256. Only the owner can read it;
another user's run reads as not found. The URLs, content type and reason are
site- or agent-controlled text: the api stores them escaped and the CLI strips
control characters when it renders them, but treat them as untrusted.

The reason codes a refusal carries include `off_list`, `redirect_off_list`,
`private_address`, `not_https`, `userinfo`, `ip_literal`, `port`, `invalid_url`,
`url_too_long`, `too_many_redirects`, `content_encoding`, `too_large`,
`upstream_status` (the site answered a non-2xx status), `dns_failed`,
`connect_failed`, `tls`, `timeout`, `admission_refused`, `credential_invalid`,
`control_unavailable`, `log_failed` and `busy`.

## What is not guaranteed

These are the residual risks. None is closed by the lane, and none is measured
on a live cluster yet.

- **DNS is a channel.** A lane pod must resolve names to reach the api, the
  fetcher and its model, so cluster DNS stays reachable, and DNS queries can
  carry data out. Abusing it needs code execution in the pod. The fixed tool set
  is what prevents that: the lane removes the general network client, and the
  tool set keeps the agent from getting one back. The network policy alone is not
  the boundary.
- **The model API is a channel.** The lane must reach the model host, and an
  agent's own requests to it can carry data. This is the same class of exposure
  the standard tier accepts for `*.anthropic.com` ([ADR-0285](../adr/0285-worker-egress-tier-trust-model.md)).
  When PRD #50 (an LLM egress proxy, still a draft) lands, the lane is meant to
  drop direct model egress and go through the api.
- **The model host check is point-in-time.** On 2026-09-29 the addresses of
  `api.anthropic.com` were registered to Anthropic, PBC, not a shared CDN. That
  can change. The FQDN rule is enforced per resolved address, not per host name,
  so if the host moves onto shared hosting, every other site on those addresses
  becomes reachable at the network layer. That opens no content path by itself,
  because the agent has no client to use it, but re-check the hosting when you
  enable the lane and periodically. Add exact names to the lane only from a real
  failure, never a wildcard.
- **Multi-publisher hosts.** The fetcher checks the host, not the path. Allowing
  a host that many publishers share (a code host, an object-storage host, a
  documentation platform, a forum) allows every publisher on it. The site-list
  editor refuses such an entry unless an admin ticks the explicit override, and
  the override is a decision to admit all of them. The built-in list of these
  hosts cannot be complete; review a vendor's community and download hosts
  yourself.
- **Content is not vetted.** The fetcher restricts where content comes from, not
  what it says. A page on an approved host can still hold text that tries to
  steer the model. Treat the analysis a run produces as untrusted input to
  whatever consumes it.
- **Enforcement depends on the CNI.** The lane's policy is only as strong as the
  cluster's enforcement of it; KinD's default CNI enforces no NetworkPolicy at
  all. The fetcher's own address checks are a second layer for the internal
  ranges, but the lane pods' default-deny needs a CNI that enforces it.

## Operations

- **Rotating the fetcher's CA.** The controller reads the CA file once at boot
  and copies it into each lane worker's own token Secret when the worker is
  created. A rotated CA reaches only lane workers created after the controller is
  restarted; a lane worker already running keeps the CA it was created with.
  Restart the controller after a rotation.
- **Changing the fetcher URL.** The URL is part of a lane worker's pod
  environment, so changing it changes the pod spec and the controller rolls lane
  pods like any other drift (a busy worker is drained first).
- **Turning the lane off.** With the lane unset the controller stops managing the
  lane namespace: existing lane workers, Secrets and PVCs are left untouched, not
  torn down. Clean them up by hand (delete the leftover `uzi-hw-*` objects in the
  lane namespace), and the chart no longer renders the namespace, policies or
  fetcher. Runs already bound to a list stay bound and cannot be claimed by any
  other kind of worker.
- **Generated fetcher token under Argo CD.** The `generated` source keeps the
  same token across `helm upgrade` by looking up the existing Secret. Argo CD
  renders with `helm template`, where that lookup returns nothing, so every sync
  would generate a new token and hash. For a
  GitOps install set `token.source: existing` and put the token and its hash in
  the managed Secret, for example:

  ```sh
  TOKEN=$(openssl rand -base64 32)
  printf '%s' "$TOKEN" | shasum -a 256
  ```

  Store `$TOKEN` under `UZI_FETCHER_TOKEN` and the printed hex digest under
  `UZI_FETCHER_TOKEN_SHA256` (or the key names you set in the values).
- **Resizing the fetcher.** Raise the memory limit together with `maxInflight`
  or `maxFileBytes` (see the sizing note above), or the kernel can kill the
  fetcher under load.

## See also

- [Egress profiles](egress-profiles.md): site lists, entry rules and the
  multi-publisher override.
- [Admin settings](admin-settings.md#research-fetch-caps): the fetch caps.
- [Hosted workers](hosted-workers.md): the worker controller and run-bound
  workers.
- [ADR-1906](../adr/1906-isolated-fetch-lane.md): why the lane is built this way
  and what a future change must not break.
