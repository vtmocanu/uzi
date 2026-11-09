# A red CI test: attribute it locally, narrowly

When a required job fails on a named test, attribute that test locally before spending a
CI rerun. A rerun costs a full job, a slot under the account's concurrent-job cap, and
often more than an hour, to learn one bit. Local evidence never waives exact-head CI,
another gate, coordinator rules or a user decision the attribution rule in `SKILL.md`
requires.

1. **Classify first.** Read the log; never infer the cause from the job name.
   - Cancelled usually means superseded by a newer push: reconcile the live PR head and
     its run, and never rerun a superseded SHA. Never let an old `main` rerun cancel the
     current head's CI.
   - Infrastructure (a release download 504, a registry or runner error): rerun the failed
     jobs only once the run is complete and still on the intended head. No local run.
2. **Run only the failing tests**, by name, from the CI log. Never the shard, a component
   gate or the full suite by default; one diagnostic whole-file run needs a demonstrated
   cross-test dependency or the user's ask. Use the single-test forms in the root
   `CLAUDE.md` (agent: keep `--test-timeout=120000`), under the local test caps, one heavy
   job at a time, behind a background poller (*Waiting, uniformly*).
3. **Pin both sides.** The PR head, and the base it actually incorporates: the merged-in
   `main` commit, or a recorded conservative merge-base for a multi-merge branch. Each runs
   in its own detached worktree, with the same runtime, toolchain, configuration and
   selection; record the revisions and every difference. Share one dependency tree only when
   manifests and locks match and the deps check passes. For a dependency-changing PR, use
   each side's own pinned tree and name the dependency as the variable. Same run count on
   both sides, alternating order: about 5 for a timing or bounded-wait assertion, 1 to 2 for
   a deterministic one.
4. **Require execution.** Count a run only when the named cases actually RAN: check the exit
   status and the tally, with zero unexpected skips or cancellations. A filter matching zero
   cases, a fixture setup failure or a broken instrument is not green. A test the host
   cannot run (verified, e.g. a Linux-only lane on macOS) is local NOT RUN; use an already
   supported equivalent lane, or rely on CI and say so. Never add privileges or
   infrastructure for this.
5. **Read the result as evidence, not proof.** Compare the matching failure (same
   assertion, error and lane) and the rates separately.
   - Red on head only: the PR is suspect. Call it PR-caused only with causal evidence (a
     controlled reproduction, revert or mutant); otherwise keep investigating or fix.
   - Red on both: the failure exists on the base. The PR may still aggravate it; compare rates.
   - Green on head, red on base: note it; it says nothing about the CI red.
   - Green on both: not reproduced. Rerun the failed CI job once (step 1's conditions).
     A pass shows the head is intermittent, not that the failure is pre-existing.
   - `unknown` is a valid verdict.
   An unresolved second matching CI failure blocks normal landing. A different new red is
   its own attribution, not proof of either cause. Before filing a flake, search open and
   recent issues by the test's path.
6. Go failures: finish with `S/attribute-failures.sh`. Record the verdict, revisions and
   counts in the trail (`ci red(<test>): head N/M, base N/M (<sha>) → <verdict>`).
