---
paths:
  - "agent/**"
---

# agent (Node 24 + tsx, Claude Agent SDK worker)

Loaded when you touch `agent/`. The repo-wide map is the root `CLAUDE.md`.

## Commands

```sh
task gate:agent        # deps-check + lint + deadcode + typecheck + test
task lint:agent        # lint slot alone (oxlint; NOT ratcheted)
task deadcode:agent    # dead-code slot alone (knip)
task test:agent        # node --test via tsx
task typecheck:agent
cd agent && node --import tsx --test --test-timeout=120000 test/worker.test.ts   # single file — no Task target
```

- knip gates every rule at `error`, including the unused-export/type family (#597): a new unused export reddens `task deadcode:agent`. A missing knip loud-SKIPs (exit 0) locally, exits 2 in CI (`UZI_DEADCODE_AGENT_REQUIRED=1`).
- Carry `--test-timeout=120000` on the single-file form; `agent/package.json`'s `test` script has it, and a single-file recipe that differs from the gate can return a different verdict. The flag does bind: a 3s body under `--test-timeout=1000` is cancelled.

## `--test-timeout`

- What the flag caps differs by node major: local node v26 shares a process across files and caps each top-level suite, while CI's `node:22-alpine` runs a child process per file and caps the whole file. Read any `--test-timeout` as the tightest thing it might bind, and check which node the SLOWEST environment runs — one Taskfile target cannot make local and CI agree here.
- Split a test file that approaches the cap rather than raising 120000: node's per-file cap makes a large file a serialization point no timeout value fixes.
- A cap kill is reported `cancelled`, not `fail`, so the summary reads `fail 0` on a red job and the TAP plan shrinks with the FILE named in place of its remaining suites.
- Settle an unexplained failure by re-running one unchanged commit, rather than inventing a mechanism from the diff.
- Read the durations before assuming which cause you have: `test/judge-runner.test.ts` unrefs a 60s timer, load-bearing for wall time but not for the cap (an idle timer holding the event loop open is not body duration), while `agent/test/fake-api.ts` records the other one, a leaked listening handle on musl with every subtest passing.

## `npm ci` / `npm install` in `agent/` clobber the host's `agent-browser`

`agent/package.json` pins `agent-browser` (0.35.1) and that package's `postinstall` rewrites `/opt/homebrew/bin/agent-browser` into whatever `node_modules` just installed it, over the brew formula's symlink (0.31.1 here). Both commands do it, so adding a devDependency triggers it too; delete the worktree afterwards and the CLI is off `PATH` host-wide with a dangling link.

- npm 11.17's `npm warn allow-scripts N packages have install scripts not yet covered by allowScripts:` naming `agent-browser` is advisory: the postinstall ran anyway.
- `web/` is not exposed: `agent-browser` is in `agent/package.json` and in neither `web/package.json` nor `web/package-lock.json` (which does carry 4 `hasInstallScript` packages, esbuild x3 and fsevents, none of them this one).
- When you must write `package.json` and the lockfile, use `npm install --ignore-scripts --save-dev --save-exact <pkg>@<version>`; a repo `.npmrc` with `ignore-scripts=false` does NOT override the CLI flag (`agent/src/js-deps.ts`). It skips every package's install scripts, so anything needing a genuine native build stays unbuilt — recover by reinstalling that one package.
- Otherwise do not install at all: a validator needing deps in a throwaway worktree should symlink `node_modules` from a long-lived worktree. No install, no postinstall, no clobber, and faster than `npm ci`.
- Do not assume the `main` checkout has a `node_modules` to borrow. Symlink from any sibling worktree whose `agent/package-lock.json` is byte-identical to yours (`shasum` both first; a mismatch is a version skew the symlink would silently import), or run `npm ci --ignore-scripts` in the `main` checkout to populate a borrow source.
- The tell is silent: a clobbered link still resolves while the worktree exists, so `agent-browser --version` answers happily and differs only in the version printed (npm 0.35.1 vs brew 0.31.1). The check that discriminates is `ls -l /opt/homebrew/bin/agent-browser` — target under `/opt/homebrew/Cellar`, or under somebody's `node_modules`?
- Repair with `brew unlink agent-browser` then `brew link --overwrite agent-browser`. Each alone fails: `brew link --overwrite` answers "Already linked" and refuses; `brew unlink && brew link` removes 0 symlinks and then refuses because a file is in the way. The repair does not hold — the next `npm ci` in `agent/` undoes it.
- To drive a browser without that cycle, call the Cellar binary directly: `/opt/homebrew/Cellar/agent-browser/<version>/libexec/bin/agent-browser`, which no npm postinstall touches.

## Reading `node --test` output

- It prints `ℹ fail 0` while tests are failing, when they fail by TIMEOUT: the failures surface under `✖ failing tests:` and go uncounted in the tally, while `$?` is 1 throughout. Read the exit code and the named failing tests, never a bare tally. Mirror image of the `PASS=0` trap in `.claude/rules/go.md`.

## Worker runtime

- The guardrail DENIES reading the process environment (bare `env`), the process table (`ps` / `pgrep`) and `/proc` — `agent/src/guardrails.ts`, `REASON_ENV` / `REASON_PS` / `REASON_PROC` — because they leak the worker's join token. A trace that burned a retry on a denied read is showing the environment, not a broken tool.
- The base image is `node:24-alpine` (`agent/templates/base/Dockerfile`), but do not assume BusyBox limits: the baked toolchain at `/opt/uzi-toolchain/bin` is FIRST on the agent PATH and prepends GNU `coreutils` (so GNU flags such as `ls --time-style=full-iso` work) plus `file`, `perl` and others (`agent/devbox-global/devbox.json`, guarded by `toolchain-guard.tsv`).
- Guardrail invariant #6, `settingSources: []`, is enforced by `semgrep/settings-sources-isolation.yml` at every SDK query site under `agent/src/`. It fires on an explicit widening (`settingSources: ["project"]`, or a variable in that position) but does NOT catch an omitted key, since the SDK default is fail-open and loads every source, including a cloned repo's `.claude/`. Full boundary in [`docs/security-gate.md`](../../docs/security-gate.md).
