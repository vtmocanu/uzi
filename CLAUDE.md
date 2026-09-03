# CLAUDE.md

Guidance for Claude Code (claude.ai/code) when working in this repository.

## What this is

"Uzinele Întunecate" (uzi): an AI dark factory. Go API + React SPA + PostgreSQL + an opt-in per-user worker container, run via docker-compose on a laptop. Users connect a forge and an Anthropic token; agents work `PRD`-labeled issues end to end (plan → approval gate → implement ⇄ review → branch + MR, never touching `main`).

## Destructive operations

Always loaded: a path-scoped rule fires on a file READ, i.e. after the decision to act, and everything here is irreversible.

- 🔴 **`docker compose -p uzi down -v`, from any directory holding a compose file, destroys real data.** It removes `uzi_pgdata` and `uzi_agentdata`, which carry the real admin and forge data. `cd main && docker compose -p uzi up` brings the stack back; the volumes do not come back.
- **Never pass `-p uzi` to a `down`, and never add `-v` to one.**
- **Never `docker compose down` from a worktree.** Belt-and-braces today: the recorded `config_files` path no longer exists (see the `.env` note in `.claude/rules/stack.md`), so discovery cannot reach project `uzi` and a bare `down` resolves the worktree's own project. Re-creating that file restores the hazard.
- 🔴 **Never glob `uzi-` when tearing down containers.** The dev stack (`uzi-web-1`, `uzi-api-1`, `uzi-agent-1`, `uzi-db-1`) shares a daemon with throwaway test containers, and `uzi-db-1` shares `postgres:17` with them, so neither `--filter name=uzi-` nor `--filter ancestor=postgres:17` can tell them apart:

```
uzi-seam5b-pg    postgres:17   Up 52 seconds   <- throwaway
uzi-final-95941  postgres:17   Up 5 minutes    <- throwaway
uzi-db-1         postgres:17   Up 2 weeks      <- the REAL database
```

1. **Name throwaways OUTSIDE the `uzi-` namespace** (`cdr-*`, `aud-*`, `vm-rev-*`). Load-bearing: it removes the failure mode instead of relying on discipline.
2. **Tear down only your own container, by exact name.** Never a `uzi-*` glob, never `docker compose down` from a worktree.
3. **If you see a container you did not create, leave it.** Same for processes: a stray `run-e2e.sh` or `run-store-it.sh` may belong to another session, and refusing to kill an unowned process is correct, not obstructive.
   - Attribute a process by the **redirected log path alone**, and only when its name is distinctive. Shell-snapshot path is per-CLI-session, not per-agent; cwd is shared across agents in one worktree. Both manufacture a confident false match (measured 2026-08-02).
   - **If you cannot attribute a process, leave it.**

`./e2e/run-store-it.sh` names its container `uzi-store-it-$$`, inside the namespace rule 1 avoids. It is PID-unique and tears itself down by exact name, so it is safe to run; rule 2 holds the line there.

## Commands

Install the pinned `task`, which every command below needs:

```sh
go install github.com/go-task/task/v3/cmd/task@v3.51.1   # pinned, sumdb-verified
```

- Version-matched to CI. `brew install go-task` works but is unpinned and drifts from CI.
- `task` does **not** go in `devbox.json`: that is tier-2 *worker* config whose `packages` array is provisioned into opted-in runs (`agent/src/repo-tools.ts`), not a contributor environment.
- `go install` builds from source, so the binary is not byte-identical to the release tarball CI's `.task_setup` sha256-verifies. Matching `task --version` is the equivalence check, as for `sqlc@v1.31.1`.

### Gate targets

