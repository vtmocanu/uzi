package apitypes

// Issue #4730: wire DTOs of the scoped Quality analytics API
// (GET /api/analytics/quality and GET /api/admin/analytics/quality).
//
// Conventions shared by every type below, on top of those of analytics.go:
//   - Every count is a number of DISTINCT items of the section's population (PRs, runs or
//     attempts). Every class map is a fixed struct of counters; a class with no members is
//     zero. Arrays are never null.
//   - Every section carries applicable, reason and time_basis. A section whose kind filter
//     excludes its population reports applicable=false with reason "not_applicable" and zero
//     counts; otherwise reason is null.
//   - "index" in a bucket is the zero-based position in the response's buckets array. Every
//     calendar bucket is present, including empty ones. The envelope carries no previous
//     window and an empty rolling array.
//   - No repo ids or paths appear beyond the caller's own validated repo filter, echoed in
//     cross_check.pending.effective_filters.repo after the route's repo authority check. A
//     change that adds repo-keyed rows must apply the owner route's repo masking.

// AnalyticsQualitySectionDTO is the applicability header of one Quality measure. TimeBasis names
// the timestamp the measure's window and buckets are cut on.
type AnalyticsQualitySectionDTO struct {
	Applicable bool    `json:"applicable"`
	Reason     *string `json:"reason"`
	TimeBasis  string  `json:"time_basis"`
}

// AnalyticsQualityReworkGroupsDTO counts PRs by the number of verified retained mr_rework runs
// they have: none, one, two or three and more.
type AnalyticsQualityReworkGroupsDTO struct {
	Zero      int64 `json:"zero"`
	One       int64 `json:"one"`
	Two       int64 `json:"two"`
	ThreePlus int64 `json:"three_plus"`
}

// AnalyticsQualityReworkBucketDTO is the merged PRs whose earliest matching cohort run was
// created in the bucket.
type AnalyticsQualityReworkBucketDTO struct {
	Index  int                             `json:"index"`
	PRs    int64                           `json:"prs"`
	Groups AnalyticsQualityReworkGroupsDTO `json:"groups"`
}

// AnalyticsQualityReworkDTO is the rework-by-PR measure (time basis
// pr_first_cohort_created_at). ApplicablePRs are the cohort PRs with a merge witness from the
// scoped user; Groups and Buckets cover only them.
type AnalyticsQualityReworkDTO struct {
	AnalyticsQualitySectionDTO
	ApplicablePRs int64                             `json:"applicable_prs"`
	Groups        AnalyticsQualityReworkGroupsDTO   `json:"groups"`
	Buckets       []AnalyticsQualityReworkBucketDTO `json:"buckets"`
	// UnwitnessedPRs are cohort PRs with no merge witness. UnverifiedLinks are retained reworks
	// on a selected PR whose target run does not carry the same repo, MR and owner; they are in
	// no group. SharedExternalPRs are selected PRs whose external identity is shared by two or
	// more selected PRs, and SharedDetectionUnavailable those left out of that detection.
	UnwitnessedPRs             int64 `json:"unwitnessed_prs"`
	UnverifiedLinks            int64 `json:"unverified_links"`
	SharedExternalPRs          int64 `json:"shared_external_prs"`
	SharedDetectionUnavailable int64 `json:"shared_detection_unavailable"`
	// Retention is always "retained_runs_only": only rework runs still stored are counted, so a
	// deleted run is a lower count and not a zero-rework proof.
	Retention string `json:"retention"`
	// MergeDayActivity and RunEndToMergeSeconds are always null with reason "not_tracked".
	MergeDayActivity     AnalyticsNullableNumberDTO `json:"merge_day_activity"`
	RunEndToMergeSeconds AnalyticsNullableNumberDTO `json:"run_end_to_merge_seconds"`
}

// AnalyticsQualityCIFixBucketDTO is one creation bucket of CI-fix activity.
type AnalyticsQualityCIFixBucketDTO struct {
	Index    int                  `json:"index"`
	Runs     int64                `json:"runs"`
	Finished int64                `json:"finished"`
	InFlight int64                `json:"in_flight"`
	Outcomes AnalyticsOutcomesDTO `json:"outcomes"`
}

// AnalyticsQualityCIFixDTO is CI-fix activity (time basis created_at). Finished runs have an
// outcome class; in-flight runs have none.
type AnalyticsQualityCIFixDTO struct {
	AnalyticsQualitySectionDTO
	Runs     int64                            `json:"runs"`
	Finished int64                            `json:"finished"`
	InFlight int64                            `json:"in_flight"`
	Outcomes AnalyticsOutcomesDTO             `json:"outcomes"`
	Buckets  []AnalyticsQualityCIFixBucketDTO `json:"buckets"`
}

