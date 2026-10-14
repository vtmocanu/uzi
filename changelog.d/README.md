# Pending changelog fragments

Create one file named `changelog.d/<uuid-v4>.md`, using a lowercase canonical
UUID v4 (version nibble `4`, variant nibble `8`, `9`, `a` or `b`).
README.md is reserved. There is no shared index or manifest.

Each fragment contains exactly one category, one blank line, one bold outcome
title ending in a period, and immediately one nonempty description physical
line indented two spaces. Only blank lines may follow. Categories are Added,
Changed, Deprecated, Removed, Fixed and Security. For example:

```markdown
### Fixed

- **Pending changes survive independent merges (#2957).**
  Each pull request owns its fragment so unrelated changelog entries remain intact.
```

Cite the relevant issue or PR; the release oracle checks author/coverage identity,
while the parser checks structure. Symlinks, directories and other nonregular
entries are rejected.

Run these commands from the target repository root:

- `scripts/changelog-fragments.sh validate`: validate all pending fragments.
  Explicit fragment paths may also be supplied.
- `scripts/changelog-fragments.sh list`: validate and print incorporated paths,
  sorted by filename with LC_ALL=C, excluding README.md.
- `scripts/changelog-fragments.sh render-body <file>`: render a legacy
  Unreleased **body** plus all pending fragments.
- `scripts/changelog-fragments.sh render-unreleased <file>`: extract exactly one
  `## [Unreleased]` section from a full changelog and render its pending body.

Rendering writes only stdout. Capture output and the incorporated path list in
scratch files if needed; release integration owns later consumption. The renderer emits
categories in canonical order, keeps legacy bullets in their existing order
within each category, then appends fragments in filename order. Duplicate category
headings are grouped; equal prose is retained. Legacy one-line and multiline
bullet bytes are preserved, with normalized blank separators. An optional
release-title comment before the categories is retained. Unknown headings,
orphan prose and misplaced markers fail closed. The renderer emits only the
pending body; it does not rewrite the preamble or released sections.

The PR check validates every added or changed fragment before accepting an
entry or exemption, including untracked and uncommitted files. Only fragments
absent at the merge base count as new entries. Editing or deleting inherited
fragments alone does not satisfy coverage. Direct Unreleased additions remain
supported during migration.
