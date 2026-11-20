# Offline Codex provider characterization

Run the standalone target from the repository root:

```sh
task test:codex-provider-spike
```

It uses provisioned agent dependencies, typechecks this subtree, runs its fixture/policy
tests and executes all 36 real-runtime/control rows. No existing CI job, gate or M4 target
calls it. No dependency installation, runtime download, relay or production routing is added.
The required CLI and matching code-mode host are image-baked at the lock pin. An absent or
mismatched runtime is an incomplete prerequisite, never a passing skip.

For diagnosis only, the runner accepts one exact finite row name from agent/:
`node --import tsx ../e2e/codex-provider-spike/runner.ts callback`.
Selected rows are not evidence of a complete characterization.

## Evidence and interpretation

evidence.json is the full schema-2 output, retained verbatim in the originating PRD's Decision
Log. evidence-m1.json preserves the earlier 21-row M1 result. The PRD records invocation,
exit, pin, limitations, review and the resulting #4323 requirements.

`outcome` describes technical compatibility. A working managed-auth streamed baseline is
`observed-supported`; `safetyOutcome=observed-failure` separately records unexpected native
or unknown inventory. This distinction does not approve a production provider. At this pin
redirects forward bodies and unexpected inventory remains: #4323 is blocked on a verified
safety design. Env-key and static-header variants also remain held for their broader secret
surfaces. No vendor subscription credential is used or sent to a provider.

Rows project only finite names, safe header names (never values), request paths, bounded
failure codes, booleans and elapsed times. Callback/denial/effect/roundtrip, native inventory,
policy config, stripped environment, metadata, cancellation and resume are separate facts.
In restart rows `effect` means an additional B request after its positive control.
`deadlineObserved` remains independent of reason/outcome. Same-process resume override is
fixture-config evidence. Direct Node receiver and destination controls are fixture-control.

## What ran

The production launcher, managed API-key login, transport, broker and registry are reused
through the unchanged loopback seam. Code-host callback, host-disabled, tool-allocation/phase denial,
advice, streaming abort/control and same-session resume are real protocol cases with injected
command/file effects. Supervisor posture is synthetic: no packaged OS isolation, descendant
reaping, cross-worker quiescence or production recovery claim is made.

Fresh-root rebind saves only bounded synthetic sessions, disposes and settles A, seeds through
the existing seedSession seam, authenticates on a fresh root and resumes the same thread at B.
Both restart rows positively exercise B first. Retaining closed A times out without a new B
request. The same-process dotted provider override separately failed within the observation
window. Cancellation is owned-root abort after stream start, not a measured turn/interrupt RPC.

Resume inventory and request projections cover the measured turn only: priming A and the
B positive control are reset before the probe. `observerControlPassed` records that control
separately. Native inventory fields are absent when no inventory was observed, rather than
claiming suppression from silence. A negative resume deadline is accepted only after the
actual resume probe starts. Protocol and teardown failures are retained together;
`cleanupFailed` independently makes the harness fail even when the original error was a deadline.

The unknown-model case observes a fallback warning and config/read's nullable-integer
model_context_window property. No context-window override changes the fixed builder.
No local /v1/models request was seen. Request/stream retries are configured zero and websockets
false; analytics/feedback/startup-update disable assertions do not certify all discovery.
External auxiliary network behavior remains unverified. The altered-loopback URL control
must be rejected before spawn; it does not intercept native network traffic.

Native inventory checks traverse top-level, namespace/additional tools and nested tools
declarations with finite projections. apply_patch is not allowlisted as worker authority:
the worker alias is uzi_apply_patch. Unknown inventory is not assumed safe or effectful.
A separate native probe returned "No usable execution environment is available", with no
marker or worker effect. Advertisement is established; an effectful bypass is not.

## Bounds and isolation

Servers bind numeric loopback on dynamic ports; four active sockets are shared globally.
Request bodies and scripted streams are at most 1 MiB. Aggregate request/notification byte
and item budgets apply before retention; request bodies/descriptions are transient, not saved
as transcripts. Evidence is capped at 64 KiB per row and 1 MiB total. Synthetic session copy
rejects symlinks, excessive depth/count and more than 4 MiB. Only sessions are copied,
never the credential/configuration home.

Thirty-second case and five-minute run timers abort owned children and close owned sockets,
remaining installed through cleanup. Child close settlement is installed at spawn. Event-loop
timers are not an external hard supervisor; synchronous filesystem/crypto work can delay them.
Certificate subprocesses have five-second deadlines. No process.exit bypasses cleanup.

Provider transformations change only the generated provider table, preserve its exact header
and require numeric-loopback HTTP(S) /v1 URLs with an explicit port and no userinfo/query/fragment.
Indented/array extra sections fail. Env overrides accept only fixture key/CA variables over
the sparse replaced child environment. CA rows disable code-host execution; overrides are
absent from callback/advice/resume rows. TLS verification remains on. Both CA variables worked
independently in fixture config; production strips them. Missing/untrusted CA and a stalled
handshake all surfaced the bounded observation deadline without a distinct certificate error.

## Validation controls

The tester owns executable mutation proof in disposable exact-candidate exports. Coordinates:
wrong-key/guard removal; wireFixture restoration; receiver header/body observation; finally
observation after stalled redirect; ownChild early-exit listener; sparse OpenSSL path;
shared socket cap; finite header/error bounds; fixture table section/header validation;
broker tool-allocation/phase denial; onStream abort; sessions-only seed and closed-A binding; altered
destination assertion; second nested native member; safetyOutcome versus compatibility
classification; metadata/warning projection. Never mutate the primary candidate.

Full agent suite remains pending exact-head PR CI. The standalone experiment adds no CI
lane. Timings in the PRD are measured observations with cache/contention/queue limitations,
not isolated runtime regression claims.

The single repair pass adds an independent advice-attempt observer, distinct login/env-key
credentials and a wrong-env control, and a controlled HTTP 500 for oversized SSE.
Each separate redirect receiver is positively checked at its own address and reset before
Codex delivery. The current allocation-negative case is tool-grant-denied; it does not
pretend that role names independently deny a tool present in the trusted allocation.

evidence-run.json is machine-generated by capturing the actual Task subprocess exit and
argv, with runtime pin, output SHA-256 and source-file SHA-256 values. Its base revision
labels the checkout before the repair commit; the source fingerprints bind the tested code.
Advice's callbackAttempted field is independent from dispatched callbacks/effects and must
remain false. Receiver control booleans are recorded separately from actual redirect flags.