// AnalyticsQualityPlanResultsDTO are the stored plan verdicts of decided attempts.
type AnalyticsQualityPlanResultsDTO struct {
	Approve int64 `json:"approve"`
	Revise  int64 `json:"revise"`
	Block   int64 `json:"block"`
	Failed  int64 `json:"failed"`
}

// AnalyticsQualityCodeResultsDTO are the stored code outcomes of decided attempts.
type AnalyticsQualityCodeResultsDTO struct {
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
}

// AnalyticsQualityClassesDTO classifies decided attempts the way the cross-check history does.
type AnalyticsQualityClassesDTO struct {
	Passed       int64 `json:"passed"`
	RaisedIssues int64 `json:"raised_issues"`
	DidntFinish  int64 `json:"didnt_finish"`
	Replaced     int64 `json:"replaced"`
	Unclassified int64 `json:"unclassified"`
}

// AnalyticsQualityCheckerHarnessDTO counts attempts by checker harness; anything else is unknown.
type AnalyticsQualityCheckerHarnessDTO struct {
	Claude  int64 `json:"claude"`
	Codex   int64 `json:"codex"`
	Unknown int64 `json:"unknown"`
}

// AnalyticsQualityPlanBucketDTO is one decision-time bucket of plan attempts.
type AnalyticsQualityPlanBucketDTO struct {
	Index        int                            `json:"index"`
	Decided      int64                          `json:"decided"`
	StoredResult AnalyticsQualityPlanResultsDTO `json:"stored_result"`
	Classes      AnalyticsQualityClassesDTO     `json:"classes"`
}

// AnalyticsQualityCodeBucketDTO is one decision-time bucket of code attempts.
type AnalyticsQualityCodeBucketDTO struct {
	Index        int                            `json:"index"`
	Decided      int64                          `json:"decided"`
	StoredResult AnalyticsQualityCodeResultsDTO `json:"stored_result"`
	Classes      AnalyticsQualityClassesDTO     `json:"classes"`
}

// AnalyticsQualityPlanUndatedDTO counts decided plan attempts with no usable decision time. The
// window is not applied to them (window is always "not_applied").
type AnalyticsQualityPlanUndatedDTO struct {
	Window       string                         `json:"window"`
	Count        int64                          `json:"count"`
	StoredResult AnalyticsQualityPlanResultsDTO `json:"stored_result"`
}

// AnalyticsQualityCodeUndatedDTO is AnalyticsQualityPlanUndatedDTO for code attempts.
type AnalyticsQualityCodeUndatedDTO struct {
	Window       string                         `json:"window"`
	Count        int64                          `json:"count"`
	StoredResult AnalyticsQualityCodeResultsDTO `json:"stored_result"`
}

// AnalyticsQualityPlanStageDTO is the plan stage's decided attempts (time basis decided_at).
// StoredResult is the plan verdict field.
type AnalyticsQualityPlanStageDTO struct {
	AnalyticsQualitySectionDTO
	DecidedInWindow int64                             `json:"decided_in_window"`
	StoredResult    AnalyticsQualityPlanResultsDTO    `json:"stored_result"`
	Classes         AnalyticsQualityClassesDTO        `json:"classes"`
	CheckerHarness  AnalyticsQualityCheckerHarnessDTO `json:"checker_harness"`
	Undated         AnalyticsQualityPlanUndatedDTO    `json:"undated"`
	Buckets         []AnalyticsQualityPlanBucketDTO   `json:"buckets"`
}

// AnalyticsQualityCodeStageDTO is the code stage's decided attempts (time basis decided_at).
// StoredResult is the code outcome field; the legacy code verdict is never exposed.
type AnalyticsQualityCodeStageDTO struct {
	AnalyticsQualitySectionDTO
	DecidedInWindow int64                             `json:"decided_in_window"`
	StoredResult    AnalyticsQualityCodeResultsDTO    `json:"stored_result"`
	Classes         AnalyticsQualityClassesDTO        `json:"classes"`
	CheckerHarness  AnalyticsQualityCheckerHarnessDTO `json:"checker_harness"`
	Undated         AnalyticsQualityCodeUndatedDTO    `json:"undated"`
	Buckets         []AnalyticsQualityCodeBucketDTO   `json:"buckets"`
}

// AnalyticsQualityEffectiveFiltersDTO are the filters a live measurement actually applied.
// Repo and Harness are null when unfiltered.
type AnalyticsQualityEffectiveFiltersDTO struct {
	Repo    *string  `json:"repo"`
	Harness *string  `json:"harness"`
	Kinds   []string `json:"kinds"`
}

