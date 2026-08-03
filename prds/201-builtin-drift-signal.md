# Issue #201 — M4a: the builtin drift signal

**Status:** design-critique wave, not yet implemented.
**Branch:** `fix/201-builtin-drift`, worktree `/home/user/repos/myorg/vtmocanu/uzi/prd-201`. **Was** based on origin/main `25ebcd39`; merged `origin/main` at `c3704d25`, and is now **0 behind** (`git rev-list --count HEAD..origin/main` = 0). Citations below are at `25ebcd39` unless stated.
**Spec of record:** this file. Issue #201's body states the problem; its newest comment
(note_22449, 2026-08-03) is the settled design for the whole of #201. **This file scopes
M4a only.** Corrections amend this file with a dated entry under `## Amendments`; messages
only name the section that moved.

> **PRD #85 Phase 2 (M8-M11) contradicts the issue's brief on sequencing and must not be
> followed as written.** It orders `version` (M8) before `hash` (M9) and gates M8 behind
> #85 M1's parser change. The brief inverts that deliberately. Amending #85's Decision Log
> is part of the overall #201 work, and is **out of scope for M4a**.

---

## Why M4a ships alone, and first

`ResetAgentTemplate` (**`api/internal/handler/agent_templates.go:396-439`** — it is a
HANDLER; earlier revisions of this file said `api/internal/store/agent_templates.go`, which
does not exist) is all-or-nothing: `:425-432` is one unconditional `UpdateAgentTemplate` of
description, model, tools and prompt_body from the embedded definition. No merge, no
per-field option. An admin pressing **Reset to default** on a row they have edited loses that
edit with no diff shown anywhere. So the drift signal is a **strict prerequisite** for M4b's
auto-update: the diff view is what makes Reset safe to press, and it is the surface an
unclassifiable row needs in M4b.

Concrete stakes: issue #210 fixes ten builtin templates that named an unreachable recipient,
measured at 8 of 26 SendMessage calls failing across three real runs. That fix does not reach
dev-cluster; its CHANGELOG entry tells an admin to open ten templates and click Reset, blind.

> **#210 MERGED at ~13:57 on 2026-08-03, while this milestone was being designed.** It is on
> `origin/main` at `d367653b` (`86c43fcd`, `f108d739`), and `api/internal/agenttmpl/recipient_test.go`
> — a 430-line guard on the recipient wording — landed with it.
>
> **This block previously said the opposite, and said "do not correct this back".** That was
> right when written: the reviewer ran `glab mr view 168` during the design wave and got
> `state: open`, refuting note_22449's "the fix is merged" — which was itself written before
> the merge. All three statements were true at the moment each was made. The instruction not
> to correct it is what did not survive, and it is preserved here rather than deleted because
> a confident "do not change this" outliving its evidence is the failure this file is about.
> **Re-derive a merge state at the moment you assert it; never inherit one from this file.**

## Scope

**In:**
1. A computed, never-stored `differs_from_builtin` on the template DTO.
2. **`GET /agent-templates/{id}/builtin`** — a new read-only route serving the shipped
   `Definition`, gated to callers passing `authorizeTemplateWrite`, returning **409** with
   `ResetAgentTemplate`'s existing "no builtin definition to reset to" semantics for a
   removed builtin. **Added 2026-08-03 by owner decision; see Amendment 1.**
3. A badge on `web/src/pages/Agents.tsx` and `web/src/pages/AgentDetail.tsx`.
4. A shipped-vs-stored diff in `web/src/components/AgentTemplateEditor.tsx`, using **jsdiff**
   rendered as **React elements**. See Amendment 1 for why the HTML-string form is banned.

**Out, and deliberately:** no migration, no schema change, no boot-path change, no hash, no
historical-hash set, no auto-update, no kill switch. All of those are M4b. If M4a finds
itself needing a DATABASE column, that is a signal the design drifted, not a reason to add
one.

**🔴 Nothing may be added to `agenttmpl`'s package `init()`.** It runs before `main`, and a
parse failure there panics (`api/internal/agenttmpl/builtins.go:41-43`). On a hard singleton
that is CrashLoopBackOff. This is the one way M4a could silently violate criterion 7.

## Mechanisms — SETTLED by the design wave, 2026-08-03

The claims below were verified by architect + reviewer + auditor independently, and the
lead re-derived the load-bearing ones. **Claims 3, 4 and 5 as originally written were wrong
and are corrected here.** Line numbers are at `25ebcd39`.

1. **The shipped definition is available in-process by name.**
   `api/internal/agenttmpl/builtins.go:58` exposes `BuiltinByName(name string) (Definition, bool)`.
   `Definition` is `{Name, Description, Model string, Tools []string, PromptBody string}`
   (`api/internal/agenttmpl/render.go:13-26`). So the comparison needs no new embedding and
   no I/O.

2. **The DTO is assembled in one place.** `agentTemplateDTO` and `templateDTO(store.AgentTemplate)`
   at `api/internal/handler/agent_templates.go:53-75`. A computed field added there reaches
   every response that goes through it — **confirm that is actually every list and detail
   response, and name any handler that builds a template response some other way.**

3. **CORRECTED — compare FOUR mutable columns, not five.** `description`, `model`, `tools`,
   `prompt_body`. **`name` is the LOOKUP KEY, never a compared value**: the shipped side is
   obtained by `BuiltinByName(t.Name)` whose loop condition is `d.Name == name`
   (`builtins.go:60`), so `def.Name == t.Name` holds by construction, and `name` is immutable
   anyway (`UpdateAgentTemplate`, `agent_templates.go:354-361`, does not carry it). A
   five-column comparison contains one unfalsifiable term and sends the tester hunting a
   fifth case that cannot exist.

   **Never `Render(def)`.** Two reasons, and only the second one holds for M4a:
   - ~~#85 Decision 3 reorders the frontmatter~~ — **INERT here.** M4a renders both sides
     with the SAME binary at the same instant, so a reorder cancels. That argument is
     correct in note_22449's D2, where the stored *hash* was written by an older binary.
     It does not transfer.
   - #85 M2 stamps `version:` (`prds/85-agenttmpl-role-library-sync.md:266`), which would
     appear on the shipped side and never on the stored side (there is no version column),
     so every stamped builtin reports drift forever, silently.
   - **The strongest reason, missed originally:** `Render` OMITS `tools` and `model` when
     empty (`render.go:39-44`), so a stored `tools: []` and a stored `tools: NULL` render
     identically — a `Render`-based comparison HIDES a difference the UI displays as
     "none" vs "all" (`web/src/lib/agentTemplates.ts:172-174`).

4. **REFUTED AS WRITTEN. The normalization already exists; do not write new normalization.**
   `templateToDefinition` (`api/internal/handler/agent_templates.go:112-124`) already maps a
   stored row onto an `agenttmpl.Definition` — the exact type `BuiltinByName` returns. It
   runs `decodeTools` (`:99-110`, a `json.Unmarshal`) and folds `pgtype.Text{Valid:false}` to
   `""`. So the comparison is `Definition` vs `Definition` and the `*string`/`[]string`
   mismatch never arises; the DTO is not an input to it.

   The jsonb hazard is **real but only for a raw-byte comparison** — measured by the reviewer
   on a throwaway Postgres: `json.Marshal` gives 23 bytes, pgx reads back 25
   (`["Read", "Write", "Bash"]`), `bytes.Equal` false, decoded slices equal. Nothing in scope
   needs that form. Note a byte comparison would redden **9 of 11** builtins, not all of them
   (`coder.md` and `lead.md` carry no `tools:` line, so both sides are empty) — a partial red
   reads like genuine drift, so that failure is QUIET, the opposite of what this claim said.

   | column | rule | direction if you get it wrong |
   |---|---|---|
   | `model` | exact `==` on the folded `Definition.Model` | — (exact; `""` is unreachable in the column, `validateModel:578-580`) |
   | `tools` | `slices.Equal(decodeTools(t.Tools), def.Tools)` | — |
   | `tools` — **never sort** | order is rendered (`render.go:41` joins in order) and the editor preserves it | sorting **HIDES a real edit** |
   | `tools` — **`slices.Equal`, never `reflect.DeepEqual`** | `slices.Equal(nil, []string{})` is true, which is correct: both mean inherit-all | `DeepEqual` reports **FALSE DRIFT** on a semantically identical row |
   | `prompt_body` | exact, **never trim** | trimming HIDES a real edit; the trailing newline is pinned by the parse→Render round-trip test |
   | `description` | exact, **never trim** — see Amendment 1 §4 | trimming hides whitespace-only edits permanently |

