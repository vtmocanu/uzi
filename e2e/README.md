# M6 end-to-end harness

`run-e2e.sh` stands up an **isolated** uzi stack and drives the whole
agent-runtime path with **dummy credentials** and the **stub executor** — no
live Anthropic session and no real GitLab. It is the first true M1↔M5
integration (the per-milestone tests each used their own fake for the other
side; this exercises the real wire).

## Run it

```sh
./e2e/run-e2e.sh
```

Requirements: Docker + Compose v2, and `openssl`, `jq`, `git`, `curl` on the
host — plus **`go`** (the PRD #97 M2 CLI leg builds `api/cmd/uzi` on the host and
drives the running api with it). **Docker Engine >= 25** is recommended: the overlay's healthchecks set
`start_interval: 1s` (probe every second during `start_period`), which trims the
first-probe floor off the full-stack `--wait` boots. Older engines silently ignore
the field, so the suite still runs correctly — just a few seconds slower per boot.
First run builds the `api`/`web`/`agent` images plus the tiny `forge-fake` image
(a few minutes); later runs reuse the layer cache. On success it prints
`All E2E checks passed.` and tears everything down.

Knobs (env vars):

- `KEEP_RUNDIR=1` — keep the per-run scratch dir (certs, env-file, the local git
  remote, container logs are reachable via compose) for post-mortem.
- `KEEP_STACK=1` — do NOT tear down on exit; leave the whole stack (containers +
  volumes + rundir) running so the auditor can inspect logs, the claim-payload
  path, and the worker's `/data` against the live run. Prints the manual teardown
  command.
- `UZI_E2E_COMPOSE_PROJECT=<name>` — override the compose project name (default
  `uzi-e2e-$$`).
- `E2E_RUN_DIR=<dir>` — override the scratch dir (default `$TMPDIR/uzi-e2e-$$`).
- `UZI_E2E_COMPLETE_TIMEOUT=<seconds>` — override how long to wait for the run to
  reach `completed` (default `90` for the stub, `1800` for the `sdk` executor).
