### Fixed

- **A run created with your own plan no longer waits for a plan approval that never comes (#3272).**
  On both Claude and Codex the lead is now told the plan is owner-supplied with no plan gate, so it implements instead of re-drafting the plan or parking; Codex also receives the plan in a framed block on every implementation turn.
