# Worker UID test lane

`task test:agent:worker-uid` builds the base worker image and runs the ownership and cleanup
leaves from five test files through its root entrypoint, which drops to the worker UID with runner
group membership and the production ambient capabilities. It runs offline, with
disposable data and Nix mounts, and removes only its own named container.

To reuse an image, set `WORKER_UID_SKIP_BUILD=1` and `WORKER_UID_IMAGE=<image>`.
`WORKER_UID_TIMEOUT` controls the outer watchdog (default 600 seconds).
The separate command proof has a 115-second watchdog plus five-second kill grace
(120 seconds total). All Docker operations use GNU timeout: image metadata gets
10 seconds, the local build fallback 60 seconds, and dependency provisioning 30
seconds, each with five-second kill grace (115 seconds total including the optional
build). Cleanup attempts only the three owned container names and dependency volume
in two calls of 10 seconds plus five-second kill grace (30 seconds total). Cleanup
tries both calls even on failure and propagates nonzero status. Containers are
retained until the trap so successful invocations leave exact owned names to remove.
`WORKER_UID_SKIP_BUILD=1` requires the hosted image to exist locally; the harness
never pulls an image or installs runtime dependencies.

The host needs Docker, Node, Python, GNU timeout and the locked agent development
dependencies. Test inputs are mounted into `/app`, preserving their relative paths
and resolving runtime packages from the image.

A second, serial container starts directly as command UID/GID 10003, with all
capabilities dropped and no-new-privileges. It mounts this harness, agent source,
agent tsconfig/package metadata and vendored supervisor source read-only under `/proof`.
The package metadata preserves the source’s ESM boundary. A unique
Docker volume is seeded from the image’s locked `/app/node_modules` and mounted
read-only at `/proof/agent/node_modules`; scratch, Go cache and Go temporary files
stay in a command-owned executable tmpfs for the freshly built helpers. The fixture checks Linux, UID 10003, all five empty
capability sets and no-new-privileges before building only the two Go helpers with
`-mod=vendor`, `GOTOOLCHAIN=local`, `GOPROXY=off` and `GOSUMDB=off`.

The real supervisor and command sandbox still execute. The fixture asserts real
adopted detached descendants, protected and unattributed exclusions, injected
supervisor/root start-tick mismatch refusal before signaling, concurrent cleanup
joining one disposal, sibling and unattributed-tree survival, and the exact
structured pressure failure at the broker and output projection. Start-tick fault
injection does not claim actual kernel PID reuse. Starting at command UID does not
prove the production worker-to-command UID transition, M3 runtime wiring, or M5b
cgroup calibration. Docker execution and timing remain acceptance work for the lead.

Before execution, the locked TypeScript parser derives the leaf inventory from
the current source. The four ownership/cleanup files must each contain their
recognized UID gate and at least one leaf. The fifth file,
`agent/test/git-planning-diff.test.ts`, contributes exactly nine literal leaves
inside the exact `Unit2 runner source capture` suite: two snapshot-copy
cancellation leaves and seven symlink capture/refusal leaves. The suite itself
is not targeted: generated titles, the command sandbox leaf, other capture
leaves and the bounded stdout suite stay excluded. Missing or changed required
titles, nonliteral replacements and duplicate identities fail closed. Targeted
tests must use literal titles and static `it` / `describe` declarations. The command
inventory also recognizes explicit `test(...)` in the `.mts` fixture and requires
its exact top-level proof title. Both inventories use this harness’s scoped
`tsconfig.json`, which includes `.mts` files and future calibration fixtures
without changing the agent project. The default inventory retains the five old
files; `node e2e/worker-uid/inventory.mjs command` emits the separate command
inventory. `expected.json` / `results.xml` and `command-expected.json` /
`command-results.xml` are checked separately. The JUnit
checker requires each leaf exactly once and
rejects skip, failure, error and missing results. Node and container failures,
including exit 77 and watchdog timeouts, also fail the target even when the checker
fails too. Container stderr stays visible. Reports remain in a unique directory
under `.uzi/scratch/`, printed for diagnosis.

Failures identify the leaf file and full suite/leaf names, and report each local
or inherited skip, failure or error with its originating suite or testcase.
JUnit messages, types, text and stacks are preserved; empty diagnostics say
`no reason supplied`. Node 24 JUnit includes assertion and cancellation reasons,
so no additional reporter is needed.

`task test:worker-uid-harness` runs the Docker-free checker, shell wrapper and
required-check regression tests. The wrapper tests stub Node inventory/pattern,
Docker and timeout, use the real checker, and verify status reporting, stderr,
scratch reports and cleanup of exactly the wrapper’s owned resources. Both lanes
have status, skip, missing-report, assertion-failure and watchdog regressions.
The scoped inventory regression is Docker-free:

```sh
node --import ./agent/node_modules/tsx/dist/loader.mjs --test --test-concurrency=1 --test-timeout=120000 e2e/worker-uid/inventory.test.mts
cd agent && npx tsc --noEmit -p ../e2e/worker-uid/tsconfig.json
```

The required task runs the scoped typecheck and inventory regression through
`test:worker-uid-harness`; it retains the existing CI job and aggregate check.

Selection uses escaped, anchored full leaf names from that inventory. Any extra
executed leaf fails; unrelated leaves may only be absent or skipped. The normal
host shards continue to own the other tests in these files. This nine-leaf
selection proves only the selected capture behavior under the worker UID;
acceptance of the full capture suite remains a separate full-capture run.

CI runs the base-only lane on every main push and on PRs changing agent sources,
this harness, Taskfile or the CI workflow. The existing required `test-agent`
aggregator accepts a skipped lane only when a successful PR path filter explicitly
reports no relevant changes. Image-validation builds retain their existing role.

[Issue #2397: partial investigation](issue-2397-investigation.md) records the
available evidence and remaining diagnosis work.
