# fixtures/pr-size-line

A **cross-language golden** for the display form of the PR size line (PRD #1798 D3, M7).
`cases.json` is a hand-authored array of `{name, size, expected}` cases, where `size` is the
`PrDescriptionSize` wire shape (`api/internal/apitypes/pr_description.go`) and `expected` is
the plain-text line both read surfaces must print, or `null` for no line.

- **Go** (`api/cmd/uzi`): `TestPrSizeLineFixture` in `run_render_pr_description_test.go`,
  driving `prSizeBody` (`run_render.go`), the `uzi run get` SIZE row.
- **TypeScript** (`web/src`): `DeliveredCard.test.tsx` in `web/src/pages/runView/`, driving
  `prSizeLine` (`DeliveredCard.tsx`), the run view's "Delivered" card.

The format matches the agent's `renderSizeLine` (`agent/src/pr-size.ts`) without its markdown
bold: bucket order code, tests, docs, config, generated, vendored; `en-US` thousands
separators; U+2212 before the deleted count; ` · ` (U+00B7) between parts; `1 file` /
`N files`. One difference is inherent to the data: the agent prints a bucket any file landed
in, while the stored size has only line counts, so a bucket with `+0 −0` (for example only a
binary file) is omitted here.