5. **CORRECTED — `scope` vs `is_builtin` is unpinnable, and the separable case is a
   USER-scope name collision, not the admin shadow row.**
   `00048_agent_template_scopes.sql:22` adds `CHECK (is_builtin = (scope = 'builtin'))` over
   two NOT NULL columns, so the two discriminators are a **provable biconditional**. No
   fixture can distinguish an implementation using one from an implementation using the
   other. Use `scope` for style; do not spend time "confirming" it.

   The case that IS separable, per `00048:26-27` in its own words — *"A user may therefore
   own a 'coder' that coexists with the builtin 'coder'"* — is a **user-scope template whose
   name matches a builtin**. Any authenticated non-admin can create one; only `lead` names
   are reserved (`agent_templates.go:252`). A name-keyed implementation with no scope check
   reports `differs_from_builtin: true` on that user's private template, and
   `ResetAgentTemplate` returns **400 "only builtin templates can be reset"** (`:408-411`) —
   the badge advertises an action that fails. **This case must be in the fixture set.**

6. **CONFIRMED, with an in-tree precedent for the answer.** `BuiltinByName` returns `false`
   for a removed builtin (`builtins.go:64`), and `ResetAgentTemplate` already handles that
   state at `:412-418` with **409 "no builtin definition to reset to"**. The field reports
   `false`. It therefore conflates two states with OPPOSITE Reset outcomes (200 vs 409) —
   which is why the removed-builtin state reaches the UI through the new route's 409 rather
   than through a tri-state DTO field. Pre-existing and worth knowing:
   `AgentDetail.tsx:123-131` currently offers Reset to any `is_builtin` row, so today it
   offers a button that 409s.

## Acceptance criteria

1. `differs_from_builtin` is computed per request and stored nowhere. No migration in the diff.
2. A pristine builtin row reports `false`; a row edited in **any one** of the four mutable
   columns reports `true`. **All four are covered separately** — an implementation comparing
   only `prompt_body` must go red.
3. **A `user`-scope row whose name matches a builtin reports `false`.** This is the case that
   separates a scope-checking implementation from a name-only one; the admin shadow row does
   not (claim 5). A `global`-scope row also reports `false`.
4. A builtin row with no shipped counterpart reports `false`, not `true`, and the UI does not
   offer Reset for it.
5. `GET /agent-templates/{id}/builtin` returns the shipped definition to a caller passing
   `authorizeTemplateWrite`, **403** to one who does not, and **409** for a removed builtin.
6. The badge appears on both pages and reads **"differs from shipped"** — never "customized"
   or "edited". See Amendment 1 §3.
7. The editor shows a shipped-vs-stored diff rendered as React elements. **`git grep -F
   dangerouslySetInnerHTML -- web/src` must still return zero call sites afterwards** (it
   returns 11 hits today, every one a comment saying not to).
8. **A no-op save creates no drift**: open a pristine builtin, submit the editor unchanged,
   assert still `false`. This is the single test that catches a seed-path/write-path
   asymmetry in any of the four columns, and nothing else reaches it.
9. **Reset clears the badge with no refetch** — `templateDTO` is the reset response
   (`:438`) and `AgentDetail.tsx:59` sets state from it. Assert it; it is the interaction an
   admin judges the feature by, and it holds only while reset keeps returning the DTO.
   **PARTIALLY MET — see Amendment 7 F-T2.** The CLIENT half is measured twice over: a
   browser run with counters (reset 0→1, all three reads 0→0) and a same-visible-outcome fold
   proving the no-refetch assertion fires on its own channel. **The SERVER half is unpinned:**
   `ResetAgentTemplate` is 0.0% covered, and folding it to return the pre-reset row — so the
   badge never clears — compiles and leaves the handler package green. Do not report this
   criterion as met without that qualifier.
