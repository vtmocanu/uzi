# Task brief — issue #144 item 1: warn when the CLI is behind the server

**Worktree**: `/home/user/repos/myorg/vtmocanu/uzi/fix-144-cli-skew`
**Branch**: `fix/144-cli-version-skew`
**Tip when this brief was written**: `2d60c573d9b554618784f99058a8be2a8b986649` (== `main`).
Take the live tip if it has moved past this, and say so rather than guessing.

> **DURABILITY + SWEEP CAVEAT.** This file lives under `.claude/agent-team-tasks/`,
> which is **`.gitignore:52`**. It is **force-added and committed** on this branch, so
> it is durable and survives the worktree — but that makes it *tracked and
> simultaneously invisible to every recursive grep*. `grep -r` will NOT find strings in
> it (ugrep honours ignore files), and **`--hidden` changes nothing** — the path is
> *ignored*, not hidden, which is the wrong axis. Plain `git check-ignore` on it is
> **fail-open** (rc=1, no output); `git check-ignore -v --no-index` names `.gitignore:52`
> at once.
>
> **Any later sweep over this file must use `git grep`**, which reads the index and
> finds it by construction. See `CLAUDE.md`'s entry on this exact trap — it was measured
> on `.claude/agent-team-tasks/prd-103-m2.md`, the spec of the milestone that found it.

---

## Roster

One line per role in `.claude/agents/`. Dispatched with a SHA, or closed with a reason.
Kept current as the run proceeds — this is the durable half of the Step 2 sweep.

| role | disposition |
|---|---|
| architect | **dispatched** at `2d60c573` — design wave |
| reviewer | **dispatched** at `2d60c573` — design-critique wave (citation pass) |
| auditor | **dispatched** at `2d60c573` — design-critique wave (citation pass) |
| coder | pending — spawns against the FROZEN brief after the design wave settles |
| tester | pending — dispatch after the coder's FIRST commit, never at kickoff |
| spec-keeper | pending — `specs/` exists and this adds a CLI behaviour + an env var, so `specs/ai.md` owes an entry. After blocking findings resolve. |
| documenter | pending — `CHANGELOG.md` owes a one-line entry |
| fact-checker | pending — decide after the design wave; its citation ask may already cover the brief's claims |
| release | **closed** — this run ends at an MR. No tag, no release cut. |
| researcher | **closed** — the investigation is already done and written up in §1; there is no open question needing research. |
| web-ux | **closed** — no web surface touched. The web half of this bug was already correct (`RunCredential` rendered both tokens right); the defect is entirely CLI-side. |

---

## 1. What happened, measured 2026-08-03

The user asked why two concurrent uzi runs looked like they were on different
Anthropic tokens while the UI showed the same one. Ground truth, from
`GET /api/runs/<id>` against `https://uzi.example.com` (API `0.14.0`,
commit `2d60c573`):

| run | issue | claimed | `anthropic_secret_label` | `anthropic_select_reason` | headroom |
|---|---|---|---|---|---|
| `edbc3884-ff8c-4f75-bd0f-3ad8833c25a7` | #209 | 12:45:37Z | `meta` | `auto` | 100 |
| `a146df98-aac6-4b59-9333-c3c6799a5bc0` | #78 | 12:30:50Z | `personal` | `default` | null |

The **web UI was correct** — it rendered `token "meta" — auto, 100% headroom`
(screenshot confirmed by the user). `RunCredential` at `web/src/pages/RunView.tsx:524`.

**The CLI was not.** `uzi run get <id> --json` on the then-installed **v0.11.8**
printed `null` for all four of `anthropic_secret_id`, `anthropic_secret_label`,
`anthropic_select_reason`, `anthropic_headroom_pct`, on **both** runs — while raw
`curl` against the identical endpoint returned the values above. After
`brew upgrade uzi-cli` (0.11.8 → 0.14.0) the same commands returned
`{"iid":209,"label":"meta","reason":"auto","headroom":100}` and
`{"iid":78,"label":"personal","reason":"default","headroom":null}`.

So a CLI three minors behind the server **silently drops fields it does not know**,
and nothing anywhere says so. That is the defect.

## 2. This is issue #144 item 1, and it is HALF-DONE

Read `env -u GITLAB_TOKEN glab issue view 144`. Item 1 asks for two things:

> Suggested: `uzi version` (and/or `uzi auth status`) reports **both** binary and
> server version, **and warns when they differ**.

- The **"reports both"** half shipped: PRD #175 M4 added `serverBuildInfo` /
  `serverRows` to `api/cmd/uzi/version.go`. `uzi version` prints the server's
  build info today.
- The **"warns when they differ"** half was never built. `version.go` fetches the
  server version and never compares it to `version`.

Scope of this task is that missing half, plus the delivery decision below.

## 3. Design already settled with the user — do not re-litigate

The user was shown three placements and chose **"every command, cached probe"**,
with this exact preview:

```
$ uzi run get <id> --json
uzi: CLI v0.11.8 is behind server 0.14.0; some
     fields may be missing. Run: brew upgrade uzi-cli
{
  "id": "edbc3884-...",
  ...
}

(warning -> stderr, JSON -> stdout, exit code unchanged)
```

Binding constraints from that choice:

