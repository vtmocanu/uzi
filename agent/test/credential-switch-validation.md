# Live Codex switch: recovered validation record

This is a partial evidence ledger for issue #4202, recorded on 2026-11-17.
It does not declare m3 or m4 complete. Earlier scratch logs were absent after
recovery; historical commit messages are not treated as passing checks.

Candidate tested: 73e662c846ac9df92356dd61831967167c44d8ae.
The primary worktree remained unchanged during the controls below. Both mutants
were applied only to disposable exports of this exact candidate, with provisioned
agent dependencies linked into each export. No dependencies were installed.
Each export was removed after its command exited.

## Historical refusal and proof classification

Command:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts
```

At that revision, lines 25735–25738 in `enterCredentialSwitch` read:

```ts
    if (this.isCodexCapture(claim, flight)) {
      this.retainCaptureSource(flight);
      runLog.warn("Codex credential switch refused; source and session retained");
      return "retained_stop";
```

The retained entry also refuses at lines 27130–27133. These are refusals of
the whole operation, not a prior allowed-switch ordering. The following two
proofs are **ordering controls**, class (c) of the owner proof-method amendment.
They are not pre-feature baseline evidence.

## Real Runner ordering controls

Both exports first ran `npm run typecheck` from their agent directory, exit 0.
Each then ran:

```sh
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='M2 actualRunner .*account resumes saved thread' \
  test/codex-executor.test.ts
```

The cases exercise the actual Runner/Codex switch path: subscription to API key,
API key to subscription, and subscription to subscription, each during factory
preparation and an already running body.

| Control | Exact candidate change | Observed result |
| --- | --- | --- |
| Reconciliation ordering | Move the existing awaited `a.reconcileA(bounds, remaining())` ready check from immediately after executor suspension to after the rollout-persistence try/catch. | Exit 1; 0 passed, 6 failed; assertion `A-reconcile-ready precedes A-persist`. |
| Now drain ordering | Move the existing awaited `executor.suspendCodexEpoch!(bounds)` call ahead of `nowSummary?.suspendCodexSwitch(...)`, rather than after its awaited drain. | Exit 1; 0 passed, 6 failed; assertion `Now switch suspension precedes executor suspension`. |

The unchanged candidate then ran that same command: exit 0, 6 passed, 0 failed.
The controls therefore demonstrate sensitivity to these two specific call
relocations through the real Runner path, not merely through a helper.

Candidate anchors (line numbers at the tested SHA): Runner ordering at
`agent/src/runner.ts:17386`; completed A reconciliation assertions at
`agent/test/codex-executor.test.ts:33259`; Now drain observations at
`agent/test/codex-executor.test.ts:33857`.

Initial experimental setup failures were excluded: a relocation between
`try` and `catch` did not compile, and an export without its scratch directory
failed with ENOENT. Neither counts as regression evidence.

## Additional checks observed on this candidate

From agent:

```sh
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='M2 actualRunner credential replacement|Codex repeated actualRunner switching' \
  test/codex-executor.test.ts
```

Exit 0: 85 passed, 0 failed, 211491.665 ms. This includes repeated A→B→C
switching plus the actualRunner replacement matrix. It is a focused run,
not the full agent suite.

From the worktree root:

```sh
UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent
git diff --check HEAD
```

Both exited 0. The task's observed stages were dependency check, lint,
deadcode and TypeScript checks.

## Outstanding evidence

This ledger is deliberately limited to checks re-observed after recovery.
It does not replace the frozen requirement-to-verification map, transactional
and LiveDB evidence, the rest of the required regression controls, independent
committed-range review, `task test:codex-m4`, or m4 security and contract gates.
Those items remain to be reconciled or executed; no scope is waived here.
Full suites remain pending exact-head PR CI under the approved boundary.
Live cross-account thread resume with two real accounts remains owner acceptance,
unverified and non-closing.
