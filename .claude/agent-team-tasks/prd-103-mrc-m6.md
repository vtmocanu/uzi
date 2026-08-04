# PRD #103 — the last two units: M5's MR-C, and M6

**This file is the SPEC.** Corrections amend this file with a dated `## Amendment N`
entry; messages only name the section that moved. Do not carry a requirement in a
message.

**Sweeping this file needs `git grep`.** `.claude/agent-team-tasks/` is gitignored
(`.gitignore:52`) and this file is force-added, so it is tracked and simultaneously
invisible to `grep -r` / `rg`. `--hidden` is the wrong axis and changes nothing.
See carry-forward item 8.

- **Branch**: `prd-103`, in worktree `/home/user/myorg/workspaces/uzi/prd-103`
- **Branch point**: `0111f01c` — verified `== origin/main` after `git fetch origin`
  (0 ahead, 0 behind, clean tree) at 2026-08-04. Not `git rev-parse main`; the
  remote was fetched first.
- **Prior record**: `prds/103-dev-loop-quality-gates.md` (the PRD),
  `.claude/agent-team-tasks/prd-103-m5.md` (27 amendments; MR-C's design is its
  `# MR-C` section), `.claude/agent-team-tasks/prd-103-carry-forward.md`
  (items 1-22 — **items 5, 6, 9, 12, 16-22 bind this work directly**).

---

## Roster

**units: 2, SEQUENTIAL — Unit A (MR-C) then Unit B (M6).** Not a fan-out, and the
reason is a dependency rather than caution: MR-C ships the vitest 2→4 major, and
M6's jsdom fix has a different design on either side of it (`environmentMatchGlobs`
works on vitest 2 and is deprecated from vitest 3). They also contend on three hot
files — `.gitlab-ci.yml`, `Taskfile.yml`, `web/package.json`. One shared worktree,
lead-enforced writer token, exactly one writer unfrozen at a time.

| role | disposition |
|---|---|
| architect | **REPORTED 2026-08-04 at `848cf53d`, re-pinned to `507d1712`** — 8 ranked findings, 2 escalated to the user, full citation table clean. Found the vitest-4 `projects` inheritance hazard and the environment-precedence result. Folded in as **Amendment 3**. Evidence: `probes/prd-103-mrc-m6-architect/README.md`. |
| reviewer | **REPORTED 2026-08-04 at `848cf53d`** — 1 blocking (my react-router error), 10 non-blocking, 1 nit; all 7 citation targets resolved, all 4 day-one figures re-derived. Folded in as **Amendment 2**. Evidence: `probes/prd-103-mrc-m6-reviewer/README.md` + 43 files. |
| auditor | **REPORTED 2026-08-04 at `848cf53d`** — 4 HIGH (incl. the Success Criterion 1 conflict and the `.npmrc` disarm), 4 MEDIUM, 6 below-bar; ran `task scan:secrets` (rc=0, both canaries detected). Folded in as **Amendment 2**. Evidence: `probes/prd-103-mrc-m6-auditor/` + 24 files. |
| fact-checker | **REPORTED 2026-08-04 at `848cf53d`** — 1 refuted (R1), 1 ambiguous (A1), 6 notes, 7 targets confirmed, every `measured` figure re-derived independently. Folded in as **Amendment 1**. Evidence: `probes/fc-mrc-m6/INDEX.txt` + 12 raw files. |
| coder | pending — Unit A, then Unit B |
| tester | pending — after each unit's first commit, never at kickoff |
| documenter | pending — CHANGELOG + `docs/dev-conventions.md` owe a line per carry-forward 12 |
| spec-keeper | pending — `specs/` exists |
| web-ux | **closed — no user-facing surface.** Neither unit changes rendered behaviour, so there is nothing to drive in a browser. *(Amendment 1: the original reason said the units "touch CI config, task targets and test configuration only", which understates Unit A — a vitest 2→4 major can require edits to the 118 test files themselves. The closure stands; its stated scope was wrong. The 118/1660 predicted-count control is what covers that risk.)* |
| researcher | **closed — the research is already written.** M5's brief and the carry-forward doc are the corpus; a fresh investigation would re-derive 22 items somebody already measured. |
| release | pending — user-gated, and `/prd-full` stops at MR creation, so this is MR-open only, never merge. |

---

## Day-one state, MEASURED 2026-08-04 on `0111f01c`, in this worktree

Every figure below is mine, taken today. **The M5 record's corresponding numbers are
stale and must not be quoted** — it says so itself: its measurements predate a
134-commit merge, and "whoever asserts the vulnerability state post-merge must
re-run `govulncheck`."

### govulncheck — BOTH MODULES ARE ALREADY CLEAN ON CALLED VULNS

`go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...`, run in each module root:

```
api         0 called   0 in imported packages   2 in required modules   rc=0
controller  0 called   0 in imported packages   1 in required module    rc=0
```

Raw output: `probes/prd-103-mrc-m6-baseline/govuln-{api,controller}.txt`.

**This inverts MR-C's planned shape.** The M5 record's binding ruling 2 —
*"govulncheck's called vulns are BUMPED here, not filed"* — was written against
`api 2 called, controller 3 called`. Those were fixed by the `go.mod` bumps in
`971c5468`, which are **already on `main`**. So there is nothing to bump and the
gate can land at zero on day one. **Re-run it yourself before believing this**; it
is a claim about a remote database and it is a day old the moment it is written.

Note what the counts are NOT: the residual 2 and 1 are vulnerabilities in
*required modules* that no code path reaches. `govulncheck` exits 0 on them.
Gating on those would be a different, stricter check than the PRD specifies.

### npm audit — the full tree, per user ruling 1 (NOT `--omit=dev`)

Built from `--json`, never the text summary — see the trap below.

```
web    total=8   critical=1  high=2  moderate=5  low=0
agent  total=5   critical=0  high=2  moderate=3  low=0
```

high-and-above, with the fix each one wants:

| pkg | sev | package | range | fix | breaking |
|---|---|---|---|---|---|
| web | **critical** | `vitest` | `<=3.2.5` | `vitest@4.1.10` | **yes (semver major)** |
| web | high | `vite` | `<=6.4.2` | `vitest@4.1.10` | **yes (same major)** |
| web | high | `postcss` | `<=8.5.22` | in-range | no |
| agent | high | `fast-uri` | `3.0.0 - 3.1.4` | in-range | no |
| agent | high | `ip-address` | `<=10.3.0` | in-range | no |

Raw: `probes/prd-103-mrc-m6-baseline/audit-{web,agent}.json`.

**`agent` has changed since M5 measured it** — that record says "1 high of 3
(`fast-uri`)"; it is now **2 high of 5**, `ip-address` being new. A second
confirmation that these numbers have a shelf life.

**🔴 THE TEXT OUTPUT HIDES THE ONLY CRITICAL IN THE REPO.** M5's record states this
and it reproduced exactly: `vitest` (critical) and `vite` (high) appear in
`npm audit`'s human-readable output only as unlabelled *"Depends on vulnerable
versions of…"* lines. A fix list built by reading the text output omits them.
Use `--json`.

**The vitest major is therefore not a separate scope item bolted onto a security
MR — it IS the fix for the critical.** That is worth stating because the concern
raised against ruling 3 (a test-runner major inside a tooling MR) reads differently
once the major and the critical are the same object.

### vitest baseline — the predicted-count control for the upgrade

```
vitest/2.1.9 darwin-arm64 node-v26.4.0
Test Files  118 passed (118)
Tests       1660 passed (1660)
Duration    41.84s
```

Raw: `probes/prd-103-mrc-m6-baseline/vitest-baseline.txt`.

**Post-upgrade green must reproduce BOTH numbers.** A green with a lower collected
count is a silently narrowed suite and is invisible to the exit code. Per
carry-forward 21, write the expected pair down before the run and reconcile
afterwards — a predicted count is the one control an output cannot satisfy by
coincidence. (M5's record cites 116/1635 for this same pair; the suite has grown.
**Use 118/1660**, and re-derive it at your own tip rather than quoting either.)

### jsdom pragma coverage — M6's real distribution

```
web/src test files   118
  with `// @vitest-environment jsdom`   76
  without                               42