// AnalyticsQualityPendingDTO is the live count of undecided, uninterrupted attempts of any age
// (time basis live; the window is not applied, window is always "not_applied").
type AnalyticsQualityPendingDTO struct {
	AnalyticsQualitySectionDTO
	Window           string                              `json:"window"`
	Total            int64                               `json:"total"`
	Plan             int64                               `json:"plan"`
	Code             int64                               `json:"code"`
	EffectiveFilters AnalyticsQualityEffectiveFiltersDTO `json:"effective_filters"`
}

// AnalyticsQualityCrossCheckDTO separates the two stages and the live pending count.
type AnalyticsQualityCrossCheckDTO struct {
	Plan    AnalyticsQualityPlanStageDTO `json:"plan"`
	Code    AnalyticsQualityCodeStageDTO `json:"code"`
	Pending AnalyticsQualityPendingDTO   `json:"pending"`
}

// AnalyticsQualityJudgeCandidatesDTO counts judge-eligible-kind targets created in the window by
// terminal status.
type AnalyticsQualityJudgeCandidatesDTO struct {
	Total     int64 `json:"total"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
}

// AnalyticsQualityJudgeCompleteDTO counts eligible targets with a complete review by verdict.
type AnalyticsQualityJudgeCompleteDTO struct {
	Total  int64 `json:"total"`
	Ideal  int64 `json:"ideal"`
	OK     int64 `json:"ok"`
	Issues int64 `json:"issues"`
}

// AnalyticsQualityJudgeEligibleDTO is the eligible denominator: total = unreviewed + failed +
// complete.total. UnreviewedSkipped is the subset of unreviewed with a recorded judge skip.
type AnalyticsQualityJudgeEligibleDTO struct {
	Total             int64                            `json:"total"`
	Unreviewed        int64                            `json:"unreviewed"`
	UnreviewedSkipped int64                            `json:"unreviewed_skipped"`
	Failed            int64                            `json:"failed"`
	Complete          AnalyticsQualityJudgeCompleteDTO `json:"complete"`
}

// AnalyticsQualityJudgeExcludedDTO are targets outside the denominator. ReviewedExcluded is the
// subset of SkipOrigin that nevertheless has a review.
type AnalyticsQualityJudgeExcludedDTO struct {
	Cancelled        int64 `json:"cancelled"`
	InFlight         int64 `json:"in_flight"`
	SkipOrigin       int64 `json:"skip_origin"`
	ReviewedExcluded int64 `json:"reviewed_excluded"`
}

// AnalyticsQualityJudgeBucketDTO is one target-creation bucket. Coverage is complete over
// eligible; null with reason "no_applicable_runs" when eligible is zero.
type AnalyticsQualityJudgeBucketDTO struct {
	Index    int                        `json:"index"`
	Eligible int64                      `json:"eligible"`
	Complete int64                      `json:"complete"`
	Coverage AnalyticsNullableNumberDTO `json:"coverage"`
}

// AnalyticsQualityJudgeDTO is the judge review coverage (time basis target_created_at).
// Selection is always "retained_target_properties": the denominator is derived from properties
// the target run retains, and Limitations names what it cannot know historically.
type AnalyticsQualityJudgeDTO struct {
	AnalyticsQualitySectionDTO
	Candidates  AnalyticsQualityJudgeCandidatesDTO `json:"candidates"`
	Eligible    AnalyticsQualityJudgeEligibleDTO   `json:"eligible"`
	Excluded    AnalyticsQualityJudgeExcludedDTO   `json:"excluded"`
	Coverage    AnalyticsNullableNumberDTO         `json:"coverage"`
	Buckets     []AnalyticsQualityJudgeBucketDTO   `json:"buckets"`
	Selection   string                             `json:"selection"`
	Limitations []string                           `json:"limitations"`
}

// AnalyticsQualityDTO is the Quality response: the shared calendar envelope plus the four
// measures. Retained rework and review evidence only; nothing here is a complete lifetime history.
type AnalyticsQualityDTO struct {
	AnalyticsEnvelopeDTO
	// SelectedUser matches the options response: "all" for an admin response spanning every
	// user, else the user id (the caller on the owner route).
	SelectedUser string                        `json:"selected_user"`
	Rework       AnalyticsQualityReworkDTO     `json:"rework"`
	CIFix        AnalyticsQualityCIFixDTO      `json:"ci_fix"`
	CrossCheck   AnalyticsQualityCrossCheckDTO `json:"cross_check"`
	Judge        AnalyticsQualityJudgeDTO      `json:"judge"`
}