10. `task gate:api` and `task gate:web` green, run **SERIALLY** (concurrent gates manufacture
    a known flake, #198). `task` exits 201 on any failure, so test for non-zero, never a
    number. If `golangci-lint` reports `parallel golangci-lint is running`, another worktree
    holds a host-global lock — re-run, do not report a red gate.
11. No change to `ReconcileBuiltinTemplates`, to anything `api/cmd/server/main.go` calls
    before the listener at `:684`, or to `agenttmpl`'s package `init()`.

**Not required for M4a:** any live-DB test. The jsonb hazard is pinnable **without** a
database by handing the comparison a `store.AgentTemplate` whose `Tools []byte` holds the
Postgres-canonical `["a", "b"]` form directly (the reviewer measured that exact byte string).
**That case must be in the unit fixture**, or the suite agrees with a byte-comparison
implementation on everything it covers.

## Roster

- `architect`: reported — design wave at `a24961bf`. Refuted claim 4; found the missing diff
  transport; §5's `agenttmpl` placement is its call.
- `reviewer`: reported — design wave at `a24961bf`. Found the nonexistent-file citation, the
  unfalsifiable `name` term, and measured the jsonb round-trip on a throwaway Postgres.
- `auditor`: reported — design wave at `a24961bf`. Found the user-scope name-collision case
  and the `dangerouslySetInnerHTML` constraint; confirmed the boot path.
- `coder`: **delivered.** `bcd67c72` (api) + `10970920` (web), then `c3704d25` merging
  `origin/main`. Re-gated green on the merged result. Now mid-fix-round on Amendments 3-4.
  Three self-reported caveats, two of which the reviewer then SHARPENED rather than confirmed:
  - **Criterion 5's residual is FOUR LINES WITH NO BRANCHES**, not "everything below the row
    fetch". `h.q` is a concrete `*store.Queries` (`handler.go:38`) so no fake substitutes
    without a database — but what that leaves untested is `loadTemplateForWrite`, which is
    **pre-existing, route-agnostic, and already carries three production write handlers**
    (Update, Delete, Reset). Every decision the new route makes — authz, its ordering before
    the builtin check, the 400, the 409, the 200 body — is below the split and pinned by
    `TestWriteBuiltinDefinitionStatusMatrix`. The true gap is only that
    `GetBuiltinAgentTemplate` wires the two together in that order, which nothing pins. The
    original wording understated how little is actually below and should not be repeated.
  - The `dangerouslySetInnerHTML` count — see Amendment 3 R4; every number was right in a
    unit nobody stated.
  - This worktree's `web/node_modules` is a real directory rather than a symlink into
    `main/`, because `npm install` replaced it. (Not a hazard: `web/` has no `agent-browser`
    dependency, so CLAUDE.md's clobber warning is about `agent/` and does not apply.)
- `reviewer`: dispatched at `10970920`, re-pointed to `c3704d25`.
- `auditor`: dispatched at `10970920`, re-pointed to `c3704d25`.
- `tester`: dispatched at `10970920`, re-pointed to `c3704d25`. Given the six behaviours to
  pin and NOT a fold class — a control the lead designs carries the lead's blind spots to
  every validator at once.
- `web-ux`: dispatched. **Blocker CLEARED by the coder**, which is why the pass is worth
  running: `mockShippedBuiltins` is now a constant separate from `mockTemplates`, the mock
  computes drift as a deliberate second implementation, and nine cases discriminate
  (pristine control, tools-order-only, tools-membership, prompt_body-only, model-only,
  description-only, no-shipped-twin, global row, user row colliding with a builtin name),
  each asserted so a later fixture edit that drops one goes red.
- `fact-checker`: dispatched. Scoped to the seven commits, this file, and the comments the
  feature commits added.
- `documenter`: pending — `docs/agent-templates.md:121-135` (NOT `122-125`, which this file
  inherited from the issue body and which lands on the reset-is-verbatim paragraph). Both the
  reset paragraph and the no-auto-update paragraph at `129-135` need editing. CHANGELOG entry
  under `[Unreleased]`.
- `fact-checker`: pending — this brief and the eventual comments carry mechanism claims.
- `spec-keeper`: pending — `specs/` exists; sync after blocking findings clear.
- `researcher`: closed — no external research needed; every mechanism is in-tree.
- `release`: closed — M4a is not a release.

## Rebasing onto the moved main — DONE at `c3704d25`. Kept as the record of why.

**This section is past tense. The branch is 0 behind `origin/main`; do not act on it.** It read
"REQUIRED before the MR" until the fact-checker pointed out that a heading marked REQUIRED,
telling a reader to do something already done, is a wrong doc by this repo's own convention —
the roster section had recorded the merge for hours while this one still demanded it.

**What happened:** the branch **was** based on `25ebcd39`; `origin/main` moved to `d367653b`,
**22 commits ahead** — a historical gap, correctly measured. The merges across that range were
`78ad89b9` (13:15:35Z), `10f7b6a5` (13:21:39Z), `9ac80056` (13:49:28Z) and `d367653b`
(13:53:27Z). The coder merged at `c3704d25` and re-gated green, because **a green gate on a
stale base says nothing about the merged result.**

**If main moves again, this becomes live again** — re-derive the gap rather than trusting the
22, and the merge stays the coder's to run, never the lead's, in a worktree holding a live
writer.

> **CORRECTED.** This read *"because all three previously-in-flight MRs merged at ~13:57"* —
> wrong twice. **FOUR** MRs merged, not three: !170 was created at 13:33Z, after the sentence's
> premise was formed, and it alone contributes **3** of the 22 commits, so the set named could
> not produce the count given. And no merge happened at ~13:57; the latest is 13:53:27Z.
> **The mechanism, which outlives this instance: an in-flight MR set is a fact with a shelf
> life and a commit count is not.** State the count, name the merge SHAs, and drop the MR
> tally — which is what the paragraph above now does.

**The merge belongs to the coder, not the lead** — ref-moving commands (`merge`, `rebase`,
`reset`, `checkout`, `stash`, `push`) are never run by the lead in a worktree holding a live
writer, because uncommitted work is invisible to the person reaching in. Do it after a commit,
never on a dirty tree.

Collision surface, measured with **`git diff --stat 25ebcd39..d367653b`** over M4a's files.
*(The SHA is pinned deliberately. This originally read `25ebcd39..origin/main`, which
reproduces only while `origin/main` sits where it did — a command embedding a moving ref
silently changes what the sentence claims the next time main advances, without the text
changing at all.)*

- **`api/internal/handler/agent_templates.go` — UNTOUCHED.** M4a's primary surface is clean.
- **`web/src/pages/{Agents,AgentDetail}.tsx`, `web/src/components/AgentTemplateEditor.tsx`,
  `web/src/lib/api.ts` — UNTOUCHED.**
- **`.claude/agents/` — UNTOUCHED**, so the roster sync at `7784c037` merges clean.
- `web/src/mocks/mockApi.ts` — one line changed. Trivial, but it is a file M4a edits.
- **Ten `api/internal/agenttmpl/builtins/*.md` bodies changed** (#210's recipient fix), and
  `api/internal/agenttmpl/recipient_test.go` is new, 430 lines. M4a adds no fixture that
  hardcodes a builtin body, so this should not bite — **if you wrote one, it will.**

## Amendments

### Amendment 8 — 2026-08-03. THE LEAD MADE THE SAME ERROR TWICE. Rule, not another apology.

**Instance 1:** *"the merge altered no M4a file"* — five hand-picked paths checked, generalised
to all. Caught by the auditor. Retracted, incompletely, which then needed its own correction.

**Instance 2:** *"everything after `b0dc8dad` is docs-only"* — the lead's own commits were
docs, and it generalised over a range that also held the CODER's. Enumerated:
`74dcb9f6`, `2144d51c`, `8e270772` and `33834a21` all touch `api/` or `web/`. **`33834a21`
rewrote the exact surface four of the tester's web arms fold** (the diff renderers, now four
rather than one). The tester says it plainly: had it taken the claim at face value it would
have reported six arms as current when they were a source commit stale.

**The shape is identical and it is not carelessness — it is a SAMPLE generalised to a
POPULATION, asserted in a channel where nobody can see the sample.** Both times the sample was
correct. Both times the generalisation reached a validator and would have corrupted a result.

**THE RULE: the lead never characterises a RANGE it has not enumerated.** The check is
mechanical and takes one command — per commit, does it touch source:

```sh
for c in $(git rev-list --reverse <base>..HEAD); do
  git show --name-only --format= $c | grep -qE '^(api|web)/' \
    && echo "SOURCE $(git log -1 --format='%h %s' $c)" \
    || echo "docs   $(git log -1 --format='%h %s' $c)"
done
```

**And never state a tip in a message; state a tip and how to check it.** Three validators have
now told the lead its named SHA was stale on arrival — twice while the message was in flight,
which no care in composition can prevent (see Amendment 6's arrival-vs-authorship note). The
durable fix is the same one: **put the claim in the brief where the reader re-derives it, and
keep the message a pointer.**

#### Counting convention, before either figure gets quoted as THE number

The auditor reports the `builtins/` corpus delta as **139 body lines**; the tester measures
**+87/−56 = 143** over the same range. Neither is wrong — they count different things.
Nothing downstream depends on it. **Pin the convention when you cite it, or two correct
numbers become a contradiction later**, which this file already has one recorded instance of
(the grep unit table in Amendment 3 R4).

### Amendment 7 — 2026-08-03, tester. 24 folds, and a correction to Amendment 6's headline.

Gates green at both `c3704d25` and `37b4bfc0`, run serially in throwaway detached worktrees
with a private `GOLANGCI_LINT_CACHE` — never in `prd-201`, and it confirmed it made no write
there. Baselines carried positive evidence (`RUN=45 PASS=45 FAIL=0 SKIP=0`). **Its instrument
produced the disconfirming answer twice**, which is the property that makes the rest of it
worth reading.

**Behaviour 1 is pinned by EIGHT folds with EIGHT DISJOINT failure sets.** The one that matters
most: sorting tools reddens **only** the order case while membership stays green, and dropping
tools entirely reddens **both** — so order and membership are independently pinned, which is
the case Amendment 3 F3 worried about and nothing else reaches.

**Behaviour 5 was pinned on its own channel, not by outcome.** The obvious fold (discard the
response and `load()`) reddens the badge — ambiguous, because two assertions could be firing
as one. So it ran a fold with an **identical visible outcome**: keep `setTemplate` from the
reset response and add one redundant `getAgentTemplate`. That reddens `expected "spy" to be
called 1 times, but got 2 times`. The no-refetch assertion is live independently of the badge
assertion.

#### 🔴 CORRECTION TO AMENDMENT 6: "criterion 9 measured" was the CLIENT half only

**F-T2 (should-fix, new).** `ResetAgentTemplate` is at **0.0% coverage**. Folding it to return
`templateDTO(t)` — the **pre-reset** row it already loaded, so `differs_from_builtin` stays
`true` and the badge never clears — **compiles and leaves the whole handler package green.**
So criterion 9's own caveat (*"it holds only while reset keeps returning the DTO"*) is
unpinned on the server: the contract is held today only by the web test's **mock** of
`resetAgentTemplate` and by `mockApi`, neither of which observes the server.

**Amendment 6 and `b9a16078` say "criterion 9 measured" without that qualifier. The measurement
was real and remains valid — it is the CLIENT half.** Criterion 9 is therefore **partially
met**: the client provably clears the badge from whatever the response carries, and nothing
pins that the server puts the right thing in it.

**F-T1 (should-fix) — and it revises the coder's self-report UPWARD.** The coder reported
criterion 5's gap as "the row fetch is unreachable without a database". Measured, it is larger:
`GetBuiltinAgentTemplate` is at **0.0% coverage** — the entire function body is unexecuted, and
rewriting it to serve `templateDTO(t)` (the **stored** row rather than the shipped definition)
leaves the package green. `writeBuiltinDefinition` itself is **100%**. So the residual is not
the fetch but the **WIRING** — that the handler delegates at all, and that it passes
`loadTemplateForWrite`'s actor and row rather than the viewer loader's. **The split was the
right call and reached everything it could; "criterion 5 covered" is what must not be said.**

**Both are one structural fact:** `h.q` is a concrete `*store.Queries`, so **every** DB-touching
handler here is 0%-covered, `ListAgentTemplates` included, pre-existing. **Not M4a's to fix** —
routed to M4b or its own issue alongside R1's shared-fixture work, since both want the same
seam. Recorded rather than left to inference.

#### The `IssueView` flake — NOT reproduced, and the negative is properly bounded

Five full web-suite runs across both tips: **1652/1652 then 1655/1655, zero failures, IssueView
green every time**, 1425-3318ms against a 5000ms `asyncUtilTimeout`. The mechanism the coder
named is the one `web/src/test-setup.ts` already documents across ~25 runs, two failing *"right
on the default"* — and that file frames the class as suite-wide rather than a fixed file set.

**It then checked the one way M4a could touch an unrelated file, because "it is a documented
flake" is the answer that stops people looking.** `web/src/lib/api.ts:10` **statically** imports
`../mocks/mockApi`, so `data.ts` — which M4a grew by 174 lines — is in every test file's module
graph including IssueView's. Real mechanism, measured: IssueView alone, 3× at pre-M4a
`541c2c0b` vs 3× at `c3704d25` → **453/938/315ms vs 362/897/699ms**. Ranges overlap both ways;
the effect is below the instrument's noise floor. **Verdict: not reproduced, mechanism
plausible and pre-existing, no measurable M4a contribution — and five clean runs cannot
disprove a 1-in-N flake, which is the honest limit.**

#### The NO-BREAK SPACE, hit independently by two agents

`AgentTemplateEditor.tsx:466` is `{r.text || " "}` with a **`c2 a0`**, not a space. Read and
Grep both render it as an ordinary space, so a literal anchor there matches nothing and `gsed`
reports success having changed zero bytes. The tester's diff-line-count assertion caught it;
the fact-checker hit the same byte and caught it by traceback. **Without an assert-it-landed
control, that arm is a green run over unmutated code reported as a pinned criterion.**

### Amendment 6 — 2026-08-03, criterion 9 MEASURED **on the client**, and F5 upgraded from derived to observed.

**Criterion 9 HOLDS, measured on the mock rather than reasoned.** Clicking Reset on
`t-tester`: `resetAgentTemplate` 0→1, and `getAgentTemplate` / `getBuiltinAgentTemplate` /
`listAgentTemplates` all **0→0**. Badge cleared, panel flipped to "Matches the shipped
definition". The badge cleared **from the reset response with zero refetches**, and the change
propagated to the list on in-app navigation.

**The positive control is the part that makes it evidence.** "No refetch" is an ABSENCE, and
an absence cannot distinguish a live instrument from a dead one — so web-ux first verified
`(await import("/src/lib/api.ts")).api === mockApi` (the patch is on the exact object the
component calls) and drove one real call through the app's own binding to move the counter to
1 before zeroing and measuring.

**F5 is now OBSERVED, not code-derived, and it is worse than "discards unsaved edits".** With
`UNSAVED-MARKER-` typed into the description and not saved, one unconfirmed click discarded
**the live unsaved edit AND the stored drift together** — no dialog, no warning, no undo. That
was the half of F5's argument that had only been read off `key={template.updated_at}`.

**Provenance — RETIRED, and the result is stronger than first reported.** It was taken on the
dirty tree at 18:00:36 and originally defended by inspection (`git diff -U0` matching
`resetAgentTemplate|setNotice|differs_from_builtin` **zero** times, so `reset()`, the notice,
the badge condition and the remount key were untouched). **`2144d51c` at 18:03:09 then
committed exactly that state**, so this is now a direct measurement OF a commit rather than of
uncommitted work that transfers. The inspection argument still holds but is no longer
load-bearing. Recorded this way because web-ux declared the weaker provenance rather than
writing "measured at `b0dc8dad`", which would have been false — and the stronger claim only
became available afterwards.

#### 🔴 OPERATIONAL HAZARD FOR ANY MEASUREMENT IN THIS WORKTREE

**The coder saving a file triggers a full HMR page reload, which wipes injected instrumentation
AND re-seeds the mock's in-memory state.** Web-ux lost two observations to it, and **both
failures presented as clean, confident zeros** — the reassuring direction. A second cause hit
the same run: the detail page's `Back` is a `<button>`, not a link, so a "navigate away and
back" control never navigated and also returned zeros.

The fix that worked: run patch → control → edit → click → read as **one uninterruptible
eval**, so a mid-measurement reload surfaces as MISSING counters rather than as zeros.
Generalised: **in a worktree another agent is writing, an absence is not evidence unless you
can show the instrument was still live when you read it.**

#### 🔴 A HARNESS BUILT TO DEMONSTRATE A DEFECT INVERTS WHEN THE FIX LANDS

Raised by web-ux before the pending re-measure, which is the only time it is cheap. The F5
harness currently asserts the defect **reproduces** — marker present before the click and gone
after, badge `true` while the panel reads "Matches". **Once F5 lands, a green harness means
the defect is FIXED, and "failed to reproduce" becomes the success case rather than a null
result.**

Re-running saved assertions blind therefore reports the fix as a regression, and it does so
confidently, because the harness is working perfectly — it is answering the question it was
built for, which is no longer the question being asked. Same family as every instrument
defect in this file, arriving through the passage of time rather than through a wrong tool.

**Rule for the re-measure: re-derive the expected direction from the code at the named SHA
BEFORE running the saved harness.** Applies to S1 too if F12 moves the badge or panel path.

Filed as a team-process fact because it produced a wrong accusation from careful reasoning.
Web-ux charged the lead with relaying a claim it had already retracted; the lead accepted the
substance (the retraction went to a subset of the validators it had misinformed) and disputed
the sequence. Web-ux then settled it against timestamps and refuted its own charge:

```
c3704d25  17:34:03   the merge
b5fac2aa  17:40:31   HEAD stops being c3704d25 here
25561485  17:44:20   audit findings
76f38ae1  17:49:31   THE RETRACTION
```

The lead's message described HEAD as `c3704d25` and named no later commit, so it was composed
in a **6.5-minute window closing 9 minutes before the retraction existed**. Not merely
unproven — impossible.

**The mechanism: message delivery here queues behind a long-running turn, so a message can
ARRIVE long after it was written and after commits it does not mention.** Web-ux inferred
composition order from delivery order, which is the same shape as every instrument defect this
file records — a confident answer to a question the instrument cannot see — pointed at a
teammate rather than at code. **Two consequences worth keeping: never date a teammate's claim
by when you received it, and the lead's own fix stands — retract to the full list you asserted
to, not the subset still mid-flight.**

#### TWO MORE FIXES

**F12. The success notice is off the settled vocabulary.** `AgentDetail.tsx:82` says *"Reset to
the builtin default."* while the badge says "differs from **shipped**", the panel says "Matches
the **shipped** definition", and the reset card says "restores this template to its **shipped**
definition". Amendment 1 §3 settled that vocabulary deliberately, because M4b's classifier
answers a different question and the noun is what keeps the two coherent. One word.

**F13. F4 as currently written does NOT address F10/S6 — confirmed against the uncommitted
change.** It scopes the tooltip by `isAdmin`, which is right for F4's complaint, but leaves a
bare `title` with no `aria-describedby`, so no keyboard or screen-reader user receives either
variant. **They share one line and need one edit**; `RunCredential.tsx:64-73` does both at
once. This is the second time web-ux has flagged the pairing — treat the third as a process
failure rather than a reminder.

### Amendment 5 — 2026-08-03, fact-check. One new fix, and three corrections to THIS FILE.

Every verdict pinned to a SHA; all probes run in `git archive` copies rather than the shared
worktree. It **measured** what the reviewer and auditor re-derived: mutating
`AgentDetail.test.tsx`'s `ApiError(409, …)` to `ApiError(500, "internal error")` in an
isolated copy gives **`Tests 5 passed (5)`, exit 0**, identical to baseline. F1's vacuous
test is now a measurement, not an argument. It also re-ran both of `bcd67c72`'s control folds
and reproduced them.

#### NEW FIX — add to the round in flight

**F11. `api/internal/agenttmpl/compare.go:33` names a test that does not exist.** Verified by
the lead: `TestBuiltinsFrontmatterIsUnpadded` appears **exactly once repo-wide — in the
comment that names it**. The invariant is really asserted inside `TestBuiltinsParseAndValid`
(`render_test.go:46`, block `:67-83`), which **the very next sentence of the same comment
names correctly**. So the comment cites a fictional mechanism and then the real one, and a
reader chasing the first concludes the guard was deleted. *Fix: delete the invented name,
keep the second sentence.* Note the surrounding claim is TRUE and `bcd67c72`'s commit message
gets the name right — the dangerous shape this file keeps meeting, where only the mechanism
is wrong.

#### CORRECTIONS TO THIS FILE

**C1. Claim 5's "no fixture can separate `scope` from `is_builtin`" is AMBIGUOUS, and false in
the reading that matters to a tester.** Under "fixture = a row Postgres can hold" it is true —
the CHECK at `00048:22` is total, both columns verified NOT NULL. But **M4a mandates no
live-DB test, so every fixture is a Go struct literal, and a literal is not subject to a
CHECK.** Probed with a control: on `{IsBuiltin: true, Scope: "user"}` the scope-keyed and
is_builtin-keyed implementations disagree, while agreeing on a constraint-obeying row. The
brief's own next words — *"do not spend time confirming it"* — address someone writing unit
fixtures, i.e. the false reading. **The conclusion still holds** (reading 1 governs
production). Reword to: *no fixture representing a row Postgres can hold can separate them; a
Go literal can, so a test that separates them is testing an unreachable state.*

**C2. The rebase section's MR/commit arithmetic** — corrected in place above.

**C3. `7784c037`'s "All eleven `.claude/agents/*.md` told their role to report to 'the team
lead'" is FALSE under the literal reading its own quotation marks invite.** Ten files carried
that string; `tester.md` used the hyphenated `team-lead`. The SUBSTANCE is true — !168's
description lists `team-lead` as a bare address among the four forms `recipient_test.go`
rejects — so all eleven did name an unreachable recipient, in two different strings. Immutable
in a commit message; recorded because !168 says "ten of the eleven" about its own roster and
two documents disagreeing about one defect is the contradiction-later problem.

#### RECORDED — mechanisms worth keeping

**N3. `SameContent` is exported, so its "the shipped side is always `BuiltinByName(row.Name)`"
comment is a claim about all future callers.** True today (one caller), but
`SameContent(Builtins()[i], templateToDefinition(row))` compiles and would silently compare
mismatched names. **The name-is-unfalsifiable argument is a property of the CALL SITE, not of
the function.** To make it a property of the function, unexport it or have it take the row's
name. Relevant to M4b, which adds the second caller.

**N4. Four line RANGES in this file start or end outside the symbol they name** — claim 2's
`:53-75` clips `templateDTO`; claim 4's `:112-124` starts on `decodeTools`'s closing brace;
`decodeTools` is `:102-112` not `:99-110`; `render.go:41` is the closing brace, the join is
`:40`. Each names a real symbol, so these are notes — flagged because **this is the shape the
nonexistent-file citation hid inside for three readings.** A range starting on the previous
function's `}` is the one to fix first.

**N5. `bcd67c72`'s control claim is stated in COLUMNS and reads as TEST NAMES.** "Exactly the
other three columns and the order case" is right per column; a reader re-running the fold sees
**five** failing top-level tests, because the description column reddens both
`TestSameContentDetectsEachMutableColumn/description` and `TestSameContentDoesNotTrim`.

**N6. `TestNoOpSaveCreatesNoDrift` SIMULATES the write path.** It calls
`validateTemplateFields` then hand-assembles the row `UpdateAgentTemplate` would write. Right
call under "no live-DB test", and the test's own comment is honest — but the commit message's
"a no-op editor resubmit" reads as end-to-end. It does cover all eleven builtins.

**Unverifiable, stated rather than left silent:** the "8 of 26 SendMessage calls" figure is a
run-trace measurement whose traces are not in the repo (the *attribution* to !168's
description is verified); `roles.yaml` is outside this repo; Amendment 2's `npm audit` figures
were not re-derived, deliberately, because that is a network call in a worktree three agents
were writing to.

### Amendment 4 — 2026-08-03, browser pass. ONE new blocking item and ONE conflict the lead resolved.

Mock build, real Chromium, read-only. All nine fixture cases verified **from the DOM rather
than from the source**: the five per-column drifts badge, and the four that must not
(`t-coder` pristine, `t-spec-keeper` no shipped twin, `t-release-notes` global, `t-my-coder`
user-scope collision) do not. Criterion 7 verified **at runtime**, not just at grep level: a
markup payload typed into a description renders escaped, `window.__pwn` null.

#### 🔴 CONFLICT BETWEEN TWO ACCEPTED FINDINGS — RESOLVED HERE, DO NOT RE-DECIDE

**F2 (Amendment 3) says assert `querySelectorAll("ins, del")` is empty as the XSS canary.
F8 below says the word-diff needs `<ins>`/`<del>` for accessibility. Adopting both destroys
the canary**, because its whole discriminating power is that `convertChangesToXML` emits
exactly those tags.

**RESOLUTION: use sr-only span markers for F8, NOT `ins`/`del` elements.** The canary keeps
its shape and WCAG 1.4.1 is satisfied. If anyone later has a strong reason to prefer the
semantic elements, F2's canary must be reshaped in the SAME commit and the reason recorded —
it must never be quietly dropped, because a canary deleted to make an unrelated change
compile is how this class of guard dies. Found by web-ux, which flagged the coordination need
rather than filing its half in isolation.

#### BLOCKING — added to Amendment 3's F1 and F2

**F5. The diff and the Reset button can NEVER be co-visible, so the milestone's own premise is
not met.** Measured at 1280×633 on `/agents/t-tester`: diff panel bottom **871px**, Reset
button top **1524px** — a **653px gap against a 633px viewport**, with the full-body
"Rendered subagent file (preview)" `<pre>` sitting between them. The mock body is 11 lines;
real builtins are **27-138** (`architect.md` 138, `tester.md` 137), so in production the gap
is several viewport heights. *(That extrapolation is from line counts, not a browser
measurement — web-ux flagged the distinction itself.)*

`reset()` (`AgentDetail.tsx:77-88`) has **no confirmation**: one click, unconditional, and it
discards unsaved form edits because the editor is keyed `key={template.updated_at}` and
remounts.

**Severity, since web-ux correctly left the call to the lead:** the unconfirmed Reset is
**pre-existing and not M4a's**. What M4a changed is that the justifying evidence now exists
and sits where it cannot inform the click. **Graded BLOCKING for M4a specifically**, because
"the diff view is what makes Reset safe to press" is this milestone's entire argument for
shipping ahead of M4b — see the top of this file. Shipping the artifact without the outcome
would leave that argument false.

*Fix (either):* a Reset control inside the "Differs from shipped" panel, or a confirm on the
existing button naming the changed columns. The second also closes the pre-existing gap and is
the smaller change.

#### SHOULD-FIX — from the browser pass

**F6. Badge and diff panel state contradictory things after an unsaved edit.** On
`t-documenter`, set Model back to "Inherit" without saving: the header badge still reads
"differs from shipped" while the panel reads "Matches the shipped definition". The panel
compares shipped vs **current form state** (`AgentTemplateEditor.tsx:241-250`); the badge
reflects the **stored** DTO. Nothing on screen is false — both sentences are true of
different subjects — but the panel is ~370px below the badge, so the admin reads "Matches"
with no contradicting signal in view and navigates away leaving the row drifted. **This is
the LIVE instance of the contradiction Amendment 2 records as latent**; that one needs padded
frontmatter no revision has, this one needs one select change.

**F7. `LineDiff` renders byte-identical text as both removed and added on a trailing-newline
mismatch**, with no whitespace indicator. Reproduced on the pristine control by appending one
`\n`: two rows, identical text, one green one red. jsdiff's `diffLines` keeps the newline in
the token. Reachable on real data — `prompt_body` is submitted verbatim and a textarea adds
no trailing newline, so an admin who retypes the last line gets a permanent unexplained
"changed" line. *Fix:* normalize both sides to exactly one trailing `\n` before `diffLines`
(a terminator, not content, so it hides no real edit), or emit git's
`\ No newline at end of file` marker.

**F7b (FIXTURE, trivial, do it with F7).** `builtinBody` (`web/src/mocks/data.ts:1534-1535`)
produces a body with **no trailing newline**; every real builtin `.md` ends with one. So F7's
phantom row is the FIRST thing in the diff on `t-tester`, the demo's flagship case — **the
mock makes the diff look broken in a way real data would not.** Append `\n`.

**F8. The tools-ORDER diff reads as "Bash removed, Bash added".** `t-reviewer` renders
`- Bash` / `Read` / `+ Bash` / rest. Correct as a sequence diff, unreadable as an answer to
"what changed" — and this is the one case the brief says nothing else pins. *Fix:* use the
two-line before/after shape the MODEL diff already uses, which web-ux rates by far the most
legible of the four.

**F9. The description word-diff carries meaning in COLOUR ALONE** (WCAG 1.4.1): five spans,
no `+`/`-` prefix, no `title`, no sr-only text, differing only by `text-ok` vs `text-danger`.
`LineDiff` and `ToolsDiff` both carry prefixes, so it is an internal inconsistency too.
Contrast itself is fine and needs no work (green 8.09:1, red 6.13:1, muted 7.33:1 against
`bg-ink`). **Fix with sr-only markers per the conflict resolution above.**

**F10. Both drift badges are `title`-only** — `aria-describedby`, `tabindex` and `role` all
null. The list badge's title is the **only** place "Open it to see the diff" exists, so it
reaches mouse users only. In-repo precedent is `web/src/components/RunCredential.tsx:64-73`
(`title` + `aria-describedby` → sr-only span), and `Badge` already accepts
`aria-describedby` (`ui.tsx:240`). **Fix together with F4** (Amendment 3), which changes that
same copy. Web-ux scoped this honestly: the adjacent `shadowed` badge has the identical gap,
so it is page-wide, not a regression this change introduced — fix both.

#### ENHANCEMENTS — relayed to the owner, NOT scheduled

E1 a count + filter on the list ("10 of 11 builtin templates differ from shipped"), which
turns #210 recovery from a scan into a worklist. E2 deep-link the badge to `/agents/:id#diff`,
which also reduces F5's distance problem. E3 fall back from `diffWords` on a wholesale
rewrite, which currently renders as interleaved soup. E4 name the diff region
(`role="region"` + `aria-labelledby`, `aria-live="polite"`). **E5 extract
`InlineDiff`/`LineDiff`/`ToolsDiff`/`DiffField` into `components/Diff.tsx`** — M4b surfaces
the same shipped-vs-stored question, and §5's argument about a duplicated *comparison* covers
the *renderer* equally; it also gives F2's widened test somewhere to mount all four.

#### Scope web-ux stated rather than let be inferred

It filed **no** finding about whether the comparison is CORRECT: `mockApi.sameContent` is a
deliberate second implementation, so its agreement with the fixtures proves nothing about the
server. That is the tester's matrix. F7b is explicitly a finding about the fixture, not the
renderer.

### Amendment 3 — 2026-08-03, THE FIX LIST. Reviewer + auditor both reported; this is what the coder does.

Round 1 of findings. The severity bar is NOT armed: this round's deliverable is executable
content and it moved, so the raise-the-bar trigger does not apply.

**Reviewer's verdict on the api half: the strongest work it has reviewed on this repo.** Every
case in `compare_test.go` is falsified by a specific production edit (drop a column, use
`reflect.DeepEqual`, sort the tools, add a trim, switch to a `Render` byte-compare);
`TestNoOpSaveCreatesNoDrift` runs the real `validateTemplateFields` across all eleven builtins
behind a `t.Fatalf` precondition; `TestWriteBuiltinDefinitionStatusMatrix`'s 404 case gates the
existence-oracle property and fails if the `!t.IsBuiltin` check is ever moved above authz.
Gates verified green with a real positive control — every new test named, `--- PASS`, zero
`--- SKIP`. **The defect is one `catch` clause in the web half.**

#### BLOCKING — do these two

**F1. Discriminate the error status in `AgentDetail.tsx`.** Found INDEPENDENTLY by the
auditor (A1) and the reviewer (B1); the lead verified it. Details in Amendment 2 §A1. The
reviewer adds two things that sharpen it:

- **It is a REGRESSION IN REACH, not just a swallowed error.** On `origin/main`,
  `AgentDetail.tsx:123` offered Reset for every `is_builtin` row, so no transient failure
  could take the button away. Now one can.
- **The comment at `:43-45` was ACCURATE when written and went stale inside the same commit.**
  It was true while `builtin` only fed the diff panel; it stopped being true the moment
  `!builtin` started driving the Reset copy at `:159`.
- The 403 half of that comment is **fine and needs no change** — `canEdit` (`:108`) is the
  exact mirror of `authorizeTemplateWrite` (`agent_templates.go:146-164`), so the Reset card
  only renders for callers the endpoint would not 403. The two paths cannot coincide.

Fix: `err instanceof ApiError && err.status === 409` (and 403) means "no shipped side".
Anything else leaves the Reset card at its pre-change behaviour. **Add a test asserting a 500
leaves the button present** — without it the fix has the same hole as the defect.

**F2. Make the XSS control discriminate, and widen its fixture.** Details in Amendment 2 §A3.
Two assertions and a fixture change: add `expect(container.querySelectorAll("ins, del"))
.toHaveLength(0)` (catches `convertChangesToXML` by output shape, which escaping cannot hide),
and widen the fixture so all four columns differ with markup in `description` and in a tool
name, so `InlineDiff`, the model span and `ToolsDiff` mount rather than `LineDiff` alone.
Also correct the comment's framing: the grep is STRICTLY STRONGER than this test, not the
other way round.

> **🔴 SHARPENED BY MEASUREMENT — the `ins, del` assertion is not a nice-to-have, it is the
> ONLY thing that will discriminate after F9.** The fact-checker mutated `LineDiff`'s call
> site three ways. The first two reddened and **both reds were on
> `getAllByText(/onerror/, {selector: "span"})`** — the test noticing the DOM SHAPE changed,
> not unsafe HTML appearing. The third, which preserves `LineDiff`'s own per-row `<span>`
> structure and pipes the body through `convertChangesToXML`, is **GREEN 5/5** against an
> unsafe implementation. **`container.querySelector("img")` — the assertion that looks like
> it encodes the security property — is null and passes under ALL THREE unsafe forms. It
> never discriminates.**
>
> **This lands directly on my own conflict resolution.** The accidental protection came from
> `selector: "span"` breaking; **F9's sr-only markers preserve spans by design**, so that
> accident disappears and every existing assertion goes green on an unsafe renderer. The
> resolution stays — sr-only over `ins`/`del` is still right — but it makes F2's new assertion
> load-bearing rather than belt-and-braces. **Do not land F9 before F2, and do not let F2's
> `ins, del` assertion be dropped later to make a refactor compile.**
>
> Two more from the same run. The overclaiming artifact is the TEST COMMENT
> (`AgentDetail.test.tsx:151-152`, *"the tag below would become a real element"*) — false for
> `convertChangesToXML`, which escapes. `10970920`'s commit message says only that such a
> library *"would introduce the first `dangerouslySetInnerHTML`"*, which is true and a
> different claim; fix the comment, not the message. And the fact-checker's own third run
> **first reported `5 passed` from a run where the mutation had not landed** — the pattern was
> absent because `AgentTemplateEditor.tsx:466` is `{r.text || " "}` with a **non-breaking
> space**, indistinguishable from ASCII in a terminal. Only a visible traceback caught it.
> Its successful run carries an assert-it-landed control (`grep -c` = 2 mutated, 1 restored),
> which is the shape any fold here needs.

#### SHOULD-FIX — cheap, and each has an in-tree pattern to copy

**F3. Give the tools-order case a non-vacuity guard.** `agent_templates_drift_test.go:87-91`
swaps tools[0] and tools[1]; if those were ever equal the swap is a no-op and the case proves
nothing silently. Its own sibling `TestDiffersFromBuiltinPostgresCanonicalTools:183-185`
already carries exactly this guard as a `t.Fatalf`. Three lines.

**F4. Fix the Agents-list tooltip.** `Agents.tsx:210` says "Open it to see the diff", and the
badge renders for every authenticated viewer — but a non-admin gets `ReadOnlyView`
(`AgentDetail.tsx:190`), which has no diff panel, and `/{id}/builtin` 403s anyway. The badge
is honest; the sentence beside it is not. Scope the copy by `is_admin` or drop the second
sentence.

#### RECORDED, NOT FOR M4a

**R1. The drift predicate now has THREE implementations and nothing pins their agreement** —
`agenttmpl.SameContent`, `mockApi.ts`'s `sameContent`, and `AgentTemplateEditor`'s
`changedFields`. `compare.go`'s own doc comment names this as the hazard it exists to prevent.
The reviewer **tried to construct a divergence and could not**, which is why this is not
blocking: all three fold null/`""` and null/`[]` identically, compare tools order-sensitively,
and never trim. The one asymmetry (`changedFields` trims `model`) is unreachable, held shut by
`ValidateModel` rejecting whitespace AND the new `TestBuiltinsParseAndValid` assertion —
**two independent guards, neither of which mentions this predicate.**

**This is a PREREQUISITE FOR M4b, and the repo already has the pattern:** `fixtures/run-usage/`
is read by both `api/internal/workersvc/run_usage_contract_test.go` and
`web/src/lib/runUsageContract.test.ts`, and CLAUDE.md cites it by name. A shared JSON case
table of (shipped, stored, expected) fed to all three would pin them to one artifact. **M4b
adds a fourth consumer — the reconciler's classifier — which is exactly when a divergence
stops being cosmetic and starts overwriting rows.** Do it there, not here.

**R2. `templateDTO` decodes the tools jsonb twice per row** (`:76`, and again via
`differsFromBuiltin` → `templateToDefinition`). Harmless at this scale; if R1's shared-fixture
work ever refactors this, single-decode is the form to land on.

**R3. The mock's `storedBuiltin` aliases the shipped tools array** (`tools: def.tools` copies
the reference). Nothing mutates in place today and the per-column fixture assertion would
catch it — filed because a shared-baseline fixture whose two sides can move together is the
exact failure the separate-constant design exists to remove.

**R4. THE `dangerouslySetInnerHTML` COUNT: every number in circulation was correct, in a unit
nobody stated.** Measured by the reviewer across four refs, both units:

```
ref                        files   occurrences
25ebcd39 (branch base)        10            11
541c2c0b (scope base)         10            11
10970920 (scope tip)          12            13
b0dc8dad (committed tip)      12            13
```

The brief's original "11 hits" was an **occurrence** count; the coder's "now 12" is a **file**
count. Both true. The sentence comparing them crosses units, and because 11 and 12 are
adjacent it reads as "+1" when the change actually added **two files and two occurrences** —
`RunEvent.tsx` carrying two is the whole discrepancy. The reviewer's own N1 ("13, off by one
from 12") made the same error in the opposite direction, comparing its occurrence count to a
file count *in a finding about unit confusion*, and retracted it.

**Criterion 7 is unaffected by all of this, because the property is ZERO CALL SITES, not any
count.** Confirmed independently, and the first instrument used was itself unsound: a negated
bracket expression under `grep -E` on a host where `grep` is ugrep is the documented defect,
so it was calibrated against a two-line fixture holding one real call site and one comment
before being trusted. **Report the property; state the unit whenever a count appears at all.**

### Amendment 2 — 2026-08-03, audit findings at `c3704d25` (ACCEPTED, fix not yet dispatched)

Auditor verdict: **no security regression**, both design-wave blockers landed correctly. The
new route is gated *more* tightly than the data it describes — shipped bodies now reach
admins only, while the stored bodies of the same rows have always been readable by every
authenticated user, so this is a net reduction in exposure. `npm audit` base-vs-head shows
`diff@9.0.0` adds **zero** advisories, with the prod-dep count moving 112→113 as the positive
control that the two runs read different inputs.

**A1 (Medium) — BLOCKING. `AgentDetail`'s catch cannot tell "no shipped definition" from
"the request failed", and prints a false sentence when it cannot.** Verified by the lead.
`web/src/pages/AgentDetail.tsx:42` is a **parameterless** `catch {`, so every rejection maps
to `builtin = null`, and `!builtin` at `:163` is the sole input to copy reading *"This release
no longer ships a definition for `<name>`"* — plus removal of the Reset button. A 500, a 502
or a dropped connection therefore makes the page assert something false, silently, on exactly
the recovery path this milestone exists to enable: an admin working through #210's ten
templates is told the definitions do not exist.

The comment at `:43-45` shows the conflation of 409 and 403 was deliberate. What is not
deliberate is that it also swallows every transient failure into the same copy.

**And the test that looks like it pins this does not.** `AgentDetail.test.tsx:172-191` rejects
with `ApiError(409, "no builtin definition to reset to")` and asserts the copy — but because
the catch takes no parameter, **it passes identically with `ApiError(500)` or a bare
`Error`.** The `409` is decorative: nothing observes it. Same class as a fixture whose
discriminating value the code cannot reach.

*Fix direction:* bind the error; treat only `ApiError` 409 and 403 as "no shipped side";
let anything else leave `builtin` null WITHOUT switching the copy — better, keep Reset offered
with a muted "couldn't load the shipped definition" note. Then flipping the fixture's 409 to a
500 reddens, which is the point.

**A2 (Low) — `Builtins()`' doc comment is false as written.** Verified by the lead.
`api/internal/agenttmpl/builtins.go:49-50` says *"The returned slice is a copy so callers
cannot mutate the package state."* `copy(out, builtins)` is **shallow**, and `Definition.Tools`
is a `[]string`, so every returned struct's `Tools` aliases the package's backing array.
Pre-existing and not live — the only write to a `Definition.Tools` in `api/` is inside
`parse()` on a fresh value — but M4a's response DTO assigns `Tools: def.Tools`, so the alias
now escapes the package through one more path. Fix the comment, or deep-copy `Tools`.

**Accepted non-blocking, promoted because each names a MECHANISM rather than a preference:**

- **Every non-admin viewing any builtin detail page issues a request guaranteed to 403.** The
  condition is `template.is_builtin`, not `canEdit && template.is_builtin`. Cost is a wasted
  round-trip and a steady 403 stream in the access log — and routine authz denials are what a
  real one hides in.
- **The editor trims `model` but the server's `SameContent` trims neither field.** If a stored
  builtin ever carried a padded `model`, the badge would read "differs from shipped" while the
  panel below it read "Matches the shipped definition" — two contradictory sentences on one
  page. Closed today only by the new `render_test.go` `TrimSpace` assertions; the auditor
  walked 202 sha/file pairs with a positive control to confirm no reachable revision has
  padded frontmatter. Recorded because that assertion looks cosmetic and is the only thing
  holding this shut.

**Noted, no action:** `react-router-dom` is a floating `^6.28.0` on a prod dep with a live
moderate advisory (pre-existing, not M4a's); `ToolsDiff`'s `as string[]` is a type assertion
over a library type, bounded by the exact pin; the 409 body is worded for a write
("no builtin definition to reset to") on a GET.

**Evidence kind, as the auditor stated it:** A1 and A2 are re-derivations from source, not
executions. It deliberately did not run the mutation, because reviewer, tester and web-ux are
all mid-flight in this shared worktree and a `cp`-restore round would silently invalidate
their runs. That was the correct call.

**A3 (Medium) — BLOCKING. The XSS positive control passes, fully green, against the exact
unsafe implementation its own comment names.** Verified by the lead against the pinned,
installed artifact.

`AgentDetail.test.tsx:149-154` says a diff library returning an HTML string — naming
`diff2html` and **jsdiff's `convertChangesToXML`** — would make the payload "a real element".
That is false for the second one. In `web/node_modules/diff@9.0.0/dist/diff.js`,
`convertChangesToXML` (`:2251`) pushes `escapeHTML(change.value)` (`:2261`), and `escapeHTML`
(`:2271-2278`) replaces `<` and `>`. The payload dies before it can become an element, so
BOTH assertions still pass against an unsafe `dangerouslySetInnerHTML` built on it:
`querySelector("img")` is null because escaping meant no element was ever created, and the
`onerror` text is present because a `<span dangerouslySetInnerHTML>` carries it in
`textContent`.

**Be precise about what it does catch**, because it is not worthless: against unescaped
concatenation (`__html: \`<span>${p.value}</span>\``) a real `<img>` appears and the first
assertion fails. That is the more dangerous form. The defect is that the comment claims both
and delivers one.

**Second, structural narrowing.** The fixture differs only in `prompt_body` (`row()` is the
pristine baseline, proved by the sibling test asserting "Matches the shipped definition"), so
`changedFields` is `["prompt body"]` and **`LineDiff` is the only renderer mounted**.
`InlineDiff` (description, `diffWords`), the model span and `ToolsDiff` (`diffArrays`) never
render, and no other test feeds markup anywhere. **1 of 4 renderers covered**, with
`description` admin-editable on exactly the same footing as `prompt_body`.

*Fix:* assert `container.querySelectorAll("ins, del")` is empty — that catches
`convertChangesToXML` by its output shape, which escaping cannot hide — and widen the fixture
so all four columns differ, with markup in `description` and in a tool name. Also correct the
comment's framing: it calls itself "the positive control behind criterion 7's grep", but the
grep is **strictly stronger** (both forms, every renderer). This test is a narrower
complement, and stated backwards it will let a future reader widen the grep's exceptions
believing the test still covers them.

*Live risk today is nil* — the shipped renderer uses structured `Change[]` into text nodes and
`web/src` has zero call sites. This is a finding about the GUARD, whose whole job is the
future regression.

**LEAD CORRECTION — "the merge altered no M4a file" was FALSE and was relayed to three
validators.** `git diff --name-only 10970920..c3704d25` includes ten
`api/internal/agenttmpl/builtins/*.md` bodies, `recipient_test.go`, and
`web/src/mocks/mockApi.ts`. The lead verified five hand-picked paths and generalised from
them. The builtin bodies are **inputs to the drift comparison**, so this was not a harmless
over-statement. Retracted to the reviewer and the tester directly; the auditor caught it.
The conclusion it supported still holds — no M4a *behaviour* moved, and `10970920` remains an
ancestor — which is exactly why the sentence was dangerous rather than obviously wrong.

**Corpus delta, measured by the auditor with a non-zero byte control:** 10 files, 139 body
lines, **zero frontmatter lines**, so the whitespace-hygiene invariant is untouched.
**Consequence worth stating before web-ux and the CHANGELOG land: on an already-seeded
install, 10 of 11 builtins report `differs_from_builtin: true` immediately on deploy.** That
is the #210 recovery path working as designed, not a defect — but the badge firing on nearly
the whole roster at once will surprise someone.

### Amendment 1 — 2026-08-03, after the design-critique wave

Architect, reviewer and auditor reported independently; the lead re-derived the load-bearing
citations. Claims 3, 4 and 5 above are rewritten in place. What follows is what the wave
added that is not a claim correction.

**§1 — The diff had no data source, and Scope now authorizes one.** All three derived this
independently by different enumerations: a boolean says *that* a row differs, and nothing in
the API serves the shipped text. `BuiltinByName` has one non-test caller
(`agent_templates.go:412`, inside `ResetAgentTemplate`), so today the shipped body reaches a
client only AFTER Reset has already overwritten the row — the destructive act the diff exists
to prevent. **Owner decision: a new route, not a nested DTO field.** The three proposals were
a route (architect), shipped fields on the detail response only (auditor), and a nullable
`builtin` object on the DTO (reviewer). The route wins on two grounds the others each fail
one of: it matches the existing `/{id}/rendered` sub-resource precedent, and it keeps the
~44 KB shipped corpus out of the LIST response, which the DTO-field form would carry on
every row.

*Staleness, scored per variant, because "refuted" does NOT cover all three and a later
revisit would misread it that way.* The objection is that the shipped copy goes stale after a
save unless the client refetches. It is **refuted for the reviewer's nested-DTO form**:
`templateDTO` is shared by update (`:367`) and reset (`:438`), so a nested field refreshes
itself — the reviewer's NB4, resting on the architect's own five-call-site enumeration. It
**stands for the auditor's detail-response-only form**, where the shipped body rides outside
the DTO and is therefore absent from every write response. The lead relayed this objection as
the architect's and against nesting; it was the architect's against the detail-only variant,
and the lead's own against nesting. **The decision is unaffected either way** — the route
wins on payload and precedent.

**§2 — jsdiff, rendered as React elements. Owner decision.** `web/package.json` carries no
diff library. **The auditor's constraint is binding: most JS diff libraries return HTML
strings by default** (`diff2html`, jsdiff's own `convertChangesToXML`), and using one would
introduce the FIRST `dangerouslySetInnerHTML` in `web/src`, which today has zero call sites
and eleven comments saying not to. Use structured hunks rendered as React text nodes.
**Install with `npm install --ignore-scripts`** — per CLAUDE.md a plain install in this repo
rewrites `/opt/homebrew/bin/agent-browser` host-wide, and npm 11.17 prints an `allow-scripts`
warning that reads as though it skipped them when it did not.

**§3 — Badge copy is "differs from shipped".** M4a answers *"does this row differ from the
body shipped in THIS binary?"*. M4b's classifier answers a different question: *"does this
row differ from the body it was SEEDED with?"*. A row that is pristine-as-seeded but behind
the current release answers **true** to the first and **pristine** to the second — M4b will
auto-update it and the badge will vanish. Coherent only if the badge never claims a human
edited it. Getting this wrong makes M4b's fix a copy change, and CLAUDE.md documents at
length what a copy change does to the negative assertions guarding it.

**§4 — The `description` trim asymmetry: fix at the SOURCE, do not normalize.** The write
path trims (`validateTemplateFields:513`) and the builtin parser does not
(`builtins.go`, `strings.Cut(line, ": ")`), so a builtin `.md` with trailing whitespace on a
frontmatter line would seed a row that flips to a trimmed value on the first no-op save and
report drift with nothing changed. **Latent, not live** — all three agents scanned the 11
builtins and found no padded frontmatter value. The wave split on the fix: the reviewer said
trim both sides; the architect and auditor said never trim. **Decision: never trim.** Add the
whitespace-hygiene assertion to `TestBuiltinsParseAndValid`
(`api/internal/agenttmpl/render_test.go:38-60`), which already validates name/description/
body. That makes it a corpus invariant instead of a permanent comparison blind spot, and
criterion 8's no-op-save test catches it either way.

**§5 — Land the canonical comparison in `agenttmpl`, not in `handler`.** Something of the
shape `func SameContent(a, b Definition) bool`. `Definition` holds a `[]string` so it is not
`==`-comparable and a helper is needed regardless; the only question is which package owns
it. `agenttmpl` has no DB dependency and BOTH `handler` and `store` already import it.
**This is the highest-value decision in the milestone**: M4b's classifier answers the same
question over the same columns, and if M4a writes ad-hoc comparisons inside `handler`, M4b
must reimplement in `store` or refactor under time pressure. The moment there are two, the
badge and the auto-updater can disagree — the UI says customized while the reconciler
overwrites it. **Do not add drift-specific behaviour to `templateToDefinition`**: it is on
the `/rendered` export path that writes into an agent workspace.

**§6 — Make `differs_from_builtin` NON-optional in the TS type.** `AgentTemplate`
(`web/src/lib/api.ts:105-118`) has no optional fields today. Declared `differs_from_builtin:
boolean`, `task typecheck:web` goes red at every literal until each is updated
(`mocks/data.ts` `tmpl()` factory, `mockApi.ts` create literal, two sites in
`Agents.test.tsx`). Declared with a `?`, the mocks stay silent and the badge never renders in
mock mode — the structural blindness the roster line warns about, arriving through one
character.

**§7 — The mock is a SECOND IMPLEMENTATION of the comparison, and one drifted fixture is not
enough.** `mockApi.ts:212` clones `mockTemplates` into the mutable `templates`, and
`resetAgentTemplate` already treats `mockTemplates` as the shipped baseline — a working seam,
so the mock CAN compute drift honestly rather than hardcoding a flag. But on a fresh load
every row is a pristine clone, so **nothing carries the badge until someone edits**, and
editing `mockTemplates` does nothing because it is both sides of the comparison. Seed a
drifted row against a baseline constant separate from `mockTemplates`, or have the initial
clone apply one edit. Fixture cases that actually discriminate: `prompt_body`-only,
`description`-only, `model`-only (shipped null vs stored set), `tools` membership, **`tools`
ORDER only** (nothing else pins "do not sort"), a pristine control, a `global` row, a
**`user`-scope row colliding with a builtin name** (missing today), and a **builtin with no
shipped twin** (missing today). Add a test asserting the fixture set still contains each, or
a later fixture edit silently removes a case and everything stays green. A golden snapshotted
from the mock data is the trap here: it agrees on everything it covers and locks in the blind
spot.

**§8 — Discharged.** CLI: **no change**, verified independently by all three
(`git grep -i -F 'agent-template' -- api/cmd/uzi/` is empty; the command set is
`root.go:141-155` and `admin.go` exposes users/runs/workers/usage/rate-limits/cli-tokens).
State it in the MR description the way PRD #85 Decision 8 did. Two other template-shaped
serializers exist and must NOT gain the field: `templateAllocationDTO`
(`template_allocations.go:22-31`, no content columns; `Agents.tsx` renders rows from the
templates list, so the badge reaches that page through `templateDTO`) and `ClaimAgent`
(`workersvc/claim.go:191-203`, the worker wire contract, pinned by a golden — adding drift
there leaks an admin-UI concept into a contract the worker parses).

**§9 — Boot path re-confirmed.** `api/cmd/server/main.go:159` is `ReconcileBuiltinTemplates`;
the server is constructed at `:633` and `ListenAndServe` is at `:684`, so pre-listen ordering
holds. Hard singleton confirmed in the chart (`api-deployment.yaml:12-16` `type: Recreate`,
`values.yaml:62-63` `replicaCount: 1`).
