package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Issue #4730: typed mirror of the jsonb document that AnalyticsOwnerQuality and
// AnalyticsAllQuality return. The SQL is the authority for the shape; DecodeAnalyticsQualityDoc
// rejects unknown keys so a statement change that is not mirrored here fails loudly instead of
// being silently dropped by the handler's DTO mapping.
//
// Conventions shared by every type below:
//   - Every count is a number of DISTINCT items of the named population (PRs, runs or
//     attempts). Every class map has a fixed key list; a class with no members is zero.
//   - "index" in a bucket cell is the zero-based position in the bucket_starts / bucket_ends
//     arrays the caller passed; every bucket is present, including empty ones.
//   - Whether a measure applies (a kind filter that excludes the measure's kinds) is decided by
//     the caller. Every measure is gated by the kinds array (rework and judge targets must
//     also be in it; CI fix and cross-check leads are selected by it), and each by its own
//     array too (delivery kinds for rework, judge kinds for judge), so an empty array for
//     any of them zeroes that measure.

// AnalyticsQualityDoc is the whole Quality document.
type AnalyticsQualityDoc struct {
	Rework     AnalyticsQualityRework     `json:"rework"`
	CIFix      AnalyticsQualityCIFix      `json:"ci_fix"`
	CrossCheck AnalyticsQualityCrossCheck `json:"cross_check"`
	Judge      AnalyticsQualityJudge      `json:"judge"`
}

// AnalyticsQualityReworkGroups counts selected PRs by the number of verified retained
// mr_rework runs they have: none, one, two or three and more.
type AnalyticsQualityReworkGroups struct {
	Zero      int64 `json:"zero"`
	One       int64 `json:"one"`
	Two       int64 `json:"two"`
	ThreePlus int64 `json:"three_plus"`
}

// AnalyticsQualityReworkBucket is one creation-cohort bucket: the selected PRs whose earliest
// matching cohort run was created in the bucket.
type AnalyticsQualityReworkBucket struct {
	Index  int                          `json:"index"`
	PRs    int64                        `json:"prs"`
	Groups AnalyticsQualityReworkGroups `json:"groups"`
}

// AnalyticsQualityRework is the rework-by-PR measure (time basis: the PR's first cohort run's
// created_at). ApplicablePRs are the cohort PRs with a merge witness from the scoped user.
type AnalyticsQualityRework struct {
	ApplicablePRs int64                          `json:"applicable_prs"`
	Groups        AnalyticsQualityReworkGroups   `json:"groups"`
	Buckets       []AnalyticsQualityReworkBucket `json:"buckets"`
	// UnwitnessedPRs are cohort PRs with no merge witness (not in ApplicablePRs).
	UnwitnessedPRs int64 `json:"unwitnessed_prs"`
	// UnverifiedLinks are distinct retained reworks on a selected PR whose target run does not
	// carry the same repo, MR and owner (or has no MR); they are in no group.
	UnverifiedLinks int64 `json:"unverified_links"`
	// SharedExternalPRs are selected PRs whose connection-independent external identity is
	// shared by two or more selected PRs. SharedDetectionUnavailable are selected PRs whose
	// connection is not the scoped user's, so they are left out of the detection.
	SharedExternalPRs          int64 `json:"shared_external_prs"`
	SharedDetectionUnavailable int64 `json:"shared_detection_unavailable"`
}

