# Design — issue #144 item 1: warn when the CLI is behind the server

**Worktree**: `/home/user/repos/myorg/uzi/fix-144-cli-skew`
**Branch**: `fix/144-cli-version-skew`
**Revision 2**, derived at `d1c80862` (revision 1 was at `2d60c573`). Revision 1 answered
the original dispatch; this one reconciles against brief §6 (reviewer, `662c2d7e`) and §7
(auditor, `d1c80862`). **Two of my revision-1 calls were wrong and are corrected in §0.5.**

Input: `.claude/agent-team-tasks/144-cli-version-skew.md`. The placement decision (every
command, cached probe, stderr, exit code unchanged, `brew upgrade uzi-cli`) is the user's
and is NOT re-litigated.

---

## 0. Citation pass on brief §4 (task #1's deliverable)

All eight mechanisms hold at HEAD; independently 10/10 by the reviewer. Verified
individually: `skill.go:191-198` / `:204-220`, `root.go:109-113`, `config.go:20-38`,
`root.go:36-38`/`:55`/`:157`, `version.go:15-22`/`:81-111`, `go.mod:22`,
`Formula/uzi-cli.rb:48`, `apitypes/buildinfo.go:22-30`, `scripts/brew-local-test.sh:72-75`,
`handler/handler.go:444-446`. No refutations.

**Measured, not inferred** (`golang.org/x/mod@v0.38.0`, on the literal shipped shapes):
with no normalization, `semver.Compare("v0.11.8", "0.14.0") == +1`. The CLI is always the
valid operand and the server always the invalid one, so an un-normalized comparison
returns **+1 for every input this project can produce** — "CLI is ahead" — i.e. it
**never warns, ever**. Stronger than CLAUDE.md's general warning (which is about
invalid==invalid returning 0), and it is what §7.1's fixture is built on.

---

## 0.5 Reconciliation with brief §6 and §7 — what changed and why

### I was wrong twice. Both corrections stand on executed evidence, not argument.

**(1) Exemptions: I said `uzi auth token` should NOT be exempt. Auditor H3 is right and I
was wrong.** My revision-1 reasoning asked *"does the probe work without a credential?"*
(yes, the route is unauthenticated) and never asked *"does the probe EMIT a credential the
command would otherwise never emit?"* — which is the question that matters. The auditor
executed it with a canary token: the route is unauthenticated, **the request is not**
(`newRequest` attaches the bearer unconditionally, `client.go:302-304`). I had even quoted
`client.go:100-111` saying exactly that in revision 1 §0 and drew the wrong conclusion from
it. Corrected in §4, with the enumeration generalized past the two commands the auditor
named — **there is a third, `uzi auth status`, which neither report lists**.

**(2) Cache key: I said "normalized raw URL". Auditor H4(b) is right and I was wrong.** I
reasoned about the key from *correctness* (does it identify the right server) and never
from *what the key persists*. `credentialSafeBase` does not strip userinfo — executed:
`--url 'http://alice:hunter2@127.0.0.1:…'` is served — and the auditor's control shows a
`--url` base is **never persisted today** outside `uzi login`, so this cache would be the
first path writing one, at 0644. Corrected to a **SHA-256 hash of the normalized URL** in
§3.2. A hash cannot leak what it does not contain, which is a stronger property than any
strip-the-userinfo helper I could write correctly.

### Cross-confirmed — revision 1 already had these, no change needed

- **B1** (unparseable must be SILENT, not warn; the forgejo precedent is the *shape*, not
  the disposition) — §3.1 rules 2-3, and §7.1 row 6 (`dev`/`0.14.0`) is its discriminating
  fixture.
- **B2 / auditor "silence is ambiguous"** (negative caching required) — §3.2, already a
  named property.
- **B3 / H4(a)** (key on the resolved URL) — kept; the key's *form* changed, not the fact.
- **N1** (cache the OBSERVATION, never the VERDICT) — §3.2; see the `cli_version`
  resolution there, which harmonizes N1 with the auditor's ungraded item.
- **N2** (the cache lives in `uzicli` beside `Store`, because `writeFileAtomic` is
  unexported) — §2 file map. This is also the lead's sharpened boundary question; the
  answer is unchanged.
- **N3** (`--help`, `--version`, non-runnable, `ValidateArgs` all return above the hook) —
  I read `cobra@v1.10.2 command.go:918-993` independently and get the same lines. **Now
  decided rather than merely noted**, §4.
- **N6** (gate on an `Env` field) — §3.4.
- **n1** (the message escapes `instructionRE` by one character, twice) — §5, derived from
  the regex source; converges with the reviewer's measurement.

### New requirements adopted from §7, none of which revision 1 had

**H1/H2 sanitization**, **H4(c) sanitize-on-read**, **H4(b) hash key**, **H3 exemptions**,
the **`fmt.Fprintf`-not-`Printer` toolchain constraint**, **N4** (single-occupancy seam
comment), **N7** (`e2e/run-e2e.sh`), **N11** (`docs/cli.md` *and* `SKILL.md`, both by
hand), and the **four-attack test fixture**.

### One measurement that sharpens H1/H2 rather than contradicting them

I ran the auditor's four attack strings through `semver.IsValid(normSemver(v))`, because if
the validity guard rejects them then a reader can talk themselves out of `cellText` on the
warning path. **It rejects all four — and that reasoning is still wrong**, because build
metadata gives an unbounded *valid* string:

| attack | `IsValid` after normalization | reaches the message? |
|---|---|---|
| `0.14.0\rWARNING: run …` | false | no |
| `0.14.0\x1b[2J\x1b[H` | false | no |
| `0.14.0\x1b]8;;http://evil\x07…` (OSC 8) | false | no |
| `0.14.0‮gnp.exe` (U+202E) | false | no |
| `0.14.0` + 1 MiB of `A` | false | no |
| **`0.14.0+` + 1 MiB of `A`** | **true** | **YES — 1 MiB on stderr** |
| `0.14.0+g1a2b\rEVIL` | false | no |

SemVer's build-metadata charset is `[0-9A-Za-z-]` **with no length limit**, and x/mod
implements it faithfully. So on the **new warning path** the validity guard happens to stop
H1 (control characters) and **does not stop H2** (unbounded).

