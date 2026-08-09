# PRD #264: uzi CLI run-watching & agent-driving ergonomics (`uzi run wait`)

**GitLab Issue**: [#264](https://gitlab.example.com/vtmocanu/uzi/-/issues/264)
**Status**: Draft (created 2026-08-09)
**Priority**: Medium

## Problem

Driving a **gated** uzi run headless is a multi-step loop: `run create` → wait for the
plan gate → review → `run approve` → wait for the MR. The CLI has no primitive for the
"wait" half. `uzi run logs --follow` returns **only** on the terminal states
(`completed`/`failed`/`cancelled`), so it cannot be used to wait for a plan gate
(`awaiting_approval`) or a clarification park (`awaiting_input`) — the skill itself tells
you to "poll `uzi run get` status" there.

So every session hand-rolls a `for … uzi run get --json | jq .status; sleep` loop. This
PRD was written after a session that wrote that loop **four times**, and two of those
instances silently went blind. The blindness is instructive and is the second finding
here: the loops piped the CLI's JSON through **zsh `echo`**, and zsh's `echo` interprets
`\uXXXX` escapes, so it turned the CLI's *correctly* `\u00XX`-escaped control characters
(agent output can contain raw control bytes) back into raw bytes, producing invalid JSON
that broke `jq` — which then returned an empty status forever. **The CLI's `--json` output
is valid; the footgun is the shell pipeline**, and the fix that mattered was switching to
`printf '%s'`.

Two smaller gaps compounded the friction, both measured against the shipped v0.21 surface:

- `uzi review backlog` **has** a `--category` filter (`improve_uzi`, `install_worker_tool`,
  …) but the **skill reference omits it**, so an agent triaging one category pulls the
  entire backlog and filters locally.
- `uzi run approve --json` returns an undocumented envelope (`{"server_side": false}`), the
  run stays `awaiting_approval` for a beat **after** a successful approve (async flip to
  `running`), and a **second** approve is a benign no-op (exit 0, not the exit-5 conflict
  the exit-code table implies). All three read as failures to a first-time caller.

## Goal

Make headless run-driving a first-class, low-footgun path: one command to wait for a state,
one cheap way to read a scalar, and a skill/doc surface that matches the binary.

## Non-goals

- No change to run semantics, the state machine, or the approval model.
- No server-side long-poll/streaming endpoint. `run wait` polls the existing
  `GET /api/runs/:id`; keeping it client-side means zero API surface and no new failure mode.
- No auto-merge or forge interaction. CI status and merge stay a forge-CLI concern; the run
  object already carries `mr_web_url` / `mr_state` / `pipeline_web_url` for the caller to use.

## Decision Log

- **D1 — `run wait` is client-side polling, not a server long-poll.** It reuses
  `GET /api/runs/:id` at a bounded interval. No new API, no new auth surface, and it composes
  with `UZI_URL`/`UZI_TOKEN` exactly like the other verbs.
- **D2 — default `--until` is the "actionable" set.** With no flag, `run wait` stops on any
  state that needs the caller or ends the run: `awaiting_approval`, `awaiting_input`,
  `completed`, `failed`, `cancelled`. It does **not** stop on `queued`/`claimed`/`running`
  (still working) or `limit_wait` (auto-resumes; parking on it is legitimate). `--until
  <comma-list>` overrides the set (validated against the nine-status enum). This makes the
  common case — "wait for the plan gate OR the end" — a bare `uzi run wait <id>`.
  **Corollary (narrow after acting on a stop-state).** The default set includes
  `awaiting_approval`, and a run lingers at `awaiting_approval` for a beat *after* a
  successful `run approve` (the async flip to `running`, see Problem). So the second wait in a
  gated loop must narrow: after approving, call `run wait <id> --until
  completed,failed,cancelled`, not a bare `run wait`, or it returns immediately at the gate it
  just cleared. Documented as a usage rule (M3); M4's acceptance encodes it.
- **D3 — exit codes reuse the documented contract and add exactly one.** `0` = a target
  state was reached (including if the run was already in one at call time — return
  immediately); `4` = run not found; `6` = server unreachable; **new `7` = `--timeout`
  elapsed before any target state** (chosen because it is the one outcome none of the
  existing 0-6 codes expresses). The skill's exit-code table gains the row.
- **D4 — `--json` prints the final run object; transitions go to stderr as human lines.**
  On success `run wait --json` emits the same top-level run object as `run get --json` (so a
  caller reads `.status`/`.mr_web_url` from one document); progress transitions print to
  **stderr** so they never pollute the JSON on stdout. An NDJSON transition stream is
  explicitly deferred (D8).
- **D5 — a scalar projection so pollers never parse the whole object.** Add `uzi run get
  --field <name>` (repeatable), printing the named top-level scalar(s) one per line, raw and
  unquoted. This removes the need to JSON-parse at all for the status case — the exact
  situation that produced the zsh-`echo` footgun — and it keeps the payload tiny. `--field`
  is refused together with `--json` (two output modes). A **non-scalar** field is a usage
  error (exit 2): `RunDTO` carries four array fields (`milestones`, `milestones_candidate`,
  `milestones_completed`, `milestones_in_progress`), which have no meaningful one-line raw
  form. Implement the projection by marshaling the concrete `RunDTO` to
  `map[string]json.RawMessage`: the key set *is* the field enum (self-maintaining, no
  restated list), `null` handling falls out for free, and a `RawMessage` whose first byte is
  `[` or `{` is the non-scalar case to reject.
- **D6 — no default timeout.** A plan gate is *itself* a default stop state (D2), so a bare
  `run wait` cannot hang on a healthy gated run; it returns at the gate. `--timeout` is opt-in
  for callers that want a ceiling (e.g. CI).
- **D7 — the skill/doc corrections ship in this PRD, not a follow-up.** They are the
  cheapest, highest-traffic half of the fix and they describe the very commands this PRD adds.
  Scope: document `review backlog --category`; document `run approve`'s `--json` return, the
  post-approve async flip, and benign re-approve; document the `--json`-through-`echo` footgun
  (use `printf '%s'`, a file, or `--field`); repoint the "poll `run get`" guidance at
  `run wait`.
- **D9 — polling survives a transient blip.** A single mid-wait `ExitUnreachable` (5xx /
  network, which the skill itself calls "transient; back off and retry") must **not** end the
  wait, or a multi-hour gate-wait dies on the first blink and reintroduces the fragility this
  PRD removes. `run wait` retries with bounded backoff and surfaces exit 6 only after N
  consecutive failures (default N small, e.g. 5). A `4` (not found) is immediate, not retried.
- **D8 — deferred:** an NDJSON `--stream` transition mode on `run wait`, and a `--field` with
  nested dot-paths. Both are additive later if a consumer needs them; neither blocks this PRD.

## Milestones

**Milestone-cut note (skill-drift gate).** `TestSkillMatchesCommandTree`
(`api/cmd/uzi/skill_drift_test.go`) asserts SKILL.md ↔ command-tree parity **both ways**: a
runnable non-hidden command SKILL.md does not document fails the test, and a `--flag` the doc
names that does not exist in the tree fails it too. So each new verb's SKILL.md line must land
in the **same** milestone as the verb (M1, M2), not deferred to a docs milestone; M3 is only
the corrections that are independent of the new code.

- [x] **M1 — `uzi run wait <run-id>` (+ its SKILL.md/`docs/cli.md` lines).** Blocks until the
  run reaches a `--until` state (default per D2), polling `GET /api/runs/:id` at `--interval`
  (sane default, e.g. 3s), honoring `--timeout` (D6) and the transient-blip resilience (D9),
  printing transitions to stderr and, under `--json`, the final run object to stdout (D4).
  Exit codes per D3. Unknown/unrecognized status is surfaced and treated as non-terminal
  (never a silent forever-wait). Ships the `run wait` command-reference line in SKILL.md and
  `docs/cli.md` in this same milestone (drift gate). Unit-tested against a fake API that
  scripts a status *sequence* — which requires extending `FakeClient.GetRun`
  (`api/internal/uzicli/fake.go`) with a per-id status-queue / hook seam, since it currently
  returns a static run — covering: already-in-target (immediate return), gate then terminal,
  timeout (exit 7), not-found (exit 4), and a transient exit-6 that recovers (D9).
- [x] **M2 — `uzi run get --field <name>` scalar projection (+ its doc line).** Repeatable;
  prints the named top-level scalar(s) raw, one per line; mutually exclusive with `--json`;
  non-scalar field is a usage error (D5). Ships its SKILL.md/`docs/cli.md` line in this
  milestone (drift gate). Unit tests cover a present field, a null field (empty line), an
  unknown field (usage error, exit 2), a non-scalar `milestones` field (exit 2), and `--field`
  + `--json` rejected.
- [x] **M3 — code-independent skill + doc corrections (D7).** Edit the embedded skill source
  (`api/internal/uzicli/skill/SKILL.md`) and `docs/cli.md` for the parts that do **not** add
  code: add `review backlog --category`; document `run approve`'s `--json` return, the
  post-approve async flip, and benign re-approve; add the `--json`/shell-`echo` robustness
  note; repoint the "poll `run get`" guidance at `run wait`; add the new exit-code `7` row to
  both the SKILL.md table and `docs/cli.md`'s exit-code section. Also correct the stale
  `run.go` comment that still says the status check constrains **eight** values (it is nine
  since migration `00092`), since M1/M2 edit that file anyway. `web/scripts/check-docs.mjs`
  and `TestSkillMatchesCommandTree` stay green.
- [x] **M4 — Acceptance: drive a real gated run end-to-end with the new verbs.** On a live
  instance, `run create` (gated) → `run wait <id>` returns at `awaiting_approval` → `run
  approve` → `run wait <id> --until completed,failed,cancelled` (narrowed per D2's corollary,
  *not* a bare wait) returns at `completed`, with the MR url readable via `run get --field
  mr_web_url`. Capture the transcript as evidence. No hand-rolled poll loop anywhere.
  - _Verified live 2026-08-09 on server `0.23.0+g818d164d`:_ `uzi run wait` on run
    `121a6640` (#267) blocked while the run was `running` and returned at `completed` (stderr
    streamed `running → completed`), driven with `--until completed,failed,cancelled`; the
    final run object came back on stdout, `run get --field mr_web_url` printed MR `!229` raw,
    and `--field` + `--json` was rejected with exit 2. Driven entirely with the new verbs, no
    hand-rolled poll loop.

## Success Criteria

1. A bare `uzi run wait <id>` returns exactly once the run hits the plan gate or ends, with a
   correct exit code, and needs no `jq`, no `sleep`, and no shell parsing.
2. Reading a run's status/MR needs no whole-object JSON parse (`run get --field` suffices),
   removing the shell-`echo` footgun class entirely for the common case.
3. The shipped skill reference matches the binary: `backlog --category`, `run approve`
   semantics, the `--json` robustness note, and the two new verbs are all documented.
4. `task gate:api` green; the new commands are unit-tested; `check-docs.mjs` green.

## Technical scope (pointers, not a plan)

- CLI verbs live in `api/cmd/uzi/` with client plumbing in `api/internal/uzicli/`; the
  existing `run get`/`run logs` commands are the template for flags, the `--json` envelope,
  and exit-code mapping.
- The nine-status enum and terminal-state definition are already documented in the skill and
  enforced in `run logs --follow`; `run wait` must share that source of truth, not restate it.
- Exit-code table and the JSON-envelope-per-verb notes are in the embedded
  `api/internal/uzicli/skill/SKILL.md`; M3 amends the same file the CLI reinstalls on every run.
- The new exit code `7` has several landing sites beyond the SKILL.md table: the constant in
  `api/internal/uzicli/output.go` (0-6 defined there today), `TestExitCodeFor` in
  `api/internal/uzicli/output_test.go`, and the exit-code table in `docs/cli.md`. Enumerate
  them in the M1 handoff so none is missed.
- No migration, no schema, no forge driver, no worker/agent change.

## Risks

- **R1 — `run wait` masking a stuck run.** Mitigated by D2 (it stops at gates and terminals,
  not on `running`) and D6/`--timeout`; it never waits on `limit_wait` by default, and unknown
  statuses are surfaced rather than silently waited on.
- **R2 — scope creep into a streaming/long-poll design.** Explicitly deferred (D8); M1 is a
  poll loop, which is all the observed need requires.
- **R3 — skill/doc drift re-appearing.** M3 edits the single embedded source of truth the
  binary reinstalls, so the reference cannot drift from the binary the way the missing
  `--category` line did.