- `UZI_E2E_EXECUTOR=sdk` — the OPTIONAL live capstone (see below).
- `E2E_FORGE_POLL_INTERVAL=<dur>` — the api's poll cadence (overlay default `24h`;
  the MR-close phase sets `2s` internally — together with `FORGE_RECONCILE_EVERY=2` —
  and recreates the api). The per-tick deadline is floored at 2x the forge HTTP
  timeout (issue #139), so it no longer collapses to the interval: a short cadence
  keeps its speed without cancelling an in-flight sync or losing an autopilot comment.
  Overlay-only; the production default is untouched.

Phase registry knobs (PRD #966 M2 — the driver reads these):

- `E2E_ONLY=<glob[,glob]>` — run only the phases whose slug matches one of these
  comma-separated globs (e.g. `E2E_ONLY='mr-*,happy-*'`). `critical: yes` phases
  (boot seed, worker-online, happy path) always run regardless, so a subset still
  boots a usable stack.
- `E2E_SKIP=<glob[,glob]>` — the inverse: skip the phases whose slug matches. As
  with `E2E_ONLY`, critical phases are never skipped.
- `E2E_STRICT_LEAKS=1` — make an end-of-phase quarantine **LEAK** (a non-terminal
  run left behind and not declared via a `handoff:` header) a **FAIL** instead of a
  non-fatal note, so a leaked run reddens the suite.
- `E2E_FAULT_PHASE=<slug>` — inject a `fail` as the first statement of that phase,
  the live positive control for fail-soft: a non-critical target lets the suite
  continue (exactly one FAIL, exit 1), a critical one stops it (rest SKIP, exit 1).
- `E2E_FAULT_PREFLIGHT=1` — init the fake bares **without** `--shared=0777`, the live
  positive control for `00-preflight`: its first assertion then FAILs naming
  `core.sharedRepository` (the #372 cross-uid push-race invariant). Because
  `00-preflight` is `critical: yes`, the suite stops and every later phase is SKIP.

Results and artifacts (written under the rundir, `$RUNROOT`):

- `results.tsv` — one tab-separated row per phase: `slug`, status
  (`PASS`|`FAIL`|`SKIP`|`LEAK`), seconds, message.
- `junit.xml` — one `testsuite`, one `testcase` per phase (a `<failure>` carries the
  `fail` message; `<skipped>` for a SKIP).
- `summary.md` — the phase table, the tightest `wait_*` margins, and any leaks.
- `artifacts/<NN-slug>/` — captured **only** for a FAIL or LEAK phase (nothing on a
  PASS/SKIP): `phase.log` (the phase's own stdout), one `<service>.log` per service
  (`api`, `agent`, `forge-fake`, `db`, via `docker compose logs --since`), `runs.txt`
  (the run enumeration) and `run-counts.txt` (runs by status × kind). Capture is fully
  guarded, so it can never change a phase's recorded status or the suite exit code.

Fail-soft behavior: a red run now prints `summary.md` (every failing phase, not
just the first) to stdout **before** teardown, and the suite exits `1` iff any
phase FAILed (a LEAK alone exits 0 unless `E2E_STRICT_LEAKS=1`).

Evidence on red (PRD #966 M3): a **red** run — any FAIL, or any LEAK (which writes a
`$RUNROOT/.keep-rundir` sentinel even though a non-strict LEAK exits 0) — **keeps its
rundir** instead of `rm -rf`-ing it, and `cleanup` prints the retained path and the
`artifacts/` path so the per-phase evidence above survives for post-mortem (and #967's
upload). A green run with no `KEEP_RUNDIR` is removed as before.

## Live-DB candidate-selection test (`run-store-it.sh`)

The MR-close watcher's candidate-selection query (`ListMRWatchCandidates`) is the
one piece with no live-DB coverage from the fake-store unit tests. `run-store-it.sh`
spins up a **throwaway Postgres**, applies the real migrations, and asserts
candidate selection directly — including **rework suppression** (a non-completed
latest run yields no candidate) and the **no-superseded-MR-fallback** rule (a
latest *completed* run with a NULL `mr_iid` yields none, never falling back to an
older run's MR). It is isolated (unique container + loopback port, torn down on
exit) and self-contained:

```sh
./e2e/run-store-it.sh
```

The Go test (`api/internal/store/mr_watch_integration_test.go`) skips cleanly under
a plain `go test ./...` when `UZI_TEST_DATABASE_URL` is unset. The full PRD #24 gate
is `./e2e/run-e2e.sh` **and** `./e2e/run-store-it.sh`.

## Phase registry (what it asserts)

The suite is now a **phase registry**: each assertion group lives in its own file
`e2e/phases/NN-<slug>.sh`, sourced in `NN` order by a fail-soft driver
(`e2e/driver.sh`). The driver runs each phase in an errexit-safe subshell, records
a PASS/FAIL/SKIP/LEAK verdict, and keeps going after a non-critical failure so one
red phase no longer discards every downstream verdict. `./e2e/run-e2e.sh --list`
prints the table below (it reads only the phase-file headers, so it needs no
docker). The table is generated — `task check:e2e-registry-doc` fails if it drifts
from the phase files, and `task e2e:registry-doc` regenerates it. Edit the phase
headers, not the rows.

<!-- registry:begin (generated by `task e2e:registry-doc`; do not edit by hand) -->

| NN | slug | lane | critical | title |
|---|---|---|---|---|
| 00 | preflight | any | yes | PRD #966 M3: preflight — harness invariants (#366 dubious-ownership / #372 cross-uid push race) |
| 05 | lane-forgejo | forgejo | no | PRD #65 M9: the Forgejo lane (UZI_E2E_FORGE=forgejo) |
| 06 | lane-github | github | no | PRD #238 M8: the GitHub lane (UZI_E2E_FORGE=github) |
| 10 | least-privilege | gitlab | no | PRD #5 privilege checks: over-privileged connect is rejected + stored nothing; compliant connection is least-privilege |
| 11 | skills-authz | gitlab | no | PRD #16 skills authz: a non-admin cannot reach admin / other-user surfaces |
| 12 | cancel-queued | gitlab | no | cancel path: a queued run is cancelled server-side (no live poller) |
| 13 | worker-join-online | gitlab | yes | issue a worker join token and bring the worker online |
| 14 | worker-resource-stats | gitlab | no | worker self-reports container CPU/memory stats (PRD #49) |
| 15 | happy-path-restart | gitlab | yes | happy path: create a PRD issue and start a run |
| 16 | usage-limit-park | gitlab | no | PRD #35: opt-in park -> sweeper promotes -> resume SKIPS the gate -> completes |
| 17 | live-ws-cookie | gitlab | no | PRD #97 M2: a live /api/ws subscription receives a run_message frame during a run (not REST replay) |
| 18 | cli-smoke | gitlab | no | PRD #97 M2: uzi CLI drives the live api (run list matches + approve advances a run) |
| 19 | live-ws-bearer | gitlab | no | PRD #112 M1: a Bearer (uzc_) /api/ws subscription receives a live run_message frame |
| 20 | git-push-basic-auth | gitlab | no | PRD #97 M1: worker pushes the agent branch over git-over-HTTPS Basic auth (default coverage) |
| 21 | protected-branch-refused | gitlab | no | PRD #97 M1: the fake remote refuses a push to main under BOTH transports (protected-branch backstop) |
| 22 | steer-queue-delivery | gitlab | no | PRD #95: steer-queue delivery — Queued -> Delivered on consume, no run_message, no forge/token |
| 23 | secret-hygiene | gitlab | no | secret-hygiene assertions |
| 24 | xff-trust-boundary | gitlab | no | PRD #58: XFF forgery from the agent container must NOT mint fresh rate-limit buckets |
| 25 | uid-boundary | gitlab | no | PRD #51 M6: uid-boundary regression assertions (live image, setpriv-to-uid) |
| 26 | plan-reject-verbatim | gitlab | no | PRD #33: live plan reject with a verbatim reason -> verbatim failure_reason back through the worker |
| 27 | mr-close-watcher | gitlab | no | PRD #24: MR-close watcher (Human Review <-> In Progress on MR close/reopen) |
| 28 | skills-templates-tools | gitlab | no | PRD #16: skill delivery (builtin allocated -> claim -> synthesized plugin dir) |
| 29 | autopilot-lifecycle | gitlab | no | PRD #19 autopilot: map + opt-in the repo owner |
| 30 | autopilot-carry-item | gitlab | no | carry-item: concurrent cross-key settings PUT — the FOR UPDATE serialization rejects the equal-label race |
| 31 | ci-status-fix | gitlab | no | PRD #6: CI status sync + Fix CI + the verification stamp |
| 32 | agent-mr-fix-crosskind | gitlab | no | PRD #6: agent-MR same-branch fix + cross-kind race |
| 33 | uzi-eligibility-gate | gitlab | no | PRD #764: uzi run-eligibility gate (no PRD link required + Promote) |
| 34 | vault | gitlab | no | PRD #32: per-user vault (dek sealing, claim gating, restart lock, lazy rewrap) |
| 35 | judge-funnel | gitlab | no | PRD #46: run judge (stub) — funnel enqueue -> claim -> review -> persist-first notification |
| 36 | file-forge-issue | gitlab | no | PRD #68: file a forge issue from a judge recommendation |
| 37 | printed-instructions-menu | gitlab | no | PRD #98 M8c: printed instructions EXECUTED verbatim from the emitting command's own output |
| 38 | closed-issue-poller | gitlab | no | PRD #98 M8b/B6': a closed forge issue reaches Done THROUGH THE POLLER (M6's wiring) |
| 39 | review-row-cap | gitlab | no | PRD #98 M8b/B4': the server's row cap, and the truncation remedy executed against it |
| 40 | chat-agent | gitlab | no | PRD #39: in-app chat agent (stub) — create -> read(red-team) -> propose -> confirm -> dismiss -> idle -> continue |
| 41 | interleave-stream | gitlab | no | PRD #43 M5: interleaved multi-agent stream persists + replays (gapless seq, per-agent attribution) |
| 43 | plan-revision-loop | gitlab | no | PRD #41: plan-revision loop (revise_plan -> re-plan -> re-park -> approve -> MR) |
| 44 | ask-user-clarification | gitlab | no | PRD #88: ask-user clarification (park -> answer -> resume -> approve -> MR) |
| 45 | bounded-concurrency | gitlab | no | PRD #42 bounded-concurrency scenario (stub-only) |
| 46 | run-health | gitlab | no | PRD #47: run-health detection (stall / loop / in-flight suppression) |
| 47 | rate-limit-meters | gitlab | no | PRD #53/#104: per-token rate-limit meters (seeded gauge row -> /me) |
| 48 | auto-selection | gitlab | no | PRD #111 M6: an auto worker picks the emptiest pooled token, and degrades safely |
| 49 | docker-sidecar | gitlab | no | PRD #83 M2: rootless DinD sidecar + Decision-3 efficacy |
| 50 | worker-token-binding | gitlab | no | PRD #104: a worker's Anthropic binding reaches the claim payload; a rebind lands on the next claim |
| 51 | auto-stop-poison | gitlab | no | PRD #108 M5: auto-stop kills a run whose messages can't be saved (direct-to-API poison) |
| 60 | schedules-sweep | gitlab | no | PRD #966 M4: scheduled Planned-sweep (catalog enable, run-now tallies, uzi-gate skip, open-MR skip) |
| 61 | schedules-prompt | gitlab | no | PRD #966 M4: scheduled prompt run (issue-less repo->MR run via run-now) |
| 62 | self-improve | gitlab | no | PRD #966 M4: scheduled self_improve run (vault-unlocked, tracking issue, MR opened) |

<!-- registry:end -->

## How the fakes are wired (no real GitLab, no live session)

- **`forge-fake`** (`e2e/forge-fake/`) serves, over HTTPS on `forge-fake.e2e:443`,
  the GitLab v4 subset the api (verify/list/get/create issue, **single-MR GET**)
  and the worker (create/list merge request) call. It applies issue label
  add/remove **faithfully** (so a card move survives a poller reconcile), exposes
  an `/_e2e/mrs/<iid>/state` mutator to flip an MR's state (the PRD #24 reviewer
  stand-in), and records what it saw at `/state/state.json` — **persisted on a
  bind mount** so the restart-resilience `down`/`up` does not make it forget
  issues/MRs (a real forge wouldn't). HTTPS is real: the api trusts the
  self-signed cert via `SSL_CERT_FILE`, the worker's `fetch` via
  `NODE_EXTRA_CA_CERTS`, so the worker's https-only MR guard is genuinely
  exercised.
- **The git remote** is a local bare repo the harness seeds on the host; the
  worker reaches it because a `url.<local>.insteadOf` gitconfig rewrites the
  https clone/push URL the api hands out. The worker's clone → worktree → commit
  → **push** path runs for real against it; the branch-pushed assertion reads the
  bare repo directly. This local-path leg stays the fast, hermetic default for
  every phase (and is the only transport that keeps `repo.git`/`repo2.git` as two
  independent bares, which the PRD #42 two-repo concurrency phase requires).
- **git-over-HTTPS Basic auth is exercised on EVERY default run** (PRD #97 M1). A
  local-path push ignores all `http.*` config, so it never sends the worker's
  `Authorization: Basic` header — the exact blind spot the shipped
  `PRIVATE-TOKEN`-vs-Basic auth bug slipped through. So one dedicated leg drops the
  `insteadOf` rewrite for a single `group/repo` run and lets the worker fetch+push
  against `forge-fake`'s real git-smart-HTTP endpoint, which **401s any git op
  lacking a valid `Authorization: Basic` (`uzi-bot:PAT`)**. A run that reaches
  `completed` with its branch on the bare therefore proves the worker sent Basic; a
  credential-injection regression turns the run red. (The opt-in
  `E2E_GIT_SMART_HTTP=1` variant routes the *whole* suite over smart-HTTP; because
  `forge-fake` collapses every repo path to one bare, that variant's #42 phase
  asserts against the one shared bare — the default run keeps the full independent-
  bare check.)
- **`main` is never touched — and the fake remote enforces it** (PRD #97 M1). Each
  seeded bare carries a `pre-receive` hook that refuses `refs/heads/main`, so the
  "push to main is refused" assertion is real, not vacuous. The backstop is
  self-tested under **both** transports (the hook fires in the agent image for a
  local-path push and in `forge-fake` for smart-HTTP, since the bare is one shared
  host dir): a `main` push is refused, a non-`main` branch push is accepted.
- **The executor** is the M2 stub with `UZI_STUB_PLAN_GATE=1`, so it drives the
  full M4 plan gate (emit plan → `awaiting_approval` → await verdict → implement)
  with no SDK.

Isolation: a unique compose project (`-p`), a per-run `--env-file` and scratch
dir, a random published web port, and `down -v` on exit. The user's own `uzi`
stack and the repo `.env` are never touched.

## Optional live capstone (user-gated — NOT part of the milestone gate)

Per the testing-credentials policy, M6 is provable on dummy creds + stub; a live
run is optional and **only the user triggers it**. To flip the harness to the
real Claude Agent SDK:

1. In `e2e/docker-compose.e2e.yml`, set the api's `UZI_SEED_ANTHROPIC_TOKEN` to
   **your existing** token (do NOT run `claude setup-token` / mint one).
2. Run with the SDK executor:

   ```sh
   UZI_E2E_EXECUTOR=sdk ./e2e/run-e2e.sh
   ```

The plan-gate + restart + cancel structure is identical; only the "work" step
becomes a real agent turn. No milestone assertion depends on this path.

Because a real agent turn takes minutes (not the stub's seconds), the completion
wait defaults to `1800`s under `UZI_E2E_EXECUTOR=sdk` (vs `90`s for the stub). The
seeded template spawns a reviewer subagent, so long turns are by design: an
observed live capstone took ~13 min (780s, 34 turns). Override with
`UZI_E2E_COMPLETE_TIMEOUT=<seconds>` if your turn runs longer.

Note: the base `docker-compose.yml` now wires `UZI_SEED_ANTHROPIC_TOKEN` from
`.env` into the api service (an M6 fix — it was missing, so the token boot-seed
was inert on a fresh stack and every real SDK run failed fast with "no Anthropic
token"). A normal `docker compose up` with the var set in `.env` seeds the token;
the E2E overlay sets it directly, so the harness does not depend on `.env`.
