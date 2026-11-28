# PRD #4812: Owner-curated eval holdouts

**Status**: Draft  
**Priority**: Normal  
**Umbrella**: #4808  
**Blocked by**: #4809, #4810

## Problem

Read-only reviewers can assess the diff. Owners also need to measure whether a
candidate satisfies a specific behavior with tests the implementation agent could
never inspect. The holdout and the candidate code require a separate execution
boundary from the credentialed worker and from grading.

## Outcome

1. An owner attaches a holdout to an unstarted eval for open issue 42. On its
   pinned base the declared regression assertion fails. CLI verification reports
   verified red and the latest revision can be confirmed. A missing offline base
   dependency returns a setup error and confirmation remains blocked.
2. An eval's model review passes two trials. The first passes the holdout and
   counts as passed. The second conflicts with the overlay and counts as
   ungraded. The owner report shows each model and holdout outcome separately.
3. An ordinary worker advertises the same capability as a holdout worker.
   The API refuses the claim and bundle fetch. Once the hosted profile is
   enabled, only the controller-provisioned worker bound to the exact holdout
   attempt and pod can run it and return a bounded result.

## Out of scope

Web UI (#4814), Compose and manually registered holdout workers, mining tests
from closed issues or PRs, arbitrary archives, runtime dependency downloads,
privileged or networked suites, and holdout data in model grades. Draft
publication is independent. Implementation and validation leave
`.github/workflows/**` untouched.

## Modules and seams

- Add `eval_holdouts` and `eval_holdout_results`. Store one immutable bundle
  version selected for a start. A replacement before start invalidates any
  verification receipt. Store verdict, reason, and exact evidence coordinates in
  results. Scope each upload, verify, retry and result read to the owning eval.
  Use ownership checks on both lookup and mutation. Return metadata only from
  read endpoints. Never copy the record into run context or recovery payloads.
- Accept a versioned JSON manifest with UTF-8 test files, safe relative paths,
  separate build and test argv, a selected image digest, and one declared
  `classname.name` assertion. Cap the request and decoded bundle at 2 MiB,
  with 64 regular files, 256 KiB per file, 240 bytes per path, and at most
  32 entries of 512 bytes per argv. Reject duplicate paths, traversal,
  special files, symlinks, and build or dependency configuration overlays.
  Use `http.MaxBytesReader` before decoding. Recommend holdout-specific
  filenames that avoid paths the implementation is expected to modify.
- Encrypt bundle and command at rest with `secretbox.SealWithAAD`, binding owner,
  eval, version, and digest. The owner verify action queues work. Decrypt only
  for authorized current holdout custody. Keep plaintext and paths out of user
  read responses, prompts, traces, logs, notifications, caches and recovery
  archives. Retire ciphertext on eval deletion or seven days after terminal
  state. Keep summary results and document backup retention separately.
- Verify red on the exact base with the same launcher, overlay, command and
  image digest used for the trials. Bind the verification receipt to bundle
  digest, base SHA, image digest and declared assertion. On confirm, recheck
  the receipt and draft revision transactionally and verify issue eligibility.
  A bundle or image change bumps the revision and invalidates the receipt.
  If the base cannot build offline, report `base_setup_failed` and direct the
  owner to an approved image containing its offline prerequisites.
- Materialize an exact base using `GitCache.runnerCloneAtCommit` as a starting
  point, or reconstruct the frozen trial tree and verify its digest and
  `tree_sha`. Export the tree without history into private attempt storage.
  Apply the overlay without following symlinks or changing any candidate-touched
  path or directory topology. Replace a base file only after matching its
  expected content. Mount the resulting tree read-only.
- Add an admin-only holdout image allowlist through the API and CLI
  (`uzi admin holdout-image add|list|remove`); its web admin page ships with the
  Evals tab (#4814). Admins allowlist immutable image digests. Owners choose one approved digest
  before verifying an eval. Use that exact image's embedded tools and offline
  dependency inputs exclusively. The controller prepares the pinned image
  before releasing the bundle. No registry credential or daemon socket enters
  the sandbox. Do not provision dependencies through a repo tool manifest.
  Revalidate the digest and its allowlist entry before releasing a bundle.
- Add repo-ful, issue-less `eval_holdout` to the run-kind registry, DB constraint,
  and agent/web mirrors. A run verifies one pinned base or tests one frozen
  trial artifact. Every predicate is stated (A's rule): `Supporting` true, `Listed` false,
  `PlanningCapable` false, `WallTimed` false (its own bounded deadline),
  `JudgeEligible`, `CodeCrossCheckable` and `PlanCrossCheckable` false; it is
  ineligible for model work, normal claim routes, publication and archive
  recovery. The worker role `eval_holdout_v1` is assigned server-side at
  provisioning and is never accepted from a worker's own capability list. Allow one outstanding
  holdout run per eval and one sandbox per pod.
- Provision an ephemeral hosted pod via the controller, following
  `EphemeralProvisioner.provisionOne` and the run binding in
  `CreateEphemeralHostedWorker`. Provision a server-assigned holdout-only role,
  private one-run join identity, and exact `ephemeral_run_id`. Bind claim,
  bundle retrieval and result writes to the authenticated worker, owner,
  exact run, claim generation and pod UID observed and authorized by the
  controller. Worker-supplied pod identity and advertised capabilities alone
  grant no authority. On worker loss, fence the old attempt before replacement
  or terminal settlement. Exclude the role from ordinary claims, warm leases,
  hibernation and worker reuse. Preserve server-side checks against an ordinary
  worker using its own otherwise-valid join identity. Compose refuses holdouts.
- Reuse the hosted rootless DinD renderer (`withDinD`, `dindContainer`,
  `WorkerDinDRootless`) with a private daemon and socket per holdout pod. The
  daemon UID differs from the worker service UID. Use one fresh, non-root
  container with `--network=none`, read-only root, private PID view, all
  capabilities dropped, no new privileges, seccomp, no persistent logs, and no
  access to worker secrets, credentials, service account material, or sockets.
  The only data mount is the staged tree. Scratch and test output use bounded
  container-local writable storage. The worker uses fixed launcher arguments
  and an explicit executable search root; it never interpolates owner argv.
  Rootless operation still trusts the Kubernetes node, kernel and container
  runtime. The worker and daemon remain part of the trusted execution boundary.
- Enforce CPU, memory and PID limits at the pod/container cgroup boundary even
  where nested Docker flags are unavailable. Start with 1 CPU, 2 GiB, 128
  processes for the execution envelope, 180 seconds, 512 MiB writable scratch,
  and a 256 MiB staged tree. Refuse pod shapes or nodes that cannot enforce the
  declared envelope. Bound queued provisioning and retries. On cancel, expiry,
  or loss, revoke bundle/result authority, stop the pod, and delete the scoped
  volumes and join material. If the controller cannot prove teardown, record it
  and hold retry until reconciled. A pod is never reused.
- Read only JUnit XML from a per-attempt random path in a fresh output
  directory, chosen by the launcher and passed to the owner's test argv at
  start. Two guarantees follow: a report pre-seeded by the candidate at a
  conventional path (such as `/out/junit.xml`) is ignored, and the report must
  agree with the test command's exit status (declared case failing with a
  non-zero exit, or passing with a zero exit), else the result is ungradable
  `inconsistent_result`. The report is a bounded regular file of at most
  64 KiB. Residual, recorded: candidate code imported by the tests runs in the
  test process, can read the supplied path and can tamper with that run; the
  holdout reduces, not removes, that risk. Parse a bounded subset of JUnit XML with no
  external references. Identify the declared case by exact `classname.name`;
  reject ambiguity, malformed XML, missing or duplicated case, skipped case,
  `<error>`, or zero cases as setup failure. Base verification is red only if
  the declared case is present with `<failure>` after the build succeeds.
  A completed passing case on base is unexpectedly green. Store only status,
  closed-enum reason, immutable input coordinates, and elapsed time. Never
  persist raw XML, process output, file names, or assertion text in results.
- Apply the following trial result mapping after reconstructing the verified tree.

  | Evidence | Holdout result |
  |---|---|
  | Declared case passes after successful build | pass |
  | Declared case contains `<failure>` | fail |
  | Candidate fails compilation with the verified offline inputs | fail |
  | Overlay conflicts with candidate changes | ungradable, `overlay_conflict` |
  | Candidate introduces a dependency absent from the selected image | ungradable, `dependency_unavailable` |
  | Timeout or OOM | ungradable, `timeout` or `oom` |
  | Missing, invalid or unreadable report, or other setup failure | ungradable, `setup_failed` |

  Use the separate build phase to attribute compile failures. If evidence
  cannot distinguish setup or dependency failure from compilation, return
  ungradable. Grade and trial claims never receive holdout files, commands,
  paths, raw output, or results as a model input.
- When a holdout is attached, a trial passes only when B's review rule passes and
  the holdout passes. A valid holdout assertion failure yields fail when the
  review evidence is complete. A pending or ungradable holdout stays separately
  visible and makes a completed trial ungraded. Preserve implementation failure
  as its own terminal outcome. Evals without a holdout keep B's rule.

## Testing decisions

Use synthetic bundles and a Docker-client fake in-run. Verify effective launch
configuration, staged-tree and overlay mapping, pinned image admission, bounded
JUnit parsing, and teardown requests on cancellation, timeout, and worker loss.
Test the API with forged capability advertisements, wrong run, worker, attempt
and pod identities, and a stale result after fencing. Assert that the holdout
sentinel is absent from normal API DTOs, grader/trial inputs, logs and archives.

The holdout feature ships default off behind the admin-only instance setting
`eval_holdouts_enabled`, the one switch in this effort (umbrella #4808). The owner tracks #4815, the linked
`acceptance` issue for the real hosted proof. Before enabling, measure CPU,
memory and PID enforcement and scratch bounds, verify network and mount
isolation, and prove pod, volume and join-material teardown after normal
completion, cancellation and worker loss. Verify a stale pod cannot retrieve
the bundle or submit a result. Verify a report pre-seeded at a conventional path
(such as `/out/junit.xml`) is ignored, and that an XML/exit-status mismatch
returns `inconsistent_result`. This is a release condition with the owner,
separate from the in-run fake-based acceptance. Use existing hosted test
entry points and leave workflow files untouched.

## Milestones

### M1. Verify a holdout against the pinned base

**Blocked by** #4809 and #4810.

Ship `eval_holdout`, the hosted profile, the admin image allowlist, encrypted
bundle storage, and `uzi eval holdout set|verify`. A holdout draft can verify
on an enabled profile. Admission of a holdout-enabled eval starts with M2.
Acceptance: fake the supported hosted pod and Docker client to exercise
examples 1 and 3 through verification. Missing offline dependencies are setup
failure. A changed bundle invalidates verification. An ordinary worker and a
stale pod receive no bundle, and the capability defaults off.

### M2. Check frozen trial artifacts and report results

**Blocked by** M1.

Require a matching receipt at confirm. Execute the verified holdout against
eligible frozen trials and show its separate result and joined pass rule
through CLI and API. Add hosted setup, images, release evidence and retention
to `docs/evals.md`, run `task docs:sync`, and add a shipping changelog fragment.
Acceptance: examples 1 and 2 end to end using the fake seam. Cover every
mapping row, atomic start racing replacement, and stale result/cancel races.
Document the owner-held live isolation issue as the gate to enablement.

## Landing obligations

- full agent suite: exact-head PR CI

## Decision Log

- Use owner-curated file manifests to make extraction, validation and secrecy
  auditable. Only the grader-independent holdout compartment reads the tests.
- Reuse run-bound hosted provisioning and the private rootless sidecar. The
  controller assigns holdout custody. Effective-limit and teardown evidence
  on the real profile gates enablement.
- A verified negative baseline distinguishes a regression assertion from setup
  failure. The same frozen artifact and runner profile underpin the candidate result.
