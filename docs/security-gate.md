---
title: Security gate (SAST)
audience: contributor
---

# Security gate (SAST)

`task gate` blocks on three SAST layers, added by PRD #862 because this repo's
code is authored by AI agents at volume: gosec and depguard ride the existing
`golangci-lint` gate, and semgrep runs uzi's own guardrail invariants as
deterministic rules. CodeQL (GitHub default setup) stays as-is alongside these
and is not described here beyond the division of labour below.

## gosec and depguard (golangci-lint)

Both are entries in `.golangci.yml`'s `linters.enable` list, so they ride the
gate's existing ratchet (`issues: new-from-merge-base: origin/main,
whole-files: true`, see [dev-conventions.md](./dev-conventions.md)): only
findings your branch introduces block `task lint:api` / `task lint:controller`
(and, since `whole-files` is set, any other finding in a file your branch
touches). The pre-existing backlog is visible but non-gating via
`task lint:api:all` / `task lint:controller:all`.

- **gosec** — generic Go security smells (weak randomness, unsafe file perms,
  SQL string-building, and so on). No `linters.settings.gosec` exclusions
  ship today; if a future finding is a known-safe idiom, add one there with a
  one-line justification, per this repo's "every disabled linter owes a
  justification" convention.
- **depguard**, rule `forge-sdk-isolation` — enforces guardrail invariant #7:
  the raw forge SDK packages (`github.com/google/go-github`,
  `code.gitea.io/sdk/gitea`, `gitlab.com/gitlab-org/api/client-go`) may be
  imported only from `api/internal/forge`. **depguard's `deny.pkg` matches by
  package-tree prefix, not glob** — a `*`/`**` inside `pkg:` is taken
  literally and matches nothing, so the rule lists the bare module-path
  prefixes rather than a wildcarded path.

## semgrep

`scripts/semgrep-gate.sh` is a bring-your-own-on-PATH wrapper, mirroring
`scripts/lint-yaml.sh`: it fail-open SKIPs (exit 0, banner printed) when
`semgrep` is absent locally, and is required (exit 2 on absence) whenever
`CI` or `UZI_SAST_REQUIRED` is set. It is wired into `gate:repo` as
`task sast:semgrep`, run last (a whole-tree scan dominates, same as
`scan:secrets`). Rule files live under `semgrep/`.

**The canary is the control.** A SAST gate whose healthy state is silence
cannot tell a clean tree from a scanner that never ran, so the wrapper first
scans `scripts/semgrep-canary.txt` with the proof rule (`semgrep/canary.yml`)
and requires it to fire before trusting a 0-findings verdict on the real
tree — the same discipline `scan:secrets`' gitleaks canary uses. An unfired
canary is an instrument failure (exit 2), never a clean run.

### Adding a new invariant rule

1. Add a rule file under `semgrep/` (one invariant per file, named for what it
   enforces).
2. **Scope it with `paths.include`** (and `paths.exclude` for a legitimate
   carve-out) to the files the invariant actually governs. An unscoped rule
   either over-fires on unrelated code or, worse, silently matches nothing
   (as the depguard glob gotcha above did) — scoping it to real files is what
   makes a zero-findings result mean something.
3. **Verify it with a positive and a negative control before committing**:
   temporarily introduce the violation and confirm the rule fires, then
   revert and confirm the clean tree reports zero. Write both results in a
   comment above `rules:` — the shipped rules do this and it's what makes a
   reviewer able to trust a rule's "zero findings" without re-deriving it.
4. It rides `sast:semgrep` automatically; no Taskfile change is needed for a
   new rule file, only for a new rules directory.

### Shipped rules and their reliability boundaries

- **`worker-routes-cookie-free.yml`** (invariant #3) — worker/controller
  Bearer-only handlers must never read a session cookie or CSRF
  (`$R.Cookie(...)`, `$R.Cookies(...)`, `$R.Header.Get("Cookie")`,
  `auth.ValidateCSRF(...)`). It is **file-glob scoped** to the known
  handler files (`worker_*.go`, `controller_*.go`, `judge_worker.go`,
  `task_review.go`, excluding `worker_upgrade_summary.go`, which is a
  legitimately cookie-capable `RequireUser` route). A new worker handler
  added under a differently-named file escapes it until the glob is widened
  — code review is the backstop for that gap. (There is no route-enumeration
  test asserting the cookie-free property today: `route_limiter_mounts_test.go`
  enumerates routes but checks rate-limiter mounts, not the auth boundary. A
  real route-auth-enumeration test is tracked as a follow-up.)
- **`settings-sources-isolation.yml`** (invariant #6) — the agent SDK's
  `settingSources` must stay `[]` (the defense against a checked-out repo's
  `.claude/` widening the agent's own permissions). It matches an **explicit
  non-empty value** (`settingSources: ["project"]`, or a variable). It does
  **not** catch an omitted key: the SDK's own default is fail-open (no
  `settingSources` loads every source), and a rule that instead demanded the
  key's presence would false-positive on every legitimate
  `{ ...baseOptions, ... }` spread site. That gap is a review + test concern,
  not a semgrep one.

## Division of labour with CodeQL

semgrep here is intentionally narrow: it covers **syntactic, path-scoped**
invariants that a single-file AST match can express. CodeQL (GitHub's
interprocedural, cross-function taint engine) remains the owner of:

- **#1** a PAT or sealed value reaching a log sink except through the
  redactor, and **#4** an outbound forge request bypassing the allowlisted
  base-URL check — both interprocedural taint, which semgrep OSS cannot
  trace across function/file boundaries without a high false-negative rate.
- **#2** no handler reveals a decrypted secretbox-sealed value — also
  interprocedural by design in this codebase (the decrypt happens in a
  service layer, the response is built in a handler), so it belongs with
  #1/#4 rather than as a semgrep rule.
- **#5** the guardrail deny-hook covers every push/force/rewrite shape — a
  completeness property, not a presence/absence match, which semgrep cannot
  prove.

Do not read a shipped semgrep rule as covering more than its stated
invariant: the reliability boundaries above are real limits, not caveats to
skim past.
