# Grouped cross-check Runs mock

Interactive design example selected for the Runs cross-check visibility proposal.
Open [the standalone HTML](./cross-check-runs-mock.html) in a browser.

This is a synthetic prototype, not production code or evidence of a real run.
It needs no build step or artifact runtime. Google Fonts is optional; system fonts
are the fallback. No product API requests are made. The scope preference is stored
only in the viewer's browser.

## Explore

- Use the top-right Runs / Cross-checks selector, styled consistently with Judge.
- Switch Active and Past, then filter by stage or review/execution outcome.
- Click a plan-round or code-check step to expand the attempt details.
- Compare the work run's status with Check, Verdict and Effect on run.
- Toggle the synthetic evidence checkbox to preview a user without checks.

## Design boundary

One card groups the retained checks of one parent work run. A card remains Active
while any check is in flight; earlier finished rounds remain in that card.
Past therefore lists settled groups, not every settled attempt independently.

The mock illustrates missing-child and missing-cause states. Its effect descriptions,
diagnostic examples, counts and next-action explanations are synthetic. Production
must show only facts supported by retained records, explicitly label unavailable
evidence, and use independently paginated owner-scoped reads. It must not infer that
a human approved a plan merely because the parent later completed.

The visibility feature does not change execution, review, approval or publication
policy. Standalone checker Slack DMs remain suppressed. New failure diagnostics and
parent-completion Slack disclosure are separate proposed changes.

## Implementation scope

The implementation issue will be the behavioral contract, including authorization,
pagination, filters, remembered scope, accessibility, CLI parity and tests. This mock
is a visual example, not a new PRD or an executable acceptance oracle.
