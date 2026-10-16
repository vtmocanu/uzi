# Pending changelog fragments

Ordinary PRs create a new `changelog.d/<uuid>.md` before the PR exists. Use a
lowercase canonical UUID v4 (version nibble `4`, variant nibble `8`, `9`, `a`
or `b`), for example generated with:

```sh
uuidgen | tr '[:upper:]' '[:lower:]'
```

README.md is reserved. There is no shared index or manifest.

## Format

Each fragment contains exactly one category, one blank line, one bold outcome
title ending in a period, and immediately one nonempty description physical
line indented two spaces. Only blank lines may follow. Categories are Added,
Changed, Deprecated, Removed, Fixed and Security. For example:

```markdown
### Fixed

- **Pending changes survive independent merges (#2957).**
  Each pull request owns its fragment so unrelated changelog entries remain intact.
```

Initially cite the relevant issue; a PR number need not exist yet. The parser
checks structure, not citation identity. The release oracle still requires the
shipping commit's identity in the assembled release section (subject issue/PR,
same-repo closing reference, or short SHA; forge lookup can resolve closed
issues). A fragment touch is not an oracle exemption.

## Validate and preview

Run these commands from the target repository root:

- `bash scripts/changelog-fragments.sh validate`: validate pending fragments.
  Explicit fragment paths may also be supplied.
- `bash scripts/changelog-fragments.sh list`: validate and print incorporated
  paths, sorted by filename with LC_ALL=C, excluding README.md.
- `bash scripts/changelog-fragments.sh render-body <file>`: render a legacy
  Unreleased **body** plus pending fragments.
- `bash scripts/changelog-fragments.sh render-unreleased <file>`: extract exactly
  one `## [Unreleased]` section from a full changelog and render its pending body.

The CLI writes rendered notes to stdout and does not edit tracked inputs.
Categories appear in the order listed above. Within each category, legacy
bullets retain their existing order, then fragments append in C filename order.
Duplicate category headings are grouped; equal prose is retained. Legacy
one-line and multiline bullet lines are preserved, with normalized blank
separators. An optional release-title comment before the categories is retained.
Unknown headings, orphan prose and misplaced markers fail closed. The renderer
emits the pending body, not the preamble or released sections.

Inputs use bounded snapshots: 65536 bytes per fragment, 1024 fragments,
4194304 bytes combined pending input (legacy body plus fragments), and
16777216 bytes for the full legacy changelog input. Symlinks, directories and
other nonregular entries are rejected; release inputs must be committed with
mode 100644. Invalid path diagnostics use C-locale quoting, bidi controls are
rejected, and directory-enumeration producer failures fail closed.
See [ADR-2957](../adr/2957-changelog-fragments.md) for enforcing source boundaries.

## PR coverage and migration

The PR check validates added or changed fragments before accepting an entry
or exemption, including untracked and uncommitted files. A valid fragment absent
at the merge base supplies a new entry. Editing or deleting inherited fragments
alone does not satisfy coverage. Existing legacy `[Unreleased]` bullets stay
until the next release; direct additions remain supported during migration.

The existing exemptions remain: `Changelog: none` in a branch commit with a
reason; docs-typed shipping commits (with a docs-typed squash subject for the
oracle); and dependency-manifest-only changes under dependency-typed commits,
which the cutter cites in bulk. See
[the PR check](../scripts/check-changelog-entry.sh) for the predicates and
[the release oracle](../scripts/assert-changelog-covers-release.sh) for final
shipping identity coverage. A malformed changed fragment still fails on an
otherwise exempt branch.

## Release consumption and recovery

The [release cutter](../.agents/skills/uzi-release/scripts/release-cut.sh) validates
and captures the pending inventory before release mutation, assembles it into
the release section, and deletes exactly the incorporated paths in the same
release commit. Later RCs append new blocks while shipped blocks remain frozen;
use a new fragment for amendments. Promotion-only promotes the candidate's own
commit, leaving main's pending fragments for the next cut.
`--changelog-file` refuses pending fragments before mutation. Explicit promotion-only
and fragment-free drafts that replace the legacy body (first RC, stable or combined
promotion) do not require main's legacy category/bullet grammar;
inventory, byte limits, control-byte and section checks still apply.

Preflight failures preserve inputs. Preparation failure or interruption can
leave assembled notes and fragment deletions staged, or a completed local commit.
Inspect HEAD, worktree and index, then use the printed starting SHA and exact
owned paths for deliberate recovery. Before commit, finish and verify preparation
or restore those paths from the starting SHA. After a completed commit, deliberate
undo first soft-resets to that SHA, then restores the owned paths. Inspect any
provisional stable tag separately. There is no automatic destructive rollback.

The renderer's legacy-line preservation does not prevent the release pipeline
from maintaining citation/compare links or reconciling the previous stable
heading date on a new-base cut. See [the release skill](../.agents/skills/uzi-release/SKILL.md)
for cut, review, tag and publish steps.
