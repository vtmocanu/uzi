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
	// sizeBudgetFactorL floors the per-run budget for a LARGE-repo run (size_class='l')
	// that froze 0 or 1 milestones — otherwise it would drop to the global default
	// (RUN_MAX_ITERATIONS=5 / RUN_TIMEOUT=2h). The freeze CASE multiplies the base
	// iteration/wall budget by this factor only in the count<=1 arm; 's'/'m'/'' stay NULL
	// (unchanged). Chosen so an 'l' run floors to run_max_iterations*5 (=25 by default) and
	// LEAST(run_timeout*5, budgetWallCeilingSeconds) (=8h), matching what the milestone-count
	// path gives a ~5-milestone run. See runtime.sql CreateApprovePlanInput / SetRunRunning.
	sizeBudgetFactorL = 5
)