- **`Taskfile.yml` at the repo root is the only place a gate recipe is written.** `task --list` enumerates them.
- `task gate` runs everything, `gate:repo` first: the checks with no component of their own (shellcheck, yamllint, GitHub Actions lint incl. embedded scripts, the Homebrew formula, migration numbering and additivity, spec numbering, no-binary-text, `scan:secrets` with gitleaks, `sast:semgrep`). `task --dry gate:repo` lists them. `gate:repo` / `gate:api` / `gate:controller` / `gate:web` / `gate:agent` run one slice each.
- `.github/workflows/ci.yml` invokes the same targets, mostly per-toolchain (`validate:*`, `lint:api`, `lint:controller`, `test:*`) plus repo-wide `lint:repo`, so local and CI cannot drift. `test:api-store-it` invokes none by design: its ran/skipped assertion is CI-specific.
- Every load-bearing flag lives in `Taskfile.yml` with its reason. Task echoes each command, so `-race`, `-count=1` and `--test-timeout=120000` stay visible; that echo is how you notice one going missing.
- **Component gates run serially, deliberately.** CPU contention is a measured flake source, and interleaved output defeats reading the named failing test.
- `task gate:api` carries `-race`, which the hand-typed `go test -count=1 ./...` it replaced did not and CI always did. It runs longer (51.8s, measured 2026-08-02) and can redden on a real data race.
- Read the `test:api` comment in `Taskfile.yml` for `-race` / `-count=1` provenance, not a commit cited from memory.
- Read `web/vite.config.ts` for its `testTimeout` rather than trusting a quoted figure: the suite-wide value is 20000 and two tests carry a per-test cap of 120000.
- Not everything is a target: a single-test invocation, `sqlc generate`, the compose stack and the e2e harness stay written out as commands in `.claude/rules/go.md` and `.claude/rules/stack.md`.

### Lint ratchet

- **The Go ratchet lives in `.golangci.yml`** (`issues: {new-from-merge-base: origin/main, whole-files: true}`), not on `lint:api`'s command line, so Task echoes only `../scripts/golangci-lint.sh v2.12.2 run ./...` (a pinned release binary). **Read `.golangci.yml`, not the gate's output.** The CLI form is not an option: it would need a variable spliced into a `cmds:` line, which `Taskfile.yml`'s header bans, or a CI-only flag, which would make local and CI disagree about what a finding is.
- Only findings your branch introduces block; `whole-files` also blocks pre-existing findings in a file you merely *touched*.
- `task lint:api:all` / `lint:controller:all` print the unfiltered backlog and gate nothing.
- If `origin/main` does not resolve, golangci-lint does not skip the ratchet: it reports the whole backlog behind one buried warning, reading as a huge regression. The targets pre-flight this and exit 2; run `git fetch origin main`.
- **The printed finding list is capped and nothing says it truncated.** `max-same-issues` is unset (default 3), `max-issues-per-linter` likewise (default 50); a run printing 9 findings had 24 (measured 2026-08-03). Pass `--max-same-issues=0 --max-issues-per-linter=0` before counting, quoting or dispatching a list of findings; `lint:api:all` lifts the ratchet, not the caps.
- The npm half is unratcheted: oxlint severity lives in each package's `.oxlintrc.json` plus `-D correctness` in its `lint` script, which the echo also cannot show (it echoes `npm run lint`).

### Dead code

- `task deadcode` runs all four components; each `gate:<component>` runs its own. The halves gate differently, so a green means different things for Go and npm.
- **Go**: `deadcode -test ./...` per module, via `scripts/deadcode-gate.sh`, holds both modules at zero against a committed, empty baseline. The routine fix is to delete the function, not to baseline it.
- **npm (knip)**: the whole report gates at `error` in `web/knip.jsonc` / `agent/knip.jsonc`, zero tolerance — unused files, dependencies, unlisted imports, binaries, unresolved imports, duplicate exports, and the unused-export/type family (`exports`, `types`, `nsExports`, `nsTypes`, `enumMembers`, `namespaceMembers`). A green `deadcode:web` / `deadcode:agent` does mean "no unused exports"; a new one reddens the gate. Keep DTO/contract types exported via `ignoreExportsUsedInFile`, not a blanket suppression.
- **`deadcode` exits 0 whether it finds 0, 1 or 44 dead functions.** rc=1 means a load error, and then stdout is 0 bytes, so a run-sort-diff wrapper reads a module that does not compile as clean. `scripts/deadcode-gate.sh` exists for the exit code and reads rc before output, using `fmt-check:api`'s convention (2 = instrument broken, 1 = findings).
- `task deadcode:api:all` / `deadcode:controller:all` **always exit 0**, the opposite of `lint:*:all`, which exits 1 on any finding. Read their output, not their status.
- **Calibrate this slot with an exported symbol.** golangci-lint's `unused` reports unexported symbols and runs earlier in `gate:api`, so an unexported dead function reddens lint and `deadcode` never executes.
- **Neither tool sees a dead branch.** `deadcode` finds unreachable functions, knip unused exports/files/deps; a `case` arm nothing reaches inside a live function is invisible to both, and is not a valid probe for either. Dead branches stay a review question.

### Reading a gate result