// AnalyticsQualityOutcomes are the run_outcome_classes counts.
type AnalyticsQualityOutcomes struct {
	Completed    int64 `json:"completed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	BaseDiverged int64 `json:"base_diverged"`
	Failed       int64 `json:"failed"`
}

// AnalyticsQualityCIFixBucket is one creation bucket of CI-fix activity.
type AnalyticsQualityCIFixBucket struct {
	Index    int                      `json:"index"`
	Runs     int64                    `json:"runs"`
	Finished int64                    `json:"finished"`
	InFlight int64                    `json:"in_flight"`
	Outcomes AnalyticsQualityOutcomes `json:"outcomes"`
}

// AnalyticsQualityCIFix is CI-fix activity (time basis created_at). Finished runs have an
// outcome class; in-flight runs have none.
type AnalyticsQualityCIFix struct {
	Runs     int64                         `json:"runs"`
	Finished int64                         `json:"finished"`
	InFlight int64                         `json:"in_flight"`
	Outcomes AnalyticsQualityOutcomes      `json:"outcomes"`
	Buckets  []AnalyticsQualityCIFixBucket `json:"buckets"`
}

// AnalyticsQualityPlanResults are the stored plan verdicts of decided attempts.
type AnalyticsQualityPlanResults struct {
	Approve int64 `json:"approve"`
	Revise  int64 `json:"revise"`
	Block   int64 `json:"block"`
	Failed  int64 `json:"failed"`
}

// AnalyticsQualityCodeResults are the stored code outcomes of decided attempts. The legacy
// code verdict column is never read.
type AnalyticsQualityCodeResults struct {
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
}

// AnalyticsQualityClasses are the classes of cross_check_history.sql's classified expression.
// Unclassified is its empty category.
type AnalyticsQualityClasses struct {
	Passed       int64 `json:"passed"`
	RaisedIssues int64 `json:"raised_issues"`
	DidntFinish  int64 `json:"didnt_finish"`
	Replaced     int64 `json:"replaced"`
	Unclassified int64 `json:"unclassified"`
}

// AnalyticsQualityCheckerHarness counts attempts by checker harness; a NULL or other value is
// Unknown.
type AnalyticsQualityCheckerHarness struct {
	Claude  int64 `json:"claude"`
	Codex   int64 `json:"codex"`
	Unknown int64 `json:"unknown"`
}

// AnalyticsQualityPlanBucket is one decision-time bucket of plan attempts.
type AnalyticsQualityPlanBucket struct {
	Index        int                         `json:"index"`
	Decided      int64                       `json:"decided"`
	StoredResult AnalyticsQualityPlanResults `json:"stored_result"`
	Classes      AnalyticsQualityClasses     `json:"classes"`
}

// AnalyticsQualityCodeBucket is one decision-time bucket of code attempts.
type AnalyticsQualityCodeBucket struct {
	Index        int                         `json:"index"`
	Decided      int64                       `json:"decided"`
	StoredResult AnalyticsQualityCodeResults `json:"stored_result"`
	Classes      AnalyticsQualityClasses     `json:"classes"`
}

// AnalyticsQualityPlanUndated counts decided plan attempts with no usable decision time
// (NULL, infinite or earlier than creation), all time.
type AnalyticsQualityPlanUndated struct {
	Count        int64                       `json:"count"`
	StoredResult AnalyticsQualityPlanResults `json:"stored_result"`
}

// AnalyticsQualityCodeUndated is AnalyticsQualityPlanUndated for code attempts.
type AnalyticsQualityCodeUndated struct {
	Count        int64                       `json:"count"`
	StoredResult AnalyticsQualityCodeResults `json:"stored_result"`
}

// AnalyticsQualityPlanStage is the plan stage's decided attempts in the window.
type AnalyticsQualityPlanStage struct {
	DecidedInWindow int64                          `json:"decided_in_window"`
	StoredResult    AnalyticsQualityPlanResults    `json:"stored_result"`
	Classes         AnalyticsQualityClasses        `json:"classes"`
	CheckerHarness  AnalyticsQualityCheckerHarness `json:"checker_harness"`
	Undated         AnalyticsQualityPlanUndated    `json:"undated"`
	Buckets         []AnalyticsQualityPlanBucket   `json:"buckets"`
}

// AnalyticsQualityCodeStage is the code stage's decided attempts in the window.
type AnalyticsQualityCodeStage struct {
	DecidedInWindow int64                          `json:"decided_in_window"`
	StoredResult    AnalyticsQualityCodeResults    `json:"stored_result"`
	Classes         AnalyticsQualityClasses        `json:"classes"`
	CheckerHarness  AnalyticsQualityCheckerHarness `json:"checker_harness"`
	Undated         AnalyticsQualityCodeUndated    `json:"undated"`
	Buckets         []AnalyticsQualityCodeBucket   `json:"buckets"`
}

// AnalyticsQualityPending is the live count of undecided, uninterrupted attempts (any age).
type AnalyticsQualityPending struct {
	Total int64 `json:"total"`
	Plan  int64 `json:"plan"`
	Code  int64 `json:"code"`
}

// AnalyticsQualityCrossCheck is the cross-check measure (time basis: decided_at).
type AnalyticsQualityCrossCheck struct {
	Plan    AnalyticsQualityPlanStage `json:"plan"`
	Code    AnalyticsQualityCodeStage `json:"code"`
	Pending AnalyticsQualityPending   `json:"pending"`
}

// AnalyticsQualityJudgeCandidates counts judge-eligible-kind targets created in the window by
// terminal status. Total is completed + failed.
type AnalyticsQualityJudgeCandidates struct {
	Total     int64 `json:"total"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
}