```

`web/vite.config.ts`'s `test:` block sets `testTimeout: 20000` and **no
`environment`**, so those 42 run under node today.

**This is the measurement that decides M6's approach and it cuts against the easy
fix.** Flipping the global default to `jsdom` moves 42 files from node to jsdom —
the same silent-wrong-environment bug in the opposite direction, which is precisely
what the PRD warns about. That warning stands unchanged.

**🔴 ALL 42 ARE NODE-SIDE. THERE IS NO MISSING-PRAGMA POPULATION.** *(Amendment 1,
2026-08-04. This paragraph originally read "some are genuinely node-side (`lib/`,
contract tests) and some are DOM tests missing their pragma. Those two populations
need separating before any config change is designed … its result may change the
design." The second population does not exist.)*

```
36  web/src/lib      6  web/src/mocks      0 elsewhere
42  .ts              0  .tsx
```

**The load-bearing argument is STRUCTURAL, and it does not depend on the suite being
green:** of the 42, **zero** are `.tsx` and **zero** import `@testing-library/react`.
There is nothing there that could render a component.

**🔴 AMENDMENT 2: THE ARGUMENT THIS PARAGRAPH ORIGINALLY GAVE DOES NOT CARRY, THOUGH
ITS CONCLUSION DOES.** Amendment 1 argued: *"A DOM test running under node throws
`ReferenceError: document is not defined`; the baseline is 118/118 files green with
those 42 under node. A green suite is therefore a proof that no DOM test is among
them."* **The universal claim is false.** `web/src/lib/prefs.ts:13,23` guards every
access with `if (typeof window === "undefined") return fallback;`, so a pragma-less
test importing it passes **2/2 under node and 2/2 under jsdom** while never touching
`localStorage` under node — built and run, both ways. What a green suite actually
proves is the weaker *"nothing in the 42 THROWS on a missing DOM global"*, and a
vacuously-passing DOM test is precisely the thing that does not throw.

Keep the green-suite observation as corroboration; do not cite it as the proof.
*(Two data points that follow from the same probe: `prefs.test.ts` **does** carry the
pragma, so it is not among the 42 and there is no live instance of this; and **4 of
the 76** carry the pragma while using no DOM at all — `rateLimits`,
`engineQuestion`, `mockApi.limitWait`, `mockApi.notifications` — so a `projects`
split keyed purely on directory makes those four pay jsdom cost for nothing.)*

Three independent probes agree, each shaped to fail differently and each carrying a
positive control over pragma-carrying `.tsx` files: DOM-global references → 1 hit,
the word `localStorage` inside a test *description string*; react / testing-library
imports → 1 hit, a file importing a bare constant with no render; a wider DOM-token
set → 2 hits, the word "screen" in prose comments. Zero real DOM usage.

**So Unit B's CLASSIFICATION step is discharged** — every one of the 42 is node-side.
**The PARTITION is a separate question and is still open.**

**🔴 AMENDMENT 3: "the `projects` split is mechanical — `src/lib/**` and
`src/mocks/**` node, everything else jsdom" WAS MINE, IS NEW IN AMENDMENT 1, AND IS
FALSE IN THE DANGEROUS DIRECTION.** The real partition:

```
                 test files   CARRY pragma   pragma-less
web/src/lib          42            6             36
web/src/mocks        14            8              6
elsewhere            62           62              0
                    ---          ---            ---
                    118           76             42     ✓ reconciles
```

**Those directories are MIXED.** A directory-keyed split assigning `src/lib` and
`src/mocks` to node targets **14 files that run under jsdom today** — six in `lib`
(`prefs`, `rateLimits`, `theme`, `useFollowScroll`, `usePollWhileVisible`,
`useRunStream`; three of them `.tsx` hook tests) and eight in `mocks`. That is the
silent-wrong-environment bug the PRD warns about, introduced **by the fix**.

**The inference gap is precise and worth carrying past this instance:** the
established result is *every pragma-less file is node-side* — a property of **files**.
I converted it into *every file in those directories is node-side* — a property of
**directories**. Nothing licensed that step, and the directories are mixed. (Amendment
2 caught a weaker shadow of this, saying four files would "pay jsdom cost for nothing";
the real exposure is **fourteen losing it**.)

### 🔴 AND TWO VALIDATORS DIRECTLY CONFLICT ON WHETHER THAT MATTERS. DO NOT PICK ONE.

- The **architect measured environment precedence** on vitest 2.1.9, six
  discriminating cells: **docblock pragma > `environmentMatchGlobs` >
  `test.environment` > default(node)**. If that holds, **no config-level mechanism
  can move a pragma-carrying file** — the 76 are frozen, only the 42 are movable, and
  a directory-keyed split *cannot* break those 14.
- The **fact-checker's R3** assumes a directory-keyed `projects` config *does* move
  them, which is what makes the mixed directories dangerous.

**Both are right about what they measured, and the disagreement is not resolvable
today**: `environmentMatchGlobs` is gone in vitest 4 and `test.projects` is a
different layer, so the 2.1.9 precedence result may simply not transfer. The
architect flagged exactly this and said its own finding is void if the pragma stops
winning. **Nobody can settle it until Unit A installs vitest 4.**

**RULING: do not settle it by argument. Make it Unit B's first measurement, and adopt
an acceptance criterion that is correct under BOTH answers:**

> **The per-file environment census is IDENTICAL before and after.**

Enumerate, for all 118 files, which environment each one actually runs under, on
vitest 4, before the config change and after it. That is an assertion on **content**,
not on a count or on a classification being right — so a wrong partition is
*detectable* rather than merely *unlikely*, and being right about precedence is not
required. Re-derive the partition at your own tip either way; files get added.

### Two instrument failures I hit taking these numbers, recorded because they are the shape this milestone is about

1. **`npm audit` reported web and agent as byte-identical** (`total=8 crit=1 high=2
   mod=5` for both). Cause: the second invocation had no `cd` — the Bash tool's cwd
   persists, so both runs measured `web`. Caught by the *uniform result across
   every cell is an instrument failure* rule, not by suspecting the data.
2. **`grep -c '^Vulnerability #'` returned 0 for both modules**, which reads as
   "clean" and happens to be the right answer for the wrong reason: govulncheck
   prints that header only when findings exist, so the grep cannot distinguish
   *no findings* from *wrong pattern*. The discriminating read was the prose
   summary, which reports 2 and 1 uncalled — i.e. the file was never empty.

**Failure 1 reproduced independently THREE MORE TIMES within hours**, by the
fact-checker verifying this brief: `git ls-files web`, a `git grep -- web` and a
`git grep -- .gitlab-ci.yml`, each with cwd left inside `web/` by an earlier
command, **each returning a clean empty result**. The Bash tool's cwd persists
between calls and an empty result is indistinguishable from a true negative. Four
instances in one day, by two careful parties, is a property of the tool rather than
of anyone's attention: **pass absolute paths, or `cd` in the same command.**

**A sixth instance, this one mine, while verifying the partition above**: two
`git ls-files 'src/lib/**/*.test.ts'` calls returned **0** — `**` does not match
zero directory levels in that pathspec, so a real 42-file directory read as empty.
Same signature as all the others: a clean, confident, wrong empty result.

*(Amendment 3 DELETES a seventh "instance" that was recorded here and was not one.
This paragraph claimed `git grep -F -- '--audit-level' <paths>` "finds NOTHING,
because the leading `--` is parsed as end-of-options" and that "a pattern beginning
with `-` needs `-e`". **Measured, all four forms: `-F -- '<pat>' -- <paths>` WORKS,
`-F -e '<pat>' -- <paths>` WORKS, `-F -- '<pat>' <paths>` WORKS, and the bare
`-F '<pat>'` form fails LOUDLY with `error: unknown option`.** The real cause of the
empty result was **the persistent cwd — failure 1 again, a fifth instance** — the
command was run with cwd left inside `probes/fc-mrc-m6/`, so the pathspecs resolved
under that directory; identical command, 0 lines from there and 10 from the repo
root. Two inversions in the retired rule: the form it blamed is the one that works,
and the failure it described is silent where the real bare-dash failure is loud. It
came to me from the fact-checker, which has since retracted it — **I transmitted it
faithfully, and faithful transmission of a wrong mechanism still puts a wrong rule in
the spec.** The one genuine lesson is that the cwd trap has now produced **six**
independent clean-empty results in one day across three parties.)*

---

## Unit A — MR-C

**Deliverable**: `govulncheck` and `npm audit` gating in CI, the npm high-and-above
findings at zero, vitest on 4.x.

### Binding user rulings, carried verbatim from `.claude/agent-team-tasks/prd-103-m5.md`

1. **`npm audit` gates the FULL tree, fixed first, at zero.** Not `--omit=dev`.

   **🔴 THE GATING FLAG IS `--audit-level=high`, AND OMITTING IT MAKES THE GATE RED
   ON DAY ONE AFTER EVERY PLANNED FIX LANDS.** *(Amendment 1, 2026-08-04: the flag
   was in the PRD (`:1877`) and in M5's record (six sites) and **nowhere in this
   brief**, which said only "at zero". That is under-specified in the direction that
   costs a red pipeline.)* Measured: **bare `npm audit` exits 1 on both packages
   today**, and it still will afterwards. `vitest@4.1.10` clears web's
   `@vitest/mocker`, `esbuild` and `vite-node` moderates as a side effect, but
   **five moderates survive every planned fix** — web's `react-router` and
   `react-router-dom`, agent's `@hono/node-server`, `@modelcontextprotocol/sdk` and
   `hono`. **"At zero" means high-and-above at zero.**

   **🔴 AMENDMENT 2, 2026-08-04 — THE SENTENCE THAT USED TO END THIS NOTE WAS MINE
   AND WAS WRONG.** It read: *"All five report `fixAvailable: true` (in-range, no
   major), so clearing them is possible; it is not what the PRD specifies, and
   widening scope to do it is a decision to surface, not to take."* For web's two
   that is **false**, and it is exactly the half a reader prices the scope decision
   from:

   - both `react-router` advisories carry range **`6.0.0 - 7.17.0`**, and the latest
     published 6.x is **`6.30.4`** — the whole 6.x line sits inside the range, so
     **no patched 6.x exists**;
   - `react-router-dom` is declared `^6.28.0` under **`dependencies`** — the SPA's
     runtime router, not a dev tool;
   - `npm audit fix --force --dry-run` proposes vitest and postcss and **contains no
     react-router line at all**. It will not attempt it.

   So clearing web's two is a **React Router 6 → 7.18+/8 major migration of shipped
   runtime code**. `fixAvailable: true` is npm's flag meaning *a fixed version
   exists somewhere*, **not** *`npm audit fix` will do it*. Agent's three do clear,
   but via `@hono/node-server 1.19.14 → 2.1.0` — a transitive **major** landing in
   the worker's MCP transport under the exact-pinned
   `@anthropic-ai/claude-agent-sdk@0.3.219`. "No major" was wrong there too.

   **Act on this instead:** web's two react-router moderates cannot be cleared
   without a runtime router major and are **out of scope**. Agent's three clear with
   `npm audit fix --package-lock-only` (measured: agent to **total=0**,
   `package.json` unchanged, 5 lockfile bumps, **0 new package names**) but cross a
   major under the pinned SDK and **owe test evidence**.
2. **`govulncheck`'s called vulns are BUMPED here, not filed.** *(Vacuously satisfied
   today — there are none. Do not read that as licence to skip the gate.)*
3. **The vitest 2.x → 4.x major is IN.** Raised as a concern and reaffirmed.
4. **No `allow_failure` anywhere.** The pipeline has none today and M5 does not
   introduce the first.

### Placement, already designed — do not re-derive

- **`govulncheck` folds into `lint:api` / `lint:controller`**, which already carry
  the `lint-` cache prefix for the GOCACHE reason it needs.
- **`npm audit` folds into `validate:web` / `validate:agent`**, which already run
  `npm ci`.
- **Zero `.gate_needs` / `.publish_needs` edits** — and *verify that by parsing,
  not by grepping* (carry-forward 2 and 7: this repo lost a chart object for days
  to a grep that found `kind:` in a merged document).
