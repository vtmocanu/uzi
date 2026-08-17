# PRD #245 — PRD done/open counts on the version build-info popover

**Issue**: [#245](https://github.com/vtmocanu/uzi/-/issues/245) · **Label**: PRD · **Priority**: Low
**Parent**: [#175](https://github.com/vtmocanu/uzi/-/issues/175) (the build-info endpoint + popover this extends).
**Area**: a fourth stamped coordinate pair on `GET /api/version` — `api/internal/apitypes/buildinfo.go` (two new DTO fields) + `api/internal/apitypes/wire_test.go` (its tag test), `api/internal/handler/handler.go` (`BuildStamp`, `SetBuildInfo`, `Version`, the handler fields) + `api/internal/handler/version_test.go` (parse/omit + the closed-key-set trust test), `api/cmd/server/main.go` (two ldflags vars + wiring), `api/Dockerfile` (two ARGs + the ldflags line), `.gitlab-ci.yml` (`publish:assert-changelog` computes the counts, delivers them by dotenv, `publish:api` passes them as build-args) · `web/src/lib/api.ts` (`BuildInfo` type), `web/src/components/BuildInfoPopover.tsx` (the new row), `web/src/mocks/data.ts` (fixture parity) · `api/cmd/uzi/` (the `uzi version` consumer of the same DTO).
**Mockup**: [`prds/mockups/245-version-prd-counts-mock.html`](../mockups/245-version-prd-counts-mock.html) — approved **Variant A** (a `PRDs` row reading `N done · M open`).
**Line references** are against `ab2d033`.
**Status**: complete — shipped 2026-08-08 (M1–M3 all landed on branch `agent/issue-245`).

## Problem

The sidebar version badge opens a build-info popover on hover/focus/tap
(`web/src/components/BuildInfoPopover.tsx`, PRD #175). It states the coordinates a
deployed instance can say about itself: `uzi vX.Y.Z`, a subtitle of `N days old ·
N,NNN commits`, and a definition list of **Founded / Built / Commit / Uptime**
(`BuildInfoPopover.tsx:359-381`). Every one of those is a build stamp, a const, or
a process-uptime computation; the panel makes **no** DB query and is
presentational by design (`BuildInfoPopover.tsx:5-16`).

What it does not show is any sense of the **project's own roadmap progress**. uzi
tracks its work as PRDs: active ones live in `prds/*.md`, completed ones move to
`prds/done/*.md` (root `CLAUDE.md`, Conventions). At `ab2d033` that is **80 done,
32 open** (112 total, ~71%). A reader of the version panel — which already answers
"what build is this and how old is the project" — cannot see "how much of uzi is
actually built" without cloning the repo and counting files. This PRD adds that one
coordinate.

## Solution

Add a **done/open PRD count** to `GET /api/version`, stamped at build time, and
render it as one row on the popover — the approved **Variant A**:

```
Founded   3 Jul 2026
Built     8 Aug 2026 14:20 UTC
Commit    ab2d033
Uptime    3d 4h
PRDs      80 done · 32 open      ← new
```

The count is **not** runtime data and **not** a DB query — it is a build stamp, in
the same class as the commit count, and it degrades the same way. The whole design
is "do exactly what `commits` does", because `commits` already solved the one hard
problem here (see below).

### Why it must be a build stamp computed in CI, not counted at runtime

This is the load-bearing constraint, so it is stated first. The obvious
implementations are both impossible or wrong here:

1. **The API cannot count the files at runtime.** The `prds/` directory lives at
   the repo root; the API image's build context is the `api/` subdirectory only
   (`docker-compose.yml:29` `build: ./api`; CI `UZI_BUILD_CONTEXT: "$CI_PROJECT_DIR/api"`,
   `.gitlab-ci.yml:1697`; and the root `.dockerignore:1-4` states it explicitly).
   `prds/` is never copied into the image — the only `COPY`s in `api/Dockerfile` are
   `go.mod go.sum` (`:7`) and `COPY . .` where `.` is the `api/` context (`:10`). So
   the running container has no `prds/` to count.
2. **The Dockerfile cannot count them at build time either**, for the same reason
   `commits` is not computed there: there is no `.git` and no repo root in the `api/`
   context, which is exactly why the Go build passes `-buildvcs=false`
   (`api/Dockerfile:30-40`, and its mirror `api/cmd/server/main.go:73-76`: *"commits
   is the one that needs history the image build does not have — the publish context
   is api/ with no .git … CI computes it in publish:assert-changelog and delivers it
   as a dotenv report"*).

So the count follows the **exact `commits` path**: compute it on the CI host, where
a full checkout with `prds/` and `.git` is present, and inject it via ldflags. The
`publish:assert-changelog` job already runs with `GIT_DEPTH: 0` + `GIT_STRATEGY:
clone` at the repo root (`.gitlab-ci.yml:1636-1637`), where `prds/*.md` and
`prds/done/*.md` are tracked and present, and it already writes a dotenv report
(`commit_count.env`) consumed by `publish:api` (`.gitlab-ci.yml:1659-1669`,
`:1775`). We extend that job and that dotenv rather than inventing a new mechanism.

### It degrades to "omitted", exactly like the commit count

Only the tag (publish) build passes the stamps. On a plain `go build`, a `docker
compose build`, and the MR/main validation image, the count vars take their empty
default and the response **omits** the fields — never serves `0` or a zero-value
lie (`api/cmd/server/main.go:82-91`; the omit rule is enforced in `Version`,
`handler.go:397-424`, and documented on `BuildInfoDTO`, `buildinfo.go:17-20`).

**Consequence the user will meet first, stated up front:** on a **local `docker
compose` stack the `PRDs` row does not appear** — precisely as the `Commit`,
`Built` and commit-count fields already do not (`mockBuildInfoUnstamped`,
`web/src/mocks/data.ts:3554-3558`, is the laptop shape and carries none of them).
The row shows on published images: the hosted k8s deployment (dev-cluster) and any
released build. This matches `commits` and is deliberate — a dev build's PRD count
is not a release coordinate. See open question 1 for the alternative and why it is
rejected.

### The count is exact and whole-tree

`done = |prds/done/*.md|`, `open = |prds/*.md|` (top-level only). At `ab2d033`:
done 80, open 32 (both verified against the tree — see Review findings). `total` is
**derived by the consumer** (`done + open`), not a third stamp — one fewer value to
keep consistent, and Variant A does not render a total anyway. The counting command
must be non-recursive and count only `.md` files, because `prds/` also holds
directories (`prds/done/`, `prds/mockups/`, `prds/43-m0-probe/`). The material
over-count a recursive command would produce today is `prds/done/` itself — its 80
`.md` files counted as "open"; `prds/mockups/` and `prds/43-m0-probe/` currently hold
no `.md` at all (verified). `-maxdepth 1` excludes all three regardless, which is why
it is the load-bearing guard — see Technical scope for the exact command.

Note the count is a **live build-time snapshot**, not a frozen figure: it is
recomputed on every publish build, so it drifts as PRDs are added and completed. This
PRD is itself a top-level `prds/*.md`, so the first publish build after it lands reads
`open = 33` — this PRD counting itself, which is exactly right (an open PRD is open),
not double-counting.

### Two fields, both stamped together or both omitted

Following the `Commits *int` precedent (`buildinfo.go:52-56`), the DTO gains two
optional pointer fields, `prds_done` and `prds_open`. They are computed in one CI
step and travel together: a response either has both or neither. The consumer
renders the row only when **both** are present (see Technical scope) — a half-known
count is treated as unknown, the same "unknown beats wrong" rule the rest of this
endpoint obeys.

## User journey

1. A user hovers the sidebar version badge on a **published** instance (hosted k8s,
   or any release image). The popover opens with a new **`PRDs  80 done · 32 open`**
   row beneath Uptime.
2. They read it as "uzi is ~80 of 112 PRDs in" without leaving the app or cloning
   the repo.
3. On their **local `docker compose`** dev stack the row is absent — the same build
   that already hides Commit / Built / the commit count, because none of those are
   stamped on a dev image. Nothing renders "unknown" or "0"; the row simply is not
   there.
4. `uzi version` from the CLI (a second consumer of the same endpoint, PRD #175 M4)
   prints the same counts on a line beneath the build coordinates it already shows.

## Open questions

### 1. Omit on dev builds (like `commits`), or count on the host for every build? **Omit.** (recommended)
Making the row appear on a local `docker compose` stack would mean computing the
counts on the developer's host and threading them through `docker-compose.yml` as
build-args (which today has no `args:` block, `docker-compose.yml:29`) — a new
mechanism `commits` deliberately does not use, for a value that is not a release
coordinate on a dev build. Recommendation: **mirror `commits` exactly** — stamped
on the publish build, omitted otherwise. If a dev-visible count is later wanted, it
is a compose-args follow-up, not this PRD.

### 2. Two int fields, or one combined string? **Two ints.** (recommended)
A combined `prds: "80/112"` string would ride the **unquoted** kaniko expansion of
`UZI_BUILD_ARGS` (`.gitlab-ci.yml:1740-1758`), where **whitespace** is the
word-splitting hazard the CI comment warns about. (A `/` actually survives POSIX
field-splitting fine — the slash itself is not the risk — so the real cost of a
combined value is that it cannot take the numeric shape-guard `commits` has and would
need bespoke parsing on the server.) Two plain decimal ints match the `Commits *int`
convention, each get that same guard (`.gitlab-ci.yml:1661`), and let the consumer
derive the total. Ship two ints.

### 3. Show a total, or just done · open? **Just done · open; derive total in the consumer.** (recommended)
Variant A renders `N done · M open` and no total. A `total` stamp would be a third
value that must always equal `done + open` — a consistency invariant with no
upside, since any consumer that wants the total can add two numbers. If a future
variant wants `80/112`, the consumer computes it.

### 4. Is a private-repo file count safe on an unauthenticated, unrate-limited endpoint? **Yes — same class as the commit count.** (recommended)
`GET /api/version` is world-readable to anyone who can reach the ingress
(`handler.go:363-372`), and its key set is pinned closed by
`TestVersionEndpointCarriesNothingPrivate` (`version_test.go:450`). The commit
**count** is already on that endpoint and classified *"already public: a count over
that repo"* (`version_test.go:458`) — a count derived from the tracked tree, not a
secret, even though the repo itself is private. A PRD count is the same class: a
scalar derived from tracked file names, revealing no identity, topology, path, or
content. It is added to the closed-key-set allowlist **with** that reason (see
Technical scope). It is not `uptime_seconds`-class (a runtime disclosure); it is a
build fact about a public-by-construction count.

### 5. Does the CLI (`uzi version`) get the counts? **Yes — one line, same DTO.** (recommended)
Per root `CLAUDE.md` ("New uzi functionality ⇒ check whether `api/cmd/uzi/` needs a
matching CLI change"): `uzi version` already fetches this endpoint via
`uzicli.(*HTTPClient).BuildInfo` (PRD #175 M4, `handler.go:106-113`) and renders the
build coordinates. Surfacing the counts there is one extra line off a DTO it already
decodes, and keeps the CLI and web popover honest to the same source. Included in
scope (M2). It prints only when both fields are present, mirroring the web guard.

## Technical scope

### DTO (`api/internal/apitypes/buildinfo.go`) + wire test
Two new fields, mirroring `Commits *int` (`:52-56`) — pointer + `omitempty` so absent
is distinguishable from a real zero, and both dropped together on an unstamped build:

```go
// PrdsDone / PrdsOpen are the count of completed (prds/done/*.md) and active
// (prds/*.md) PRDs in the source tree the image was built from (#245). Like
// Commits, they need the repo root the api/ build context lacks, so they are
// computed in CI and stamped via ldflags; both are omitted on an unstamped build.
// The consumer derives the total (done+open); there is deliberately no total stamp.
PrdsDone *int `json:"prds_done,omitempty"`
PrdsOpen *int `json:"prds_open,omitempty"`
```

Extend `TestBuildInfoDTOTags` (`wire_test.go:519-538`): the `(stamped)` assertion
gains `prds_done` and `prds_open`; the `(unstamped)` assertion stays `version`,
`founded` (both omitted when unset).

### Handler (`api/internal/handler/handler.go`)
- Two raw-string fields beside `commit/builtAt/commits` (`:122-124`): `prdsDone`,
  `prdsOpen`.
- Extend `BuildStamp` (`:251-258`) with `PrdsDone`, `PrdsOpen` string fields, and
  `SetBuildInfo` (`:266-270`) to `strings.TrimSpace` them into the handler fields —
  unconditional assignment, exactly like the other three (empty means "unknown",
  which the response omits).
- In `Version` (`:397-424`), after the `commits` block (`:410-412`), parse each with
  `strconv.Atoi` and set the pointer only on `err == nil && n >= 0` — the identical
  guard `commits` uses. A value that is absent, negative, or non-numeric is omitted.
- Update the `Version` doc comment's enumeration of what the body carries
  (`:359-361`, `:374-376`) to name the two fields as build facts (public-by-
  construction counts), consistent with how it already frames `commits`.

### The closed-key-set trust test (`api/internal/handler/version_test.go`)
`TestVersionEndpointCarriesNothingPrivate` (`:450`) pins the emittable key set. Add
both keys to the `public` map (`:453-462`) **with their reason**, matching the
`commits` entry's wording:

```go
"prds_done": "already public: a count over that repo (completed PRDs)",
"prds_open": "already public: a count over that repo (active PRDs)",
```

Add corresponding stamped values to the premise-guard handler at `:494-496` so the
response-level pass sees them rendered. No new class of disclosure — do not touch
the `uptime_seconds` reasoning.

### Server ldflags (`api/cmd/server/main.go`)
Two package-level vars beside `commit/builtAt/commits` (`:87-91`), default `""`:

```go
prdsDone = ""
prdsOpen = ""
```

Pass them through `SetBuildInfo` where `SetVersion`/`SetBuildInfo` are already
called (main's build-stamp wiring, the `h.SetBuildInfo(handler.BuildStamp{...})`
site). Extend the ldflags comment block (`:66-91`) to name the two new `-X main.*`
targets and to repeat the "computed in CI, not in the image" reason.

### Dockerfile (`api/Dockerfile`)
Two new `ARG`s beside `UZI_COMMIT_COUNT` (`:35`), defaulting empty, and two new
`-X main.prdsDone=${UZI_PRDS_DONE} -X main.prdsOpen=${UZI_PRDS_OPEN}` on the single
ldflags line (`:41`).

### CI (`.gitlab-ci.yml`)
- In `publish:assert-changelog` (`:1618-1669`), beside the `git rev-list --count`
  that produces `UZI_COMMIT_COUNT` (`:1660`), count the PRDs from the full checkout
  and **guard each numerically** the way the count is guarded (`:1661`). Robust,
  non-recursive, `.md`-only (the directory holds sub-dirs `done/`, `mockups/`,
  `43-m0-probe/`):

  ```sh
  DONE="$(find prds/done -maxdepth 1 -name '*.md' | wc -l | tr -d ' ')"
  OPEN="$(find prds     -maxdepth 1 -name '*.md' | wc -l | tr -d ' ')"
  case "$DONE" in ''|*[!0-9]*) echo "bad PRD done count: $DONE" >&2; exit 1;; esac
  case "$OPEN" in ''|*[!0-9]*) echo "bad PRD open count: $OPEN" >&2; exit 1;; esac
  { echo "UZI_PRDS_DONE=$DONE"; echo "UZI_PRDS_OPEN=$OPEN"; } >> commit_count.env
  ```

  (Appending to the existing dotenv report — `:1663-1669` — rather than adding a new
  artifact, so the `*publish_needs` edge already carries them.)
- In `publish:api`, extend `UZI_BUILD_ARGS` (`:1775`) with
  `--build-arg UZI_PRDS_DONE=$UZI_PRDS_DONE --build-arg UZI_PRDS_OPEN=$UZI_PRDS_OPEN`.
  No default is declared for these on `publish:api` (deliberate, matching
  `UZI_COMMIT_COUNT`, `:1769-1774`): an absent dotenv degrades to an empty build-arg
  → empty ldflag → omitted field.
- **Widen the fifth-stamp warning.** The comment at `.gitlab-ci.yml:1740-1758`
  ("If you add a fifth stamp, give it a gate or widen this paragraph", `:1755`) is
  discharged here: these are stamps five and six, each carries its numeric gate
  above, and the paragraph is updated to say so.

### Web type (`web/src/lib/api.ts`)
Add to `BuildInfo` (`:588-612`), mirroring the `commits?` comment (`:605-607`) —
optional, independently droppable, consumer renders correctly without them:

```ts
// Completed / active PRD counts in the source tree the image was built from
// (#245). Stamped only on a publish build, like `commits`; absent otherwise.
prds_done?: number;
prds_open?: number;
```

### Web render (`web/src/components/BuildInfoPopover.tsx`)
- Type-guard both at the response boundary, exactly like `commits`
  (`:249-250`): `typeof info.prds_done === "number" && Number.isFinite(info.prds_done)`,
  same for `prds_open`. This keeps `null`/`NaN`/`Infinity` from reaching the render
  (the file's boundary-guard discipline, `:231-255`).
- Render a new `Row` after `Uptime` (`:380`), only when **both** guards pass:
  `<Row label="PRDs" value={`${done} done · ${open} open`} />`. A row appears only
  when the full pair is known.
- To colour the value as the approved mock does (done in the accent, `· open`
  faint), widen `Row`'s `value` prop from `string` to `React.ReactNode`
  (`:174-195`) — a backward-compatible signature change (a string is a ReactNode) —
  and pass a small `<span>` composition. If we keep it monochrome to match the other
  rows exactly, `value` stays a string and no `Row` change is needed. Decision left
  to M2; the mock shows the coloured form.

### CLI (`api/cmd/uzi/`)
`uzi version` decodes this DTO via `uzicli.(*HTTPClient).BuildInfo`
(`api/internal/uzicli/client.go:789`) and renders it through `serverRows`
(`api/cmd/uzi/version.go:156`), which already appends a `commits` row guarded by `if
b.Commits != nil` (`:164-165`). Add one row there, printing `PRDs: N done, M open`
when **both** fields are present (`if b.PrdsDone != nil && b.PrdsOpen != nil`) — the
same both-or-neither guard as the web row. Both counts are `*int` rendered with `%d`,
so there is no unbounded-string / `CellText` concern (that hazard is for
server-controlled *strings*, not decimals). One row, no new fetch.

### Mock parity (`web/src/mocks/data.ts`)
Add `prds_done: 80, prds_open: 32` to `mockBuildInfo` (`:3541-3548`, the **stamped**
fixture — the shape a published image serves). Leave `mockBuildInfoUnstamped`
(`:3554-3558`) and `mockBuildInfoNoUptime` (`:3564-3567`) untouched — they are the
dev/struct-literal shapes and must keep omitting the stamped fields, so mock mode
exercises both the present and absent renders. `mockApi.version` already returns
`mockBuildInfo` (`web/src/mocks/mockApi.ts:1052`); no change there.

### Tests
- **Handler** (`version_test.go`): a stamped build renders both counts; an unstamped
  build omits both; a negative / non-numeric / half-present stamp omits (mirror the
  existing `commits` parse/omit cases). `TestVersionEndpointCarriesNothingPrivate`
  updated (both keys classified, premise-guard values set).
- **wire_test**: `TestBuildInfoDTOTags` gains the two keys on the stamped assertion.
- **Web** (`BuildInfoPopover.test.tsx`): the row renders `80 done · 32 open` when
  both present; is **absent** when either is missing (dev shape); `null`/`NaN` for
  either degrades to absent, not to "NaN" — following the file's existing
  guard-test pattern for `commits`.
- **Mock**: `mockBuildInfo` carries the pair; the unstamped fixtures do not (a
  render test over both proves present-and-absent).
- **CI/stamp flow** is not unit-testable (no e2e exists for the `commits` stamp
  either); M3 verifies the shape in mock mode and notes the real stamp is
  observable only on a tag/publish build.

### Docs, changelog, specs
`CHANGELOG.md` entry; a `specs/ai.md` note recording the new stamped coordinate, the
"computed in CI like commits (api/ context lacks the repo root)" decision, the
omit-on-dev behaviour, and the public-by-construction security classification. PRD
#175 has no dedicated user doc for the endpoint, so no `docs/*.md` page is required;
if one is added later it inherits this coordinate. No `specs/human.md` change without
user approval.

## Milestones

- [x] **M1 — Server + build/CI stamp plumbing, end to end.** The two DTO fields +
      `wire_test` tag test; `BuildStamp` / `SetBuildInfo` / `Version` parse-and-omit +
      handler fields; the `main.go` ldflags vars + wiring; the `api/Dockerfile` ARGs +
      ldflags line; the `.gitlab-ci.yml` compute-guard-dotenv-buildarg change with the
      fifth-stamp comment widened; the closed-key-set trust test updated. Exercised by
      Go tests and by `curl` against a locally-stamped binary (build with the `-X`
      flags set by hand to prove the parse/omit paths). No web change yet.
- [x] **M2 — Consumers: the popover row + `uzi version` line.** `BuildInfo` TS
      fields; the type-guarded `PRDs` row (Variant A) with the optional `Row` ReactNode
      widening; `mockApi`/fixture parity; the `uzi version` line. Unit tests: row
      renders the pair, absent on a missing field, `null`/`NaN` degrade to absent.
- [x] **M3 — Docs, changelog, specs, verification.** `CHANGELOG.md`, the `specs/ai.md`
      note, and a browser check in mock mode (`VITE_UZI_MOCK=1`, never a live-proxying
      `vite dev`/`preview` per `.claude/rules/web.md`) that the shipped row matches the
      mockup, plus a note that the real stamp appears only on a tag/publish build.

### Parallelisation
M1 is the dependency: M2 consumes the two DTO fields. Agree the
`prds_done`/`prds_open` `*int` shape up front and M2 can build the row, the CLI line,
and the mock against it in parallel with M1's server/CI work, accepting one merge on
the DTO. M3 is last (it verifies the shipped UI).

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| Someone tries to count `prds/` in the Dockerfile or at runtime | It is not in the `api/` build context and the container has no repo root; the PRD computes it in CI on the full checkout, exactly like `commits`. Stated as the first design constraint. |
| The count recurses into `prds/done/`, `prds/mockups/`, `43-m0-probe/` and over/under-counts | `find … -maxdepth 1 -name '*.md'` is non-recursive and `.md`-only; each count is numerically shape-guarded in CI before it becomes a build-arg. |
| An unexpanded/garbage CI variable reaches the wire (`commit`'s original bug) | Server parses with `strconv.Atoi` and omits on error/negative; the CI numeric guard rejects a non-decimal before it is stamped. Unknown beats wrong. |
| A new world-readable field on an unauthenticated endpoint leaks something | Same class as the commit count (a scalar over the tracked tree, no identity/topology/path); added to the closed-key-set allowlist with its reason; `TestVersionEndpointCarriesNothingPrivate` fails if it is added without one. |
| The row shows "0" or "NaN" on a dev build | Both fields are `omitempty` pointers dropped together on an unstamped build; the consumer renders the row only when both guards pass; fixtures cover present and absent. |
| Half a count (one field stamped, the other not) renders a misleading row | Both are computed in one CI step and travel together; the consumer requires both — a lone field is treated as unknown. |

## Success criteria

- On a published (tag) build, the version popover shows a `PRDs  N done · M open`
  row sourced from `GET /api/version`, and `uzi version` prints the same counts.
- On a local `docker compose` / dev build the row and CLI line are **absent** — no
  "0", no "NaN" — exactly as the commit/built/commit-count fields already are.
- The counts are `|prds/done/*.md|` and `|prds/*.md|` (top-level), computed in CI
  from the full checkout and stamped via ldflags; the API makes no DB query and the
  panel stays presentational.
- `GET /api/version` remains free of any non-public field: the two new keys are
  classified public-by-construction in the closed-key-set trust test.
- The wire tag test pins `prds_done`/`prds_open` on the stamped response and their
  absence on the unstamped one.

## Decision log

1. **A build stamp computed in CI, never a runtime count.** `prds/` is outside the
   `api/` build context and absent from the running container; the Dockerfile has no
   `.git`/repo root either. The counts follow the `commits` path exactly — computed in
   `publish:assert-changelog`, delivered by dotenv, injected via ldflags.
2. **Omitted on unstamped (dev) builds.** Only the publish build stamps them; a
   local `docker compose` stack shows no row, matching `commit`/`built_at`/`commits`.
   A dev-visible count would need a new compose-args mechanism `commits` deliberately
   avoids (open question 1).
3. **Two `*int` fields, `prds_done` + `prds_open`, both-or-neither.** Mirrors the
   `Commits *int` convention; the consumer derives the total; no third `total` stamp
   (open questions 2, 3). Two plain decimal ints keep the unquoted kaniko build-arg
   expansion safe and each get the numeric guard `commits` has.
4. **Public-by-construction, added to the closed-key-set allowlist with a reason.**
   A scalar over the tracked tree is the same disclosure class as the commit count,
   not a runtime disclosure like uptime; `TestVersionEndpointCarriesNothingPrivate`
   enforces that the classification is deliberate (open question 4).
5. **`uzi version` gets the counts too.** The CLI already decodes this DTO; one line,
   same both-or-neither guard, keeps the two consumers honest to one source (open
   question 5, per the CLAUDE.md CLI-parity check).
6. **Non-recursive, `.md`-only, numerically guarded count.** `prds/` holds
   sub-directories; the count is `find -maxdepth 1 -name '*.md'` and each value is
   shape-checked in CI before it becomes a build-arg — discharging the existing
   "give a fifth stamp a gate" CI warning (`.gitlab-ci.yml:1755`).

## Review findings

Reviewed 2026-08-08 by an architect subagent instructed to open every code citation
and assume some were wrong, to trace the two load-bearing correctness claims, and to
measure the counts. Line refs were checked against HEAD `b0c5113`; the `git diff
ab2d033..b0c5113` touches only `agent/`, `.claude/rules/agent.md`, `CHANGELOG.md` and
`specs/ai.md`, so **none of the files this PRD cites changed between `ab2d033` and
HEAD** — verifying at HEAD equals verifying at `ab2d033` for every citation here.
Verdict: **sound-with-fixes** — the design is complete and faithfully mirrors the
`commits` / PRD #175 precedent end to end, and the ~40 file:line citations were
accurate to an unusual degree.

**The two load-bearing correctness claims — both confirmed against code:**
- **`prds/` and `.git` are not in the `api/` Docker build context.** `docker-compose.yml:29`
  is `build: ./api`; `.gitlab-ci.yml:1697` is `UZI_BUILD_CONTEXT: "$CI_PROJECT_DIR/api"`;
  root `.dockerignore:1-4` states the api image builds from its own subdir context and
  `:27` excludes `.git`. `api/Dockerfile`'s only `COPY`s are `go.mod go.sum` (`:7`) and
  `COPY . .` over the `api/` context (`:10`); `-buildvcs=false` at `:41` with the
  "publish context is api/ with no .git" reason at `:30-40`. So the running container
  and the image build both lack `prds/` and `.git` — the runtime/Dockerfile counting
  paths really are impossible.
- **`commits` is computed in `publish:assert-changelog` and delivered by dotenv.**
  `git rev-list --count HEAD` at `.gitlab-ci.yml:1660`, numeric-guarded at `:1661`,
  written to `commit_count.env` (`:1662`) and published as `dotenv:` (`:1663-1669`);
  the job runs `GIT_DEPTH: 0` + `GIT_STRATEGY: clone` (`:1636-1637`). `publish:api`
  passes it via `UZI_BUILD_ARGS` at `:1775`, with the "no default declared" rationale
  at `:1769-1775`. The **fifth-stamp gate comment exists exactly where cited**: "If you
  add a fifth stamp, give it a gate or widen this paragraph." is verbatim at
  `.gitlab-ci.yml:1755`, inside the `:1748-1758` paragraph.

**Every other citation opened and confirmed:**
- DTO: `Commits *int` at `buildinfo.go:52-56`; omit-rule doc at `:17-20`.
- Wire test: `TestBuildInfoDTOTags` at `wire_test.go:519`, stamped assertion at
  `:531-532`. (Nit: the function body runs to `:540`, not `:538` — there is a third
  `(uptime 0)` assertion at `:538-539` the PRD does not need to touch. Range corrected
  in-text is not required; noted here.)
- Handler: raw fields `:122-124`; `BuildStamp` `:251-258`; `SetBuildInfo` `:266-270`;
  `Version` `:397-424` with the `strconv.Atoi ... err == nil && n >= 0` commits block
  at `:410-412`; doc enumeration `:359-361` / `:374-376`; world-readable comment
  `:363-372`; the CLI-consumer note (`uzicli.(*HTTPClient).BuildInfo`) at `:106-113`.
- Trust test: `TestVersionEndpointCarriesNothingPrivate` at `version_test.go:450`;
  `public` map `:453-462`; the `commits` reason `"already public: a count over that
  repo"` at `:458`; premise-guard `SetBuildInfo` literal at `:494-500`. The existing
  `commits` parse/omit cases the PRD says to mirror are real and rich: the omit table
  (`:186-211`, incl. `"negative count" = "-1"`), the whitespace-trim case (`:258-265`),
  and `TestVersionEndpointZeroCommitsPresent` (`:274`) proving `"0"` renders present —
  so `prds_*: 0` will correctly render, not omit.
- Server: `version` var `:64`, `commit/builtAt/commits` var block `:87-91`, comment
  `:66-91`; the mirror quote at `main.go:73-76`.
- Dockerfile: `ARG UZI_COMMIT_COUNT` `:35`, ldflags line `:41`.
- Web: `BuildInfo` `api.ts:588-612` with `commits?` `:605-607`; `BuildInfoPopover.tsx`
  "PRESENTATIONAL ON PURPOSE" `:5-16`, `Row` `:174-195`, the `commits` boundary guard
  `:249-250`, guard block `:231-255`, the Founded/Built/Commit/Uptime `dl` `:359-381`,
  Uptime row `:380`; `mockBuildInfo` `data.ts:3541-3548`, `mockBuildInfoUnstamped`
  `:3554-3558`, `mockBuildInfoNoUptime` `:3564-3567`; `mockApi.version` `:1052`.
- CLI: `uzicli.(*HTTPClient).BuildInfo` at `api/internal/uzicli/client.go:789`; the
  render seam `serverRows` at `api/cmd/uzi/version.go:156`, appending `commits` under
  `if b.Commits != nil` at `:164-165` — the exact place the new row goes.

**Counts measured (not asserted):**
- `find prds/done -maxdepth 1 -name '*.md'` = **80** (`git ls-files 'prds/done/*.md'`
  agrees).
- Tracked top-level open PRDs = **32** (`git ls-files 'prds/*.md'` = 112 — git pathspec
  `*` spans `/`, so that is recursive — minus the 80 in `done/`). Matches the PRD's
  "80 done, 32 open" at `ab2d033`. (The live `find prds -maxdepth 1 -name '*.md'` reads
  33 only because the untracked `245-*.md` is on disk; it is 32 as tracked at
  `ab2d033`.)
- Sub-directory hazard confirmed and made precise: `prds/done/`, `prds/mockups/`,
  `prds/43-m0-probe/` all exist, but **only `done/` holds `.md`** — `mockups/` is `.html`/`.png`,
  `43-m0-probe/` is `.ts`/`.json`/`.log`. So the material recursive over-count is
  `done/` (80 files); `-maxdepth 1` is what earns its keep.
- No top-level `prds/README.md` / template / index that would inflate the open count —
  every one of the 32 is an `NNN-slug.md` PRD.
- Mockup (`prds/mockups/245-version-prd-counts-mock.html`) confirmed as **Variant A**
  (line 79: `PRDs` row, `80 done` in accent · `32 open` faint), matching the PRD's
  render section; it also carries the rejected B/C variants and the `80/32/112/71%`
  figures.

**Design soundness (complete, nothing from PRD #175's shape missing):** the two-`*int`,
both-or-neither, omit-on-dev, public-by-construction, derive-total-in-consumer design
is coherent. It includes the three things #175's shape implies and that are easy to
skip — the closed-key-set security test (open q4 + Technical scope), the wire tag test
(DTO + Tests sections), and the CLI consumer (open q5 + M2, per the CLAUDE.md
CLI-parity check). Deriving `total` in the consumer rather than stamping a third value
correctly avoids a `total == done + open` invariant with no upside. `n >= 0` (not
`> 0`) is right: a real `0` is a known count, not "unknown".

**Fixes folded in:**
- **Open question 2:** the "space or slash is a hazard" claim was half-right. The CI
  comment warns about **whitespace** (unquoted `$UZI_BUILD_ARGS` field-splitting); a
  `/` survives POSIX splitting fine. Reworded — the real reason to prefer two ints over
  a combined string is convention + reusing the numeric guard, not the slash.
- **"The count is exact" section:** made the recursion hazard precise (`done/` is the
  material over-count; `mockups/` and `43-m0-probe/` hold no `.md` today) and added
  that the count is a live build-time snapshot which reads `open = 33` on the first
  publish after this PRD lands — the PRD counting itself, which is correct.
- **CLI Technical scope:** replaced the vague "add a line" with the concrete seam
  (`serverRows` at `version.go:156`, beside the `commits` row at `:164-165`) and noted
  that `*int`/`%d` rendering means no `CellText`/unbounded-string concern applies here.

Nothing was found that blocks implementation; the fixes above are precision, not
correctness reversals.