| exit | meaning |
|---|---|
| `201` | a target ran and failed. `task` reports its own code, not the command's (a component exiting 7 surfaces as `exit status 7` with rc=201) |
| `109` | malformed `Taskfile.yml`; nothing ran. Loud and self-locating: `task: Failed to parse Taskfile.yml:` plus the YAML error and its line, e.g. a bare `: ` inside a plain scalar in a `desc:` |

`!= 0` is the correct failure test; never compare `$?` to a specific number.

## Rule files

Per-component detail loads on demand: Claude Code includes a `.claude/rules/*.md` when it reads a file matching that rule's `paths:`.

| Rule file | Loads when you touch | Covers |
|---|---|---|
| `.claude/rules/go.md` | `api/**/*.go`, `api/**/*.sql`, `controller/**/*.go`, `Taskfile.yml`, `.golangci.yml` | both Go modules: live-DB tests, the `PASS=0` family, goose `+goose`, sqlc inference, the mutation-testing discipline, `gofmt` exit codes, the gate-status reporting traps, semver `v`-prefix, `cellText` vs `CellText`. The lint ratchet, its caps and the `-race` rationale stay in Commands above; `go.md` names those slots without explaining them |
| `.claude/rules/web.md` | `web/**` | `task gate:web`, mock mode, the live-stack `vite preview` hazard, the blind browser instruments, vacuous negative assertions |
| `.claude/rules/agent.md` | `agent/**` | `task gate:agent`, `--test-timeout`, the `agent-browser` symlink clobber, the `node --test` tally trap |
| `.claude/rules/stack.md` | `docker-compose.yml`, `e2e/**`, `scripts/**`, `deploy/**`, `.github/workflows/**` | isolating a test stack, the `.env` mechanism, `run-e2e.sh` / `smoke.sh` and the `smoke.sh` recipe, CI, the Helm `-}}` object-deleting trap. It does not restate the destructive rules: `-p uzi` / `down -v` and throwaway-container naming live in *Destructive operations* above, always loaded |
| `.claude/rules/tui.md` | `api/cmd/uzi/tui_*.go`, `api/cmd/uzi/sketch.go`, `api/cmd/uzi/uxlab/**` | the TUI and the uxlab render harness: D7 untrusted-field rendering through `renderer.Plain`, the colorprofile downgrade check, sketch work never lands on `main` |
| `.claude/rules/prds.md` | `prds/**` | the workflow-scope constraint on PRD authoring: a PRD sent to uzi must keep `.github/workflows/**` out of both implementation and validation (the worker PAT lacks `workflow` scope, so any workflow-file touch in the branch diff is an atomic push rejection losing the whole branch). Split a needed workflow edit into a separate maintainer/local-only issue, or do it in-session; cross-refs the `uzi-watcher` skill's guardrail |

- **A rule fires on a file READ.** Running a gate without opening a file in that component does not pull its rule in: open the file, or read the rule directly.
- Nested `CLAUDE.md` files are not re-injected on `/compact` while the root file is; the docs say nothing about `.claude/rules/`. Do not rely on a rule surviving a compaction; re-read it if it matters.

Everything below is repo-wide and stays loaded for every agent.

## grep on this host

`grep` is `ugrep`. The first defect is specific to its POSIX modes; the rest are portable traps.

- **A negated bracket expression misbehaves in plain and `-E` mode** (measured 2026-07-27): `printf 'abcs---\n' | grep -cE '[^-]---$'` returns 0, while `'[a-z]---$'` and `grep -cP '[^-]---$'` both return 1. A guard written with a negated class passes on the render it exists to reject. Check `grep --version` before trusting one, and use `-P` or `awk` for anything load-bearing.
- **Escaping a brace to make it literal is what turns it into a quantifier.** Not a ugrep defect: BSD grep behaves identically.

```
grep     -c 'tabIndex={0}'    correct — bare { is LITERAL in BRE
grep  -F -c 'tabIndex={0}'    identical; -F changes nothing here
grep  -E -c 'tabIndex={0}'    ERE: interval — matches lines 1, 2, 4
grep  -P -c 'tabIndex={0}'    same as -E
grep     -c 'tabIndex=\{0\}'  <- THE TRAP: escaping IS the POSIX interval syntax
      (fixture: `tabIndex={0}` / `tabIndex` / `tabInde` / `tabIndexZZZ`)
```

  `x{0}` = "the preceding character, zero times", so under `-E`/`-P` the pattern widens to its own prefix: `{0}` quantifies the `=`, so `tabIndex={0}` degrades to `tabIndex` and matches any line carrying that prefix, a comment included. BRE spells the interval `\{0\}`, so plain `grep` is unaffected and the trap needs `-E` or `-P`. The inflation is not "+1": it is however many other lines carry the prefix.
