package workersvc

// Per-run budget scaling constants (PRD #122 M2, Decision 5/5b/12). The budget is
// derived SERVER-SIDE at freeze from the frozen milestone count and persisted on the
// run row (budget_max_iterations / budget_wall_seconds); these constants are the
// caps the SQL freeze applies. A worker-side cap is not a control (Decision 12), so
// the scaling lives where the server can enforce it.
const (
	// milestoneBudgetCap bounds the COUNT that scales the budget. It is separate from
	// maxMilestonesPerRun (50, the storage cap): a run may FREEZE up to 50 milestones,
	// but the budget scales by at most 12× so a lead that emits 40 milestones does not
	// buy itself a 40× budget (Risks). factor = min(n, milestoneBudgetCap).
	milestoneBudgetCap = 12
	// budgetWallCeilingSeconds is the absolute 8h wall-clock ceiling on a scaled run's
	// derived timeout, independent of milestone count — the second half of the hard
	// ceiling the count cap gives (Risks). 8 * 60 * 60 = 28800.
	budgetWallCeilingSeconds = 8 * 60 * 60
)