- **Neither may enter `task gate` or any `gate:*`** — but **BOTH GET THEIR OWN
  `Taskfile.yml` TARGETS, AND CI CALLS THOSE TARGETS.**

  *(Amendment 2, 2026-08-04. This bullet used to end "They make network calls and a
  contributor's gate stays offline — the same reason carry-forward 6 bans a bare
  `npx`." **The stated reason is false and the omission was a real design gap.**)*

  **The false half, measured on the pinned tree:** `task gate` is **not** offline
  today. On a cold module cache it needs the network **three times over** —
  golangci-lint, deadcode and gitleaks are all `go run pkg@version`. Control:
  `task scan:secrets` with `GOPROXY=off` returns 201 with *"module lookup disabled
  by GOPROXY=off"* and the script's own instrument-failure branch firing, while the
  same target on the same tree with a warm cache returns 0 with both canaries
  printed. **The true property is that the gate's VERDICT must not depend on a
  network call** — a pinned `go run` fetches a checksum-verified artifact once and
  then answers from the tree; these two query a mutable oracle on every run. State
  the true property: the false one invites someone to "fix" gitleaks out of
  `gate:repo`.

  **The gap: as briefed, Unit A violates Success Criterion 1**, which reads *"CI runs
  no toolchain check that `task gate` does not"* and carries an explicit exclusion
  list (`prds/103-dev-loop-quality-gates.md:1999-2007`) that names four kaniko
  builds, `helm_chart`, the sqlc-drift diff, `test:api-store-it` and
  `e2e:kind-smoke` — and **not** these two. They are toolchain checks (go, npm) in
  per-toolchain jobs. M5 avoided this by putting gitleaks *inside* `task gate`.

  **RULING (lead, 2026-08-04), and it is the shape that satisfies both constraints:**

  1. Define `audit:deps:api`, `audit:deps:controller`, `audit:deps:web`,
     `audit:deps:agent` (name them as you see fit) in `Taskfile.yml`, plus an
     `audit:deps` composite. **CI invokes those targets, never a hand-typed command
     line.** That is what Success Criterion 1 exists to buy — local and CI cannot
     drift apart — and Success Criterion 3 requires it outright: gate recipes are
     defined once, in `Taskfile.yml`.
  2. **Do NOT add them to `gate`/`gate:*`.** A contributor's gate must be
     deterministic against the tree; a target whose verdict is a function of a
     remote mutable database makes `task gate` answer differently on two runs of one
     commit.
  3. **Amend Success Criterion 1's exclusion list in the same MR**, adding these two
     with the reason stated as *the verdict is a function of a remote mutable
     database, not of the tree* — which is a different and more honest exclusion
     ground than the existing list's "cannot run meaningfully from a plain local
     checkout". Both tools run fine locally; that is not why they are excluded.

  A criterion this PRD wrote and then quietly violates is worse than one it amends
  on the record. Amend it.

### Three findings that change the implementation, measured during M5 and not to be re-derived

- **`go run` destroys govulncheck's exit-code discrimination, and the split is
  FOUR-way, not three.** *(Amendment 1, 2026-08-04: this said "exits 3 for findings
  and 1 for its own errors". There is a third nonzero code and a wrapper built on a
  two-way split misreads it.)* Read out of `x/vuln@v1.1.4`: **0** clean, **3**
  findings (`internal/scan/text.go:84`), **2** usage error (`errUsage`,
  `internal/scan/errors.go:26`, e.g. a typo'd flag), **1** everything else via
  `main.go`'s `default:`. Under `go run` every nonzero flattens to **1** — this
  repo's "there are findings" code. Use `go install …@vX` into a temp GOBIN: keeps
  the pin *and* the discrimination. *(My baseline above used `go run` deliberately —
  I wanted counts, and rc=0 is unambiguous either way. The gate needs the codes.)*
- **EVERY NON-TEXT FORMAT FAILS OPEN, not just json.** *(Amendment 2: this said
  "`-format json` always exits 0". True and too narrow.)* Measured against a fixture
  with a genuinely CALLED vuln (`golang.org/x/text@v0.3.0` → `language.Parse`,
  GO-2021-0113), installed binary v1.1.4: `text` → **rc=3**; `-format json`,
  `-json`, **`-format sarif`** and **`-format openvex`** → **rc=0, all four**. So a
  wrapper reaching for sarif to feed GitLab's security dashboard is permanently
  green. The mechanism explains all of them: `errVulnerabilitiesFound` has exactly
  one return site, in the **text** formatter, and its source comment says so —
  *"This returns exit status 3 when running without the -json flag."*
- **🔴 GOVULNCHECK'S EXIT CODES ARE INVERTED AGAINST THIS REPO'S CONVENTION. MAP
  THEM, DO NOT PASS THEM THROUGH.** This repo uses **2 = instrument broken**,
  **1 = there are findings** (`fmt-check:api`, `scripts/deadcode-gate.sh`, the
  golangci-lint pre-flight). govulncheck uses **3 = findings**, **1 = its own
  error**. And a DB outage lands in govulncheck's `1`: measured,
  `-db http://127.0.0.1:1` → **rc=1**, the same code as "no packages matched" and
  "outside a module". So the wrapper must map **govulncheck 3 → repo 1** and
  **govulncheck 1 → repo 2**. Getting it backwards reports a `vuln.go.dev` outage as
  *"there are findings"*, which is the loud-but-misleading shape carry-forward 3
  names.
- **Two positives worth banking so nobody spends design time on them.**
  `GOVULNDB=http://127.0.0.1:1` is **ignored** (rc=3, vuln still found) — only the
  `-db` flag works, and a flag is visible in `Taskfile.yml`, so the environment-
  variable hijack that forced `scan-secrets.sh` to *refuse* `GITLEAKS_CONFIG` has no
  analogue here. And govulncheck caches nothing under the project dir, so it neither
  enters gitleaks' walk nor forces a cache-key decision.
- **govulncheck has NO suppression, baseline or allowlist mechanism** — `-h` at
  v1.1.4 offers only `-C -db -format -mode -scan -show -tags -test`. Every other
  gate here has an escape (golangci-lint ratchets, deadcode has a committed
  baseline, knip stages severity, gitleaks has allow markers). Under ruling 4 (no
  `allow_failure`) that means a future called vuln with no fix is an unremediable
  red: **api already carries `GO-2026-5932` (`golang.org/x/crypto@v0.53.0`) with
  `Fixed in: N/A`**, uncalled today. Cheap derisk available now: `GO-2026-5942`
  (`x/net@v0.55.0`, fixed in v0.56.0) is uncalled in both modules — bump it and
  remove one future tripwire.
- **`govulncheck -test` defaults to `false`, the opposite of this repo's deadcode
  convention** (`scripts/deadcode-gate.sh` runs `deadcode -test`), so a reader will
  assume symmetry. It excludes 293 api and 10 controller test files from the call
  graph. Enabling it changes nothing today (still 0/0/2 and 0/0/1). **Say which you
  chose and why** — this is a scope decision to write down, not a live gap.
- **A version BUMP of a devDependency fails SILENTLY, unlike a new one.** Carry-forward
  12 is about a *new* dep: `npm run` puts `node_modules/.bin` on PATH and fails
  closed with `command not found`. A bump cannot fire that — the binary is already
  there, so a stale checkout runs **vitest 2 while the lockfile says 4**, green,
  with nothing naming the mismatch. The remedy is **not**
  `--ignore-scripts --save-dev --save-exact`; that guards the other hazard. It wants
  a lockfile-versus-`node_modules` staleness check, or a `vitest --version`
  assertion in the gate. **Design this deliberately; it is the one genuinely open
  design question in Unit A.**

### Amendment 2 — five more things Unit A must handle, all measured in the design wave

**A2-1. 🔴 A ONE-FILE `.npmrc` IN THE SAME MR DISARMS THE WHOLE WEB npm-audit GATE,
AT EXIT 0.** This is the `.gitleaks.toml` shape M5 spent a milestone closing,
reappearing in a tool that has no canary. All three of web's high-and-above are
`dev=true` (`vitest`, `vite`, `postcss` — lockfile-verified), so:

```
no .npmrc                    rc=1   <- armed
.npmrc:  omit=dev            rc=0   <- DISARMED, prints "found 0 vulnerabilities"
.npmrc:  audit-level=none    rc=1   <- CLI --audit-level wins for THAT key
```

`--audit-level` on the command line does beat `.npmrc`, but **`omit` is a different
key**, so nothing on the command line contradicts it. There is no `.npmrc` anywhere
today (root, `web/`, `agent/`, `~`), so this is an *add*, not an edit, and it would
appear in the diff — but nothing fails.

**Remedy, measured to work: put `--include=dev` (or `--omit=`) on the gate command**;
rc goes 0 → 1 again under both the `omit=dev` and `omit[]=dev` spellings. Keep the
severity on the command line too, never in `.npmrc`. And **`web/.npmrc` and
`agent/.npmrc` join the gate-config file list** that reviewers watch.

**🔴 AMENDMENT 5: THERE IS A SECOND DISARM VECTOR AND IT LEAVES NOTHING IN THE DIFF
AT ALL — `NPM_CONFIG_OMIT=dev` AS AN ENVIRONMENT VARIABLE.** Six-arm control measured
at `1c751ae3`, the last commit where a dev-only high still exists so the disarm is
observable:

```
0  no .npmrc, no env                        rc=1   crit 1 / high 2 / mod 5
1  .npmrc: omit=dev                         rc=0   crit 0 / high 0 / mod 2   DISARMED
2  .npmrc: omit=dev   + --include=dev       rc=1   8 back
3  .npmrc: omit[]=dev + --include=dev       rc=1   8 back
4  env NPM_CONFIG_OMIT=dev, NO .npmrc       rc=0   crit 0 / high 0 / mod 2   DISARMED
5  env NPM_CONFIG_OMIT=dev + --include=dev  rc=1   8 back
```

**Arm 4 is strictly worse than the `.npmrc` route this section was written about.**
A2-1's threat model was *"a one-file `.npmrc` would at least appear in the diff"*. A
**GitLab CI variable named `NPM_CONFIG_OMIT`** reaches every npm invocation in every
job and **never appears in a merge request at all** — no file, no diff, no review
surface. It is set in project settings by anyone with the rights, and nothing in the
repo records it.

**Arm 5 shows the same one-flag remedy closes both — but ONLY if `--include=dev`
lives in the version-controlled `Taskfile.yml` target.** Put it in a CI job script
instead and the flag itself becomes editable in the same invisible place as the
attack. This is the concrete reason the Placement ruling insists the recipe lives in
the Taskfile and CI merely calls the target.

Reconfirmed across all six arms: `metadata.dependencies` is byte-identical, so there
is still **no in-band canary** for npm audit.

**A non-remedy, checked and retracted inside the auditor's own probe**:
`metadata.dependencies` is byte-identical (515/403) armed and disarmed, so it cannot
serve as an in-band canary. **npm audit has no in-band positive observation that
separates an armed run from an omit-disarmed one.** That is why A2-2 matters.

**A2-2. `npm audit`'s rc=1 is AMBIGUOUS and one branch is a network failure.**
Registry unreachable → **rc=1** with `npm error audit endpoint returned an error`;
findings → **rc=1**. Same with `--json`. It fails closed, which is right, but it
collapses the two states the Go half is being carefully engineered to separate. The
wrapper must read the output (or `--json`'s `error` object) and **exit 2** on the
network branch. A typo'd level fails closed loudly (`npm warn invalid config`), so
`--audit-level=none` is the only silent hole and A2-1's remedy is what closes it.

**A2-3. DO AGENT FIRST — the fix ordering is inverted relative to shipped exposure.**
web's three high-and-above are **all dev-only**: build-time, not in the web image's
runtime. agent's two are **production dependencies that ship in the worker image** —
confirmed at `agent/templates/base/Dockerfile:191`, `RUN npm ci --omit=dev`, with all
seven packages `dev=false`.

**🔴 AMENDMENT 5 CORRECTION: THE DEPENDENCY PATH AND THE SSRF LINK WERE BOTH MINE AND
BOTH WRONG, IN THE OVERSTATING DIRECTION.** This paragraph said the two are *"reached
via `@anthropic-ai/claude-agent-sdk` → `@modelcontextprotocol/sdk`"* and that *"this
repo runs an https-only SSRF allowlist as a named control, so that class is not
theoretical here."*

- **The hop that decides the threat model was missing.** Sole dependents, measured:
  `ip-address@10.4.0` ← **`express-rate-limit@8.5.2`** (rate-limit key derivation
  from a client IP), and `fast-uri@3.1.5` ← **`ajv@8.20.0`** and `ajv-formats` (JSON
  Schema `$ref`/`$id` URI resolution). Both sit under the SDK, but naming the SDK as
  the consumer skips the layer that says what the flaw can actually reach.
- **`FORGE_ALLOWED_BASE_URLS` IS GO.** It lives in `api/internal/config/config.go`.
  **No npm package participates in it**, and the api and the worker are different
  processes. My sentence connected two things that do not touch.

**The shipped-versus-not argument, which is the one that actually orders the work, is
unaffected and was correct.** Only the mechanism sentence was inflated.

**And the advisory count is FIVE, not two**: `ip-address` carries three
(`GHSA-mwp4-54f8-5fhr` high, `GHSA-4xrf-jv44-h6hh`, `GHSA-22jq-vg5j-6vgg`) and
`fast-uri` two (`GHSA-v2hh-gcrm-f6hx`, plus `GHSA-7p8r-x3mc-p8w7` which the design
wave never named). All five are genuinely closed by the installed versions, verified
two ways — range arithmetic against `api.github.com/advisories`'
`first_patched_version`, and the registry's own matcher via `npm audit` on the
post-fix tree reporting `total=0`, which is the stronger check because it is the same
oracle the gate will use. Note `fast-uri@3.1.5` sits at **exactly**
`GHSA-7p8r-x3mc-p8w7`'s first patched version, zero margin — the next 3.x advisory
reddens the agent gate with nobody's diff in it.
**No reachable call path was traced, so this is not a claim of exploitability** — the
argument is shipped-versus-not, which is measured. The agent fix is nearly free
(total=0, no `package.json` change, 0 new package names) and **does not depend on the
vitest major landing**, so it is a one-commit derisk that can go first.

**A2-4. THE VITEST-4 SUPPLY DELTA, AND THE ONE ITEM A VERSION NUMBER DOES NOT TELL
YOU.** Resolved with `npm install --package-lock-only --ignore-scripts` in scratch:
**454 → 450** distinct packages, **4 new, 8 removed, 39 version-changed**. Three of
the four new are unremarkable (`@types/chai`, `@types/deep-eql`,
`@standard-schema/spec`). The fourth is not: **`obug@2.1.4`, npm-created 2025-11-11,
single maintainer, pulled DIRECTLY by `vitest` at `^2.1.1`** — a roughly nine-month-old
single-maintainer package becoming a direct dependency of the test runner, on a caret
range, so future minors flow in on any lockfile refresh. The MR owes: its publish
provenance; explicit acknowledgement that **five further majors ride along**
(`chai 5→6`, `tinyrainbow 1→3`, `std-env 3→4`, `es-module-lexer 1→2`,
`tinyexec 0.3→1`); whether any of the 39 changed packages gained an install script
(the `agent-browser` class — the lockfile records `hasInstallScript`); and that the
lockfile came from an **explicit pin**, never from `npm audit fix --force`.

Two facts that remove worries rather than add them: **the vitest major does not force
a vite major** (vitest@4.1.10 wants `vite ^6 || ^7 || ^8`, web declares `^6.0.5` and
has 6.4.3 installed — the flagged `vite <=6.4.2` is the *nested* 5.4.21 under vitest
2), and its engines are `^20 || ^22 || >=24`, so `node:22-alpine` is fine. But
**vitest@4.1.10 alone leaves web at high=1 (`postcss`) plus moderates**, so the major
does not satisfy ruling 1 on its own — postcss 8.5.23/24/25 are inside `^8.4.49` and
land lockfile-only.

**A2-5. THE OPEN DESIGN QUESTION IS ANSWERED: USE `npm ls`.** The brief floated a
lockfile-versus-`node_modules` staleness check *or* a `vitest --version` assertion,
and did not choose. Measured on the real git-pull shape (v1 installed;
`package.json` + lockfile replaced from a separate resolve; hidden lockfile
untouched):

```
npm ls <pkg>       rc=1   'ms@1.0.0 invalid: "2.1.3" from the root project'   <- THE DETECTOR
npm ci --dry-run   rc=0   says "change ms 1.0.0 => 2.1.3" in its OUTPUT only
npm audit          rc=0   structurally blind — it reads the lockfile
npm outdated       rc=0
```

`npm ls` exits **1** with a **positive observation naming both versions**, needs no
new dependency, is offline, and covers **every** dependency rather than the one bump
you remembered — strictly better than the `vitest --version` assertion.

**🔴 AMENDMENT 4: DO NOT DESCRIBE IT AS A "LOCKFILE-VERSUS-`node_modules` STALENESS
CHECK". THAT PHRASE IS THIS BRIEF'S AND IT IS FALSE.** `npm ls` reports `invalid`
only when an installed version violates a **declared range in `package.json`** — so
it is **inert for a lockfile-only transitive bump**, at any `--depth`, not just 0.
Measured against `1c751ae3`, a lockfile-only commit **in this very MR**: all five
bumped packages are transitive and each parent's declared range admits *both* the
pre-fix and post-fix version —

```
express-rate-limit -> ip-address         ^10.2.0             10.2.0  and 10.4.0  both OK
ajv                -> fast-uri           ^3.0.1              3.1.3   and 3.1.5   both OK
mcp-sdk            -> hono               ^4.11.4             4.12.27 and 4.13.0  both OK
mcp-sdk            -> @hono/node-server  ^1.19.9 || ^2.0.5   1.19.14 and 2.1.0   both OK
```

— and `npm ls --depth=0` in `agent/` returns rc=0 with **none of the five appearing**.

**The detector is not broken and the choice stands**: controlled both ways on a
handmade package, a stale *direct* dep gives rc=1 naming `foo@1.0.0 invalid:
"2.0.0" from the root project`, and matching the version gives rc=0. It genuinely
covers the `web`/`vitest` case, which is the hazard it was chosen for, and it is why
`121d7610`'s exact pin matters — `^4.1.10` would have made it loose.

**ACCURATE WORDING, required in the Taskfile comment, the CHANGELOG and the MR
description:** *catches a stale DIRECT dependency; a lockfile-only transitive bump is
invisible to it, and `npm ci` is the only guarantee there.* A shipped comment
claiming the lockfile framing is a **blocking** defect, not a nit — it would tell
every future reader the gate covers a case it cannot see. **Where it
lies:** in the *other* stale shape, `npm install --package-lock-only` run in place
(which also rewrites `node_modules/.package-lock.json`), `npm ls` returns **rc=0 and
prints `2.1.3` while `node_modules/ms/package.json` says `1.0.0`** — confidently
wrong. A git pull cannot produce that state, but **someone testing this will**. Also
measured: CI is not exposed (`npm ci` restores), and `npm ci` itself fails loudly
(rc=1, `EUSAGE`) on a `package.json`/lockfile mismatch.

**A2-6. THE CALIBRATION PROBE ALREADY EXISTS — DO NOT MUTATE `api/go.mod` TO BUILD
ONE.** Both modules are at 0 called, so nothing in this repo can redden the
govulncheck gate. The reviewer built a throwaway module on
`golang.org/x/text@v0.3.5` calling `language.Parse` that settles all three exit-code
claims at once: **binary rc=3, `go run` rc=1, `-format json` rc=0**. Two further arms
confirm what the `go install` route buys: unreachable DB → binary rc=1, unparseable
Go file → binary rc=1, both flattened to 1 under `go run`. Reuse it. **Note for
calibration property 3**: the unparseable-file arm prints **absolute** paths, so the
repo-root-relative check applies to *finding* lines, not to govulncheck's loader
errors.

### `node_modules` in THIS worktree — the standing hazard does NOT apply, and check before you trust that

Carry-forward 19 says a PRD worktree's `node_modules` are symlinks into `main`, so
an install writes *through* them into the shared reference tree. **That is not the
state here.** Both were ABSENT; the lead installed real local trees on 2026-08-04
with `npm ci --ignore-scripts` in each, and verified
`/opt/homebrew/bin/agent-browser` still points into
`/opt/homebrew/Cellar/agent-browser/0.31.1` on both sides of the `agent/` install.

**Re-check with `ls -ld web/node_modules agent/node_modules` before any npm
operation** rather than trusting this paragraph — it is exactly the kind of claim
that is true when written and false when read, and `git status` cannot show you
(they are gitignored, so no diff can reveal them).

Any further install in `agent/` still needs `--ignore-scripts`: npm 11.17 prints
`npm warn allow-scripts … not yet covered by allowScripts:` naming
`agent-browser`, **which reads as "these were skipped" and is advisory — the
postinstall runs anyway.**

---

## Unit B — M6

**Deliverable**: coverage visible on every MR for `api`, `controller` and `web`;
`-race` on `test:controller`; the jsdom environment fix; `.gitignore`'s vestigial
`coverage.out` line resolved.

- **`-race` is scoped to `test:controller` ONLY.** `test:api` already has it
  (`go test -race -count=1 ./...`), and `test:api-store-it` has it in its
  `-run 'LiveDB$'` sweep. The PRD previously claimed otherwise and corrects itself.
- **No failing threshold** (Decision 6). Totals printed in CI job output, GitLab's
  coverage regex wired so MRs show the number.
- **`web` needs `@vitest/coverage-v8`** added — it is not currently a dependency.
  Per carry-forward 12 that owes a CHANGELOG line and a `docs/dev-conventions.md`
  line, and per carry-forward 6 the Task target **delegates to a `package.json`
  script** rather than reimplementing the command.
- **jsdom: `environmentMatchGlobs` is REMOVED in vitest 4, not merely deprecated.**
  Settled against three package tarballs rather than doc prose: 2.1.9 has it
  implemented with no `@deprecated` tag; 3.0.0 has it with a `@deprecated` JSDoc
  *and* a runtime `logger.warn`; **4.1.10 has zero occurrences of it in the entire
  package.** After Unit A the tree is on 4.x, so it is gone, not noisy.
- **The route is `test.projects`, and `test.workspace` is NOT an alternative.**
  *(Amendment 1, 2026-08-04. This line read "the projects / workspace config is the
  only route", inherited verbatim from `prds/103-dev-loop-quality-gates.md:1947-1949`
  where it was written against vitest ≤3.)* `test.workspace` was **removed in
  Vitest 4** and throws: *"The `test.workspace` option was removed in Vitest 4.
  Please, migrate to `test.projects` instead."* It fails loud rather than silent, so
  this is a spec precision fix and not a live hazard — but the brief is the spec.
  M5's recommendation, adopted: **re-scope M6 onto `test.projects`; do not pull the
  jsdom migration into MR-C.** Start from the discharged 76/42 split above.
- **If the first `-race` run on `controller` is RED, STOP.** File the races as their
  own issue and land `-race` behind `allow_failure: true` **with a dated expiry
  note** rather than leaving a permanent advisory job (Decision 3). Note this is the
  one sanctioned exception to Unit A's ruling 4, and it is Decision 3's, not a
  licence to reach for `allow_failure` elsewhere.
- **Calibration**: confirm the coverage number MOVES when a test is deleted, and
  that the GitLab regex picks it up on the MR. A coverage job reporting nothing
  looks identical to one reporting a stable number.

### Amendment 2 — four measured facts Unit B should start from

**B2-1. 🔴 ADDING `@vitest/coverage-v8` REDDENS M4's `deadcode:web` GATE, AND THE FIX
IS A CONFIG DECLARATION RATHER THAN AN IGNORE ENTRY.** `web/knip.jsonc` sets
`"devDependencies": "error"`, which is a *gating* tier. Three-arm control, restored
and `git status`-verified:

```
arm 0  baseline                                    rc=0   22 warn findings
arm 1  dep added, no coverage config               rc=1   "Unused devDependencies (1)
                                                           @vitest/coverage-v8  package.json:39:6"
arm 2  arm 1 + coverage: { provider: "v8" } in
       vite.config.ts's test: block                rc=0   zero mentions
```

All four calibration properties hold on arm 1 (nonzero exit, tool name in output,
repo-root-relative path, green-on-restore by VCS). **Declare the provider in
`vite.config.ts`; do not reach for `ignoreDependencies`** — the declaration is what
makes knip's finding genuinely false, the ignore entry just silences a true one.

**B2-2. `-race` ON `controller` IS GREEN — Decision 3's `allow_failure` escape hatch
is not needed.** `go test -race -count=1 ./...` in `controller/` → rc=0, 6 packages
ok, no `WARNING: DATA RACE`, no FAIL. **Caveat that keeps this honest**: one run, on
darwin/arm64. CI is linux/amd64 under contention and race detection is probabilistic,
so this **de-risks rather than settles**. The "if the first run is red, STOP" branch
above stays live; it is now unlikely rather than expected.

**B2-3. `.gitignore` carries `coverage.out` and nothing else coverage-shaped** — no
`web/coverage/`, no lcov pattern. vitest's default reporters write to
`web/coverage/`, so either pick text-only reporters or add the entry. Note
`scan:secrets` is scoped to **tracked** files, so the gitleaks exposure M5's handover
flagged for `web/dist` does **not** apply to a coverage directory.

**B2-4. Parsed, not grepped: there is no `coverage:` key and no
`artifacts:reports:coverage_report` anywhere in `.gitlab-ci.yml` today.** M6 adds the
first of each.

**And a counter-example to Hard constraint 2 that a reader will hit**: `e2e:kind-smoke`
is in **neither** `.gate_needs` nor `.publish_needs`. Pre-existing and out of scope —
recorded here so that the amended "ANY new CI job" reads as a rule with a known
standing exception rather than as a rule already being broken.

---

## Hard constraints — both units

These are not style preferences; each one is a measured failure somewhere in
`CLAUDE.md` or the carry-forward doc.

1. **`Taskfile.yml`**: no `sources:`/`generates:`/`status:`, no `dotenv:`, no
   `includes:`, no dynamic root `vars:`, **no unquoted variable spliced into a
   `cmds:` line** (branch names legally contain `;`, `$` and backticks, and the MR
   author picks the target branch). npm targets **delegate** to a `package.json`
   script — a target that reimplements the command drops that script's flags
   silently.
2. **A new GATE job — any blocking check, in any stage — must be added to BOTH
   `.gate_needs` and `.publish_needs`**, and gets **its own `cache:` prefix** if it
   builds something the jobs sharing that key do not. Verify list membership by
   parsing, never by grepping.

   *(Amendment 3, 2026-08-04, and this line has now been wrong in BOTH directions.
   It first read "a new **lint-stage** CI job" — too narrow, and it would have missed
   M6's coverage job. Amendment 1 corrected it to "**ANY** new CI job" — too wide,
   and measurably so: **12 existing jobs are correctly in neither list**
   (`build:{api,web,controller,agent}`, `publish:{api,web,controller,chart,agent}`,
   `publish_brew`, `e2e:kind-smoke`, `demo-fail`), and adding a new publish job to
   `.publish_needs` would be a self-dependency.*

   *The middle term was already written in the repo and neither of my versions used
   it: `.gitlab-ci.yml:292` says "A new **gate** job MUST be added to this list AND
   to `*publish_needs`". Carry-forward 2's wording is **descriptive** — "any new CI
   job that is not in both is invisible to them" is a statement about visibility, not
   an imperative — which is how I misread it as licence to widen. **Two corrections
   to one line, in opposite directions, both by paraphrasing a source that was
   already precise.**)*
3. **Prefer a pinned `go run pkg@version` / `go install pkg@version` inside the
   already-pinned `golang:1.26`** over a new image family. Do not add dev tooling
   to root `devbox.json` — that file is tier-2 *worker* config.
4. **Never a bare `npx <tool>`** — it fetches from the network when the dep is
   missing, which a gate may not do.
5. **The CI-skip marker in a commit message skips the pipeline**, quoted or not,
   including in a merge tip. Check with
   `git log -1 --format=%B | grep -c -F '[skip ci]'` before pushing a tip you need
   a pipeline for. A `skipped` pipeline is not `failed` — the MR still reads
   mergeable, because `allow_merge_on_skipped_pipeline: true` is set here.
6. **Commit with a pathspec** — `git commit -- <paths>`. Staging by path is
   necessary and not sufficient: `git commit` takes the whole index, and in a
   shared worktree another agent's staged work is in it (carry-forward 20, where
   exactly this swept a third agent's source file into a docs commit).
7. **Restore a mutation with a `cp`-based backup, never `git checkout --`** — it
   reverts to HEAD and silently wipes uncommitted work. A broken restore reddens
   loudly and reads as proof.
8. **`grep` here is ugrep**: use `-F` for literals, `git grep` when the question is
   about tracked content. `$?` after a pipe reads the last command — redirect to a
   file, read `$?` on the next line, then grep the file. zsh is
   `${pipestatus[1]}`, one-indexed, **not** bash's `${PIPESTATUS[0]}`, which
   expands to nothing here.

## Calibration — Success Criterion 8, which is the PRD's whole point

**Every check either unit adds ships with the mutation that proves it is live**,
recorded in the MR description: the check reddens on a deliberate violation and
greens on its removal. A PRD whose subject is quality gates must not ship a gate
whose liveness is unverified.

The bar is **four properties, not one** (carry-forward 5):

1. **Non-zero exit.** `task` returns **201** on any failure, never the underlying
   code. Test for non-zero, never a number. *(And 109 means the Taskfile did not
   parse and nothing ran.)*
2. **The rule or tool NAME in the output.** rc≠0 alone is satisfiable by an
   unrelated finding.
3. **A sane, repo-root-relative path.** A `../` in a finding path is an invalid run,
   not a finding.
4. **Green on restore, verified with `git status`** — not a grep count. A count
   only means something if you already know how many occurrences ought to exist;
   the VCS does not require you to know that.

**A control that produces no output is not a control**, and the frequency is the
point: carry-forward 17 records **four** non-controls in a single round, by a
careful agent, in a milestone about gate liveness — all four failing toward
"looks fine". Assert that the control **observed something specific**.

## Reporting discipline (carry-forward 9)

Three milestones' worth of validator reports have been long, correct, and expensive.
For these two units:

- **Lead with your tip SHA.** Verify THAT SHA, never `HEAD` — `HEAD` moves under you.
- Findings as a **short ranked list**.
- **Every measurement written to a file inside the worktree**, with the report
  naming the path. The evidence must exist; it must not all arrive in a message.
- **No inline transcripts** the lead can read for itself.
- **Label every claim `measured` or `suspected`.** M5's handover did this and it is
  the format worth copying — half of it was suspected, and shipping those as facts
  is the failure this PRD is about.

---

## Documentation this work must correct, in its own MR (fix-the-doc)

**D1. 🔴 SUCCESS CRITERION 5 DOES NOT SAY WHAT FIVE DOCUMENTS CLAIM IT SAYS, AND ITS
LITERAL PREDICATE IS ALREADY MET.** Verbatim at
`prds/103-dev-loop-quality-gates.md:2059-2060`:

> `.claude/agents/auditor.md` no longer documents the absence of a secret scanner,
> because one runs.

Whitespace-flattened, the entire 74-line Success Criteria section contains **zero**
occurrences of `gitleaks`, `govulncheck` or `npm audit`; `secret scanner` appears
**once**. Control on the same instrument, same section: `gofmt` 2, `shellcheck` 2,
`knip` 2, `coverage` 2 — it is live, it simply has nothing to find. And the criterion's
own predicate is satisfied: `.claude/agents/auditor.md:116` reads *"secrets are
covered as of PRD #103 M5 MR-B"*.

Yet *"Success Criterion 5 is atomic across gitleaks AND govulncheck AND npm audit"*
appears in **five places** — `.claude/agents/auditor.md:124-126`,
`.claude/agent-team-tasks/prd-103-m5.md:243` and `:1072`,
`prds/103-dev-loop-quality-gates.md:4` and `:1804-1805` — **and in this brief**. It
is the stated reason M5 has stayed unticked.

**The decision it produced is right and its stated ground is not.** The real residual
is visible one line further on in the same file: `auditor.md` now documents the
absence of *dependency-vulnerability* scanning. That is a genuine gap and it is
exactly what Unit A closes.

**Ruling: amend Success Criterion 5's TEXT to name all three tools**, making the
five restatements true, rather than amending five restatements to match a criterion
that has already been satisfied. Cheapest honest fix, and it keeps the record
consistent with what everyone has been reasoning from.

*(Recorded with its own instrument failure, because the auditor caught and corrected
it in place: a first pass reported `secret scanner: 0`, an artifact of the phrase
being line-wrapped in the source. **A line-oriented grep cannot see a wrapped
phrase** — hence the flatten above.)*

**D2. Two PRD sentences Unit A falsifies.**
- `prds/…:1877-1878` — *"`govulncheck` … and `npm audit --audit-level=high` …
  initially **`allow_failure` only** until the current finding count is known."*
  Overridden by user ruling 4; the finding counts are now known. Stale.
- `prds/…:349-350` — *"Its checks (`shellcheck`, `yamllint`, `gitleaks`,
  `govulncheck`) are **repo-wide**, so … they genuinely cannot fold into a
  per-toolchain `validate:*` job."* **govulncheck is per-Go-module, not repo-wide.**
  The brief's placement is right and this sentence is wrong; it is the one a future
  reader would follow into opening two needless jobs.

**D3. Success Criterion 1's exclusion list** gains govulncheck and npm audit, with
the reason stated as *verdict is a function of a remote mutable database* — see the
RULING in Unit A's Placement section.

---

## Amendment 1 — 2026-08-04, from the fact-checker's design-wave pass

**Landed while the architect, reviewer and auditor were still reading `848cf53d`.**
Their reports are pinned to that SHA and are **EARLY, not stale**: correct at the
state they read, and correct forever at that state. Reconcile them against this
amendment; do not re-dispatch them for it. *(That distinction is the skill's own and
the two have different fixes — stale means re-pin what you verify, early means
re-read, not re-run.)*

Every claim I labelled `measured` was **re-derived independently** and every one
reproduced: govulncheck 0/0/2 and 0/0/1; web 8/1/2/5/0 and agent 5/0/2/3/0 (with a
`cmp` distinctness control aimed squarely at my instrument-failure #1); all five
high-and-above rows exact; 118 files / 1660 tests; 76/42 from two separate
enumerations. The text-output-hides-the-critical claim reproduced exactly.

What moved, in order of what a wrong answer would have cost:

| | section | change |
|---|---|---|
| **R1** | jsdom coverage | **REFUTED.** All 42 pragma-less files are node-side; the missing-pragma population does not exist. Unit B's classification step is discharged, not deferred. |
| **N1** | Unit A ruling 1 | `--audit-level=high` was in the PRD and M5 and **absent here**. Without it the gate is red on day one after every planned fix, because five moderates survive. |
| **N2** | Hard constraint 2 | Widened back to **any** new CI job. The narrow "lint-stage" form would have missed M6's coverage job. |
| **A1** | Unit B jsdom | `test.workspace` is **removed** in vitest 4 and throws; the route is `test.projects` alone. |
| **N3** | Unit A findings | govulncheck's exit split is four-way (0/1/2/3), not three; and the `-format json` fail-open has a source-level proof rather than an observation. |
| **N5** | Roster, web-ux | Closure reason understated Unit A's blast radius. Closure itself unchanged. |
| **N6** | instrument failures | Failure 1 reproduced three more times, by a second party, within hours. Plus the `git grep -e` leading-dash trap. |

**One transit change reviewed and KEPT**: Hard constraint 3 extended carry-forward 6's
`go run pkg@version` to `go run … / go install …`. That is a deliberate widening,
justified by N3 — `go run` cannot preserve the exit codes the gate needs — and the
source's rationale (pin the version, leave `go.mod` untouched, stay inside the
already-pinned image) carries over to `go install` unchanged.

**Nothing in the brief was found unverifiable.**

---

## Amendment 2 — 2026-08-04, from the reviewer's and auditor's design-wave passes

Both worked in **detached worktrees of `848cf53d`**, never in the shared tree. Both
re-derived all four of my day-one figures independently; **all four reproduced
exactly, for the third and fourth time**. Every mechanism the brief asserts resolved
to a file, in two independent citation tables that agree.

**TWO OF THE FOUR CORRECTIONS ARE TO AMENDMENT 1, AND BOTH ARE MINE.** Amendment 1
was written to record a *widening* finding, and introduced two of its own in the act
of restating someone else's measurement — which is carry-forward 16(d) exactly, at
the site the rule warns about (the fast-read paraphrase at the top of a section)
rather than at the original measurement:

- **the react-router claim** — the fact-checker said the five moderates are ones "no
  planned fix touches", which is true. **I added "in-range, no major, so clearing
  them is possible."** That is false for web's two and it is the half a reader prices
  the scope decision from.
- **the jsdom proof** — the fact-checker gave a structural argument. **I promoted a
  green suite to "a proof that no DOM test is among them."** The conclusion survives;
  the argument does not, because a guarded helper passes vacuously under node.

Recorded in full rather than quietly fixed: the failure was not in the measurement,
it was in the step where one agent's careful result passes through a lead on the way
to being read fast.

| | where | change |
|---|---|---|
| **B1** | Unit A ruling 1 | react-router: no patched 6.x exists, `react-router-dom` is a runtime dependency, `--force` will not touch it. Out of scope, stated as such. |
| **B2** | jsdom section | the green-suite proof does not carry; the structural argument (0 `.tsx`, 0 `@testing-library/react`) does. |
| **F2** | Placement | **Unit A as briefed violates Success Criterion 1.** Ruled: own Taskfile targets that CI calls, kept out of `gate`/`gate:*`, and SC1's exclusion list amended in the same MR. |
| **F1** | A2-1 | a one-file `.npmrc` with `omit=dev` disarms the web gate at exit 0. Remedy `--include=dev`, measured. npm audit has **no** in-band canary. |
| **F3** | Unit A findings | fail-open is **every non-text format** (json, sarif, openvex), and govulncheck's exit codes are **inverted** against this repo's 2/1 convention. Map them. |
| **F5** | Placement | *"a contributor's gate stays offline"* is **false** — `task gate` needs the network three times on a cold cache. The true property is about the **verdict**. |
| **F6** | A2-3 | agent's two high are **production** deps shipping in the worker image; web's three are dev-only. Do agent first. |
| **F7** | A2-4 | vitest 4 supply delta: 4 new / 8 removed / 39 changed, five further majors riding along, and `obug@2.1.4` as a new direct dep of vitest. |
| **F8** | A2-5 | the open design question is answered: **`npm ls`**, offline, positive observation, covers every dep. |
| **#2** | Unit B | `@vitest/coverage-v8` **reddens `deadcode:web`** unless the provider is declared in `vite.config.ts`. Three-arm control. |
| **#9** | Unit B | `-race` on controller is **green** — one run, darwin/arm64, de-risks rather than settles. |
| **B1(aud)** | D1 | Success Criterion 5 does not name the three tools and its literal predicate is already met. Amend the criterion, not the five restatements. |
| **B6** | D2 | two PRD sentences Unit A falsifies. |

**One claim of mine downgraded rather than refuted**: *"the text output hides the only
critical"* overstates by a notch — the tally line does read `1 critical`. What it
hides is **which package**. So `--json` is needed for the **fix list**, not for
gating; the exit code alone gates.

**Design wave verdict from Amendment 2 — superseded by Amendment 3.** It read: *"the
spec is now settled enough to freeze. Three independent citation passes, no
unresolved mechanism, and every open design question answered."* The architect had
not yet reported and the fact-checker's second pass had not run; between them they
found three more errors of mine and one live conflict. **Declaring a wave settled
before its last validator reports is the same class of mistake as everything else in
this file.**

---

## Amendment 3 — 2026-08-04, from the architect and the fact-checker's second pass

**THE SCORE ON MY OWN RESTATEMENTS, STATED PLAINLY BECAUSE IT IS THE FINDING.** Across
Amendments 1 and 2 I introduced **three** errors and faithfully transmitted **one**:
the react-router scope claim (A2), the green-suite proof (A2), the directory split
(A3, R3), and the retired `git grep` leading-dash rule (A3, R4 — the fact-checker's
error, transmitted correctly by me, which does not make it less wrong in the spec).
Hard constraint 2 has now been wrong **in both directions** across two amendments.
Every one arose in the step where another agent's measurement passes through me on
its way to being read fast, and every one was caught by a validator re-deriving rather
than agreeing. **The lead is the highest-traffic single point of distortion on this
team, and the brief-amendment protocol concentrates rather than reduces that.**

### DECISION — react-router, and the shape of "at zero" on `web`

Two-party measurement (architect + fact-checker, independently): `react-router` and
`react-router-dom` are installed at **6.30.4, the newest 6.x that exists**; both
advisories are patched only at **7.18.0**; the declared range is `^6.28.0`;
`npm audit fix` changes postcss only, and **`--force` emits no react-router entry at
all**. `overrides` is not an option — there is nothing patched to override to. So
"full tree, at zero" is **unreachable on `web`** under any option, and the two are
**live CVEs in shipped SPA routing code** (open redirect → XSS; arbitrary constructor
injection via `deserializeErrors()`), not dev tooling.

**Ruled (lead): take `--audit-level=high`, and file react-router 6 → 7 as its own
issue in the same commit.** This is the move this repo already made for knip's
unused-export tier (issue #206) rather than widening a tooling milestone, and pulling
a runtime router major through every route in an MR about quality gates is exactly
the scope creep M3's own ruling refused. **Recorded as a decision, not a discovery:
it accepts two ungating moderate CVEs in shipped code until that issue lands, and it
is reversible by the user.**

### Corrections to Amendment 2's own numbers

- **Only TWO moderates survive, not five, and they are both web's.** Measured with
  `npm audit fix --dry-run` (non-force), tree confirmed unmodified: **agent goes to
  `total=0`** in one command — `ip-address 10.2.0→10.4.0`, `fast-uri 3.1.3→3.1.5`
  (the two highs) **plus** `hono 4.12.27→4.13.0`, `@modelcontextprotocol/sdk
  1.29.0→1.30.0`, `@hono/node-server 1.19.14→2.1.0` (all three moderates). Amendment 2
  said agent's three "clear but owe test evidence" — right about the evidence, wrong
  that they were separate from the high fixes. Flag for the MR unchanged:
  `@hono/node-server` crosses **1.x → 2.x**, permitted without `--force` only because
  it is transitive.
- **`--audit-level=high` is RED today and that is expected**, not the remedy failing:
  it goes green only after the vitest major plus the in-range postcss / `fast-uri` /
  `ip-address` fixes land. Say so in the MR so nobody reads a day-one rc=1 as a
  broken gate.
- **Count fix**: `--audit-level=high` appears at **six sites across the two documents**
  (5 in M5's record + 1 in the PRD), not "six in M5".

### 🔴 The finding that could have silently undone PRD #98's flake fix

**vitest 4's `projects` INHERITS NOTHING from the root config unless `extends` is
set** — upstream's own wording: *"None of the configuration options are inherited
from the root-level config file."* A projects config that omits `extends` loses
`setupFiles: ["./src/test-setup.ts"]` (so `configure({ asyncUtilTimeout: 5000 })`
goes, back to Testing Library's 1s default) **and** `testTimeout: 20000` (back to 5s).
Both reinstate the exact PRD #98 flake class those two lines were added to kill, and
both fail intermittently under CPU contention.

**The 118/1660 predicted-count control CANNOT see this** — a flaky pass still counts
1660. It is the one hazard in Unit B that the brief's own headline control is blind
to. Second `projects` hazard in the other direction: overlapping `include` globs run a
file **twice**, surfacing as a count **above** 1660. **Reconcile the predicted pair in
both directions, and assert on `setupFiles`/`testTimeout` directly rather than
inferring them from a green run.**

### Adopted from the architect, with its reasoning

- **`npm ls --depth=0`, delegated through a `package.json` script** — confirms
  Amendment 2's A2-5 with a 2×2 {clean,stale}×{default,`--offline`} matrix (0,0/1,1),
  rc=0 in the real tree offline, ~3.2s, and a failure string that satisfies
  calibration property 2 unwrapped: `vitest@2.1.9 invalid: "^4.1.10" from the root
  project`. Two riders: **pin `vitest` with `--save-exact`** — `^4.1.10` is satisfied
  by 4.9.0, so a caret makes the check loose, and repo precedent already exact-pins
  gate-relevant devDeps (`oxlint: "1.76.0"`, `knip: "6.31.0"`) with `vitest: "^2.1.9"`
  the outlier — and **place the check FIRST in `gate:web`/`gate:agent`** despite
  breaking cheapest-first, because a stale tree makes every later slot's verdict a
  verdict about the wrong tree. Limits to write down: `--depth=0` sees direct deps
  only, and it compares against `package.json` ranges rather than the lockfile.
- **Land `-race` on `test:controller` as Unit A's FIRST commit.** It is 100% disjoint
  from everything MR-C touches and it is the only item in either unit carrying a
  stop-and-file branch. Local green (7 packages, rc=0) makes it cheap insurance;
  discovering a race on day 3 rather than day 1 is a schedule event. *(This moves one
  line of M6 into Unit A. Deliberate, and the only cross-unit move in the plan.)*
- **"Folds into `lint:api`" is ambiguous and the wrong reading is dangerous** —
  `lint:api` names both a **CI job** and a **Taskfile target**, and the target is
  reachable from `task gate` via `gate → gate:api → lint:api`. The precedent for the
  right shape is inside that very job: `deadcode:api` is its own target invoked as a
  **second script line**. So the new targets are invoked as extra *script lines* of
  the four CI jobs and belong to **no composed target**, with an explicit
  "deliberately NOT here" comment mirroring `scan:secrets`'s.
- **Decomposition confirmed: 2 sequential units is right.** The Go half of M6 is
  genuinely disjoint at the target/job level, but **parallelism buys nothing under
  one shared worktree with one writer token** — the binding constraint is the token,
  not the dependency graph. Splitting would need a second worktree and is not worth
  paying for.
- **The release-blocking property is inherited, not chosen.** All four host jobs are
  in `.publish_needs`, so a CVE published on a Tuesday reddens a `v*` tag publish with
  nobody's diff in it. Rulings 1 and 4 imply accepting it; it needs no list edit, and
  it goes in the MR description in those words so the first Tuesday-red release is
  diagnosed in a minute rather than an afternoon.

### RULING — the `testTimeout` raise in `121d7610`, and why it is accepted

**The situation, measured by the coder:** vitest 4 makes the web suite flake at the
shipped `testTimeout: 20000` — **1 red in 3** full-suite runs, 49 failed, all in
`src/pages/RunView.test.tsx`, which passes **100/100 alone**. Exactly one test times
out (a 150-tick fake-timer poll) and the other 48 are its cascade. That test's own
steady-state duration is **3.90s**, so it was **starved, not slowed** — a >5x spike
over its own median under full-suite CPU contention. Raised to 60000, ~15x steady
state, deliberately not tuned to just-above-worst-observed.

**This is the SECOND raise of this knob for the same cause.** `a5b65617` (PRD #98)
took it from vitest's 5000 default to 20000 for CPU contention; `121d7610` takes it
to 60000 for CPU contention. A knob raised twice for one reason is a structural
signal, and this repo has already learned the general form once, in the `agent`
suite: *"the general lesson is about the UNIT, not the number … if a file approaches
the cap again, split it before raising."* There, splitting `runner.test.ts` into seven
files is what removed the knife-edge; the raised cap only bought margin, and it cut
the suite from 112s to 46s as a side effect.

**Ruled (lead): ACCEPT the raise in MR-C, and file the `RunView.test.tsx` split as
its own issue.**

- **Not accepting is worse than accepting.** A gate that reddens 1-in-3 is *weaker*
  verification than a slow one, because the documented human response is to re-run
  and the retry destroys the evidence. Shipping vitest 4 with a known 33% flake would
  be knowingly installing that.
- **The sizing is right.** 15x steady state, chosen with the agent-cap knife-edge
  explicitly in mind, is margin rather than a new edge.
- **The durable fix is test refactoring inside a tooling MR**, which is the same
  scope argument that deferred react-router. Consistency matters here: both durable
  fixes get filed, neither gets smuggled in.
- **State the cost honestly in the MR**: MR-C carries a second contention-driven
  raise of a knob whose real problem is a 100-test file, and the raise does not fix
  that.

*(Note the mechanism differs from the `agent` case and the difference is worth
holding: node's per-file cap made a large file a serialization point, whereas vitest's
`testTimeout` is per-test and the pressure here is scheduler starvation across 118
parallel files. The transferable half is not the cap semantics, it is that **a raised
timeout buys margin and leaves the knife-edge in place.**)*

### Corrections and additions from the coder's commits 1-3, at `121d7610`

- **`--audit-level=high` will be GREEN from `121d7610`, not red on day one.** Web went
  critical 1 / high 2 / moderate 5 → **0 / 0 / 2**, both survivors react-router; agent
  went 5 → **0 total**. Amendment 3's "red today, green only after the fixes land" was
  right and those fixes have now landed. Supersedes that note.
- **SIX majors ride along with vitest 4, not five.** The design wave named chai 5→6,
  tinyrainbow 1→3, std-env 3→4, es-module-lexer 1→2, tinyexec 0.3→1; measured, add
  **`pathe` 1.1.2 → 2.0.3**. The 454→450 / 4-new / 8-removed prediction reproduced
  exactly, **no package gained an install script**, and the install-script set
  *shrank* 4 → 2 as the duplicate nested esbuild trees deduplicated — which is also
  what removes the flagged nested `vite` 5.4.21.
- **`obug`'s npm name was RECLAIMED, not newly created.** Its registry `time` map
  holds a `1.0.1` dated **2016-12-04** that is absent from the published-versions
  list, while `.created` reads 2025-11-11. MIT, zero deps, one registry signature
  plus npm build attestations. A reclaimed name is a different supply-chain question
  from a fresh one; put the distinction in the MR description rather than the
  headline "9-month-old package".
- **🔴 `task gate:agent` GOING GREEN SAYS NOTHING ABOUT THE FIVE AGENT BUMPS, AND THE
  COODER PROVED IT RATHER THAN ASSUMING EITHER WAY.** An ESM `resolve()` hook across
  every test process logged **11156 resolutions**: `@hono/node-server` **0**,
  `@modelcontextprotocol` **0**, `ip-address` **0**, `fast-uri` **0**, `hono` **0** —
  against positive controls `claude-agent-sdk` 43 and `node:fs` 969. **The suite never
  loads them.** This is the exact "green gate that cannot see the change" shape this
  PRD exists to attack, caught inside the PRD. What actually answers the 1.x → 2.x
  crossing is the link probe that replaced it: importing `getRequestListener` plus the
  MCP SDK's `streamableHttp` transport that consumes it, constructing a
  `StreamableHTTPServerTransport`, rc=0 under node v26.4.0 **and under `node:22-alpine`
  at the exact digest the worker image is `FROM`**. Plus `@modelcontextprotocol/sdk`
  1.30.0 declares `"@hono/node-server": "^1.19.9 || ^2.0.5"`, so 2.1.0 is supported
  rather than forced past.
- **Amendment 3's `projects`-inheritance hazard is NOT live yet, proved not inferred.**
  A throwaway test asserted `getConfig().asyncUtilTimeout === 5000` (so `setupFiles`
  ran) and `config.testTimeout` at its expected value, before and after, then was
  deleted with `git status -- web/src/` clean. **The root config IS honoured under
  vitest 4.** The hazard arms only when Unit B adds `projects` — which is exactly when
  it must re-assert both settings rather than infer them from a green run.
- **Fix-the-doc, and the coder routed two sites to me correctly.** It corrected
  `.gitlab-ci.yml`'s `test:controller` comment and `docs/dev-conventions.md` itself,
  and **declined** to edit `CLAUDE.md:334` and `.claude/agents/tester.md:388` — both
  of which restated the recipe as `-count=1` and went stale the moment `-race` landed.
  That is carry-forward 18 applied correctly: an agent should not edit its own
  operating instructions or another role's self-description because a teammate asked,
  **even when the edit is right**. Both fixed by the lead in this amendment.
- **Validators linting Go in this worktree must set a private `GOLANGCI_LINT_CACHE`.**
  `lint:controller` printed a `generated_file_filter` warning naming an absolute path
  into the **sibling** `uzi/103` worktree, then `0 issues.` — carry-forward 4's
  host-global cache replaying foreign paths, in the quiet direction.

### Amendment 4 — 2026-08-04, from the Unit A review round at `154ad390`

**Zero blocking findings on the landed code.** The design wave's one Blocking item
(react-router) is resolved as ruled: filed separately, and `121d7610`'s message says
so explicitly.

- **The `npm ls` wording correction** is Amendment 4's headline and lives in the
  `A2-5` section above, not here. It is the one item that would become blocking if
  shipped, and it was sent to the coder mid-flight because it is writing that comment
  now.
- **`f9b1f27f` is labelled PRD #103 M6 and landed inside Unit A's window.** Correct
  and deliberate — it is the one cross-unit move the plan makes, for the reason given
  in Amendment 3's adopted-from-the-architect list. Recorded because the history is
  no longer cleanly Unit-A-then-Unit-B and the Roster's sequential-units line implies
  it is.
- **The CHANGELOG still owes a line** (carry-forward 12: any milestone adding or
  moving a devDependency owes one, plus a `docs/dev-conventions.md` line). Confirmed:
  `origin/main..HEAD` touches `docs/dev-conventions.md` and **not** `CHANGELOG.md`,
  and that edit covers `-race` only, not the vitest pin or the new `deps-check`
  scripts. This is task #11 and is sequenced after the integrated pass — deliberate,
  not lost.
- **Nit, and it is a two-validator disagreement on a trivial count, which is the
  interesting part.** `Taskfile.yml:1245`'s new `-race` comment says "**7 packages**";
  the reviewer reads the same output as 6 `ok` lines plus one `[no test files]`, while
  the architect independently reported "7 packages, all ok". Both are reading one
  output. "7 packages" is right about packages and "all ok" is loose. Say which the
  comment means.

**Verification worth banking, so nobody re-runs it:** knip on `web` at vitest 4 is
rc=0 with findings **byte-identical** to the pre-upgrade baseline (9 unused exports +
13 unused types, empty `diff`) — the major orphaned no dependency and revealed none.
**No vitest-2-only API or config key survives anywhere in `web/`**: `environmentMatchGlobs`,
`test.workspace`, `deps.inline`, `testTransformMode`, `poolMatchGlobs` and
`vitest/globals` are 0 files each (`vi.mocked` at 39 files is still current in v4, not
a leftover). `.gate_needs`/`.publish_needs` parse to **13 and 15** with the job set
**identical** to `848cf53d`, so no list edit was owed and none is missing. And the
agent lockfile holds **225 package names before and after**, 0 added, 0 removed.

**Scope note for the next round.** That review covered the three landed commits and
**not** Unit A's actual gate recipes — `Taskfile.yml` carried ~97 uncommitted lines
with the `govulncheck` and `npm audit` targets at the time. Those need their own
review dispatch, pinned to a SHA, once commits 4-6 land. Expected, not a miss.

### Amendment 5 — 2026-08-04, from the Unit A audit at `121d7610`

**🔴 THE GATE WAS RED AND I BROKE IT, IN A COMMIT ARCHIVING THE EVIDENCE FOR AN
AMENDMENT ABOUT RIGOUR.** `task scan:secrets` at `121d7610`: rc=201, **60 findings**,
all in `probes/prd-103-mrc-m6-reviewer/rev-vulnprobe-json.txt`, all
`sourcegraph-access-token`, all false positives — gitleaks reads the bare 40-hex
commit SHAs in `go.googlesource.com/<repo>/+/<sha>` fix URLs inside archived
`govulncheck -format json` output as tokens. Introduced by **`4e54f805`** (my
Amendment 2 commit); `main` at `0111f01c` is clean. `lint:repo` would have reddened
the first MR pipeline.

**Two things kept it invisible for hours, and neither is carelessness:**

1. **Every docs commit I made carries the CI-skip marker**, correctly — they are
   docs-only — so no pipeline ever ran against them. The marker is right and its
   consequence is that the *repo's* check never fired either.
2. **`probes/README.md` said, in terms, "Adding or removing files here cannot move a
   gate."** That sentence is why nobody ran the gate. It is now corrected in place
   with the failure recorded: `scan:secrets` walks **tracked files**, and everything
   under `probes/` is tracked, so a committed probe is inside the scan root by
   construction. The narrower true claim: *removing* a file here cannot redden a
   gate, **adding one can**.

**And the four component gates that WERE run are exactly the four that do not hold
the secret scanner.** `gate:api`, `gate:controller`, `gate:web` and `gate:agent` were
each recorded; `gate:repo` owns `scan:secrets` (`Taskfile.yml:218`) and had no owner
in this unit's evidence set at all. **Checklists should say `task gate`** — which runs
`gate:repo` first — rather than enumerating component gates.

**Fixed by redacting the token-shaped bytes**, not by a `.gitleaks.toml`: a
directory-scoped allowlist is the self-disarming shape M5 spent a milestone closing,
and `scripts/scan-secrets.sh`'s own message says one scoped to a directory holding no
canary slips past both canaries. **`task gate:repo` now rc=0, `clean -- 0 findings in
tracked files (1654 in the index)`, both canaries DETECTED.**

*(My first redaction pass anchored on the literal `/go/+/` quoted in the report and
took the gate 60 → **1**, missing 2 under `/text/` and 1 under `/net/`. **Anchor on
the shape, not on the example you were handed** — the same lesson carry-forward 16
records for sweeps, arriving through a `gsed` pattern.)*

**Other audit findings folded in above**: the `NPM_CONFIG_OMIT=dev` env-var disarm
(in `A2-1`), and the dependency-path / SSRF-link correction plus the five-advisory
count (in `A2-3`).

**Three more, recorded rather than actioned:**

- **A probe artifact recording only pass/fail cannot evidence a claim about a
  VALUE**, and vitest's default reporter makes that the easy mistake — it suppresses
  `console.log`, so a probe that *logs* a value records nothing. `121d7610`'s
  `vite.config.ts` comment says both settings were "CONFIRMED IN EFFECT rather than
  inferred from a green", and its two committed artifacts are five lines each of
  `Tests 2 passed (2)` + `RC=0`, holding neither value. **The claim is true** — it was
  re-derived independently, including a mutation where the runner printed
  `AssertionError: expected 60000 to be 20000`. The technique: **assert the wrong
  value and let the runner print the actual.** M6's coverage calibration has exactly
  this shape and should use it.
- **`obug`'s caret should NOT be pinned.** `vitest` is now exact-pinned at `4.1.10`,
  so `obug` cannot move without an explicit vitest bump and `npm ci` reinstalls the
  exact tarball by integrity. An `overrides` entry would be this repo's first and
  would freeze it against future security patches to buy nothing the lockfile does
  not already give. It carries **SLSA v1 provenance naming `github.com/sxzz/obug` at
  `refs/tags/v2.1.4`** — more than 5 of the 12 new/major packages have. The reclaim
  is real and its live hazard is to *other people's* trees: sxzz republished
  overlapping `1.0.0` and `1.0.2`, so a 2016-era `^1.0.0` range now resolves to
  entirely different code. Not ours.
- **41 packages changed, not 39** — the extra two are `postcss 8.5.16→8.5.25` and its
  dep `nanoid 3.3.15→3.3.17`; A2-4's 39 was a vitest-only resolve and the commit
  bundles postcss. **Quote 41 in the MR.** And word the majors carefully:
  `vite 5.4.21,6.4.3 → 6.4.3` and `esbuild 0.21.5,0.25.12 → 0.25.12` are **dedupes of
  a nested duplicate, not bumps** — a name→version table renders the first as
  "5.4.21 → 6.4.3", which reads as a vite major that did not happen. All 462 web and
  224 agent lockfile entries resolve to `registry.npmjs.org` with **zero** missing
  `integrity` hashes.

### The react-router deferral is defensible on STRONGER grounds than my ruling gave

This belongs in the MR description, not just "two moderates accepted" — the residual
is far smaller than the raw advisory list implies, and the next person to run
`npm audit` needs to know why it is being left.

- **`GHSA-337j-9hxr-rhxg` (constructor injection via `deserializeErrors()`) is
  STRUCTURALLY UNREACHABLE here.** From the published tarballs: the symbol is absent
  from `react-router@6.30.4` and `@remix-run/router@1.23.3` (both greps carried live
  positive controls) and is defined only in `react-router-dom@6.30.4`. Its **only**
  caller is `parseHydrationData()`, whose **only** callers are `createBrowserRouter()`
  and `createHashRouter()`. This app mounts `<BrowserRouter>` (`web/src/main.tsx`),
  and `createBrowserRouter|createHashRouter|RouterProvider|__staticRouterHydrationData`
  returns **zero matches** across `web/src`. No SSR anywhere, and the sink
  additionally reads a same-origin `window` global.
- **Both open-redirect advisories are mitigated at the only attacker-controlled
  navigation sink, and the mitigation is TESTED.** That sink is
  `web/src/pages/Login.tsx:74`, `safeNextPath(searchParams.get("next"))`, fed by
  `CliAuth.tsx`. `safeNextPath` admits only `startsWith("/") && !startsWith("//") &&
  !includes("\\")` — it rejects **the backslash vector specifically**, which is what
  both advisories exploit, and `Login.test.tsx:104` asserts it. No other
  `useSearchParams`/`location.search` value reaches `navigate()` or `<Link to>`.
- `GHSA-jjmj-jmhj-qwj2` reports `first_patched_version: NONE` for `react-router-dom`
  — independent confirmation that no patched 6.x exists.

**One invariant the filed 6→7 issue must carry**: `safeNextPath` is a *per-call-site*
guard, so any future `navigate(<value derived from a URL or the server>)` reopens the
hole without touching react-router at all.

### Two instrument failures from the architect, both worth keeping

- **`npx vitest run --environment node <a component test>` → rc=0, 61 passing.** Reads
  as "a DOM test is fine under node"; the pragma silently outranks the flag. **The arm
  cannot produce the disconfirming answer** — it is the same shape as everything else
  in this file, arriving through a CLI flag.
- **A packaged precedence probe returned 5 of 6 cells at rc=1 with no output, and the
  single survivor printed the OPPOSITE of the true answer.** Cause: macOS `$TMPDIR`
  lives under `/var/folders/…`, `/var` symlinks to `/private/var`, and Vite's resolved
  id then does not match `--root`; `pwd -P` fixes it. **A near-uniform failure with one
  plausible survivor is worse than a uniform one**, which at least announces itself as
  an instrument failure.
