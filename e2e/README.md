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
host. First run builds the `api`/`web`/`agent` images plus the tiny `forge-fake`
image (a few minutes); later runs reuse the layer cache. On success it prints
`All M6 E2E checks passed.` and tears everything down.

Knobs (env vars):

- `KEEP_RUNDIR=1` — keep the per-run scratch dir (certs, env-file, the local git
  remote, container logs are reachable via compose) for post-mortem.
- `UZI_E2E_PROJECT=<name>` — override the compose project name (default
  `uzi-e2e-$$`).
- `E2E_RUN_DIR=<dir>` — override the scratch dir (default `$TMPDIR/uzi-e2e-$$`).
- `UZI_E2E_EXECUTOR=sdk` — the OPTIONAL live capstone (see below).

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
5. **Restart-resilience**: `docker compose down && up` (keeping volumes) while the
   run is parked at the gate; the orphaned run is re-queued, re-claimed, and
   driven to completion, with a **gapless** `run_messages` seq across the
   restart.
6. **Secret hygiene**: the bot PAT, the Anthropic token, and the worker join
   token appear in **no** container log and **nowhere** on the worker's `/data`.
7. **`/proc` hardening (M6)**: the join token is delivered by file
   (`UZI_WORKER_TOKEN_FILE`), so it is absent from every process's
   `/proc/<pid>/environ`, and its delivery file was unlinked after read. See
   [../docs/proc-hardening.md](../docs/proc-hardening.md).

## How the fakes are wired (no real GitLab, no live session)

- **`forge-fake`** (`e2e/forge-fake/`) serves, over HTTPS on `forge-fake.e2e:443`,
  the GitLab v4 subset the api (verify/list/get/create issue) and the worker
  (create/list merge request) call, and records what it saw at
  `/tmp/forge-fake-state.json` (read for the MR assertion). HTTPS is real: the
  api trusts the self-signed cert via `SSL_CERT_FILE`, the worker's `fetch` via
  `NODE_EXTRA_CA_CERTS`, so the worker's https-only MR guard is genuinely
  exercised.
- **The git remote** is a local bare repo the harness seeds on the host; the
  worker reaches it because a `url.<local>.insteadOf` gitconfig rewrites the
  https clone/push URL the api hands out. The worker's clone → worktree → commit
  → **push** path runs for real against it; the branch-pushed assertion reads the
  bare repo directly.
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
