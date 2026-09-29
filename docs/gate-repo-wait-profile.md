# Issue #1896: gate:repo wait profile

Recorded before changing tests, on 2026-09-29. These figures are baselines, not a claim that the gate is green on this worker.

## CI gate baseline

GitHub Actions main run `36543347387` (commit `0de32d9c`), `lint-repo` job `109323761573`, step `gate:repo (shell + yaml + formula + actions + secrets + sast)`: 08:32:57–08:36:43 UTC, **226 seconds**. Source: the job's GitHub Actions `steps[].started_at` and `completed_at`. Compare a PR run's same step, not whole-job duration, for the 40-second acceptance target.

## Worker script baseline

The checked-out worker has BusyBox `timeout` and `env`, so `e2e/codex-git-trust/run.test.sh` exits 2 at its GNU `timeout --foreground` preflight; it was **not run locally**. `actionlint` and `zizmor` are absent. The pinned gitleaks Go module is unavailable offline. These limits prevent a complete local `task gate:repo`.

| Script | Seconds | Result | Log |
| --- | ---: | --- | --- |
| `e2e/lib.test.sh` | 12.91 | 20/20 pass | `.uzi/scratch/gate-log.DbJApp` |
| `scripts/golangci-lint.test.sh` | 12.54 | pass | `.uzi/scratch/gate-log.kjJePo` |
| `.agents/skills/uzi-lander/scripts/cr-rate-limit.test.sh` | 13.16 | pass | `.uzi/scratch/gate-log.FhjBNd` |
| `.agents/skills/uzi-lander/scripts/watch-pr.test.sh` | 73.04 | pass | `.uzi/scratch/gate-log.kGanLC` |

Scratch log names identify this run's evidence and are not durable across a fresh worker. The other named lander tests were timed in the planning probe on this worker: `takeover.test.sh` 25.15 s, `pr-findings.test.sh` 45.47 s, and `land-prep-basemove.test.sh` 21.91 s. That probe also measured `watch-pr.test.sh` 80.97 s; the repeat above shows substantial local fixture-time variation.

## Wait map

- `e2e/lib.test.sh`: stale or nonterminal `wait_regated` cases can exhaust the 2-second timeout while polling every 0.3 s. `failed` and `cancelled` cases exit early when those states are read. The never-terminal `settle_runs_terminal` case consumes its 5-second timeout while polling every 0.5 s. The existing `regate` helper checks pass/fail, so a faster timeout test should explicitly prove the timeout branch; the settle test must retain multiple reads.
- `scripts/golangci-lint.test.sh`: the forced-timeout case configures a 4-second child-readiness delay but cancels it after five 20 ms polls; this protects the short-timeout branch without paying four seconds. The INT case waits for its 4-second marker to prove readiness beyond the former 2-second cap. Both configurations remain. The two 5-second watchdog sleeps are failure ceilings normally cancelled after the subject exits. A fake sleep already records the retry interval without waiting.
- `.agents/skills/uzi-lander/scripts/cr-rate-limit.test.sh`: two real `/bin/sleep 1` calls separate ask and reply timestamps to one-second resolution. Its ordinary poll sleep is already stubbed.
- `.agents/skills/uzi-lander/scripts/watch-pr.test.sh`: a fake `sleep` exits immediately; its measured time is repeated fixture commands, not a fixed wait.
- `.agents/skills/uzi-lander/scripts/takeover.test.sh`, `pr-findings.test.sh`, and `land-prep-basemove.test.sh`: source inspection found no real fixed sleep in their tests; the base-move test builds local Git fixtures.
- `e2e/codex-git-trust/run.test.sh`: the ignored-TERM case waits out the subject's 10-second `--kill-after` grace. Its 15-second deadline and fake Docker's 60-second loop are failure bounds. Production retains the 10-second grace; only a test-local GNU timeout wrapper may compress the observed grace.
