# fixtures/pr-size-line

A **cross-language golden** for the display form of the PR size line (PRD #1798 D3, M7).
`cases.json` is a hand-authored array of `{name, size, expected}` cases, where `size` is the
`PrDescriptionSize` wire shape (`api/internal/apitypes/pr_description.go`) and `expected` is
the plain-text line both read surfaces must print, or `null` for no line. A case may also carry
`numstat`, the synthetic diff entries (`{path, added, deleted, binary, attrs?}`) the stored
`size` was computed from, and `agent_expected`, the agent's line where it is defined to differ.

- **Go** (`api/cmd/uzi`): `TestPrSizeLineFixture` in `run_render_pr_description_test.go`,
  driving `prSizeBody` (`run_render.go`), the `uzi run get` SIZE row.
- **TypeScript** (`web/src`): `DeliveredCard.test.tsx` in `web/src/pages/runView/`, driving
  `prSizeLine` (`DeliveredCard.tsx`), the run view's "Delivered" card.
- **Agent** (`agent/test/pr-size-fixture.test.ts`): drives `renderSizeLine`
  (`agent/src/pr-size.ts`) from each case's `numstat`, strips its markdown bold, and compares
  with `agent_expected` when present, else `expected`. It also checks that the `numstat`,
  bucketed by `classifyPath`, sums to the case's `size`, so both halves describe one diff.

The format matches the agent's `renderSizeLine` without its markdown bold: bucket order code,
tests, docs, config, generated, vendored; `en-US` thousands separators; U+2212 before the
deleted count; ` · ` (U+00B7) between parts; `1 file` / `N files`.

**The one defined divergence** is inherent to the data: the agent prints a bucket any file
landed in, while the stored size has only line counts, so a bucket with `+0 −0` (for example
one holding only a binary file) is omitted by the read surfaces. A binary-only change reads
`Size: 1 file` on the web and in the CLI and `**Size:** code +0 −0 · 1 file` in the PR body;
the cases that exercise it carry that agent line as `agent_expected`.
