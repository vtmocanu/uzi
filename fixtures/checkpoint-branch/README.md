# checkpoint-branch: the forge-checkpoint branch-derivation cross-language contract (PRD #1062 M3)

A forge checkpoint is pushed to `refs/uzi-checkpoints/<branch>`, and `<branch>` is
derived **twice** — once server-side (Go, from validated run-row fields) and once
worker-side (TypeScript, when the runner clones/opens the MR). The two derivations must
agree byte-for-byte, because the server publishes/deletes the ref the worker's clone
pushed to. This fixture is the hand-authored source of truth for that derivation, and
each side pins to it independently.

## The two checkpoint-eligible kinds, and the kind-first rule

Checkpointing fires for the **checkpoint-eligible set**: an `issue` run
(`agent/issue-<iid>`) and a `self_improve` run (`uzi/self-improve/<run_id>`, added in M3).
A `self_improve` run also carries a **valid `issue_iid`** (a stable tracking-issue
container reused across cycles), so the derivation MUST dispatch on **kind first** —
gating on `issue_iid` alone would misroute a self_improve run to the issue branch. Every
other kind is ineligible: the publish/delete is a benign `unsupported` skip / no-op.

## 🔴 Hand-authored — do NOT tidy, regenerate, sort, or reformat

```
fixtures/checkpoint-branch/cases.json   the eligible cases + their expected branch, and the ineligible kinds
fixtures/checkpoint-branch/README.md    this file
```

`cases.json` is **hand-authored** and there is deliberately **no `-update` flag** on
either reader. If a test here goes red, a reader (or the derivation) changed — fix the
reader, not the fixture. In particular the Go drift guard asserts the fixture's
`eligible` kinds EXACTLY equal the set of kinds the server's `checkpointBranch` returns
`ok=true` for, so a newly-enabled kind the fixture forgot turns the contract red.

## The two readers, and neither reads the other's source

| | reads it | how |
|---|---|---|
| Go | `api/internal/workersvc/checkpoint_branch_contract_test.go` | relative `os.ReadFile` (`go:embed` cannot escape the `api/` module) |
| node:test | `agent/test/checkpoint-branch-contract.test.ts` | `readFileSync` + `fileURLToPath(import.meta.url)` |

Each side folds its **own** production derivation and compares against the **same**
hand-authored fixture — the Go side calls `checkpointBranch`, the TS side folds the
worker's actual per-kind derivation (`RUN_KIND_PROFILES.self_improve.cloneBranch` for
self_improve, the `agent/issue-${iid}` `createOrAttachRunnerClone` default for issue).
Never Go against TS directly: a direct diff can report only *that* they disagree, never
*which* side drifted, and it would make `npm test` depend on a Go toolchain.

An unreadable fixture is a **fatal**, never a skip, on both sides — a skipped contract
asserts nothing and would look identical to a passing one.

## 🔴 The Go half needs `-count=1`

This directory sits above `api/`, so every byte of it is outside that module and
contributes **nothing** to `internal/workersvc`'s cache key. A fixture-only edit
therefore leaves a bare `go test ./internal/workersvc/` printing `ok (cached)` over a
changed fixture: a green that means "Go never ran". `task test:api` / `task gate:api`
carry `-count=1`, so the gate catches it; a bare scoped `go test` without `-count=1` will
not. The node:test half has no such cache and needs no flag.

```sh
cd api && go test -count=1 ./internal/workersvc/
cd agent && node --test test/checkpoint-branch-contract.test.ts
```

Fixture note: `fixtures/**` is not walked by `web/scripts/check-docs.mjs`, so this file
carries no doc frontmatter and its relative paths are not link-checked — kept accurate by
hand instead.
