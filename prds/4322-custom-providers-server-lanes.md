# PRD #4322: Claude custom providers on judge, jobs, plan/code cross-check and isolated research lanes

**Issue**: [#4322](https://github.com/vtmocanu/uzi/issues/4322)
**Umbrella**: #4319
**Priority / area**: High / runtime (isolated network-policy trust is part of the reviewed scope)
**Status**: Draft refresh after Opus/xhigh and Astra/high review
**Entry prerequisite**: #4320 is merged (PR #4464). #4505 must merge before any milestone with its final activation, exact-key redaction and all-upload scan contract. Re-read the merged code and ADR-4320 at dispatch: neither activation alone nor the current issue body proves every later scan requirement landed. Pending hosted acceptance is separate from unfinished implementation. No relay, #4509, #4323 or #50 dependency. Preparation is authorized, but dispatch requires the user's explicit go and final high-priority architecture approval.

## Problem and outcome

#4320 and #4505 enable ordinary Claude issue/task-review/chat provider execution. Judge, job, both plan/code cross-check and research-lane consumers still reject or strip provider bindings. This is actual server and worker wiring work: the existing ordinary SDK adapter alone does not make these lanes provider-capable.

Deliver provider execution on all four lanes while preserving their separate credential authority and lifecycle. The judge resolves its own policy, checkers resolve the opposite engine, jobs fail rather than park, and isolated jobs keep their fixed tool/fetcher boundary with an explicit operator-trusted model-channel exception.

Examples:

1. A provider-only user with Claude judge mode default/auto gets a provider-frozen judge at creation (today skipped as provider_lane_unsupported). A valid explicitly pinned vendor judge still uses that token on the provider run's trace. A Codex judge uses only its Codex vendor authority until #4323; the Claude source is not consulted for that choice.
2. A Codex lead's Claude plan or code checker freezes the user's Claude source and uses only it. A Claude provider lead's Codex checker uses its independent Codex vendor authority. Provider mappings cannot silently move a checker to another credential or engine.
3. A provider-sourced owner's Claude job freezes the source and runs without a vendor credential. A revoked binding at assembly or the final delivery recheck fails terminally with the typed reason, never parked. Gateway overload is provider_gateway_limit, not a vendor reset or pool fallback.
4. An operator lists a public gateway host for the isolated lane; a site-list job can execute through it with no general network client and with agent-initiated URL fetching still confined to the fetcher. The operator accepts the separate model/content-channel residual below. Real CNI/gateway behavior is hosted acceptance, not an in-run claim.

## Accepted activation and isolated content residual

The owner accepted runtime redirect forwarding on 2026-11-19 and explicitly extended it on **2026-11-20 to judge, job, cross-check and isolated lanes** (D5). Newly gateway-bound data includes judged traces, including vendor traces judged by default/auto provider policy; job inputs/files; plan/code checker snapshots; and fetched pages in research jobs. Both supported Claude auth styles remain available; Bearer is recommended, not required. Bodies can reach redirect destinations; keys can reach same-host sibling paths, and cross-host with x_api_key. A pinned vendor judge or opposite-engine vendor checker can likewise receive provider-run content as part of its independent authorized policy. Document these destinations instead of implying the lead's source controls every evaluator.

#4496 would remove the redirect residual but remains parked. Starting URLs still need the exact operator allowlist; probes still refuse redirects. On the isolated lane, configured model egress is only `modelHost` plus operator `providerHosts`, TCP 443, subject to ADR-0285's existing IP/FQDN/shared-IP limitations. This is not an exact per-request hostname proof.

Amend ADR-1906 with a **new model-destination invariant**, keeping invariant 1 intact rather than rewriting it. The added model destinations are operator-listed exact names, TCP 443. Explain the residual: a listed gateway is an operator-trusted content channel; the official/site-list source guarantee holds only for gateways that neither search nor proxy arbitrary web content, and the operator docs rule is advice, not enforcement. Agent-initiated URL retrieval remains through the fetcher and source log. Redirects can only use network reach granted by the unchanged policy and its known ADR-0285 limitations; do not claim cryptographic confinement to host labels.

API/CNI rollout and host removal are asynchronous, not instantaneous revocation of a held claim. Preserve next-actual-claim credential authority and no broad extra egress. Internal/in-cluster gateways remain unreachable behind the existing non-public/cluster Drop belts; docs must not promise private gateway reach. Existing private-CA delivery remains, including its UID-split limitation in docs/providers.md; isolated workers currently are not UID-split. No TLS-verification bypass.

## Shared implementation contract

### Frozen credentials, models and new worker capability

Use the shared #4320 freeze/claim helpers and the actual five run columns `model_provider_id`, `model_provider_revision`, `model_provider_key_mode`, `model_provider_key_secret_id`, `model_provider_key_rev`. Retain exact claim exclusivity, owner/key-kind/revision/enablement fences, claim-time allowlist checks and no vendor fallback. Model validation is lane-aware: judge's actual resolved judge model, job's actual model, checker model at its existing claim-time resolution, and template models only for lanes carrying those templates. Do not run ordinary issue-template checks on a repo-less lane.

Wire judge-runner, job-runner, claude-cross-check/cross-check-runner, isolated-runner/preflightReason, isolated-executor and stripForIsolation; several currently require OAuth, refuse provider bindings or clear Secrets.Provider. Constructors use sdkCredentialFromBinding, resolveProviderModel, stripProviderCost and the inherited gateway-limit classifier, not a key relabeled as OAuth. Route future shared-provider key modes through these same helpers once #4324 and its child add them.

Add one protocol capability per newly wired consumer, alongside #4505's claude_provider_v1: claude_provider_judge_v1 (M1a), claude_provider_job_v1 (M1b), claude_provider_checker_v1 (M2, both stages) and claude_provider_isolated_v1 (M3, lane jobs, together with the job capability). Each milestone advertises only its own, once that consumer is complete. A provider-bound lane claim requires claude_provider_v1, that lane's capability and its existing lane/job/checker capabilities, in ClaimRun, the claim_assembly.go backstop and fn_placement_admitting_workers. API-first rollout: old workers lack the lane capability and receive no claim. Worker-first rollout: an unused capability is harmless.

Update applicable placement, demand, wake and reap predicates, including isolated job paths. Preserve #4505 provider-only demand and capable-worker suppression, saturation checks, paired old-image reap predicates and busy/custody guards, and provider-chat wake filtering. `LockUnservableEphemeralJobWorkers` must account for both provider and new lane capabilities on provider-bound jobs: an incapable bound ephemeral job worker leads to terminal no_job_capable_worker through that job path, rather than endless generic provider reprovisioning. Vendor jobs and ordinary issue/chat rules remain unchanged. No image-proof service, version floor or provisioning subsystem.

fn_placement_admitting_workers is replaced by a migration (draft number; its Down restores the 00486 body). ClaimRun's provider clause (runtime.sql, claude_provider_v1) gains the lane capability. The provider queued-reason rung in health.go (CountOnlineWorkersSatisfyingClaudeProvider) counts lane-capable workers for provider-bound lane runs, so a run waiting on a #4505-only fleet names the missing capability. Sweep e2e/ for any changed string.

### Exact-key redaction and output publication

Register providerKeyForms with each lane runner's logger/redactor for the **whole claim**, not only refusal branches. Reuse #4505 redaction before parsing, truncation, persistence or output, including internal catch-and-log paths, model IDs/metadata and errors. Judge onResult and limit/usage facts receive only scope-redacted values, never the raw SDK frame; pass provider usage through stripProviderCost so cost_status stays unreported and no vendor prices/headroom/reset is fabricated.

Before uploadJobOutputs, scan the exact immutable bytes to be uploaded for every providerKeyForms value. A hit or inability to validate prevents that file upload and records existing `worker_unreadable`, as unsafe names already do; the job can still complete with its other result/files. Do not rewrite binary content, scan one path then reopen different bytes, or turn optional-file refusal into a parked job. Test opaque key in filename/content, scanner failure, replacement between capture/upload and a clean-file control. Structured job results/findings/errors are separately redacted before their existing parse/size/persistence boundaries.

Preserve #4505 exact-bound-key scanning before every GitHub/GitLab/Forgejo finalize/checkpoint/park/pause/shutdown upload reachable from a lane; a hit/scanner error blocks network writes and preserves existing custody/typed refusal. This is accidental-publication protection, not prevention of deliberate exfiltration by code already trusted with a key.

### Lane-specific settlement, including final locked races

Apply this matrix to assembly errors **and** the final locked authority recheck in claim_recovery.go; generic provider holds must not accidentally park repo-less evaluators:

- Ordinary custody-holding runs keep existing provider hold behavior.
- Judge/job/checker provider binding revocation fails the child terminally with its typed failure_reason and fail_origin credential_unavailable; no new fail-origin enum. Judge's existing vendor disabled-token behavior stays unchanged; provider refusals do not inherit generic provider parking.
- A failed plan checker leads to the existing human gate without a synthetic verdict. A code checker records the existing unavailable/failure result through RecordCodeCrossCheckFailure; there is no plan gate at code completion and no fabricated approval.
- Gateway 429/529 fails judge/job with provider_gateway_limit, with no vendor reset/pool fallback. A checker uses existing model_error settlement and its stage-specific lead outcome. Jobs never park.

Test rotation/revocation between assembly and final delivery for judge, job, plan checker and code checker, including exact claim/lead generation fencing. New judge creation skip reasons and display strings require additive judge_skips CHECK changes in the existing NOT VALID/validate/guarded-Down pattern and CLI rendering; historical lane_unsupported rows remain honest history. Do not confuse an enqueue skip with a later created judge's terminal failure.

## Lane modules

### Judge

At both judge creation sites (`judge_enqueue.go` and manual `judge_read.go`), choose the judge harness and provider-versus-vendor authority independently of the target and ordinary run resolver. A real pinned vendor token remains vendor even if the Claude source is set/unusable. Pinned with a NULL pointer is effective default (00197; effectiveBindMode), so it follows the default row: the provider when the Claude source is set, the vendor default otherwise. Behaviour matches what clients already display. Test it explicitly. For Claude default/auto mode with a source, freeze that provider/key/revision at judge creation and bypass the vendor-token-presence prerequisite; a bad selected provider skips/refuses explicitly instead of spending vendor tokens. Null source leaves the entire existing vendor judge ladder, auto fallback and disabled-token behavior intact. A Codex judge ignores Claude source and stays Codex-vendor until #4323.

Later source/policy edits affect new provider-judge rows only; claims of a provider-frozen judge never call judgeChoice or reread the owner's current source. Vendor-frozen rows retain their existing claim-time vendor policy and never cross into provider resolution. Preserve independent model/effort settings and validate the actual judge model against the frozen map.

### Jobs and `/api/v1`

Freeze the owner's Claude source inside CreateJobRun's transaction. The provider model is lane-aware; source/model/host failures become documented typed v1 reasons, not a logged 500. A separate small error-mapping issue may land before this PRD while lane support stays disabled; reuse it. Extend api/openapi/v1.yaml additively for the new error reasons and their HTTP status codes; the public job status enum is unchanged (TestV1OpenAPIEnumsMatchGo). Run check:api-v1-compat. Existing product scopes, origin visibility, input ownership, rate/cap and terminal-job rules remain unchanged.

### Plan and code checkers

Both CreatePlanCrossCheckChild and SubmitCodeCrossCheck/CreateCodeCrossCheckChild resolve the opposite engine's own source and freeze it, not the lead's binding. Remove provider refusals at both creation closures and the settings-usability path only when the actual consumer/capability is ready. Widen singleFamilyCheckerCustody to exactly one Claude credential (vendor token or provider) with no Codex credential; preserve the reverse family rule, read-only confinement and no forge credential in the checker's model process; the worker-held clone PAT used by ensureClone is unchanged. Shared helpers maintain provider key modes for future #4324, not a duplicate per-lane resolver. Sequence overlapping #2169 work before editing these closures if it is still in flight.

### Known same-model guard, additive M2b

There is no guard implementation on main: the prior 18edc84c9 reference was PRD text only. Keep existing worker/template claim-time checker model selection. For a provider-involved pair, compare the actual resolved lead/checker model identities for that claim, after frozen provider maps, **before any model execution or credential delivery**; do not compare an assumed creation-time default or require that a child not yet exist. Preserve existing observed model metadata and repeat the check on a later actual claim if its worker-dependent resolution differs. Unresolved identity in a provider-involved pair is explicitly unavailable, not proof of independent review. Vendor/vendor pairs remain outside this new guard.

Comparison rule: The guard evaluates only pairs where at least one side is provider-bound. A vendor/vendor pair is never evaluated and keeps today's behaviour exactly, including an unpinned model on either side. In a provider-involved pair, a side whose model the server cannot resolve (no pin, lane or template model) is checker_unavailable. The docs tell owners to pin the vendor side's model if such a pair should auto-approve.

If both sides are providers, same normalized base-URL host and same resolved model ID is a known match; in a provider-involved pair with one vendor side, matching resolved model ID is the conservative known-match rule. Different names/hosts do not prove independence. Use existing checker_unavailable with server-side same_model detail; do not add cross_check_same_model as a public gate reason/protocol/migration. For plan, settle the already-created child through the existing unavailable human gate. For code, call RecordCodeCrossCheckFailure with checker_unavailable and no verdict. Synthetic dual-provider fixtures test the comparison contract without enabling Codex provider claims before #4323. Test mapped aliases, different IDs/hosts and provider/vendor pairs against this exact rule.

### Isolated research lane

The currently supported site-list path is CreateJobRun writing egress_profile_id. Do admission **inside that transaction** and again at actual claim, using the frozen provider binding. There is no invented ordinary-run park branch for a research binding that cannot currently be created. Reuse the fixed tool set, init verification, fetcher custody and route allowlist.

Add chart `workers.isolatedLane.providerHosts`, exact normalized ASCII/punycode DNS names only: refuse wildcard, scheme, port, path and malformed names. Render api env under the existing isolatedLaneEnabled guard, with the same grammar; unset is empty, malformed env fails closed. Provider host must be in providerHosts, effective port 443, and initial base URL still in UZI_PROVIDER_ALLOWED_BASE_URLS; otherwise provider_host_not_admitted_isolated. Keep modelHost/vendor reach unchanged. Keep the non-public/cluster Drop belts and no broad CIDR/extra in-cluster peers.

Both policies are required: Antrea exact FQDN Allows after the last Drop on TCP 443, OVN exact dnsName Allows before final Deny. Extend scripts/assert-chart-render.sh and scripts/assert-openshift-render.sh section (h); absent providerHosts must preserve today's objects. Host-list edits change API env/policy, not worker pods. Document asynchronous CNI/API skew and held-claim limits, not a claim of atomic network revocation. Correct ADR-1906/ARCHITECTURE's stale inert-until-#1908 status because site-list jobs already exist. Add the new invariant and D5 residual without rewriting invariant 1.

## Verification and milestones

Each unit has watched regressions and focused review. Each milestone is one PR. M1a and M1b are independent. M2 and M2b ship in one PR, so provider checkers never run without the guard. M3 depends on M1b. These are independently shippable slices within this PRD, not external unfinished-PRD dependencies. Before each gated run, freeze the permitted slice in its dispatch scope (M1a, M1b, M2+M2b or M3); do not ask one run to ship all remaining milestones as one PR. Partial deliveries remain non-closing until the remaining PRD work and recorded acceptance are complete. No milestone advertises another consumer's capability. The user reviews this decomposition before go; this is not an on-deck task.

### M1a: Independent provider judge authority

Creation/freeze at enqueue/manual sites, policy table including pinned-null and Codex controls, lane-aware models, real runner/SDK wiring, redacted onResult/usage, typed skips/claim settlement and docs. Test provider-only owner, pinned vendor, source/policy change while queued, revoked final delivery and provider gateway limits.

### M1b: Terminal provider jobs and safe output publication

Creation/claim freeze, worker/preflight/SDK wiring, typed v1 errors and unservable bound-worker capability handling, structured redaction and exact uploaded-file-byte refusal. Jobs remain terminal and optional unsafe files are omitted as worker_unreadable. Test no vendor call, before/after final-claim revocation, old workers, normal clean outputs and v1 compatibility.

### M2: Opposite-engine plan and code provider checkers

Both child-creation families, source/freeze, custody exclusivity, actual SDK consumer and stage-specific failures; preserve settings and pin resolution, no hidden Codex implementation. Test both engine directions and both plan/code stages, plus final locked races and no synthetic approval.

### M2b: Actual resolved-model independence guard

Existing claim-time models, the exact comparison rule and checker_unavailable outcomes above, pure comparison fixtures plus actual claim/lead-stage tests. A vendor/vendor plan and code cross-check with both models unset is created and runs unchanged. No new gate-reason vocabulary or assumed default. The final plan must identify where actual lead model identity is available for provider-involved pairs; if it cannot establish it, refuse explicitly rather than label unknown as independent or silently freeze a new vendor model policy. M2 and M2b are one PR, and the checker capability is advertised only with this guard complete.

### M3: Explicit operator-listed isolated model channels

Research claim stripping/preflight/runner/executor/job SDK wiring, the isolated-provider capability in addition to the job-provider capability, both chart/CNI arms and env parser, transactional admission and recheck, ADR/new invariant/residual and docs. Test listed/unlisted/443/malformed hosts, legacy/ordinary-only workers, no broad network tool and unchanged vendor/Compose refusal. Live real-gateway/site-list behavior is in the linked hosted acceptance issue, not an in-run milestone proof.

### In-run boundary

`UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` plus touched/directly affected tests; `task check:api` plus affected Go packages `-race -count=1`; applicable SQL generation/drift, additive migration/numbering and DTO checks. LiveDB only via `./e2e/run-store-it.sh` on an owned Docker-capable DB with named PASS and zero SKIP, serial package binaries on a shared DB (`-p 1`); otherwise pending exact-head test-api-store-it. Use prefixless runtime-assembled secrets, scan:secrets with detected canaries and token-literal checks.

Chart: helm template with deploy/values/ci-render.yaml through the existing assert-chart-render.sh pattern, assert-openshift-render.sh, lint:shell, lint:yaml, check:api-v1-compat, docs:sync/check-docs:web and UUID changelog. Update ADR-4320 D9/ARCHITECTURE lane status, ADR-2149 checker custody, ADR-1906's new invariant/residual, and AI-synced specs/human.md. Sweep changed reasons/claim/output/forge contracts across E2E helpers, phases and forge fake; run test:e2e-lib and test:forge-fake and name affected phases left unrun. No new CI job or `.github/workflows/**` edits.

Landing obligations outside milestones: **full agent suite: exact-head PR CI**, applicable API/repo/LiveDB/worker-UID checks. The linked maintainer acceptance issue covers real hosted gateway lanes, actual CNI reach/refusal, private-CA behavior and vendor controls, without a sweep/deadline. No offline fake is called live certification.

## Decision Log

- **D1. Judge authority is independent.** Provider-versus-vendor branch is chosen at creation; provider bindings stay frozen, vendor claim policy remains compatible. Explicit pinned vendor semantics remain even on provider traces; pinned-with-NULL is default. Default/auto provider mode never silently falls back.
- **D2. Checkers resolve their own engine.** Both plan and code are included by owner decision 2026-11-20. Existing opposite-family rule, model pins and confinement remain; no inheritance of the lead credential.
- **D3. Jobs never park.** Provider refusal is terminal at assembly and final recheck. Unsafe optional output files are omitted with worker_unreadable, without upload or binary rewriting; a clean job result may still complete.
- **D4. Research destinations are an explicit operator list.** modelHost plus providerHosts, exact names/443 in configured policy, with existing ADR-0285 enforcement limitations. No wildcard, general network tool, per-run policy or new proxy; the model channel has D5's accepted content residual.
- **D5. Redirect risk extends to all four lanes (owner, 2026-11-20).** Judge traces, job inputs/files, plan/code snapshots and fetched pages may be forwarded through gateway redirects under both existing auth styles. Isolated model-channel reach remains its explicit policy. Add a new ADR-1906 model-destination invariant and conditional gateway content residual; do not rewrite invariant 1. The docs prohibition on gateway web proxy/search is advice, not enforced origin proof.
- **D6. #4505 is the required merged base.** Its output/log redaction and all-upload exact-key scan are reused and extended at newly reachable result/file seams. Accepted redirects do not authorize accidental publication. #4496 stays parked; #4509/#4323 are independent Codex work.
- **D7. Per-lane capability means actual consumers, not ordinary activation.** Alongside claude_provider_v1, judge/job/checker/isolated consumers advertise only their own claude_provider_judge_v1, claude_provider_job_v1, claude_provider_checker_v1 or claude_provider_isolated_v1 once complete. Isolated jobs also require the job capability. ClaimRun, assembly backstop, placement, health, unservable-job and reap predicates stay consistent; #4505-only workers never receive these claims. No version-floor or fleet-proof subsystem.
- **D8. Independently shippable slices (lead Option A, 2026-11-20).** M1a judge and M1b jobs are independent PRs; M2 plan/code checker and M2b same-model guard ship together in one PR; M3 isolated channels depends on M1b and is another PR. No extra PRD is introduced. Freeze one permitted slice per gated run and keep partial delivery non-closing. The user must say go after final architecture/scope review; preparation is not dispatch.
- **D9. Provider-involved same-model match uses existing unavailability.** Compare actual resolved IDs at claim only when a provider is involved, not a pre-child default or vendor/vendor pair. Use checker_unavailable with a server detail and the stage's existing outcome; unknown provider-pair identity is not independent-review proof. No new public reason/migration is justified solely for this guard.
