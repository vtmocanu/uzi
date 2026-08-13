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

## What it asserts

1. **Boot seed** provisions the admin, the (dummy) Anthropic token, and the forge
   connection + one enabled repo — deterministically, no registration race.
2. **Cancel path**: a run with no live poller is cancelled server-side
   (`queued → cancelled`).
3. **Token → online**: a worker join token is issued via the API and the worker
   registers and reports `online`.
4. **Happy path**: create a PRD issue → start a run → the run halts at
   `awaiting_approval` with a plan → approve → the worker pushes
   `agent/issue-N` to the remote and opens an MR → `completed` with `branch` +
   `mr_iid`.
   - **Repo agents, detect→choose→apply (PRD #37)**: the seeded repo ships a
     `.claude/agents/` roster (`repo-coder`, `repo-reviewer`), so the parked run
     reports `repo_agents` at the gate (detection is executor-independent — the
     stub exercises it). The harness then approves with a structured `selection`
     (`source: repo`, excluding `repo-reviewer`) and asserts the completed run
     persisted `agent_source=repo` + `agent_exclusions=[repo-reviewer]`.
5. **Restart-resilience**: `docker compose down && up` (keeping volumes) while the
   run is parked at the gate; the orphaned run is re-queued, re-claimed, and
   driven to completion, with a **gapless** `run_messages` seq across the
   restart.
6. **Secret hygiene**: the bot PAT, the Anthropic token, and the worker join
   token appear in **no** container log and **nowhere** on the worker's `/data`.
7. **`/proc` hardening (M6) + uid boundary (PRD #51 M4/M5)**: the join token is
   delivered by file — the `worker_token` Docker secret at
   `/run/secrets/worker_token` (same path prod uses), so it is absent from every
   process's `/proc/<pid>/environ`; and the entrypoint forces that secret to
   `0400 worker:worker`, so a `setpriv`-to-`runner` read is **denied** while a
   `setpriv`-to-`worker` read **succeeds** — the runner uid, which runs the
   untrusted agent/checks/provision, cannot read the worker's credential. See
   [../docs/proc-hardening.md](../docs/proc-hardening.md).
8. **MR-close watcher (PRD #24)**: with the poller sped to ~2s, closing the
   completed run's MR *without merging* (via forge-fake's `/_e2e` mutator) moves
   the card **Human Review → In Progress**; reopening restores it
   **In Progress → Human Review**; and a **manual drag wins** — after the card is
   dragged to another column, reopening the MR does not fight the placement (the
   reopen edge's source-column guard backs off). The candidate-selection SQL is
   covered separately against a real Postgres — see `run-store-it.sh` below.
9. **Bounded worker concurrency (PRD #42, stub-only, last phase)**: the single
   worker is reconfigured to `WORKER_MAX_CONCURRENT_RUNS=2` and enables a **second**
   repo (`group/repo2`, served by the fake only when `FORGE_FAKE_PROJECT2` is set).
   Two runs on the two **different** repos are then shown to run **genuinely
   concurrently** — both reach `awaiting_approval` at once, which a cap-1 worker
   cannot (a slot is held across the gate), on the **same** worker — the API worker
   listing reports `active_runs=2`/`max_concurrent_runs=2` while both are live, both
   land MRs on **independent** git bare-caches with no message cross-talk, and a
   **mid-run `SIGKILL`** of the agent re-queues **both** in-flight runs together (the
   sweeper's worker-loss recovery at N=2), after which a restarted worker re-claims
   both by affinity and completes them. **Stated limit**: the stub executor is
   already concurrency-safe, so this exercises the worker-loop + server + API path,
   **not** the M1 per-run executor kill/reap isolation fix (guarded by an `agent/`
   unit test).
10. **Live `/api/ws` + `uzi` CLI (PRD #97 M2)** — two full-wire-only consumers no
    lower layer reaches. **WS**: the agent's Node 22 global `WebSocket` subscribes to
    `/api/ws?run=<id>` (the browser's primary real-time transport; every *other*
    stream assertion here uses the REST `?after=<seq>` replay), authenticating with
    the admin **session cookie** plumbed into the upgrade — a GET upgrade behind
    `RequireUser`, with a same-origin (CSWSH) check and per-run authz. It subscribes
    **first**, then approves the parked plan on socket open, and asserts it receives a
    live `run_message` frame (hub-broadcast → client wire, not REST replay); a
    **no-cookie** upgrade is separately asserted to be **rejected**, so the gate is
    non-vacuous. A third leg (PRD #112 M1) repeats the subscribe over a **Bearer
    `uzc_` token with no `Origin` header** — the headless `uzi tui` shape — approving
    from inside the socket over the same token, with a **bogus-Bearer** rejection as
    its own negative control; that is the only place both credential classes are
    driven against one route. **CLI**: `api/cmd/uzi` is built on the host and pointed at the live
    api, authed headless via a minted `uzc_` `$UZI_TOKEN` (from `POST
    /api/me/cli-tokens`); `uzi run list --json` must parse and its run-id set must
    equal `GET /api/runs`'s, then `uzi run approve` drives a parked run to
    `completed` — so DTO/route drift in either consumer turns the run red.

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
