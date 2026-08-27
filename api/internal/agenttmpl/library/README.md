# library — syncing uzi's builtins from the upstream skills library

`manifest.json`, next to this file, is a vendored, distilled `name → version` map
for the 11 shipped builtin agent roles that are derived from an upstream role
library. This README is the procedure for refreshing that sync.

## The two sides

- **Builtins** — `api/internal/agenttmpl/builtins/*.md`. 12 files. `lead.md` is
  uzi-only (no upstream counterpart, carries no `version:` frontmatter line). The
  other 11 — `architect`, `auditor`, `coder`, `documenter`, `fact-checker`,
  `researcher`, `reviewer`, `spec-keeper`, `tester`, `ux-designer`, `web-ux` — each
  carry a `version: N` frontmatter line.
- **Upstream** — `github.com/vtmocanu/skills`, file
  `skills/agent-kit/agent-team/roles.yaml`. Each role there has a `version:`
  integer. `manifest.json` pins the upstream commit this repo last synced
  against (`upstream_sha`) — read the live value in `manifest.json`, do not
  assume it from this file.

`manifest.json` deliberately does **not** vendor the role bodies, only the
version numbers plus `upstream_repo`, `upstream_sha`, `path`, and `synced`. A
body byte-compare would permanently redden the roles that are deliberately
adapted from upstream (see the caveat below).

Upstream ships more roles than uzi builds in: 14 total as of the last sync,
including `release`, `tui-ux`, and `skill-reviewer`, none of which uzi ships as
a builtin. `manifest.json` lists only the 11 uzi does ship. Read the current
upstream role set live when you sync — do not assume it is still 14.

## What enforces this

`TestBuiltinLibraryDrift` (`api/internal/agenttmpl/library_test.go`, part of
`go test ./...`, which the `test:api` CI gate runs) compares each stamped
builtin's `version:` against its `manifest.json` entry. It fails when:

- a builtin's stamp is **behind** its manifest entry (the body needs a port),
- a stamped builtin claims a version the manifest doesn't have (unknown/ahead), or
- a manifest role has no matching stamped builtin.

It does **not** assert roster completeness against the full upstream library —
it only walks the manifest's 11 roles, so it never reddens on the 3 roles uzi
doesn't ship. `lead` (unstamped) is excluded from every check.

## Sync procedure

1. Clone `github.com/vtmocanu/skills` and pick one upstream commit to sync to;
   note its SHA. (Cloning `github.com` needs egress — this works from the
   docker worker tier or a dev machine, not every sandboxed environment.)
2. For each of the 11 library-derived roles, read the upstream `version:` in
   `roles.yaml` and compare it to the `version:` line in the matching
   `builtins/<role>.md`. Refresh `manifest.json`'s `roles` map to the new
   versions (plus `upstream_sha` and `synced`, step 6) — that refresh is what
   makes `TestBuiltinLibraryDrift` go red for every role whose upstream version
   is now ahead of its builtin's stamp.
3. For each role the test now reddens on: port the upstream body change into
   `builtins/<role>.md`. Adapt any tail-referencing library prose into
   self-discovery — a builtin ships into a stranger's repo and has no
   `## For this repo` section, so upstream prose that assumes one does not
   copy verbatim. Port the substantive change; keep the local adaptation.
4. Bump that role's `version:` stamp in `builtins/<role>.md` **in the same
   commit as the body port**, so the stamp and the body never disagree in
   history (see the caveat below).
5. Re-run `cd api && go test ./internal/agenttmpl/` (or the full
   `go test ./...`). Green means the sync is complete for that role.
6. Record the new `upstream_sha` and `synced` date in `manifest.json`.

## Caveat: the stamp is provenance, not identity — and nothing checks the body

`version: N` on a builtin means "this body was last ported from library vN",
**not** "this body is byte-identical to library vN". Several bodies are
deliberately adapted from their upstream counterparts (local style, and the
tail-referencing rewrite in step 3 above), so a byte-compare was never the
design.

That leaves exactly one drift no tooling here catches: **bumping the `version:`
stamp without actually porting the body.** `TestBuiltinLibraryDrift` only
compares numbers — it never inspects a builtin's body against upstream — so a
stamp bumped ahead of a stale body passes the drift check clean. "Bump the
stamp only in the same commit as the real body port" (step 4) is therefore a
discipline the maintainer and code review must uphold; nothing mechanical
enforces it.

## Adding or dropping a role

- **New upstream role uzi wants to ship**: add `builtins/<name>.md` with a
  `version:` frontmatter line, and add `<name>` to `manifest.json`'s `roles`.
- **Uzi deliberately doesn't ship an upstream role**: just leave it out of
  `manifest.json`. `TestBuiltinLibraryDrift` only walks the manifest's roster,
  so an omitted role never fails the check.
