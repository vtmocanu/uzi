# codex-m4 maintainer acceptance record

Parent #1106 M4 acceptance requires **maintainer-only evidence the uzi worker cannot produce**:
a fresh packaged snapshot below the REAL Go supervisor on a Landlock-capable runtime, run on the
actual merge-candidate images. That evidence is gated by `task check:codex-m4-receipts`, the
lead's pre-merge gate, deliberately **not** folded into `gate:agent` / `test:codex-m4` so an
owed packaged proof never reddens the worker's ordinary gate.

For PR #1309, the packaged O proof and strict macOS Linux-container run are complete. Current-head
GitHub CI after the recovered evidence-only commits remains the final acceptance item before parent
M4 is marked complete.

## Candidate evidence (2026-09-13)

- Proven runtime revision: `d054eace7ebb0dcb8dbdb56eef4e40daa737c243`.
- Packaged proof: [GitHub Actions run 34757302169](https://github.com/vtmocanu/uzi/actions/runs/34757302169), both jobs green at 19/19 tests.
- `base` digest: `sha256:d818a9b17848cdf01e77527e510ed6b576ebacd339691e8793fa6481037d0260`.
- `jvm` digest: `sha256:586fa43a4d277d601d0c737f01c8ee66ea0fa12075e8935f7f0fd4cb6430e4a5`.
- Both images reported `tests=7`, `callbacks=1`, `delegations=1`, `roots=5`; each real-path record
  reported nonzero provider turns, callbacks, delegation, roots, checkpoints, signals and
  finalization, with API-key refresh equal to zero.
- The real manifest bound all three O clauses to that proof and passed
  `task check:codex-m4-receipts` at recovered head `704eadad` with an evidence-only tail.
- `task gate:agent` passed at `704eadad` on macOS, including the offline container P suite at 5/5
  and the 53-result `[claude/U, codex/U, codex/P]` completeness matrix.
- Independent reviewer and security-auditor passes found no blocking issue.

The machine manifest remains gitignored as the lead's out-of-band attestation. This committed record
preserves the proof coordinates and image digests for long-term audit.

## The gate, redesigned around a fixed point

The committed O registry (`e2e/codex-m4/clauses-codex.ts`) now holds **stable requirements
only** — each O row is `inherited` (a prior source proves the same unchanged mechanism) or
`owed` (no proof exists; the maintainer owns running it). It carries **no image digest and no
`receipt-present` disposition**. All candidate-specific evidence — the two proven merge-candidate
image digests and the per-clause discharging records — lives in a **trusted, out-of-band,
gitignored manifest** at `e2e/codex-m4/receipt-manifest.json` (schema template committed at
`e2e/codex-m4/receipt-manifest.example.json`; never commit the real one).

This replaces the earlier design, which pinned candidate digests directly into
`clauses-codex.ts` and required `manifest.candidateCommit == git rev-parse HEAD` — a design with
**no fixed point**: committing a digest changes HEAD, and because the worker Dockerfiles `COPY .
/opt/uzi-src` and stamp `UZI_SRC_SHA`, changing HEAD changes the image digest too, so the
just-committed digest could never match the image it named.

`task check:codex-m4-receipts` now certifies the **current candidate** (HEAD, or
`CODEX_M4_CANDIDATE_COMMIT`) iff:

1. the manifest is present, well-formed, and its proven `base`/`jvm` digests are real
   `sha256:<64-hex>` values;
2. the manifest's `provenBaseCommit` is an **ancestor** of the candidate;
3. the `provenBaseCommit..candidate` tail is **evidence-only** — every changed path classifies as
   evidence (`e2e/`, `prds/`, `docs/`, `adr/`, `specs/`, `.claude/`, `agent/test/`, `Taskfile.yml`,
   or any `*.md`); a runtime path (anything under `agent/` except `agent/test/`) or an
   unrecognized path fails closed;
4. every committed O clause has **exactly one** discharging manifest record — no missing, extra,
   or duplicate;
5. each record's `images` match the manifest's proven pair — no mismatch, no base/jvm swap;
6. a committed `owed` clause is discharged **only** by a `receipt-present` record — an `inherited`
   record can never discharge it (that would falsely claim a prior proof that does not exist). A
   committed `inherited` clause accepts either disposition.

Because the manifest is gitignored, **recording it makes no commit**, so it can never invalidate
the very candidate it certifies — that is the fixed point. Practically: the lead can reuse an
**already-successful** packaged proof (e.g. the base+jvm proof that succeeded for commit
`d054eace` in GitHub Actions run 34757302169) by setting `provenBaseCommit=d054eace` and letting
any number of evidence-only rework commits sit in the tail, with **no edit to
`clauses-codex.ts`** and no circular commit.

At the exact tested revision (`9d680c93`), with no manifest supplied (the default at this
revision, intentional), `check:codex-m4-receipts` **exits non-zero by design**:

```
run-receipts: manifest unusable [absent]: candidate manifest absent — supply CODEX_M4_RECEIPT_MANIFEST (fail closed, D8)
run-receipts: 3 committed O clause(s) with no manifest record (undischarged):
  - codex-o-command-root-home-denial
  - codex-o-descendant-code-mode-host-absence
  - codex-o-packaged-descendant-reaping
```
(Verbatim `run-receipts.ts` output at revision `9d680c93`; exit status 1.)

This is bookkeeping, not a gap left unnoticed: every committed O row is `inherited` or `owed`
(D3/D8), and the ordinary worker gate accepts a well-formed `owed` record while this separate
lead gate rejects it until a discharging manifest record exists. Do **not** mark parent M4
complete, and do **not** move `prds/done/1287-codex-guardrail-conformance.md` to `prds/done/`, until
every row below is resolved.

## Stable O requirements

### 1. Inherited row: discharged by the candidate manifest

**`codex-o-command-root-home-denial`** (File policy: command-root HOME/credential OS
separation) cites `e2e/codex-m3b/lifecycle.test.ts` Block B (PRD #1171 m5) as its prior proof,
and D8's unchanged-mechanism argument holds: C1-C5 change no `agent/codex/**`
supervisor/fileop/launcher code, and the C3 D6 repair is a policy-only `extraSecretPaths`
addition. Its committed row carries no digest — it needs only a matching `inherited` (or a fresh
`receipt-present`) record in the manifest, bound to the proven image pair. No edit to
`clauses-codex.ts` is required.

### 2. Owed rows: discharged by the fresh packaged proof

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

Because these are committed `owed` clauses, the manifest must discharge each with a
`receipt-present` record (never `inherited` — the gate rejects that as a disposition mismatch).

## How to discharge

1. **Obtain the two proven image digests** (`base` and `jvm`) from an already-successful packaged
   proof for a chosen `provenBaseCommit` — e.g. the base+jvm proof that already succeeded for
   commit `d054eace` (GitHub Actions run 34757302169) — or run
   `UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged` on both merge-candidate images and read
   their digests (e.g. `docker image inspect --format '{{.Id}}'`). This is the same both-image
   packaged run `e2e/codex-m3b/README.md` documents; it is CI/maintainer-only because it needs
   native image builds plus a Landlock-capable kernel (in-worker image builds are storage-flaky
   and arm64 is unsupported for this proof — the worker builds no image and needs no cluster
   credential).
2. **Write the manifest** at `e2e/codex-m4/receipt-manifest.json` (gitignored, never committed —
   `e2e/codex-m4/receipt-manifest.example.json` is the committed schema template) with:
   - `provenBaseCommit` — the full proven-revision commit hex (40 or 64 lowercase hex).
   - `images.base` / `images.jvm` — the two real `sha256:<64-hex>` digests from step 1.
   - `provenance` — a non-empty description of how the proof was produced (cite the run URL/
     command/date).
   - `clauses` — one record per committed O clause: `codex-o-command-root-home-denial` recorded
     as `inherited`; `codex-o-descendant-code-mode-host-absence` and
     `codex-o-packaged-descendant-reaping` recorded as `receipt-present` (each needs a fresh
     packaged snapshot below the real Go supervisor on a Landlock-capable runtime). For PR #1309,
     Actions run 34757302169 supplied that proof. Each record's `images` is the same proven pair
     from step 1.
3. **Confirm the tail is evidence-only.** `provenBaseCommit` must be an ancestor of the current
   HEAD, and the `provenBaseCommit..HEAD` diff must touch only evidence paths: `e2e/`, `prds/`,
   `docs/`, `adr/`, `specs/`, `.claude/`, `agent/test/`, `Taskfile.yml`, or any `*.md`. A change
   under `agent/` (except `agent/test/`) is a runtime change and requires a FRESH proof at a new
   `provenBaseCommit` — it cannot ride an old proof's fixed point.
4. **Run the gate** (mind `dir: agent` in the Taskfile, so use an absolute manifest path via
   `--show-toplevel`):
   ```sh
   CODEX_M4_RECEIPT_MANIFEST="$(git rev-parse --show-toplevel)/e2e/codex-m4/receipt-manifest.json" \
     CODEX_M4_CANDIDATE_COMMIT=$(git rev-parse HEAD) \
     task check:codex-m4-receipts
   ```
   and confirm it prints `run-receipts: OK — proven base <provenBaseCommit> → candidate <HEAD>;
   evidence-only tail; every O clause bound to the trusted manifest (none drifting, missing,
   extra, duplicate, undischarged, mismatched, or disposition-mismatched).`
5. **No edit to `clauses-codex.ts` is needed.** The old "pin digests into `clauses-codex.ts`" step
   — the one that created the circular commit — is gone; that is the whole point of the redesign.
6. For PR #1309, the strict macOS Linux-container run is complete. Current-head CI
   `test-agent` confirmation after the recovered commits remains lead-owned.
7. Only then tick parent #1106 M4 and move `prds/done/1287-codex-guardrail-conformance.md` to
   `prds/done/`.

## Also required before parent M4 acceptance

- **The strict macOS Linux-container invocation** (D7 point 3): reproducibly callable as
  `task test:codex-m4:macos` (and `task gate:agent` / `task test:codex-m4` auto-route the
  Linux-only P leg through the same pinned container on macOS). The runner
  (`executeMacosLinuxRun` in `macos-linux-runner.ts`) runs the COMPLETE strict P suite (all
  three P files — startup-smoke, policy-real, native-bypass) inside the pinned container —
  `docker.io/library/node:24-bookworm@sha256:6dac556d980b7f0e5498d08f08cee0ca67798b4ad6c23964a9214920e67758d0`
  (multiarch-verified amd64/arm64) — mounting only the disposable fixture/test inputs, network
  disabled during tests, no Docker socket or maintainer HOME mounted, and hard-fails on missing
  Docker or an unsupported architecture (never a silent platform skip). `e2e/codex-m4/README.md`
  documents the reproduction commands; the worker unit-tests the plan builder, the orchestrator
  and the hard-prerequisite gate (`macos-linux-runner.test.ts`). The actual macOS run passed for
  PR #1309 at recovered head `704eadad`; future candidates repeat it when the runner or P suite changes.
- **Current-head CI `test-agent` confirmation.** The worker has no CI run URL for the exact
  tested revision to cite honestly; the lead owns confirming CI is green on this revision (or
  the revision that supersedes it) and recording the URL/counts.

## Trust boundary

The gate TRUSTS the out-of-band manifest (the maintainer produced it from a real
`UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged` proof). It verifies the manifest's
internal consistency (well-formed, real image digests), full coverage (every committed O clause
has exactly one discharging record whose digests match the proven pair and whose disposition
matches the committed requirement), and no runtime drift in the tail. It does **not** — and
cannot, from inside the worker — verify that the digests were truly built from
`provenBaseCommit`; that assurance comes from the maintainer actually running the packaged proof
that produced the manifest.

No real provider credential, cluster access, or deployment rollout is required for any of the
above — every step here is the existing test/proof machinery pointed at real packaged images.