// AnalyticsQualityJudgeComplete counts eligible targets with a complete review by verdict.
type AnalyticsQualityJudgeComplete struct {
	Total  int64 `json:"total"`
	Ideal  int64 `json:"ideal"`
	OK     int64 `json:"ok"`
	Issues int64 `json:"issues"`
}

// AnalyticsQualityJudgeEligible is the eligible denominator: Total = Unreviewed + Failed +
// Complete.Total. UnreviewedSkipped is the subset of Unreviewed with a recorded judge skip.
type AnalyticsQualityJudgeEligible struct {
	Total             int64                         `json:"total"`
	Unreviewed        int64                         `json:"unreviewed"`
	UnreviewedSkipped int64                         `json:"unreviewed_skipped"`
	Failed            int64                         `json:"failed"`
	Complete          AnalyticsQualityJudgeComplete `json:"complete"`
}

// AnalyticsQualityJudgeExcluded are targets outside the denominator. ReviewedExcluded is the
// subset of SkipOrigin that nevertheless has an owner-matched review.
type AnalyticsQualityJudgeExcluded struct {
	Cancelled        int64 `json:"cancelled"`
	InFlight         int64 `json:"in_flight"`
	SkipOrigin       int64 `json:"skip_origin"`
	ReviewedExcluded int64 `json:"reviewed_excluded"`
}

// AnalyticsQualityJudgeBucket is one target-creation bucket: eligible targets and those with a
// complete review.
type AnalyticsQualityJudgeBucket struct {
	Index    int   `json:"index"`
	Eligible int64 `json:"eligible"`
	Complete int64 `json:"complete"`
}

// AnalyticsQualityJudge is the judge coverage measure (time basis: the target's created_at).
type AnalyticsQualityJudge struct {
	Candidates AnalyticsQualityJudgeCandidates `json:"candidates"`
	Eligible   AnalyticsQualityJudgeEligible   `json:"eligible"`
	Excluded   AnalyticsQualityJudgeExcluded   `json:"excluded"`
	Buckets    []AnalyticsQualityJudgeBucket   `json:"buckets"`
}

// DecodeAnalyticsQualityDoc decodes the document an Analytics*Quality statement returned,
// rejecting unknown keys and trailing data.
func DecodeAnalyticsQualityDoc(doc []byte) (AnalyticsQualityDoc, error) {
	var out AnalyticsQualityDoc
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return AnalyticsQualityDoc{}, fmt.Errorf("decode analytics quality document: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return AnalyticsQualityDoc{}, errors.New("decode analytics quality document: trailing data")
	}
	return out, nil
}
