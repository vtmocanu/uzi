# Code cross-check replay corpus

A frozen, disclosed synthetic corpus for the maintainer-run offline replay harness
(`agent/src/code-cross-check-replay.ts`, PRD 3167). The cases measure the code cross-check on
reconstructed inputs. They do not replay the original runs exactly.

## Provenance

- The original run branch commits are absent. Each pre-fix ("prefix") state is reconstructed from a
  squash merge on `main` by reverting only the named post-check fix hunks from the squash diff.
- Issue texts and plans are synthetic paraphrases written from the squash commit messages and diff
  scope (`plan_source` is `synthetic`; a plan is never a historically approved plan). Milestones are
  always `[]`: requirements stated in an issue stay in the issue input and are not evidence of
  approved milestones. #3063's squash CHANGELOG bullet cites #2991, so its cases use 2991 as the issue id.
- Sanitization (no usernames, emails, hosts, quotes, co-author lines) applies to `issue.md` and
  `plan.md`. Patches and witness blobs are byte-exact public repository content.
- Every case is `input_provenance: reconstructed`. Reports separate reconstructed-input from
  recorded-input cases.

## Cases

| case | change (squash) | base | kind | reversal |
|---|---|---|---|---|
| d-2888 | #2926 (issue #2888) | `374343d0` | defect | recovered |
| d-3063 | #3063 | `976724a5` | defect | inferred |
| d-2887 | #2933 (issue #2887) | `9190ca59` | defect | inferred |
| c-2904 | #2904 (issue #2886) | `5f819891` | control | none |
| c-2926 | #2926 (issue #2888) | `374343d0` | control | none |
| c-3063 | #3063 | `976724a5` | control | none |

Hunks removed from the squash to build each prefix (`removed.patch` is the exact reverse):

- d-2888 (recovered, the squash message names the follow-up fix): in `agent/src/client.ts`
  `readBoundedText`, the 3-line finally-block hunk (`void reader.cancel()` with its comment) goes back
  to the base `await reader.cancel()`; in `agent/test/client-checkpoint-publication.test.ts`, the
  `describe("source cancellation that never settles")` block. The publishCheckpoint hunks and the
  CHANGELOG entry stay.
- d-3063 (inferred from the squash bullet "require ids in grouped finding file requests"): one line in
  `web/src/lib/apiTypes.ts`, `FindingGroupFileRequest.ids` returns to `ids?: string[] | null;`. Other
  hunks of that bullet (mock and client typing, contract test) are ambiguous and stay.
- d-2887 (inferred): the cleanup-failure precedence test in `agent/test/fixture-repo.test.ts`: the
  `cleanupFails`/`cleanupFailure` handling and `fs.rmSync` mock and restore in
  `scratchNormalizationFailureChild`, its ledger if/else (back to the single path), and the trailing
  `keeps the ${operation} error when removing ...` tests. Reversed by hand to the shape of the sibling
  `normalizationFailureChild`.
- Controls: the prefix tree is the exact merged squash tree and `removed.patch` is empty. c-2904's
  "nothing mandatory" status comes from the #2148 three-run assessment
  (https://github.com/vtmocanu/uzi/issues/2148#issuecomment-6163933542), and its squash contains later
  in-PR commits. c-2926 and c-3063 are the fixed counterparts of d-2888 and d-3063.

## Detectability

- D3 (d-2888): read-only; the awaited cancel is pre-existing base code (`in_diff` false).
- D4 (d-3063): read-only; the optional `ids` line is in the diff.
- D1 (d-2887): read-only test gap; the missing test is in the diff.
- D2 (#2887, macOS raw-root calibration) is `execution_only` and is not reconstructed: it cannot be
  derived from the squash without inventing a state. It is excluded, and the Linux-only calibration
  commit stays applied. Execution-only defects never count toward read-only detection scoring.

## Pins and verification

`manifest.json` (hash pinned in source as `CORPUS_MANIFEST_SHA256`) lists each `case.json` hash and
the `pins.json` hash. `pins.json` holds the corpus version, the codex and claude brief hashes, the
reader implementation and the credential mode. The harness verifies all of them, every artifact
sha256, witness blob ids (git blob id and sha256), the prefix tree and commit (rebuilt with a fixed
identity), that the removed patch restores the squash tree, and that the prefix patch equals
`git diff --no-ext-diff --no-textconv --no-color --abbrev=8 <base> <prefix_commit>`. There is no
upstream-SHA override and no second source-selection mechanism.

Snapshots carry no original history (one parentless commit). The corpus test's object-dependent layer
verifies the pins and reads prefix-tree blobs through `verifyCasePins`; full snapshots are exercised by
`verify`/`replay` and the fixture tests. That layer is skipped only in a shallow checkout where every
pinned object is absent; it fails on partial presence, and `UZI_REPLAY_CORPUS_REQUIRE_OBJECTS=1` makes
it fail instead of skip. CI's agent test job checks out shallow, so in CI only the hermetic layer runs;
the pin layer runs in full clones (and fails instead of skipping with that variable set). Enabling it in
CI needs a maintainer workflow change outside this corpus.

## Scoring limits

Expected-defect matching (path, line, keyword) is a scoring proxy, not proof that a finding correctly
diagnosed the defect. The match patterns require a defect-specific combination of terms with word
boundaries rather than one generic word; they are still a proxy, and a correct finding phrased
differently can still be missed. The expected list is not exhaustive. Unmatched and control findings
are unadjudicated, never confirmed false positives. The maintainer reviews the private findings and
may supply an adjudication file before verified false-positive counts are published.

## Replacing a case

A maintainer may replace a case with recorded artifacts (`input_provenance` and `plan_source`
`recorded`) and re-pin before the baseline. Replacing inputs after a baseline requires rerunning the
old baseline for those cases.

## Maintainer follow-up

Outside issue #3357 (unnumbered): run the authorized real-model baseline and post the sanitized
aggregate on #3167. See [the replay docs](../../docs/code-cross-check-replay.md) for the commands.
