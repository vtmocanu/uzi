# Worker UID test lane

`task test:agent:worker-uid` builds the base worker image and runs the eight ownership
leaves from four test files through its root entrypoint, which drops to the worker UID with runner
group membership and the production ambient capabilities. It runs offline, with
disposable data and Nix mounts, and removes only its own named container.

To reuse an image, set `WORKER_UID_SKIP_BUILD=1` and `WORKER_UID_IMAGE=<image>`.
`WORKER_UID_TIMEOUT` controls the outer watchdog (default 600 seconds).
The host needs Docker, Node, Python, GNU timeout and the locked agent development
dependencies. Test inputs are mounted into `/app`, preserving their relative paths
and resolving runtime packages from the image.

Before execution, the locked TypeScript parser derives the leaf inventory from
the current source. Each of the four files must contain its recognized UID gate
and at least one leaf. Targeted tests must use literal titles and static `it` /
`describe` declarations. The JUnit checker requires each leaf exactly once and
rejects skip, failure, error and missing results. Node and container failures,
including exit 77, also fail the target. Reports remain in the printed temporary
directory for diagnosis.

Selection uses escaped, anchored full leaf names from that inventory. Any extra
executed leaf fails; unrelated leaves may only be absent or skipped. The normal
host shards continue to own the other tests in these files.

CI runs the base-only lane on every main push and on PRs changing agent sources,
this harness, Taskfile or the CI workflow. The existing required `test-agent`
aggregator accepts a skipped lane only when a successful PR path filter explicitly
reports no relevant changes. Image-validation builds retain their existing role.