Three conclusions, and the third is the one that matters:

1. `cellText` is **binding on the warning path** for H2, validity guard or not.
2. `cellText` is **binding on the pre-existing `serverRows` path** (`version.go:118`,
   printed at `:74`) for **all four** attacks — that path has no validity guard at all, is
   already on `main`, and is H1 as filed. My measurement does not soften it by one word.
3. **Do not let anyone reason from this table to "the validity guard is enough".** It is
   enough along one axis and not the other, and it is enough only because rule 2 happens to
   run before the interpolation — a property of statement ordering, which a refactor can
   lose silently. **Sanitize unconditionally at print time.** The measurement is recorded
   here precisely so the tempting-and-wrong inference is refuted in writing rather than
   re-derived by someone who stops reading at the first four rows.

---

## 1. Approach

**The difficult points, and the way through each.**

**(a) The comparison is a pure function and nothing else may be.** Every semver trap
(normalization, `IsValid` on **both** sides, direction, build metadata, prerelease) lives in
one place with no I/O, testable by a table with zero fixtures, no clock, no filesystem.

**(b) Everything crossing the trust boundary is sanitized AT PRINT.** The server's version
string is attacker-controlled in the threat model, and so is the cache file (H4(c): a plain
file with no integrity protection — anything that can write it controls the warning with no
network involved). One rule covers both: **`cellText` at the moment of printing, after the
cache read, on every `BuildInfoDTO` string that reaches a terminal.**

**(c) The cache key must identify the server without persisting it.** Keying is required
(B3/H4a: otherwise one `--url https://hostile` poisons every later invocation for a TTL).
Persisting the raw URL is a new credential-write path (H4b). A hash satisfies both.

**(d) The cache stores the last ATTEMPT, not the last SUCCESS.** Empty version = "we probed
and learned nothing". Without it an offline laptop pays `serverProbeTimeout` on every
command forever — the per-invocation probe constraint 3 forbids (B2).

**(e) The cache holds the SERVER's version, never the verdict** (N1). `brew upgrade
uzi-cli` clears the warning on the very next command — no TTL wait, no invalidation step —
because the CLI side is re-read live every run.

**(f) `uzi version` is a genuine special case, on correctness grounds.** `PersistentPreRun`
runs before `RunE` (`command.go:982` vs the RunE call after it), so a cached warning would
print `behind server 0.13.0` on stderr and then stdout would print `server version 0.14.0`
from its own live probe — a self-contradiction inside one invocation. Exempted from the
hook; warns **inline** from the probe it already makes (N8, answered the stronger way).

**(g) The unstamped-build short-circuit goes FIRST.** `version` is `"dev"` on every
`go build` (`root.go:16`), so `IsValid` on the CLI's own version, checked before settings
are resolved, means a dev build makes **no network call and touches no file**.

**Rejected alternatives.**

1. **Reuse `workersvc.classifyByVersion`** (`upgrade.go:116-145`) — identical semver
   problem, battle-tested. **Killed by dependency direction**: `workersvc` is a server-side
   service package (store, db, hostedsvc), and `uzicli` compiles into a binary that runs on
   a laptop with no database. The normalization is then duplicated a third time
   (`forge/forgejo.go:174-176`, `workersvc/upgrade.go:88-90`, `uzicli`) — two lines, and the
   guard against getting it wrong is a *test*, not a shared symbol. **Declined follow-up**:
   an `api/internal/verscmp` extraction repointing all three; that touches two working
   packages inside an issue-scoped MR.
2. **Async background probe** — removes all latency. **Killed by complexity**: a goroutine
   outliving `Main` needs lifecycle management; a spawned process needs supervision.
3. **Warn from `PersistentPostRun`** so the line lands after the output. **Killed because
   cobra does not run PostRun on an error path**, and a failing command is exactly the case
   most worth warning about.
4. **Strip the `Authorization` header to make the probe truly unauthenticated** — the
   obvious fix for H3. **Killed by the auditor's own reasoning, adopted verbatim**: it
   removes the stated justification for `credentialSafeBase` on this path
   (`client.go:100-111`) and re-opens `uzi … --url http://…` as a token leak. Exempt the
   commands instead.

---

## 2. File map

**Entry point: `api/cmd/uzi/root.go:109-113`** — the existing `PersistentPreRun` closure
gains a second best-effort call.

### Create

| Path | What it holds |
|---|---|
| `api/internal/uzicli/versioncheck.go` | **Library half.** `normSemver`, `SkewWarning` (pure), `versionCheckState`, `(*Store).CachedServerVersion`, `(*Store).RecordServerVersion`, `VersionCheckTTL`. No cobra, no network, no `time.Now()`. Lives here, not in `main`, because `writeFileAtomic` and the 0700 `MkdirAll` discipline are unexported (N2, H4). |
| `api/internal/uzicli/versioncheck_test.go` | §7.1 comparison table + three broken references; §7.2 cache tests. |
| `api/cmd/uzi/versioncheck.go` | **Wiring half.** `maybeWarnVersionSkew(cmd, env, gf)`, `exemptFromVersionCheck(cmd)`. Calls the *existing* `serverBuildInfo`. |
| `api/cmd/uzi/versioncheck_test.go` | §7.3 channel / exit-code / suppression / exemption / sanitization tests via `runCLI`. |

### Modify