- **Use `-F` whenever you mean a literal.** A pattern carrying `^`, `-`, `.` or `*` is read as a regex and can silently return 0 where the literal is present.
- **Use `git grep` when the question is about tracked content.** A tracked file inside an ignored directory (e.g. force-added under `.claude/agent-team-tasks/`) is invisible to every recursive `grep` sweep, since ugrep honours ignore files by default:

```
git ls-files --error-unmatch <file>       rc=0, TRACKED
grep -rl -F 'gitleaks:allow' .            NOT found        <- 2 occurrences are in there
grep -rl -F --hidden ...                  NOT found        <- the obvious fix does NOTHING
grep -rl -F --no-ignore-files ...         found
grep -c -F ... <file>                     2                <- named directly, it reads fine
git grep -F ... -- <file>                 2                <- index-aware, finds it
```

  - `--hidden` is the wrong axis: the path is ignored, not hidden. It changes nothing, and you conclude the file lacks the string.
  - `git check-ignore` fails open on a tracked file (silent rc=1, "not ignored"). Use `git check-ignore -v --no-index`, which names `.gitignore:52`; the plain form answers correctly on an untracked path in the same directory, so this is a tracked-file property, not a `check-ignore` quirk.
  - It defeats the retire-a-string sweep in `.claude/rules/web.md`: a string surviving only in an ignored-but-tracked path is unreachable and the sweep reports clean.
  - Reach for `--no-ignore-files` only when you also need untracked files.
- **Never post-filter a sweep** (`git grep <literal> | grep <words>`): it drops exactly the lines phrased differently from your template, and a numeric filter like `[0-9]{3,}` matches `2026` in a date or `103` in a path. Anchor on the invariant token, take the whole file set, read the hits yourself. A claim appearing in several phrasings has no single anchor that finds every site, so a verification sweep on the obvious one comes back clean while missing one.
- **Do not anchor on a token your own change puts into the output.** A probe file named `zz_w5_nolintlint_probe.go` made `grep -F 'nolintlint'` match its own filename.
- **Verify restores with `git status` / `git diff`, not with a grep count.** A count only tells you something if you already know how many occurrences ought to exist.

## Architecture

Full detail in `ARCHITECTURE.md`; read it for any cross-service work. The short map:

- **Services**: `web` (nginx-unprivileged; serves the SPA, reverse-proxies `/api/*` same-origin, no CORS anywhere), `api` (Go, distroless, sole holder of secrets/keys), `db` (postgres:17, `pgdata` volume), `agent` (profile-gated worker, outbound-only to `api`).
- **Trust boundaries**: only `web` publishes a port, on loopback. nginx overwrites `X-Forwarded-For`; `api` trusts it only from `TRUSTED_PROXIES`. Session = HttpOnly JWT + CSRF cookie (`api/internal/middleware/auth.go`). Workers use a Bearer join token (sha256 stored, shown once); no cookies or CSRF on `/api/worker/*`.
- **Forge layer**: `api/internal/forge` holds the `Forge` interface and neutral domain types; drivers are `gitlab.go`, `forgejo.go`, `github.go`. An interface change costs four sites: three drivers plus the shared fake `api/internal/forge/forgetest.BaseFake`.
  - `BaseFake` implements all 25 methods with loud defaults: an un-overridden method returns an error wrapping `forgetest.ErrNotStubbed`, except `LatestPipeline` / `LatestMRPipeline`, which return the absence sentinel `forge.ErrNoPipeline`.
  - Six fakes embed it and override only what they exercise (`handler/forge_test.go`, `seed/seed_test.go`, `poller/autopilot_test.go`, `poller/ci_autofix_test.go`, `privcheck/checker_test.go`, `forgesvc/sync_test.go`). `workersvc/ci_fix_snapshot_test.go` embeds `forge.Forge` itself, as an unreached nil interface.
  - No other package imports a driver directly. Errors pass a PAT-scrubbing redactor; outbound base URLs are allowlisted (`FORGE_ALLOWED_BASE_URLS`, https-only: the SSRF guard).
