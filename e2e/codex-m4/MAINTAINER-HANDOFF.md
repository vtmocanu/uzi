# codex-m4 maintainer handoff — the O-layer acceptance parent #1106 M4 still owes

Parent #1106 M4 acceptance requires **maintainer-only evidence the uzi worker cannot produce**:
a fresh packaged snapshot below the REAL Go supervisor on a Landlock-capable runtime, run on the
actual merge-candidate images. That evidence is gated by `task check:codex-m4-receipts` — the
lead's pre-merge gate, deliberately **not** folded into `gate:agent` / `test:codex-m4` so an
owed packaged proof never reddens the worker's ordinary gate. The gate no longer checks digest
SYNTAX alone: it BINDS each recorded `{base,jvm}` image digest to a trusted candidate manifest for
the expected merge-candidate commit, and fails closed when the manifest is absent, malformed,
built for the wrong commit, missing an image, swapped (base recorded as jvm or vice versa), or
mismatched (see "Trust boundary" below). At the exact tested revision (`089b6184`), with no
manifest supplied (the default at this revision, intentional), `check:codex-m4-receipts` **exits
non-zero by design**:

```
run-receipts: manifest unusable [absent]: candidate manifest absent — supply CODEX_M4_RECEIPT_MANIFEST (fail closed, D8)
run-receipts: 2 inherited/receipt-present O row(s) with an unresolved (placeholder) base/jvm digest block merge (D8) — refresh BOTH to the real merge-candidate digests:
  - codex-o-command-root-home-denial [base]
      digest: sha256:PENDING-CANDIDATE-DIGEST-BASE
  - codex-o-command-root-home-denial [jvm]
      digest: sha256:PENDING-CANDIDATE-DIGEST-JVM
run-receipts: 2 owed O assertion(s) block merge (D8):
  - codex-o-descendant-code-mode-host-absence
      target: codex worker image (base + jvm) descendant-reaping snapshot
      reason: descendant/code-mode-host absence needs a fresh packaged snapshot below the REAL Go supervisor on a Landlock-capable runtime; the uzi worker cannot prove a descendant ABSENCE from a direct harness spawn.
      owner:  maintainer (D8: fresh packaged O proof, k8s-first Linux runtime)
  - codex-o-packaged-descendant-reaping
      target: codex worker image (base + jvm) supervisor whole-root reaping snapshot
      reason: whole-root ECHILD(+__WALL) descendant reaping under the REAL Go supervisor is a packaged OS effect; the uzi worker's C4 U cases prove only the registry/safety CONTRACT with injected fake roots, so a fresh packaged snapshot below the real supervisor on a Landlock-capable runtime is required and is maintainer-owned (D8).
      owner:  maintainer (D8: fresh packaged O proof, k8s-first Linux runtime)
```
(Verbatim `run-receipts.ts` output at revision `089b6184`; exit status 1.)

This is bookkeeping, not a gap left unnoticed: every O row is one of `inherited` /
`receipt-present` / `owed` (D3/D8), and the ordinary worker gate accepts a well-formed `owed`
record while this separate lead gate rejects it. Do **not** mark parent M4 complete, and do
**not** move `prds/1287-codex-guardrail-conformance.md` to `prds/done/`, until every row below is
resolved.

## What is owed

### 1. Inherited row — pin BOTH real digests

