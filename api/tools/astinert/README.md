# astinert — AST comment-inertness checker (Go)

Proves that the difference between two Go source files is limited to **comments
and gofmt-normalized formatting** — nothing that reaches real syntax.

## What it proves

It parses each file with `go/parser` **without** `parser.ParseComments`, reprints
the resulting AST with `go/printer`, and compares the two reprints byte-for-byte.

> **Byte-identical reprint ⇒ only comments and gofmt-normalized formatting
> changed.**

This is a **sound, one-way** guarantee. Because comments are dropped before
reprinting and the printer re-normalizes whitespace, any change that reaches the
syntax tree — including a statement that was *commented out* — survives into the
reprint and makes the two differ.

It may **conservatively** report `DIFFERS` on some pure-formatting edits the
printer does not fully normalize (for example a lone blank-line toggle inside a
function body). Erring toward `DIFFERS` is the safe direction for an inertness
checker: a false `INERT` would let a real code change pass as comment-only,
whereas a false `DIFFERS` only asks a human to look. Never invert this by
"fixing" a conservative `DIFFERS`.

## Exit codes

Mirrors the repo's `fmt-check:api` convention (2 = instrument broken, 1 =
findings):

| Code | Meaning |
|------|---------|
| `0`  | `INERT` — byte-identical reprints; only comments/formatting changed |
| `1`  | `DIFFERS` — the reprints differ; code changed |
| `2`  | usage error, or a file failed to parse (the instrument is broken) |

## Usage

Run from the `api/` module directory:

```
go run ./tools/astinert OLD.go NEW.go
```

To drive it across a commit, materialize the old revision to a temp file and
compare against the working-tree file:

```
git show HEAD~1:internal/handler/skills.go > /tmp/old.go
go run ./tools/astinert /tmp/old.go internal/handler/skills.go
```

Exit `0` means the change to `skills.go` since `HEAD~1` touched only comments and
formatting; exit `1` means it changed code.

Note: `go run` collapses any non-zero program exit to its own exit `1` (it prints
`exit status 2` to stderr but exits `1` itself), so under `go run` you can tell
`INERT` (0) from not-inert (1) but not `DIFFERS` (1) from a parse/usage error (2).
When a script needs to branch on `2` specifically, build the binary first
(`go build -o astinert ./tools/astinert`) and run that. Verified 2026-08-20 on
this module: `go build` then run gives 0/1/2; `go run` gives 0/1/1.

## Why AST, not grep

A naive strip of `//`- or `#`-prefixed lines is **unsound**:

- A `//` can appear inside a string literal or a URL (`"https://…"`), so a
  textual strip mangles live code.
- A commented-out line of code (`// if got != want { t.Error(...) }`) is
  **identical at the text level** to a prose comment — the case that looks like a
  mere comment in a diff and is the whole reason this tool exists. Only parsing
  distinguishes "this comment is prose" from "this comment used to be a running
  assertion."

Parsing is what makes the guarantee sound. (This is the Go analogue of the bash
`shfmt -mn` minify-and-compare reasoning issue #101 cites.)