- **Sync**: `api/internal/forgesvc`, shared by handlers and `api/internal/poller`. The forge is the source of truth, `issues` is a cache, writes are forge-first: labels on the forge, only then the cache (a failed move snaps back).
- **Secrets at rest**: `api/internal/secretbox` (AES-256-GCM keyed by `UZI_SECRET_KEY`, validated at boot, refuse-to-start on placeholder keys) seals forge PATs and per-user Anthropic tokens. No reveal endpoints; rotating the key invalidates everything stored.
- **Run lifecycle**: the `runs` state machine `queued → claimed → running → awaiting_approval → running → completed/failed`, enforced partly by a sweeper goroutine (stale heartbeats, timeouts, requeues). The `runs.kind` domain (eight values) comes from `api/internal/runkind`, pinned to the DB `runs_kind_check` constraint, with agent and web mirrors. Workers claim via `FOR UPDATE SKIP LOCKED` with an affinity grace for resumes. `run_messages` (gapless per-run `seq`) is persisted first, then broadcast over `/api/ws`; reconnects replay via REST `?after=<seq>`.
- **Guardrails, primary directive `main` is never touched**: four independent layers, none to be weakened on the theory another covers it. Forge Developer role + protected branch; the worker (not the agent) holds the PAT and does all network git via env-scoped config; the SDK `PreToolUse` deny-hook in `agent/src/guardrails.ts` (denies `git push`, force/history rewrites, credential reads, including through shell wrappers); `settingSources: []`, so nothing from a cloned repo's `.claude/` loads.

The map stops at the run lane. The rest, one line each, with `ARCHITECTURE.md`'s section as the pointer:

