# Codex provider spike (M1)

Run serially from `agent/` with existing dependencies:

```sh
npx tsc -p ../e2e/codex-provider-spike/tsconfig.json
node --import tsx --test --test-concurrency=1 --test-timeout=30000 ../e2e/codex-provider-spike/*.test.ts
node --import tsx ../e2e/codex-provider-spike/runner.ts
```

The opt-in root Taskfile target is `test:codex-provider-spike`. No relay or forwarder exists.
The runtime must be image-baked with its matching receipt and lock. Supervisor posture is synthetic,
protocol-only; this makes no packaged OS isolation claim. No real credentials or host environment are used.

The 21 rows include production baseline, wrong-key rejection, guard removal, receiver positive control,
provider-table env-key/alternate-header variants, unsupported wire and Responses restoration through the
same fixture transform, redirects, and TLS CA variants. Controls fail the runner if their relation is wrong;
unexpected fixture errors are harness failures. Cross-host bind failure is unverified.
receiver-control uses Node's HTTP client and has path identity fixture-control; it does not
exercise the production launcher. The other 20 rows use the native runtime (production-launcher
or fixture-config). The new 307-port-stall receiver consumes the redirected body but stalls its
response. Even when the turn throws on timeout, final evidence must retain body=true,
reason=forwarded and outcome=observed-failure; losing that relation fails the runner.
Observations are copied during finally independently of turn success; an unexpected harness
error still marks the run broken even when forwarding overrides the row reason.

Schema 1 rows contain finite configuration identity, outcome/reason, elapsedMs, exact observed request
paths and header names (no values), credential equality, and redirect-target credential-header presence
and body presence independently. Production sparse env booleans are caSSL/caCodex/credentialEnv.
Actual child booleans are actualSSL/actualCodex/actualCredentialEnv. keyInConfig identifies the
alternate-header variant; env-key and alternate-header remain held for design decision.

Provider transformations run only on the single generated provider table. Runtime validation after
transformation permits only HTTP(S) numeric 127.0.0.1 or 127.0.0.2 with explicit port and /v1 path;
credentials, queries, fragments and additional sections fail before spawn.
Fixture env overrides allow only SSL_CERT_FILE, CODEX_CA_CERTIFICATE and SPIKE_KEY over the
production sparse replacement. Code mode remains disabled for CA cases. TLS verification stays enabled.

Resource limits: four concurrent sockets globally across provider and receiver; request body 1 MiB;
aggregate body consumption 4 MiB/16384 chunks per server, checked before accumulation; 16 response
placeholders; fixed SSE below 1 MiB; transient notifications 4 MiB/16384 items before callback handling.
Unexpected tool callbacks fail, with no callback results retained. Raw notifications and request bodies
are not retained. Bounded transient stderr is classified to finite codes, never emitted.
Evidence is limited to 64 rows/1 MiB and 64 KiB per row.

Independent five-minute run and 30-second case timers abort the owned child and close owned sockets,
including during auth; the case timer remains installed through cleanup. A separate 25-second child kill
timer and eight-second turn wait also apply. Child settlement is registered immediately after spawn,
and cleanup awaits its close even if exit preceded disposal. Server close is idempotent under concurrent
watchdog/finally calls. No process.exit is used. Timers are event-loop timers, not an external supervisor:
synchronous filesystem/crypto operations can delay their delivery. Certificate generation has individual
five-second subprocess timeouts.

M1 observed behavior: baseline and restored Responses stream successfully; wrong-key rejects,
and removing the guard restores streaming. Env-key and alternate-header work but remain held.
Same-origin redirects forward credential headers and bodies; cross-port/host redirects forward
bodies while stripping the credential header. Trusted SSL_CERT_FILE, CODEX_CA_CERTIFICATE
and their combination stream successfully. The pinned runtime retries TLS refusal beyond the
eight-second turn observation window: no-CA and untrusted-CA report deadline, as does TLS stall.
That deadline is the observed runtime surface; these rows do not distinguish certificate refusal
from a stalled handshake. M2 remains pending, including auxiliary-destination verification.

Timing evidence supplied by main: baseline 72 seconds versus integrated 152 seconds during
concurrent validation. Queue, cache and contention costs were not isolated, so this is not an
isolated runtime regression measurement. No CI lane was added.

Tester mutation coordinates (disposable exact-candidate export only):
- ownership.ts ownChild: install the close listener inside the returned cleanup instead of at spawn.
  The early-exit half of “owned child settlement handles exit before disposal and live disposal” must time out.
- runner.ts runOpenSSL: replace the absolute executable with bare openssl; sparse PATH regression must fail.
- runner.ts wireFixture: remove the Responses restoration conditional; fixture transform regression must fail.
- fake-provider.ts connection listener: replace global connections.size check with p.sockets.size;
  the shared-cap test must fail.
- runner.ts finally: remove the header/body observation copy (or move it back after the awaited
  runProtocolTurn). The real 307-port-stall row must lose body=true and its relation guard must
  produce a nonzero exit. Also remove the forwarding override to test reason/outcome protection.
- driver.ts validateFixtureTable: restore the column-zero-only section count; the indented tools
  and array-section fixture tests must fail.
- runner.ts receiver-control path: relabel it production-launcher; the regenerated row must fail
  the tester's fixture-control identity assertion.
No executable mutation was performed by this implementation unit. These are tester obligations.

Validation logs and remaining obligations are in the handoff to main. No repo suites, commits,
secret scan, review, hosted checks or mutation proofs were performed by this unit.