1. **Warning goes to STDERR. Never stdout.** `--json` consumers parse stdout; a
   warning there corrupts every parser. This is the whole reason the option was
   presented that way.
2. **The exit code is unchanged.** A skew warning is not a failure. `ExitCodeFor`
   must see exactly what it sees today.
3. **Cached, not per-command.** A probe on every invocation is unacceptable
   latency. Cache the server's version and re-probe on a TTL (~1h was the figure
   shown to the user; the architect may argue a different one).
4. **The proposal is `brew upgrade uzi-cli`.** That is the only documented install
   path — `Formula/uzi-cli.rb` is the source of truth and release CI publishes it
   into the `vtmocanu/homebrew-tap` tap (verified this session: the tap's `main` is
   at `tag: "v0.14.0"`, CI is healthy, the local clone was merely 8 days stale).
5. **Landing**: a scoped MR against issue #144, no new PRD.

## 4. Facts you will need, each re-derivable

**The semver trap is LIVE here and it fails open.** `uzi version` prints
`v0.14.0` (leading `v`, stamped by `-ldflags -X main.version=v#{version}` in
`Formula/uzi-cli.rb:48`) while `/api/version` returns bare `0.14.0`
(`apitypes.BuildInfoDTO.Version` doc: *"bare — the Dockerfile strips a leading v"*).
`CLAUDE.md` documents the consequence: `golang.org/x/mod/semver` treats every
invalid version as equal, so `Compare("0.11.0","0.11.7") == 0` and an
un-normalised comparison is silently dead.

- **In-repo precedent that gets it right**: `api/internal/forge/forgejo.go`,
  `checkForgejoVersion` — re-prefix with `v`, guard on `semver.IsValid`, then
  `semver.Compare`.
- `golang.org/x/mod v0.38.0` is **already** in `api/go.mod:22`. No new dependency.
- SemVer §10: build metadata is comparison-neutral, so `v0.14.0+g2d60c57` compares
  equal to `v0.14.0` — which is what we want, and is not the same as string equality.
- **The discriminating test fixture must be genuinely BEHIND.** `Compare(x,x)==0`
  is the right answer from a completely broken comparison, so an all-equal fixture
  set certifies nothing. `.claude/agent-team.md` catalogues this exact case.

**Where the hook goes.** `api/cmd/uzi/root.go` already has the seam:

```go
root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
    if env.AutoUpgradeSkill && !underSkillCmd(cmd) {
        maybeAutoUpgradeSkill(env)
    }
}
```

`underSkillCmd` (`api/cmd/uzi/skill.go:191`) walks ancestors looking for `skill`.
`maybeAutoUpgradeSkill` (`:204`) is the tone to match: **never fatal, never
blocking**, honours `UZI_SKILL_AUTO_UPGRADE=0`, warns on stderr and returns.

**Where the cache goes.** `uzicli.Store` (`api/internal/uzicli/config.go`) is
rooted at `~/.config/uzi`, **hardcoded and deliberately not XDG** — that file
carries a comment explaining why (on this team `XDG_CONFIG_HOME` points into a
git-tracked mackup repo). Do not add an XDG lookup. `Store.Dir()` is exported.
`env.Store` may be **nil** (no home dir) and every caller tolerates that.

**How the probe should behave.** `serverBuildInfo` in `api/cmd/uzi/version.go` is
the model and its doc comment states the contract: every failure returns nil, the
no-URL case is checked **before** a client is built so the common standalone
invocation makes no network call at all, and `serverProbeTimeout = 2 * time.Second`
exists specifically because `uzi version` runs inside Homebrew's sandboxed
`test do` with no server anywhere — a 30s stall there is a failed release.

**Two gates in `api/cmd/uzi/` that will bite you:**

- `instructions_test.go` — the printed-instruction registry (PRD #98 seam 5). It
  lifts every `uzi …` span out of printed strings via AST and demands a registry
  entry; a RUNTIME instruction's entry is *a claim that the instruction has been
  executed*, and `kindUnknown` **fails**. Read the file's header before you print
  anything. Note the proposed message says `brew upgrade uzi-cli`, not `uzi …` —
  confirm for yourself whether that is in or out of the extractor's scope rather
  than assuming.
- `skill_drift_test.go` — asserts the embedded `SKILL.md` matches the real cobra
  tree. The skill ships **inside** the binary
  (`api/internal/uzicli/skill.go`, `//go:embed skill/SKILL.md`), so CLI↔skill skew
  is structurally impossible; only CLI↔server skew is real. If a new env var is
  added it belongs in `api/internal/uzicli/skill/SKILL.md`'s configuration section
  next to `UZI_SKILL_AUTO_UPGRADE=0`.

**`brew-local-test.sh` constrains stdout, not stderr.** `scripts/brew-local-test.sh`
matches `case "$out" in "v$version"*)` anchored at the start of the **whole**
stdout, which is why `version.go` prints the CLI version first and alone on line
one. A stderr warning does not touch this; a stdout one breaks the release.

## 5. Gate

Run `task gate:api` (fmt-check + vet + build + lint + deadcode + test). Do not
restate its recipe. The lint slot is ratcheted against `origin/main` — if
`origin/main` does not resolve, `git fetch origin main` rather than starting a
burn-down.

`test:api` carries `-race -count=1`. Live-DB tests are not in scope here.