- **Five surfaces**: board/web (1st), forge (2nd, "Forge integration"), the worker/run lane that acts (3rd, "Agent runtime"), Slack (4th, `api/internal/slacksvc`), Chat (5th, "Chat with uzi"). Two add no service and no trust boundary: Slack is outbound-only Socket Mode (no public URL, no inbound port), Chat is a run kind (`runs.kind='chat'`) on the existing worker machinery, as `ci_fix` was. Plus `api/cmd/uzi/`, the CLI.
- **Hosted workers on k8s** (`controller/` + `api/internal/hostedsvc`): the api provisions no kube objects and holds no kube credential; `uzi-controller` does, scoped to two otherwise-empty namespaces. Shipped in the chart, `workers.enabled: false` by default. Since we mostly test in k8s, this is the primary worker path, not an alternative to compose.
- **Judge** (`agent/src/judge-runner.ts`, `api/internal/handler/judge*.go`, `docs/judge.md`): a retrospective LLM pass over each finished run's trace, on its own claim lane and the user's own Anthropic token, producing a verdict plus structured recommendations. Advice only: it never writes code. Off by default, gated twice (admin globally, then per user).
- **Self-improvement** (`self_improve` [default scheduled job](docs/scheduling.md#default-jobs), built on the schedule catalog `api/internal/schedsvc` + `api/internal/schedtmpl`): any user can enable it on a repo they own, off by default, no admin gate. Each cycle opens or extends one MR on the target repo. `main` untouched, human merges.
- **Per-user vault** (`api/internal/vault`): a Bitwarden-style key hierarchy over `secretbox` for personal secrets. Read `docs/vault-threat-model.md` before touching anything in there.
- **OIDC** (`api/internal/oidc`, `docs/oidc.md`): optional SSO alongside password auth. The `oidc` / `oidc-degraded` / `sso-only` mock scenarios (named in `docs/dev-conventions.md#the-mockdemo-build`; mock mode itself is in `.claude/rules/web.md`) are the only way to see its UX without an IdP.

## Conventions

- **We mostly test in k8s.** Hosted k8s (dev-cluster, GitOps via ArgoCD) is the primary runtime and test environment, not local compose; a feature is not done just because it works under `docker compose`. Compose must keep working (laptop dev loop, e2e/smoke harness), but where a PRD has both tracks, verify k8s as first-class, often primary, not the deferred one.
- **Detect the forge from the remote; don't assume GitLab.** Derive the CLI from `git remote get-url origin`: GitHub → `gh`, GitLab → `glab`, Forgejo/Gitea → `tea`. Never cross them. This checkout's canonical remote is GitHub (`github.com/vtmocanu/uzi`); a fork may live on any of the three. On a GitLab remote an exported `GITLAB_TOKEN` 401s on this host, so run `env -u GITLAB_TOKEN glab …`.
- **Every bug fix ships a regression test.** It must fail on the unfixed code and pass with the fix, and you must watch both directions (the mutation-testing discipline in `.claude/rules/go.md`, applied to the bug's own seam). Pin it to this bug's failure, not vaguely to the area. If you file an issue instead of fixing now, the test rides with the eventual fix, not the issue.
- **Specs contract**: `specs/human.md` is the binding contract of user-stated requirements. Autonomous runs may make terse sync/hygiene edits without approval (retire a requirement whose feature was removed, rename a retired term, fix a line reality made stale), each tagged `(AI-synced YYYY-MM-DD)` and kept terse. A new requirement, or a change to an existing one's meaning, needs user approval; route it through the lead. `specs/ai.md` can be updated directly. Goal: rebuild-from-specs.
- **PRDs**: active work in `prds/*.md`, completed ones move to `prds/done/`. They are the design-rationale record (Decision Logs); link them from `ARCHITECTURE.md` rather than duplicating.
- **ADRs** (`adr/NNNN-slug.md`) are the durable subset, numbered by originating issue or PRD rather than sequence (`0035-run-limit-retry`, `0042-worker-run-concurrency`, `0065-forgejo-driver`, `0106-revise-cap-atomicity`, `0195-run-usage-per-model-fold`, `0246-trusted-repo-instructions`, `0285-worker-egress-tier-trust-model`), so the directory does not read chronologically. Write one when a decision outlives the work that produced it (a seam other code must respect, an invariant a future change would break silently); otherwise the PRD's Decision Log is the home. Read `adr/` for the current list rather than quoting a count.
- **`fixtures/` is read by loaders, including across the module boundary**, which is why `-count=1` is mandated at both Go gates (`.claude/rules/go.md`): an edit there changes a gate's meaning while moving no Go cache key, and a deletion reddens tests. The `probes/` directory it used to be paired with has been removed from the public tree.
- **A stale identifier inside a past-tense claim about a past commit is a typo; the same identifier inside a present-tense claim about current code is a wrong doc.** Fix-the-doc applies to the second case only.
- **Goose migration numbers are assigned at merge time.** Numbers in PRDs are drafts, for collision avoidance between parallel PRDs. On the landing rebase, rename each new migration to the next free number above the live head in `api/internal/store/migrations/`. The boot runner is strict goose with no `allow-missing` (`api/internal/store/migrate.go`), so landing below an already-applied head makes every upgraded instance refuse to boot. A duplicate prefix, a different failure, is caught by `gate:repo`'s `check:migration-numbering` rather than panicking `store.Migrate` at boot.
- **Builtin agent templates**: `api/internal/agenttmpl/builtins/*.md` is the single source of truth for the twelve builtin product roles (`lead` plus eleven subagents), `go:embed`-shipped and boot-seeded into the DB. Parse/validity tests guard them, not a byte-match against another directory. `.claude/agents/*.md` is this repo's dev-team roster: decoupled, free to drift, and product changes must never touch it (`lead` lives only in `builtins/`).
  - **Boot re-applies the embedded body to pristine rows only.** `ReconcileBuiltinTemplates` inserts a missing builtin, then `RefreshPristineBuiltin` (a content-guarded UPDATE) re-applies the shipped body where `customized = false`, so a prompt fix reaches a seeded install on the next boot. A row with `customized = true` (set by `UpdateAgentTemplate`) is never overwritten; `ResetBuiltinAgentTemplate` returns it to pristine so it tracks upstream again. `customized`, not `updated_by IS NULL`, is the discriminator: that FK's `ON DELETE SET NULL` would silently mark an edited row pristine. The refresh is a separate statement, not `ON CONFLICT DO UPDATE`, so its rowcount never triggers the insert-only default-allocation seed and an admin-removed default stays removed.
  - **Genuinely different sets, not copies.** `builtins/` has `lead`; `.claude/agents/` has `release` and `tui-ux`. Neither is the other's source of truth.
  - **Divergence is a nudge, never a gate.** Nothing may fail a build because the rosters differ. `task nudge:roles` (`api/cmd/roleparity`) reports any role on one roster and absent from the other, minus the `scripts/role-parity-accepted.tsv` allowlist, so a role that earns its keep on our team surfaces as a product candidate deliberately. `lead` is product-only and must never be flagged.
  - **No test may assert on the roster's shape.** `detectRepoAgents(clonePath)` parses a *user's* cloned repo; our own directory is a corpus of real hand-authored frontmatter, a fixture rather than a spec. Assert properties (every `.md` yields one agent, `notes` empty, no denied tools), never the roster.
- **Docs**: `docs/*.md` need leading-fence frontmatter (`title`, `order`, `audience`); only `audience: user` pages render in-app at `/docs/:slug`. `web/scripts/check-docs.mjs` (runs in `npm run build`) fails on bad frontmatter, duplicate `order`, or broken relative links. See `docs/README.md`.
- **New uzi functionality: check whether `api/cmd/uzi/` needs a matching CLI change.** The CLI (`docs/cli.md`) is a second consumer of the same API the web UI drives, so a route, DTO or behaviour change that only updates `web/` can leave it silently stale. Both live in this repo, so the check is enforceable in one MR.
- **Cutting a release is driven inline by the lead** via the `uzi-release` skill's scripts (`release-cut.sh` / `release-watch.sh` / `release-verify.sh`); read `.claude/agents/release.md`, the mechanics reference, before tagging. `deploy/README.md` is still GitLab-worded and is not the authority for publish mechanics; `.github/workflows/release.yml` is.
  - `release-cut.sh <X.Y.Z> <prev-tag> --changelog-file <draft>` folds `[Unreleased]` into a dated `## [X.Y.Z]` section citing every shipping merge since the last tag, bumps `deploy/chart/Chart.yaml` to match and the worker tag if the agent runtime changed, commits direct to `main`, and runs the coverage oracle. Then push `main`, watch `ci.yml` green (`watch-run-ci.sh`, in the background), tag `vX.Y.Z`, and have the user run `! git push origin vX.Y.Z`: the harness classifier blocks the tag push even for the lead.
  - **A `vX.Y.Z` tag publishes images, chart, Release and Homebrew formula unattended; there is no approval gate.** Access control is the `protect-release-tags` ruleset, so only a repo admin can create a `v*` tag and the push itself is the authorization.
  - `release-watch.sh` watches the publish in the background, auto-rerunning a transient publish flake up to twice. `release-verify.sh` then confirms five image tags plus the chart on GHCR, cosign signing proven from the publish job log (cosign 3.x signs via the OCI referrers API, not a `sha256-<digest>.sig` tag, so do not grep the tag list for `.sig`), and the Release marked latest.

## Run economy (agents)

- **Run a gate once, to a log, then read the log.** `task gate:<component> > gate.log 2>&1; echo "EXIT=$?" >> gate.log` inside the worktree, then `tail`/`grep` the file. Never rerun the same gate on the same tree to read its output differently.
- **Non-blocking review notes are not reworked in-run.** A finding a validator labels non-blocking is filed as an incidental finding (`report_incidental_issue`) or left for MR review; only a correctness or trust-boundary finding earns a rework commit and its re-validation round.
- **Scale the validator wave to the diff's risk class.** Presentation, copy, docs and refactor diffs get one reviewer for one round; trust-boundary, data-integrity, auth and untrusted-input diffs get reviewer + tester (+ auditor). Mechanical work (anchor verification, gate running, docs) runs on the sonnet-tier roles (`researcher`, `tester`, `documenter`); judgment work stays on opus.

## Agent-team workflow

Read `.claude/agent-team.md` before PRD work: it defines the orchestrator/teammate flow. The load-bearing sections, by name:

- *Context handoff*, *Citing and dispatching across a moving tree*, *Re-derive the claim at the moment you assert it*.
- *Two negative results from instruments that share an assumption are ONE negative result*, *An instrument that cannot produce the disconfirming answer is not evidence*: the general form of the blind browser instruments (`.claude/rules/web.md`) and the `PASS=0` traps (`.claude/rules/go.md`).
- *TYPECHECK the mutated tree before reading the test result*, *Mutate at the CALL SITE, not in the shared helper*, *An assertion defines its CHANNEL*: the mutation-testing discipline that the `sqlc` and `git checkout --` entries in `.claude/rules/go.md` instantiate.
- *A claim about what would happen if you removed it is not readable from the code*, *A rule nobody is keeping is a different failure from a rule that is wrong*, *Sweep per FACT after the last behavioural commit*; plus *Quality gates*, *Project signals* and *Standing rules*.

Before writing "verified" or "green" in a report, read that file's section on why you might be wrong.