**`codex-o-command-root-home-denial`** (File policy: command-root HOME/credential OS
separation) cites `e2e/codex-m3b/lifecycle.test.ts` Block B (PRD #1171 m5) as its prior proof,
and D8's unchanged-mechanism argument holds: C1-C4 changed no `agent/codex/**`
supervisor/fileop/launcher code, and the C3 D6 repair is a policy-only `extraSecretPaths`
addition. But its `imageDigests` are still the C1-seeded placeholders
`sha256:PENDING-CANDIDATE-DIGEST-BASE` / `sha256:PENDING-CANDIDATE-DIGEST-JVM` — a merge candidate
ships TWO images (`base` and `jvm`) and one proven image cannot vouch for the other, so
`run-receipts.ts` treats a non-real digest on EITHER image as "unresolved" and blocks the merge
gate exactly like an owed row, naming which image is still unresolved. Citing a real historical
case is not enough on its own.

**Action:** pin BOTH `imageDigests.base` and `imageDigests.jvm` in `e2e/codex-m4/clauses-codex.ts`
to the actual merge-candidate images' `sha256:<64-hex>` digests, and produce the trusted candidate
manifest (below) recording the SAME digests for the SAME commit. The gate now binds each recorded
digest to that manifest, so pasting a real-looking digest into `clauses-codex.ts` is no longer
sufficient on its own — it must match the manifest for the expected merge-candidate commit before
accepting parent M4.

### 2. Owed rows — fresh packaged proof

Two rows have **no prior packaged evidence at all** — a direct harness spawn cannot prove the
absence of a descendant process or the completeness of supervisor reaping, only a real packaged
snapshot below the real supervisor can:

- **`codex-o-descendant-code-mode-host-absence`** (Native execution bypass): no
  `codex-code-mode-host` descendant appears below the supervisor under the shipped stock
  (`code_mode_host=false`) config. M3a control A already proves a code-mode host **does** appear
  when *enabled* — the positive control exists — but its converse absence under the shipped
  config has never been packaged-proven.
- **`codex-o-packaged-descendant-reaping`** (Delegation and cleanup): the real Go supervisor
  reaps a boundary-action/command root's WHOLE descendant set to `ECHILD(+__WALL)` — including a
  backgrounded/process-group-escapee descendant — BEFORE any checkpoint/finalize publication.
  The C4 U lifecycle cases (`codex-u-lifecycle-root-ordering` and friends) prove the
  registry/safety reap-ordering CONTRACT with injected fake roots; this row is the OS-level
  backing proof under the real supervisor that no packaged snapshot has yet run.

**Action:** run the required packaged proof (below) on a Landlock-capable runtime and record a
`receipt-present` O-state for each row (source, image digest, target, `recordedAt`).

## The required packaged proof

```sh
UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged
```

on **both** merge-candidate images (`base` and `jvm`), recording their digests (D8). This is the
same both-image packaged run `e2e/codex-m3b/README.md` documents; it is CI/maintainer-only
because it needs native image builds plus a Landlock-capable kernel (in-worker image builds are
storage-flaky and arm64 is unsupported for this proof — the worker builds no image and needs no
cluster credential).

From that proof, produce the trusted candidate manifest at `e2e/codex-m4/receipt-manifest.json`
(gitignored, never committed — `e2e/codex-m4/receipt-manifest.example.json` is the committed
schema template) recording:

- `candidateCommit` — the full merge-candidate commit hex (40 or 64 lowercase hex; `git rev-parse
  HEAD` on the candidate).
- `images.base` / `images.jvm` — the two real `sha256:<64-hex>` digests the packaged proof
  produced for the candidate's two images.
- `provenance` — a non-empty description of how the proof was produced (e.g. the
  `UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged` invocation, runtime, and date).

Then run the gate bound to that manifest (note `check:codex-m4-receipts` runs with `dir: agent` in
the Taskfile, so use an absolute manifest path via `--show-toplevel`):

```sh
CODEX_M4_RECEIPT_MANIFEST="$(git rev-parse --show-toplevel)/e2e/codex-m4/receipt-manifest.json" \
  CODEX_M4_CANDIDATE_COMMIT=$(git rev-parse HEAD) \
  task check:codex-m4-receipts
```

The gate binds every recorded `{base,jvm}` digest in `clauses-codex.ts` to this manifest for the
expected commit — absent, malformed, wrong-commit, missing-image, swapped (base recorded as jvm or
vice versa), or mismatched all fail closed. It is no longer enough to merely paste a real-looking
digest into `clauses-codex.ts`; the pasted digests must match the manifest for the expected commit.

## Trust boundary

The gate binds each recorded digest to the manifest for the exact expected commit; `provenance` is
a presence-checked FREE-TEXT field, not independently verified. The gate does NOT itself prove the
images exist, were built, or are signed — that assurance comes from the maintainer actually running
the packaged proof that produces the manifest.

Also required before parent M4 acceptance:

- **The strict macOS Linux-container invocation** (D7 point 3): now reproducibly callable as
  `task test:codex-m4:macos` (and `task gate:agent` / `task test:codex-m4` auto-route the
  Linux-only P leg through the same pinned container on macOS). The runner
  (`executeMacosLinuxRun` in `macos-linux-runner.ts`) runs the COMPLETE strict P suite (all
  three P files — startup-smoke, policy-real, native-bypass) inside the pinned container —
  `docker.io/library/node:24-bookworm@sha256:6dac556d980b7f0e5498d08f08cee0ca67798b4ad6c23964a9214920e67758d0`
  (multiarch-verified amd64/arm64) — mounting only the disposable fixture/test inputs, network
  disabled during tests, no Docker socket or maintainer HOME mounted, and hard-fails on missing
  Docker or an unsupported architecture (never a silent platform skip). `e2e/codex-m4/README.md`
  documents the reproduction commands; the worker unit-tests the plan builder, the orchestrator
  and the hard-prerequisite gate (`macos-linux-runner.test.ts`), but actually running it on a
  real macOS host with Docker remains the maintainer's to execute before parent M4 acceptance.
- **Current-head CI `test-agent` confirmation.** The worker has no CI run URL for the exact
  tested revision to cite honestly; the lead owns confirming CI is green on this revision (or
  the revision that supersedes it) and recording the URL/counts.

## How to discharge

1. Identify the actual merge-candidate `base` and `jvm` image digests (post-merge or from a
   candidate build), and the exact merge-candidate commit hex.
2. Run `UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged` against both images; confirm the
   positive per-image Block A counts and the Block B real-path counts described in
   `e2e/codex-m3b/README.md`.
3. Produce the trusted candidate manifest at `e2e/codex-m4/receipt-manifest.json` (schema in
   `e2e/codex-m4/receipt-manifest.example.json`; gitignored, never committed) with
   `candidateCommit`, the real `images.base`/`images.jvm` digests from step 2, and a `provenance`
   description.
4. Update `e2e/codex-m4/clauses-codex.ts`:
   - Set `codex-o-command-root-home-denial`'s `imageDigests.base` AND `imageDigests.jvm` to the
     SAME real `sha256:<64-hex>` values recorded in the manifest (both are required, and both must
     match the manifest for the expected commit — the gate rejects a mismatched or swapped digest).
   - Flip `codex-o-descendant-code-mode-host-absence` and `codex-o-packaged-descendant-reaping`
     from `owed` to `receipt-present` (or a fresh `inherited` citation for a later candidate),
     each with `source`, `imageDigests` (`{ base, jvm }`) matching the manifest, `target`, and
     optionally `recordedAt`.
5. Re-run the gate bound to the manifest:
   ```sh
   CODEX_M4_RECEIPT_MANIFEST="$(git rev-parse --show-toplevel)/e2e/codex-m4/receipt-manifest.json" \
     CODEX_M4_CANDIDATE_COMMIT=$(git rev-parse HEAD) \
     task check:codex-m4-receipts
   ```
   and confirm it prints `run-receipts: OK — every O-layer clause is inherited/receipt-present
   with base/jvm digests bound to the trusted candidate manifest for <commit>; none owed,
   unresolved, or mismatched.`
6. Run the strict macOS Linux-container invocation and confirm current-head CI `test-agent`.
7. Only then tick parent #1106 M4 and move `prds/1287-codex-guardrail-conformance.md` to
   `prds/done/`.

No real provider credential, cluster access, or deployment rollout is required for any of the
above — every step here is the existing test/proof machinery pointed at real packaged images.