| Path | Change |
|---|---|
| `api/cmd/uzi/root.go` | `Env` gains `CheckServerVersion bool` (doc'd like `AutoUpgradeSkill`, `:47-50`); `DefaultEnv` true; hook calls `maybeWarnVersionSkew` **after** `maybeAutoUpgradeSkill`. **Plus the N4 comment**: this seam is single-occupancy — cobra `break`s at the first ancestor with a `PersistentPreRun` and `EnableTraverseRunHooks` is unset (`command.go:982-999`), so a future subcommand adding its own hook silently disables **both** consumers with nothing failing. |
| `api/cmd/uzi/version.go` | **(i) H1 fix, pre-existing:** `serverRows` (`:118`) must emit `cellText(b.Version)`, and the same for every other `BuildInfoDTO` **string** it renders (`Commit`, `BuiltAt`, `Founded`; `Commits`/`UptimeSeconds` are numeric and safe). A bug on `main`, not new work, in scope because this MR multiplies its blast radius. **(ii)** after `srv := serverBuildInfo(...)`: record into the cache and emit the inline skew warning to `env.Stderr`. Both gated on `env.CheckServerVersion`. **(iii)** extend `serverProbeTimeout`'s doc (`:15-22`) to name its second consumer and to state it is per-REQUEST, not a per-invocation budget (auditor). Under this design there is exactly one request per hook invocation and `refuseRedirect` (`client.go:272`) blocks amplification, so here the two coincide — say so, because the constant reads like a total. **stdout untouched**, so the brew constraint at `:63-69` holds by construction. |
| `api/internal/uzicli/fake.go` | `FakeClient` gains `BuildInfoCalls int`, incremented in `BuildInfo` (`:346`). Required: the no-double-probe and cache-hit claims are unobservable without it. No mutex — every consumer is a single-goroutine command test. |
| `e2e/run-e2e.sh:1682` | **N7.** Add the opt-out to `uzi_cli()`'s `env -i` list beside the existing `UZI_SKILL_AUTO_UPGRADE=0`, which sits there for exactly this class of side effect. |
| `api/internal/uzicli/skill/SKILL.md` | `UZI_VERSION_CHECK=0` beside `UZI_SKILL_AUTO_UPGRADE=0` (`:327`), plus a sentence under `### Version` (`:334`) telling an agent what the stderr line means. |
| `docs/cli.md` | One paragraph in **Config and credentials** (`:583`). **N11: env vars are gated by nothing in either direction** — `skill_drift_test.go` extracts only `--flags` and command paths — so both doc files are by hand and neither is enforced. |
| `CHANGELOG.md` | `[Unreleased] → ### Added`. (Task #6's; listed so it is not orphaned.) |

**N7 is belt-and-braces, and it is worth knowing which:** `e2e/run-e2e.sh:1665-1667` builds
the CLI with a plain `go build ./cmd/uzi` and **no `-ldflags`**, so the harness binary is
`version = "dev"` and §1(g) short-circuits before any probe. Independently,
`run_printed_instructions` (`:3291-3296`) asserts exact counts over stdout **and** stderr
against a per-row ERE, and our message contains no `uzi <verb>` span to match one. Three
independent reasons it is safe; add the env var anyway, because the first evaporates the day
someone stamps the e2e build.

### Deliberately NOT modified

`api/cmd/uzi/instructions_test.go` — no new registry entry (§5).
`api/cmd/uzi/skill_drift_test.go` — no new command, no new `--flag`.

---

## 3. Contracts

### 3.1 The pure comparison — `api/internal/uzicli/versioncheck.go`

```go
// SkewWarning reports whether the CLI is BEHIND the server, and the line to print.
// ok == false means print nothing. Callers MUST sanitize serverVersion before it
// reaches a terminal; this function does not, deliberately (see §3.3).
func SkewWarning(cliVersion, serverVersion string) (msg string, ok bool)
```

Rules, in order. **Every `false` arm is silence — the warning fails closed** (B1):

1. Normalize both with `normSemver` (`"v" + TrimPrefix(TrimSpace(v), "v")`).
2. `!semver.IsValid(cli)` → **silent**. Covers `dev` (`root.go:16`), the default on every
   `go build`, every `runCLI` test and every gate run.
3. `!semver.IsValid(server)` → **silent**. Covers a `dev` server (compose), a pre-#175
   server sending no `version`, and garbage.
4. `semver.Compare(cli, server) >= 0` → **silent**. Ahead is never an alert; mirrors
   `workersvc/upgrade.go:132-136` (R6) and matches "is behind" in the user's text.
5. Otherwise warn.

Rules 2 and 3 are where B1 lands: `checkForgejoVersion` **refuses** on unparseable and its
own comment argues refusing is safer — true for a connect-time feature gate, and inverted
here, where "refuse" maps to "warn". **Write that reason in the code**, or the next reader
copies the precedent's disposition along with its shape.

Build metadata is comparison-neutral (SemVer §10, implemented by x/mod), so
`0.14.0+g2d60c57` == `0.14.0` — measured, §7.1 row 4. A prerelease sorts below its release
(§11.3), so `v0.14.0-rc1` vs `0.14.0` **does** warn — measured, row 12.

**Message, exact:**

```go
fmt.Sprintf("uzi: CLI %s is behind server %s; some fields may be missing. Run: brew upgrade uzi-cli",
    cliVersion, serverVersion)
```

rendering with the incident's values:

```
uzi: CLI v0.11.8 is behind server 0.14.0; some fields may be missing. Run: brew upgrade uzi-cli
```

Two properties are decisions, not accidents:

- **Versions print VERBATIM as each side reports them** (`v0.11.8` / `0.14.0`), normalized
  only for the comparison. Normalizing for display would make this line disagree with
  `uzi version`'s own output, which prints the CLI stamp verbatim on line one and the
  server's bare string under `server version`. The user's preview shows this asymmetry.
- **One line, not the preview's two.** Every existing stderr warning in this package is one
  line (`skill.go:214`, `:218`), and hard-wrapping at a fixed column is wrong at every width
  but one. §10 Q1 puts this to the user.

### 3.2 The cache

**Path**: `<Store.Dir()>/version-check.json` → `~/.config/uzi/version-check.json`, mode
**0644**, written through the existing `writeFileAtomic` (`config.go:187`) after
`os.MkdirAll(s.dir, 0o700)`, exactly as `SaveConfig` does (`:93-102`).

**Use `writeFileAtomic`, do not hand-roll** (H4): it is `os.CreateTemp` (0600) → `Chmod` →
`os.Rename`, and **rename replaces a symlink rather than following it**. A plain
`os.WriteFile` would follow one and would be a real vulnerability. Agents run uzi in
parallel, so the atomic rename is wanted for its own sake too (N2).

**Do NOT copy `LoadCredentials`' refuse-on-wide-perms guard** (`config.go:116-120`). The
right property for a cache is **distrust-on-read**, not refuse-on-read (H4c) — a cache that
refuses to work because of a permission bit is a cache that fails commands.

**Shape:**

```json
{
  "servers": {
    "<sha256-hex of the normalized URL>": {
      "version": "0.14.0",
      "checked_at": "2026-08-03T12:45:37Z",
      "cli_version": "v0.11.8"
    }
  }
}
```

**Key** = `hex(sha256(normalizeURL(resolvedURL)))`, where `normalizeURL` is
`strings.TrimRight(strings.TrimSpace(raw), "/")`. Two properties, both load-bearing:

- **The URL is never persisted** (H4b). `credentialSafeBase` does not strip userinfo, and
  `--url 'http://alice:hunter2@host'` is served; a hash cannot leak what it does not
  contain, which beats any strip-the-userinfo helper I could write correctly.
- **A normalization miss costs a cache MISS, never a wrong answer.** Two spellings of one
  host are two entries, each holding that host's own truth. That asymmetry is why the cheap
  normalization is the right one — and it survives hashing unchanged, since hashing affects
  only the key's *form*.

The cost, stated because it is real: the file is no longer human-debuggable — you cannot
tell which server an entry belongs to. For a cache whose entire content is a public version
string and a timestamp, that is worth approximately nothing, and it buys a
credential-persistence class that cannot recur.

**`cli_version` is FORENSICS ONLY and staleness MUST NOT key on it.** The auditor's ungraded
item asks to invalidate when the CLI version changes; that is the correct fix for a
**verdict** cache and is **redundant** under N1's observation cache, where upgrading the CLI
self-heals on the next command with no invalidation at all. Keying on it would force a
needless re-probe after every upgrade and, worse, would encode the verdict-cache mental
model N1 forbids. **The in-repo precedent is exact**: `skillState` (`skill.go:74-79`) stores
`cli_version` and says in its own comment that staleness keys on the hash, *"NOT on
cli_version — a version bump with an unchanged skill must not rewrite. cli_version is
retained for human forensics."* Mirror that verbatim, comment included.

**API:**

```go
const VersionCheckTTL = time.Hour

// CachedServerVersion returns the recorded server version for url and whether the record
// is fresh at `now`. version may be "" on a fresh record — the negative-cache case (the
// last probe failed); the caller must NOT re-probe. The returned string is UNTRUSTED and
// must be sanitized before printing (H4c).
func (s *Store) CachedServerVersion(url string, now time.Time, ttl time.Duration) (version string, fresh bool)

// RecordServerVersion stores the outcome of a probe ATTEMPT. version == "" records a
// failure. Returns an error the caller is expected to ignore.
func (s *Store) RecordServerVersion(url, version string, now time.Time) error
```

`now` is a parameter, not `time.Now()` inside — that is what makes the
clock-moved-backwards case testable without a clock interface.

**Freshness**: fresh iff `!checked_at.After(now) && now.Sub(checked_at) < ttl`. A
`checked_at` in the **future** is NOT fresh (clock moved backwards, or the file was copied
between machines). Never trust a future timestamp.

**Write-side admission rule** — a **resource** control, explicitly **not** a security
control: store `version` only if `semver.IsValid(normSemver(version))` **and**
`len(version) <= 64`; otherwise store `""` (the negative case). Rationale: the 1 MiB
build-metadata string of §0.5 is *valid* semver, so validity alone does not bound the file;
64 bytes is generous against a real coordinate (`0.14.0+g2d60c57` is 15) and rejecting a
longer one is fail-closed (unknown → silent). **This does not replace `cellText` on read** —
H4(c)'s whole point is that a write-side check is bypassed by writing the file directly.

**Degradation, all silent:**

| Condition | Behaviour |
|---|---|
| `env.Store == nil` (no home dir) | Skip the whole check, no probe. Mirrors `maybeAutoUpgradeSkill` (`skill.go:209-211`). |
| File absent | Miss. Not an error (mirrors `LoadConfig`, `recordedSHA`). |
| File corrupt | Miss, treated as empty; not deleted — the next write replaces it atomically. Mirrors `recordedSHA` (`skill.go:118-129`). |
| Entry absent for this key | Miss. |
| Write fails (read-only `$HOME`) | **Silently ignored, never surfaced.** Cost: a probe per command, capped at `serverProbeTimeout`. Documented; `UZI_VERSION_CHECK=0` is the remedy. |
| Map exceeds 16 entries | Drop the oldest `checked_at` on write. `--url` is per-invocation, so an unbounded map is reachable from a script loop. 16 is not load-bearing; unboundedness is. |

### 3.3 Call flow

```mermaid
sequenceDiagram
    participant C as cobra
    participant H as maybeWarnVersionSkew (cmd/uzi)
    participant S as Store (uzicli)
    participant P as serverBuildInfo (cmd/uzi, existing)
    participant W as SkewWarning (uzicli, pure)

    C->>H: PersistentPreRun(cmd)
    H->>H: gf.quiet? -> return          (suppresses the PROBE too, not just the print)
    H->>H: UZI_VERSION_CHECK=0? -> return
    H->>H: exemptFromVersionCheck(cmd)? -> return
    H->>H: !IsValid(normSemver(version)) -> return   (dev build: NO net, NO fs)
    H->>H: resolveSettings; URL=="" -> return        (no config: NO net, NO fs)
    H->>S: CachedServerVersion(hash(url), now, TTL)
    alt fresh
        S-->>H: version, true
    else stale / miss
        H->>P: serverBuildInfo(ctx, env, gf)
        P-->>H: *BuildInfoDTO or nil
        H->>S: RecordServerVersion(url, verOrEmpty, now)   [error ignored]
    end
    H->>W: SkewWarning(version, cellText(serverVersion))
    W-->>H: msg, ok
    opt ok
        H->>H: fmt.Fprintln(env.Stderr, msg)
    end
    Note over C: RunE proceeds. Exit code untouched.
```

**Three ordering properties are contracts, not style:**

- **`cellText` is applied to the server version AFTER the cache read and BEFORE it reaches
  `SkewWarning`** — so the one sanitization point covers the network path and the cache-file
  path together (H4c). Sanitize at fetch or at cache-write and the cache-file path is
  unguarded.
- **`--quiet` returns before the probe**, so it suppresses the network call as well as the
  print. A deliberate strengthening of N5 ("`--quiet` suppresses the warning"): a probe whose
  only output is suppressed is pure cost, and under H5 it is unrate-limited cost. Flagged as
  a strengthening so nobody reads it as a misimplementation of N5.
- **Print with `fmt.Fprintln(env.Stderr, …)`, never `uzicli.Printer`.** Not a style
  preference: `govulncheck` puts `GO-2026-5970` (x/text infinite loop) on a trace through
  `uzicli.Printer.Println` (`output.go:169`), and `version.go` avoids it today only by using
  plain `fmt`. Routing the warning through `Printer` makes a pre-existing, unfixed
  vulnerability newly reachable from every command.

The exit-code constraint is **structural, not asserted into existence**: `PersistentPreRun`
(not `PersistentPreRunE`) has no error return, so nothing this hook does can reach
`ExitCodeFor`. That is why the existing skill hook uses the same seam.

### 3.4 Env gate

`Env` gains `CheckServerVersion bool` mirroring `AutoUpgradeSkill` (`root.go:47-50`)
including its rationale: `DefaultEnv` true, `fakeEnv` false, so unrelated command tests make
no network calls and no filesystem writes (N6). The `uzi version` inline path is gated on the
same field, so existing `version_test.go` cases stay inert.

---

## 4. Exemptions — corrected

`exemptFromVersionCheck(cmd)` walks ancestors, same shape as `underSkillCmd`
(`skill.go:191-198`).

### The rule, and it is enumerable rather than a taste judgement

**Exempt every command that makes no network call of its own.** For those, and only those,
the probe would be the **first** request the command emits, and it carries the user's bearer
token (H3, executed with a canary). For a command that already talks to the server, the probe
adds a request to a stream already carrying the same header to the same host — no new
exposure class.

**Enumerated at HEAD** by grepping `env.client(` per command file and reading each RunE:

| Exempt | Evidence | Why |
|---|---|---|
| `uzi logout` | `login.go` has **0** `env.client` calls; `:177-179` says *"it never calls the API"* | H3. Would ship the token to the server on the way to **deleting it**. |
| `uzi auth token` | `auth.go`'s only `env.client` is `:150` (`whoami`) | H3. A command built so *"a credential must never land on argv"* would emit a request carrying the **previous** credential — `PersistentPreRun` runs before `RunE`, so it is the old one. |
| **`uzi auth status`** | same grep; `:63-85` is `resolveSettings` + print, no client | **H3, and neither report names it.** Identical property to `auth token`: reads the stored credential, makes no call. A rule stated as "the two commands the auditor found" misses it; stated as "local-only", it does not. |
| the `skill` subtree | `skill.go` has **0** `env.client` calls | H3 **and** the existing precedent (`root.go:110`) **and** `uzi skill install` is machine-invoked at every Claude Code session start by the opt-in hook, where an extra stderr line is noise in an agent's context. |
| `__complete`, `__completeNoDesc` | `cobra completions.go:31-34`, `:233-234` | Invoked on every TAB. A 2 s stall on TAB is unacceptable, and stderr during completion corrupts some shells' display. |
| `completion` | cobra default cmd | Its stdout is `eval`'d from a shell rc file; the warning would print at **every shell start**. |
| `version` | `version.go:57` | §1(f): **relocated, not silently exempt** — warns inline from its own live probe and warms the cache from it. Answers N8 the stronger way (exempt *and* share). |

**NOT exempt, decided:**

- **`uzi login`.** It is local-only by the `env.client` grep, and that grep is the wrong
  instrument for it: it builds a client directly at `login.go:53` and calls
  `StartCLIAuth`/`PollCLIAuth` against the same URL. `newRequest` attaches the stored token to
  those calls already, so the probe adds no new exposure. **This is why the rule is "makes no
  network call", not "does not call `env.client`"** — a grep-shaped rule would have exempted
  `login` for the wrong reason and, worse, would have looked right.
- **`--json`.** stderr warning + clean stdout is the entire point of the user's design.
- **`help`.** Harmless; a minimal exemption list is worth more than shaving it.

**Free — no exemption needed.** Verified in `cobra@v1.10.2 command.go:918-993`; all four
return **above** the `PersistentPreRun` loop at `:982`:

| Path | Returns at |
|---|---|
| `--help` / `-h` | `:934` |
| `--version` (root sets `Version:`, `root.go:97`) | `:944-951` |
| non-runnable parent (`uzi run`, `uzi skill` bare) | `:956` |
| `ValidateArgs` failure (wrong arg count) | `:968` |

**N3, now decided rather than noted:** `uzi --version` never warns while `uzi version` does.
**Accept.** `--version` is cobra's terse one-line form, reaching it would need a seam cobra
does not offer, and the two are not interchangeable for this purpose anyway — `uzi version`
is the command that reports the server, so it is where a version conversation belongs. Write
it down at the exemption function so it reads as decided, not overlooked.

**The honest limit on mechanization.** The forward direction is testable: each exempt path
resolves in the cobra tree and probes zero times (§7.3). **The reverse is not** — a *new*
local-only command added later would silently acquire a token-emitting probe, and no static
check catches that (it would need to prove a RunE makes no network call). It is a review
rule, and it belongs in a comment at `exemptFromVersionCheck` naming H3 so the next author
meets it. Stated rather than papered over with a check that would not check it.

---

## 5. The instruction registry — no entry needed, and why

`instructions_test.go:464`:

```go
var instructionRE = regexp.MustCompile("`(uzi [a-z][a-z0-9 %<>-]*)`|(?m)^\\s*(uzi [a-z][a-z0-9 %<>-]*)")
```

Both alternations require literal `uzi` **followed by a space** then `[a-z]`. The message has
neither shape: `uzi:` is followed by a colon, `uzi-cli` by a hyphen, and there are no
backticks around any `uzi <verb>` span. Even a match would be filtered — `liftInstructions`
(`:595-606`) drops a candidate whose second word is not a live top-level cobra command, and
`brew` is not one. Converges with the reviewer's n1, derived independently from the regex.

`api/internal/uzicli` **is** in `instructionScope` (`:474`), so the library half is scanned
too; putting the message there changes nothing.

**Two rules for the coder:**

1. **Do not name any `uzi <verb>` in this message.** "…run `uzi version` for details" lifts a
   candidate and demands a **RUNTIME** entry — *a claim the instruction was executed and its
   outcome asserted*. The reword is free; the entry is not.
2. **RUN the three tests, do not reason about them.** If one reddens, **reword the message**;
   never add a registry entry to make it pass.

---

## 6. TTL — 1 hour, now with two independent arguments

H5 changes the shape of this argument and the lead asked for it to be redone. It does not
change the answer.

**Argument from silence (N12), bounding the TTL from ABOVE.** Because the cache holds an
observation and not a verdict (§3.2), the only staleness failure is: **the server was
upgraded and the user is not told for up to one TTL.** Silence-when-you-should-warn, never a
false warning. The CLI-upgrade direction self-heals instantly. A user therefore learns within
an hour of their next command; server deploys here land on the order of daily, so an hour is
comfortably inside the noise.

**Argument from load (H5), bounding the TTL from BELOW.** `/api/version` is unauthenticated
and carries **no rate limiter** (`route_limiter_mounts_test.go:203`, `noLimiter`), and this
design points every CLI invocation from every user and agent at it.

**But be precise about what the mitigation is, because H5's wording invites over-reading: the
EXISTENCE of the TTL is load-bearing; its VALUE is not.** Worked:

| regime | requests/hour to `/api/version`, 50-agent fleet |
|---|---|
| no TTL (per-invocation) — an agent polling `uzi run list --json` every 2 s | ~90,000 |
| TTL = 1 min | 3,000 |
| **TTL = 1 h** | **50** |
| TTL = 24 h | ~2 |

The three-order-of-magnitude drop is entirely between "no TTL" and "any TTL". Going 1 h → 24 h
buys a further 24× against a baseline that is already negligible, and costs a 24× longer
silence window. So H5 forbids seconds and is indifferent between hours; N12 prefers the short
end of that range. **1 hour, and the grade H5 assigns should not move.**

**One TTL for both outcomes — and the intuition to resist is a SHORTER negative TTL.**
Reviewer B2 asks for a negative observation "with its own TTL", which reads as shorter (get
back to checking sooner). The cost structure inverts that: **a failed probe is the expensive
one** — it burns the full `serverProbeTimeout` (2 s), where a successful one costs ~50 ms.
Shortening the negative TTL maximizes the cost of the case it applies to. If the two were to
differ at all it should be **longer** on failure; that asymmetry is not worth the extra
constant. One value, with this reasoning written beside it so nobody "optimizes" it down.

---

## 7. Test strategy

### 7.1 `SkewWarning` — table, `api/internal/uzicli/versioncheck_test.go`

`N` = naive (raw `Compare < 0`, no normalization, no guards). `G` = normalized, **no
`IsValid` guards**. `A` = normalized + guarded, direction dropped (`!= 0`). All measured.

| # | cli | server | correct | N | G | A | kills |
|---|---|---|---|---|---|---|---|
| 1 | `v0.11.8` | `0.14.0` | **warn** | – | warn | warn | **N** ← the live incident |
| 2 | `0.11.8` | `0.14.0` | **warn** | – | warn | warn | **N** |
| 3 | `v0.14.0` | `0.14.0` | silent | – | – | – | — |
| 4 | `v0.14.0` | `0.14.0+g2d60c57` | silent | – | – | – | — (§10 pin) |
| 5 | `v0.14.0` | `0.15.0+g1a2b3c4` | **warn** | – | warn | warn | **N** |
| 6 | `dev` | `0.14.0` | silent | – | **warn** | **warn** | **G, A** ← B1 |
| 7 | `v0.14.0` | `dev` | silent | – | – | – | — |
| 8 | `v0.14.0` | `` (empty) | silent | – | – | – | — |
| 9 | `v0.15.0` | `0.14.0` | silent | – | – | **warn** | **A** |
| 10 | `v0.11.7.1` | `0.14.0` | silent | – | **warn** | **warn** | **G, A** |
| 11 | `v0.14.0` | `0.14.0.1` | silent | – | – | – | — |
| 12 | `v0.14.0-rc1` | `0.14.0` | **warn** | – | warn | warn | **N** (§11.3 pin) |
| 13 | `v0.14.0` | `0.14.1` | **warn** | – | warn | warn | **N** |

**Read the `N` column: it never warns, on any row — including all five where the correct
answer is warn.** Against this project's real strings the un-normalized comparison is not
partially wrong, it is **inert**. Rows 3, 4, 7, 8, 11 — the natural
"check-it-doesn't-false-positive" cases — all agree with `N`, so **a fixture built from those
alone certifies nothing**. They are documentation pins and are labelled as such: not padding,
but not evidence either.

Each row carries `want` plus `killsNaive` / `killsUnguarded` / `killsDirection`. The test:

1. asserts `SkewWarning` matches `want` on every row;
2. runs the three broken references (three lines each, in the test file) and asserts each
   disagrees with `want` on **exactly** its flagged rows — an exact per-row agreement, not
   "at least N", because a count cannot see *which* row it lost;
3. asserts each flag is carried by ≥1 row. That floor is the vacuity guard: without it
   someone can flip every flag false and the differential passes over a fixture that
   discriminates nothing.

Also assert on **content**: `msg` contains both version strings **verbatim as passed** (row 1
→ `v0.11.8` and `0.14.0`, not `v0.14.0`) and `brew upgrade uzi-cli`.

### 7.2 Cache — same file, `NewStore(t.TempDir())`

| Test | Discriminates against |
|---|---|
| record → read fresh | nothing works |
| record at `now-2h`, read at `now` → not fresh | TTL ignored |
| record at `now+1h`, read at `now` → **not fresh**, no panic | future timestamp trusted |
| record URL A, read URL B → **miss** | a single-blob cache (B3/H4a) |
| record `https://x/`, read `https://x` → hit | key normalization dropped |
| **`--url 'http://alice:hunter2@h'` recorded → the file bytes contain neither `alice` nor `hunter2`** | **H4(b) — the raw URL persisted** |
| record `""` version, read → **fresh, version `""`** | negative caching absent (B2) |
| record a 1 MiB valid build-metadata version → stored as `""`, file stays small | the write-side admission rule |
| record `dev` → stored as `""` | admission rule (invalid semver) |
| CLI version changes between record and read → **entry still fresh, still used** | someone keying staleness on `cli_version` (N1) |
| garbage bytes → miss, no error out | corrupt file surfaces |
| 17 URLs → ≤16 entries, newest retained | unbounded growth |
| unwritable dir → `RecordServerVersion` returns err | (caller-ignores half is 7.3) |

### 7.3 Command level — `api/cmd/uzi/versioncheck_test.go`, via `runCLI`

`runCLI` returns `(stdout, stderr, code)` separately (`commands_test.go:35-45`) — exactly the
channel separation these need.

- **CHANNEL.** Fake with a newer `Build.Version`; assert **stderr contains** the warning
  **AND stdout does not**. Both halves explicitly — per `.claude/agent-team.md`'s "an
  assertion defines its CHANNEL", a single "output contains" covers one channel and reads
  like it covers two.
- **`--json` integrity.** Assert `json.Unmarshal(stdout, &any)` **succeeds**, not
  `!strings.Contains(stdout, "behind")`. The negative-string form passes even if the warning
  landed on stdout in different wording, and this repo has a documented case of exactly that
  vacuity (`WorkerUpgradeBadge.test.tsx`). Parseability is the property the user's constraint
  is about.
- **EXIT CODE, differentially.** Same invocation with `CheckServerVersion` false then true;
  assert the codes are **equal**. On a success path *and* an error path (`run get
  <unknown-id>` → 4), so the warning is proven not to mask or alter a failure. A hardcoded
  `== 0` would go stale and would not cover the error path.
- **SANITIZATION — the four attacks, binding (auditor item 5).** A well-formed `0.11.8`
  passes against a completely unsanitized implementation, so it discriminates nothing. Drive
  each of `\r`, `ESC[2J`, `U+202E`, and a 1 MiB version through **both** sinks:
  - `uzi version` → `serverRows`, the pre-existing H1 sink. Assert the emitted stdout carries
    no C0/C1 control and no U+202E, and that the field is capped.
  - the warning path. Note from §0.5 that only the **1 MiB build-metadata** form
    (`0.14.0+AAAA…`) reaches the message at all — the other three are rejected by rule 3 and
    produce **silence**. **Assert that silence explicitly**, and assert the 1 MiB case is
    capped. A test that drove only the control-character cases at the warning path would pass
    against an unsanitized implementation for the wrong reason, and would read as proof.
  - **Assert on the emitted BYTES** (`strings.ContainsFunc(out, unicode.IsControl)` plus an
    explicit `‮` check), not on a rendered screenshot or a `len()`. §0.5's own lesson: a count
    cannot see a difference the count does not project.
- **Suppression:** `--quiet` → no stderr line **and** `BuildInfoCalls == 0`.
  `t.Setenv("UZI_VERSION_CHECK", "0")` → same pair. Both assert the *probe* is skipped, not
  merely the print.
- **Exemptions, table-driven over the §4 list:** `uzi logout`, `uzi auth token`,
  `uzi auth status`, `uzi skill status`, `__complete …` → `BuildInfoCalls == 0`.
  `uzi version` → `BuildInfoCalls == 1` (no double probe) **and** stdout still begins with the
  CLI version **and** stderr carries the warning.
- **Cache hit end-to-end:** two `runCLI` calls sharing one `Env` and one
  `NewStore(t.TempDir())` → `BuildInfoCalls == 1` total, warning printed **both** times. The
  only test proving the cache is wired to the hook rather than merely correct alone.
- **`Store == nil`** → `BuildInfoCalls == 0`.
- **Dev build:** leave `version` at `dev` (no `withVersion`) → `BuildInfoCalls == 0`. The
  §1(g) short-circuit, asserted rather than assumed.

**Positive control for the whole file:** at least one test must show the warning *appearing*.
A file made entirely of "no warning here" assertions passes against an implementation never
wired into `root.go` — a control that produces no output is not a control.

### 7.4 Gate

`task gate:api`. Live-DB tests out of scope. Lint is ratcheted against `origin/main` —
`git fetch origin main` first if it does not resolve.

---

## 8. Risks

**The riskiest assumption, and how to falsify it in five minutes:** that `serverBuildInfo`'s
best-effort contract holds under a **second** caller. It was written for one command with a
human waiting. Validate early by temporarily flipping `CheckServerVersion: true` on `fakeEnv`
and running `./cmd/uzi/...` — if any pre-existing command test reddens or slows, the contract
does not hold and the design needs revisiting before more code lands. Revert the flip; it is a
probe, not the shipped state.

| Risk | Handling |
|---|---|
| **H1 is pre-existing on `main` and this MR multiplies it** | `cellText` on both sinks (§2, §3.3). Fixing the existing sink is in scope precisely because the hook increases its blast radius from "one deliberate command" to "every command, on the channel the skill tells agents to read" (`SKILL.md:43`). |
| **Prompt injection into an agent's context** | Same fix. `--json` is **not** a shield — `json.Marshal` escapes C0 but not U+202E. |
| A `dev`-stamped CLI never warns | Correct by construction (§3.1 rule 2). The skewed population is brew users, who are stamped — and it means `brew upgrade uzi-cli` is always the right remedy when the message *can* appear, since a `go install` build is `dev` and exempt. |
| Read-only `$HOME` → probe per command | Documented, capped at 2 s, remedied by `UZI_VERSION_CHECK=0`. Same class as the existing hook's read-only-`$HOME` tolerance (`root.go:106-108`). |
| Plain-http server → no probe, silently | Inherited from `credentialSafeBase`, deliberate, and **not to be "fixed"** (rejected alternative 4). |
| Unrate-limited endpoint (H5) | The TTL, §6. |
| Cache file as an attack surface (H4c) | Distrust-on-read; `cellText` after the read; `writeFileAtomic` so a symlink is replaced, not followed. |
| Migration / compatibility | **None.** No schema, no API contract, no wire format. The file is created on first use and its absence is normal; an unknown shape reads as corrupt → miss. Nothing to roll back but the binary. |
| Third copy of `normSemver` | Accepted with reasons (§1, alt. 1); each copy carries its own discriminating test. Extraction is declined scope, not an oversight. |
| `compactText` byte-slices at its 200 cap (`run.go:1009-1012`) and can split a multibyte rune | Known and already documented in-repo at `run.go:986-993` as the inconsistent one. Not fixed here: `cellText` is the house tool and changing it changes output this MR does not own. Noted so it does not read as missed. |

---

## 9. Handoff

**Order** (each step leaves the tree green):

1. `api/cmd/uzi/version.go` — **the `cellText` fix on `serverRows` first**, with its own test.
   It is a live defect on `main`, independent of everything else here, and landing it first
   means the rest of the MR is not gated behind a security fix.
2. `api/internal/uzicli/versioncheck.go` — `normSemver` + `SkewWarning`. §7.1 table + three
   broken references.
3. Same file — cache type + the two `*Store` methods. §7.2.
4. `api/internal/uzicli/fake.go` — `BuildInfoCalls`.
5. `api/cmd/uzi/versioncheck.go` — `exemptFromVersionCheck` + `maybeWarnVersionSkew` in the
   §3.3 order. `root.go` — `Env.CheckServerVersion`, `DefaultEnv`, the hook call, the N4
   comment.
6. `version.go` — inline warning + cache warm.
7. §7.3 command tests, including the four attacks.
8. `e2e/run-e2e.sh`, `SKILL.md`, `docs/cli.md`, `CHANGELOG.md`.

**Acceptance criteria, mechanically checkable:**

- [ ] `task gate:api` green, including all three `instructions_test.go` tests **with no new
      entry in `knownInstructions`**.
- [ ] `SkewWarning` warns on §7.1 rows 1, 2, 5, 12, 13 and is silent on 3, 4, 6, 7, 8, 9, 10,
      11.
- [ ] Each broken reference disagrees with `want` on **exactly** its flagged rows; each flag
      is carried by ≥1 row.
- [ ] All four attack strings, through **both** sinks: emitted bytes contain no
      `unicode.IsControl` rune and no U+202E; the 1 MiB case is capped; the three
      control-character cases produce **silence** on the warning path specifically.
- [ ] Skewed pair: stderr contains the message, stdout does not, and under `--json`
      `json.Unmarshal(stdout)` succeeds.
- [ ] Exit codes **equal** with the check on and off, on a success path and a 404 path.
- [ ] `BuildInfoCalls` is `0` for `--quiet`, `UZI_VERSION_CHECK=0`, `Store == nil`,
      `__complete`, `uzi logout`, `uzi auth token`, `uzi auth status`, `uzi skill status`, and
      a `dev`-stamped binary; `1` for `uzi version`; `1` across two consecutive commands
      sharing one store.
- [ ] The cache file contains neither `alice` nor `hunter2` after a userinfo `--url`.
- [ ] `docs/cli.md` **and** `SKILL.md` **and** `e2e/run-e2e.sh:1682` all name
      `UZI_VERSION_CHECK`.

**For the tester (task #5):** the mutation that proves the semver half is live is deleting
`normSemver`'s `"v" +` prefix — expect §7.1 rows 1, 2, 5, 12, 13 to redden. The mutation that
proves the sanitization half is live is removing the `cellText` call — expect the attack tests
to redden **and check WHICH ones**: only the 1 MiB row should redden on the warning path,
because the other three are already stopped by rule 3 (§0.5). If all four redden there, the
guard order is not what this design specifies. Mutate the **shipped** function, not a copy in
the test file, and restore with a `cp` backup — not `git checkout --`, which reverts to HEAD
and would wipe the uncommitted implementation.

---

## 10. Open questions — for the USER

1. **One line or two?** The preview wrapped the message across two lines with a five-space
   hanging indent. I specified **one line** (every other stderr warning in this package is one
   line; hard-wrapping is wrong at every width but one). If the two-line form was intentional
   rather than a mockup wrap, it is a one-line change and nothing depends on it.
2. **`UZI_VERSION_CHECK=0` in `SKILL.md`?** N11 requires documenting the escape hatch, and
   `SKILL.md` ships **inside the binary to agents** — so it tells every agent how to switch
   off a warning it may find noisy. H1 cuts the other way and harder: the skill already tells
   agents to read stderr (`SKILL.md:43`), which is what makes that channel worth protecting
   and worth explaining. I recommend documenting it, worded so the warning reads as
   actionable. Confirm, or say `docs/cli.md` only.

**Decided, not asked** (recorded so nobody re-opens them mid-implementation): TTL = 1 h for
both outcomes; ahead-of-server is silent; unparseable on either side is silent; exemptions per
§4; `uzi --version` never warns and that is accepted; the env var is `UZI_VERSION_CHECK`; the
cache key is a hash; `cli_version` is forensics-only; `normSemver` is duplicated rather than
extracted; `--quiet` suppresses the probe as well as the print.

## 11. Declining to build

- **No `--check` / `--no-check` flag.** It would need a `SKILL.md` line
  (`skill_drift_test.go` enforces flags), docs and tests, to duplicate `--quiet` + the env
  var.
- **No `uzi upgrade` self-updater.**
- **No warning when the SERVER is behind the CLI.** Nothing actionable for the person at the
  keyboard, and it would fire on every dev laptop pointed at a stable deployment.
- **No `api/internal/verscmp` extraction** (§1, alt. 1) — declined *in this MR*.
- **No async probe** (§1, alt. 2).
- **No stripping of the `Authorization` header** (§1, alt. 4) — the auditor's reasoning,
  adopted.
- **No fix for `compactText`'s byte-slicing** (§8) — shared with two other columns.
- **No per-field capability negotiation** ("this CLI cannot render
  `anthropic_secret_label`"). The correct long-term answer to the incident, and a protocol
  change — a PRD, not a scoped issue MR. Version skew is the cheap proxy the user chose.
