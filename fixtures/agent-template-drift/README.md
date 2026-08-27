# agent-template-drift: the drift-predicate cross-language contract (issue #223 item 3)

"Has this row drifted from the definition this binary ships?" has **three**
implementations, and `compare.go`'s own doc comment names their divergence as the hazard
it exists to prevent:

| | answers | file |
|---|---|---|
| server | `SameContent(shipped, stored) bool` | `api/internal/agenttmpl/compare.go:49` |
| mock (web dev/test double) | `sameContent(row, def) bool` | `web/src/mocks/mockApi.ts` |
| client (editor diff panel) | `driftedColumns(shipped, current) string[]` | `web/src/lib/agentTemplates.ts` |

No divergence can be **constructed** today — all three fold `null`/`""` and `null`/`[]`
identically, compare `tools` order-sensitively, and never trim. That is exactly why the
agreement is pinned to a shared artifact *before* a future consumer makes a divergence
consequential, rather than after. Per the 2026-08-23 triage on issue #223, the residual
risk is a **self-contradicting UI badge**, not data loss: M4b/#275's
`RefreshPristineBuiltin` gates admin-edit protection on the `customized`/`origin` flags,
not on any content predicate — its inline content check is a no-op `updated_at`
optimization only.

## The two readers

```
fixtures/agent-template-drift/cases.json   the shared (shipped, stored, expected) case table
fixtures/agent-template-drift/README.md    this file
```

Repo root, **owned by neither runtime** — same placement and the same reason as
`fixtures/run-usage/` and `fixtures/judge-fidelity/`. Not
`api/internal/agenttmpl/testdata/`, which is where a `go test -update` flag gets added;
not `web/src/lib/`, which is where a `toMatchSnapshot()` gets added.

Two readers, and **each folds the SAME case table with its OWN production predicate**:

| | |
|---|---|
| Go | `api/internal/agenttmpl/drift_contract_test.go` (relative `os.ReadFile`; the fixture sits above the `api/` module) |
| vitest | `web/src/lib/agentTemplateDriftContract.test.ts` (`readFileSync` + `import.meta.url`) |

Never Go against JS directly: a direct diff can report only *that* they disagree, never
*which one drifted*, and it would make `npm test` depend on a Go toolchain. The Go half
pins `SameContent` only (`differs == !same`); the vitest half pins both `sameContent`
and `driftedColumns` — it is the only reader with a column notion.

A missing or unreadable fixture is a **fatal**, never a skip, on both sides.

## The case schema

```json
{
  "name": "…",
  "shipped": { "description": "…", "model": "opus", "tools": ["Bash"], "prompt_body": "…" },
  "stored":  { "description": "…", "model": "opus", "tools": ["Bash"], "prompt_body": "…" },
  "expected": { "differs": true, "columns": ["description"] }
}
```

`shipped` and `stored` are the four mutable columns of an agent template. `model` may be
`null` (decodes to Go `""`; TS reads it `?? ""`) — both mean inherit. `tools` may be
`null`, `[]`, or an array; `null` and `[]` both mean inherit-all and compare equal.

`expected.differs` (bool) pins the two **boolean** predicates, `SameContent` and
`sameContent`, as `differs == !same`. `expected.columns` (string[]) pins `driftedColumns`
exactly, in **display order and label spelling** — note the wart: the label is
`prompt body`, with a space, not `prompt_body`. The Go half has no column notion, so it
reads `differs` only.

## 🔴 This fixture's `expected` is HAND-AUTHORED, not recorded and not computed

Every `expected` value here was written from the stated four-column rules — null equals
empty, `tools` is order-sensitive, nothing is ever trimmed — **not** recorded from a
live system and **not** computed by calling any of the three implementations. That makes
it closer to `fixtures/judge-fidelity/`'s hand-authored expectation than to
`fixtures/run-usage/`'s recorded server output: agreement across all three
implementations against an independently-authored expectation is meaningful evidence,
where computing the expectation from one of them would make the other two's agreement
with it a tautology.

There is deliberately no `-update` flag on either side. If a test here goes red, one of
the three implementations changed; fix the implementation, not the fixture.

## 🔴 The two halves are NOT symmetric, and the Go half needs `-count=1`

This directory sits above `api/`, so every byte of it is outside that module and
contributes **nothing** to `internal/agenttmpl`'s cache key — cmd/go's own rule is *"Do
not recheck files outside the module, GOPATH, or GOROOT root"*. A fixture-only edit
therefore leaves `go test` printing `ok (cached)` over a gutted fixture. The vitest half
has no such cache and needs no flag.

```sh
cd api && go test -count=1 ./internal/agenttmpl/
cd web && npx vitest run src/lib/agentTemplateDriftContract.test.ts
```

`task test:api` / `task gate:api` already carry `-count=1`.

## Do not tidy

Both contract tests assert these case names are present, so dropping one **fatals**
rather than silently weakening the contract:

- **`tools-reorder`** — pins that `tools` is compared **order-sensitively**. An
  implementation that sorted before comparing would pass every other case and go
  undetected here alone.
- **`tools-null-vs-empty-agree`** and **`model-null-vs-empty-agree`** — pin that `null`
  and empty (`[]` / `""`) are the SAME inherit state, not two distinct ones. An
  implementation that treated them as distinct would badge a pristine, never-touched row
  as drifted.
- **`description-trailing-space-not-trimmed`**, **`prompt-body-trailing-newline-not-trimmed`**,
  **`model-whitespace-not-trimmed`** — pin that free-text columns are **never trimmed**.
  Trimming would hide a whitespace-only edit permanently.
- **`all-four-differ-in-display-order`** — pins `driftedColumns`' output order
  (`description`, `model`, `tools`, `prompt body`), not just its membership.

Fixture note: `fixtures/**` is not walked by `web/scripts/check-docs.mjs`, so this file
carries no doc frontmatter and its relative paths are not link-checked — kept accurate by
hand instead.
