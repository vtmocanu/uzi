package handler

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4730: the Quality read. It reuses the analytics boundary of analytics.go (parsing,
// admission, snapshot, authority checks, calendar) and adds one statement, its decoder and the
// DTO mapping below. Quality has no previous window and no rolling series. The only repo id it
// returns is the caller's own repo filter, echoed in effective_filters after authorize has
// confirmed access; it emits no per-repo rows or paths.

// Time bases and fixed strings of the Quality DTO.
const (
	qualityTimeBasisRework     = "pr_first_cohort_created_at"
	qualityTimeBasisCIFix      = "created_at"
	qualityTimeBasisCrossCheck = "decided_at"
	qualityTimeBasisPending    = "live"
	qualityTimeBasisJudge      = "target_created_at"

	qualityWindowNotApplied = "not_applied"
	qualityReasonNotTracked = "not_tracked"
	qualityRetention        = "retained_runs_only"
	qualityJudgeSelection   = "retained_target_properties"
)

// qualityJudgeLimitations are the selection limits the judge denominator cannot remove: it is
// derived from retained target properties, not from what could have been judged at the time.
var qualityJudgeLimitations = []string{
	"credential_availability_not_historical",
	"consent_not_historical",
	"spend_guards_not_historical",
}

// AnalyticsQuality serves GET /api/analytics/quality for the caller's own scope.
func (h *Handler) AnalyticsQuality(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, false, analytics.SectionQuality)
}

// AdminAnalyticsQuality serves GET /api/admin/analytics/quality.
func (h *Handler) AdminAnalyticsQuality(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, true, analytics.SectionQuality)
}

func (d *analyticsDeps) quality(ctx context.Context, s *store.AnalyticsSnapshot, req analyticsRequest, now time.Time) (apitypes.AnalyticsQualityDTO, error) {
	if d.qualityBeforeSnapshotRead != nil {
		d.qualityBeforeSnapshotRead(ctx)
	}
	loc, err := d.authorize(ctx, s, req)
	if err != nil {
		return apitypes.AnalyticsQualityDTO{}, err
	}

	var repo pgtype.UUID
	if req.repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: req.repo, Valid: true}
	}
	var harness pgtype.Text
	if req.harness != "" {
		harness = pgtype.Text{String: req.harness, Valid: true}
	}
	var hist pgtype.Timestamptz
	if req.allUsers {
		hist, err = s.Q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{RepoID: repo, Harness: harness, Kinds: req.kinds})
	} else {
		hist, err = s.Q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: req.scopeUser, RepoID: repo, Harness: harness, Kinds: req.kinds})
	}
	if err != nil {
		return apitypes.AnalyticsQualityDTO{}, err
	}
	if d.afterFirstRead != nil {
		d.afterFirstRead(ctx)
	}
	var earliest *time.Time
	if hist.Valid {
		e := hist.Time
		earliest = &e
	}
	bounds, err := analytics.ComputeBounds(now, loc, req.window, earliest)
	if err != nil {
		return apitypes.AnalyticsQualityDTO{}, fmt.Errorf("compute bounds: %w", err)
	}

	stmt := buildQualityStatement(req, bounds)
	var raw []byte
	if stmt.owner != nil {
		raw, err = s.Q.AnalyticsOwnerQuality(ctx, *stmt.owner)
	} else {
		raw, err = s.Q.AnalyticsAllQuality(ctx, *stmt.all)
	}
	if err != nil {
		return apitypes.AnalyticsQualityDTO{}, err
	}
	doc, err := store.DecodeAnalyticsQualityDoc(raw)
	if err != nil {
		return apitypes.AnalyticsQualityDTO{}, err
	}
	return mapAnalyticsQuality(req, bounds, doc), nil
}

// analyticsQualityStatement is the one Quality statement a request runs: owner-shaped (always
// filtered to one user, including an admin's selected user) or all-scope.
type analyticsQualityStatement struct {
	owner *store.AnalyticsOwnerQualityParams
	all   *store.AnalyticsAllQualityParams
}

// qualityJudgeKinds are the run kinds the judge may review.
func qualityJudgeKinds() []string {
	var out []string
	for _, k := range runkind.All() {
		if runkind.JudgeEligible(k) {
			out = append(out, k)
		}
	}
	return out
}

// qualityPlanLeadKinds and qualityCodeLeadKinds are the lead kinds whose plan (code) cross-check
// attempts the Quality statement counts, so the population matches qualityApplicable.
func qualityPlanLeadKinds() []string { return qualityKindsWhere(runkind.PlanCrossCheckable) }

func qualityCodeLeadKinds() []string { return qualityKindsWhere(runkind.CodeCrossCheckable) }

func qualityKindsWhere(pred func(string) bool) []string {
	out := []string{}
	for _, k := range runkind.All() {
		if pred(k) {
			out = append(out, k)
		}
	}
	return out
}

func buildQualityStatement(req analyticsRequest, b analytics.Bounds) analyticsQualityStatement {
	var repo pgtype.UUID
	if req.repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: req.repo, Valid: true}
	}
	var harness pgtype.Text
	if req.harness != "" {
		harness = pgtype.Text{String: req.harness, Valid: true}
	}
	always, prestart := workersvc.JudgeSkipFailOrigins()
	starts, ends := analyticsTSList(b.BucketStarts), analyticsTSList(b.BucketEnds)
	if req.allUsers {
		return analyticsQualityStatement{all: &store.AnalyticsAllQualityParams{
			BucketStarts: starts, BucketEnds: ends, From: analyticsTS(b.From), Now: analyticsTS(b.Now),
			RepoID: repo, Harness: harness, Kinds: req.kinds, DeliveryKinds: runkind.Delivery(),
			AlwaysSkipOrigins: always, PrestartOrigins: prestart, JudgeKinds: qualityJudgeKinds(),
			PlanLeadKinds: qualityPlanLeadKinds(), CodeLeadKinds: qualityCodeLeadKinds(),
		}}
	}
	return analyticsQualityStatement{owner: &store.AnalyticsOwnerQualityParams{
		BucketStarts: starts, BucketEnds: ends, From: analyticsTS(b.From), Now: analyticsTS(b.Now),
		UserID: req.scopeUser,
		RepoID: repo, Harness: harness, Kinds: req.kinds, DeliveryKinds: runkind.Delivery(),
		AlwaysSkipOrigins: always, PrestartOrigins: prestart, JudgeKinds: qualityJudgeKinds(),
		PlanLeadKinds: qualityPlanLeadKinds(), CodeLeadKinds: qualityCodeLeadKinds(),
	}}
}

// qualityApplicability reports, per measure, whether the request's kind filter leaves any kind
// the measure's population can consist of.
type qualityApplicability struct {
	rework, ciFix, plan, code, judge bool
}

func qualityApplicable(kinds []string) qualityApplicability {
	anyKind := func(pred func(string) bool) bool { return slices.ContainsFunc(kinds, pred) }
	delivery := runkind.Delivery()
	return qualityApplicability{
		rework: anyKind(func(k string) bool { return slices.Contains(delivery, k) }),
		ciFix:  slices.Contains(kinds, runkind.CIFix),
		plan:   anyKind(runkind.PlanCrossCheckable),
		code:   anyKind(runkind.CodeCrossCheckable),
		judge:  anyKind(runkind.JudgeEligible),
	}
}

func qualitySection(applicable bool, basis string) apitypes.AnalyticsQualitySectionDTO {
	sec := apitypes.AnalyticsQualitySectionDTO{Applicable: applicable, TimeBasis: basis}
	if !applicable {
		reason := store.AnalyticsReasonNotApplicable
		sec.Reason = &reason
	}
	return sec
}

// qualityEnvelope is the shared calendar of a Quality response: no previous window, no rolling
// series (rolling travels as []).
func qualityEnvelope(req analyticsRequest, b analytics.Bounds) apitypes.AnalyticsEnvelopeDTO {
	env := apitypes.AnalyticsEnvelopeDTO{
		Scope:           req.scope(),
		GeneratedAt:     b.Now.UTC(),
		Window:          string(req.window),
		Timezone:        req.tzName,
		From:            b.From.UTC(),
		To:              b.Now.UTC(),
		BucketWidthDays: 1,
		Buckets:         make([]apitypes.AnalyticsBucketDTO, 0, len(b.Buckets)),
		Rolling:         []apitypes.AnalyticsRollingDTO{},
	}
	if b.WidthWeeks > 0 {
		env.BucketWidthDays = 7 * b.WidthWeeks
	}
	if b.HistoryStart != nil {
		hs := b.HistoryStart.UTC()
		env.HistoryStart = &hs
	}
	for _, bk := range b.Buckets {
		env.Buckets = append(env.Buckets, apitypes.AnalyticsBucketDTO{
			Label: bk.Label.String(), Start: bk.Start.UTC(), End: bk.End.UTC(), Partial: bk.Partial,
		})
	}
	return env
}

func qualityReworkGroups(g store.AnalyticsQualityReworkGroups) apitypes.AnalyticsQualityReworkGroupsDTO {
	return apitypes.AnalyticsQualityReworkGroupsDTO{Zero: g.Zero, One: g.One, Two: g.Two, ThreePlus: g.ThreePlus}
}

func qualityOutcomes(o store.AnalyticsQualityOutcomes) apitypes.AnalyticsOutcomesDTO {
	return apitypes.AnalyticsOutcomesDTO{
		Completed: o.Completed, Cancelled: o.Cancelled, PlanRejected: o.PlanRejected, BaseDiverged: o.BaseDiverged, Failed: o.Failed,
	}
}

func qualityPlanResults(r store.AnalyticsQualityPlanResults) apitypes.AnalyticsQualityPlanResultsDTO {
	return apitypes.AnalyticsQualityPlanResultsDTO{Approve: r.Approve, Revise: r.Revise, Block: r.Block, Failed: r.Failed}
}

func qualityCodeResults(r store.AnalyticsQualityCodeResults) apitypes.AnalyticsQualityCodeResultsDTO {
	return apitypes.AnalyticsQualityCodeResultsDTO{Completed: r.Completed, Failed: r.Failed}
}

func qualityClasses(c store.AnalyticsQualityClasses) apitypes.AnalyticsQualityClassesDTO {
	return apitypes.AnalyticsQualityClassesDTO{
		Passed: c.Passed, RaisedIssues: c.RaisedIssues, DidntFinish: c.DidntFinish, Replaced: c.Replaced, Unclassified: c.Unclassified,
	}
}

func qualityCheckerHarness(c store.AnalyticsQualityCheckerHarness) apitypes.AnalyticsQualityCheckerHarnessDTO {
	return apitypes.AnalyticsQualityCheckerHarnessDTO{Claude: c.Claude, Codex: c.Codex, Unknown: c.Unknown}
}

// qualityCoverage is complete over eligible, or null with no_applicable_runs when nothing is eligible.
func qualityCoverage(complete, eligible int64) apitypes.AnalyticsNullableNumberDTO {
	if eligible <= 0 {
		reason := store.AnalyticsReasonNoApplicableRuns
		return apitypes.AnalyticsNullableNumberDTO{Reason: &reason}
	}
	v := float64(complete) / float64(eligible)
	return apitypes.AnalyticsNullableNumberDTO{Value: &v}
}

func qualityNotTracked() apitypes.AnalyticsNullableNumberDTO {
	reason := qualityReasonNotTracked
	return apitypes.AnalyticsNullableNumberDTO{Reason: &reason}
}

// mapAnalyticsQuality builds the response from the calendar and the store document. Every slice
// is non-nil so it serializes as [] and never null.
func mapAnalyticsQuality(req analyticsRequest, b analytics.Bounds, doc store.AnalyticsQualityDoc) apitypes.AnalyticsQualityDTO {
	app := qualityApplicable(req.kinds)
	out := apitypes.AnalyticsQualityDTO{
		AnalyticsEnvelopeDTO: qualityEnvelope(req, b),
		SelectedUser:         req.scope().User,
	}

	rw := doc.Rework
	out.Rework = apitypes.AnalyticsQualityReworkDTO{
		AnalyticsQualitySectionDTO: qualitySection(app.rework, qualityTimeBasisRework),
		ApplicablePRs:              rw.ApplicablePRs,
		Groups:                     qualityReworkGroups(rw.Groups),
		Buckets:                    make([]apitypes.AnalyticsQualityReworkBucketDTO, 0, len(rw.Buckets)),
		UnwitnessedPRs:             rw.UnwitnessedPRs,
		UnverifiedLinks:            rw.UnverifiedLinks,
		SharedExternalPRs:          rw.SharedExternalPRs,
		SharedDetectionUnavailable: rw.SharedDetectionUnavailable,
		Retention:                  qualityRetention,
		MergeDayActivity:           qualityNotTracked(),
		RunEndToMergeSeconds:       qualityNotTracked(),
	}
	for _, bk := range rw.Buckets {
		out.Rework.Buckets = append(out.Rework.Buckets, apitypes.AnalyticsQualityReworkBucketDTO{
			Index: bk.Index, PRs: bk.PRs, Groups: qualityReworkGroups(bk.Groups),
		})
	}

	cf := doc.CIFix
	out.CIFix = apitypes.AnalyticsQualityCIFixDTO{
		AnalyticsQualitySectionDTO: qualitySection(app.ciFix, qualityTimeBasisCIFix),
		Runs:                       cf.Runs,
		Finished:                   cf.Finished,
		InFlight:                   cf.InFlight,
		Outcomes:                   qualityOutcomes(cf.Outcomes),
		Buckets:                    make([]apitypes.AnalyticsQualityCIFixBucketDTO, 0, len(cf.Buckets)),
	}
	for _, bk := range cf.Buckets {
		out.CIFix.Buckets = append(out.CIFix.Buckets, apitypes.AnalyticsQualityCIFixBucketDTO{
			Index: bk.Index, Runs: bk.Runs, Finished: bk.Finished, InFlight: bk.InFlight, Outcomes: qualityOutcomes(bk.Outcomes),
		})
	}

	pl := doc.CrossCheck.Plan
	out.CrossCheck.Plan = apitypes.AnalyticsQualityPlanStageDTO{
		AnalyticsQualitySectionDTO: qualitySection(app.plan, qualityTimeBasisCrossCheck),
		DecidedInWindow:            pl.DecidedInWindow,
		StoredResult:               qualityPlanResults(pl.StoredResult),
		Classes:                    qualityClasses(pl.Classes),
		CheckerHarness:             qualityCheckerHarness(pl.CheckerHarness),
		Undated: apitypes.AnalyticsQualityPlanUndatedDTO{
			Window: qualityWindowNotApplied, Count: pl.Undated.Count, StoredResult: qualityPlanResults(pl.Undated.StoredResult),
		},
		Buckets: make([]apitypes.AnalyticsQualityPlanBucketDTO, 0, len(pl.Buckets)),
	}
	for _, bk := range pl.Buckets {
		out.CrossCheck.Plan.Buckets = append(out.CrossCheck.Plan.Buckets, apitypes.AnalyticsQualityPlanBucketDTO{
			Index: bk.Index, Decided: bk.Decided, StoredResult: qualityPlanResults(bk.StoredResult), Classes: qualityClasses(bk.Classes),
		})
	}

	cd := doc.CrossCheck.Code
	out.CrossCheck.Code = apitypes.AnalyticsQualityCodeStageDTO{
		AnalyticsQualitySectionDTO: qualitySection(app.code, qualityTimeBasisCrossCheck),
		DecidedInWindow:            cd.DecidedInWindow,
		StoredResult:               qualityCodeResults(cd.StoredResult),
		Classes:                    qualityClasses(cd.Classes),
		CheckerHarness:             qualityCheckerHarness(cd.CheckerHarness),
		Undated: apitypes.AnalyticsQualityCodeUndatedDTO{
			Window: qualityWindowNotApplied, Count: cd.Undated.Count, StoredResult: qualityCodeResults(cd.Undated.StoredResult),
		},
		Buckets: make([]apitypes.AnalyticsQualityCodeBucketDTO, 0, len(cd.Buckets)),
	}
	for _, bk := range cd.Buckets {
		out.CrossCheck.Code.Buckets = append(out.CrossCheck.Code.Buckets, apitypes.AnalyticsQualityCodeBucketDTO{
			Index: bk.Index, Decided: bk.Decided, StoredResult: qualityCodeResults(bk.StoredResult), Classes: qualityClasses(bk.Classes),
		})
	}

	pd := doc.CrossCheck.Pending
	filters := apitypes.AnalyticsQualityEffectiveFiltersDTO{Kinds: slices.Clone(req.kinds)}
	if req.repo != uuid.Nil {
		id := req.repo.String()
		filters.Repo = &id
	}
	if req.harness != "" {
		h := req.harness
		filters.Harness = &h
	}
	out.CrossCheck.Pending = apitypes.AnalyticsQualityPendingDTO{
		AnalyticsQualitySectionDTO: qualitySection(app.plan || app.code, qualityTimeBasisPending),
		Window:                     qualityWindowNotApplied,
		Total:                      pd.Total,
		Plan:                       pd.Plan,
		Code:                       pd.Code,
		EffectiveFilters:           filters,
	}

	jd := doc.Judge
	out.Judge = apitypes.AnalyticsQualityJudgeDTO{
		AnalyticsQualitySectionDTO: qualitySection(app.judge, qualityTimeBasisJudge),
		Candidates: apitypes.AnalyticsQualityJudgeCandidatesDTO{
			Total: jd.Candidates.Total, Completed: jd.Candidates.Completed, Failed: jd.Candidates.Failed,
		},
		Eligible: apitypes.AnalyticsQualityJudgeEligibleDTO{
			Total:             jd.Eligible.Total,
			Unreviewed:        jd.Eligible.Unreviewed,
			UnreviewedSkipped: jd.Eligible.UnreviewedSkipped,
			Failed:            jd.Eligible.Failed,
			Complete: apitypes.AnalyticsQualityJudgeCompleteDTO{
				Total: jd.Eligible.Complete.Total, Ideal: jd.Eligible.Complete.Ideal, OK: jd.Eligible.Complete.OK, Issues: jd.Eligible.Complete.Issues,
			},
		},
		Excluded: apitypes.AnalyticsQualityJudgeExcludedDTO{
			Cancelled: jd.Excluded.Cancelled, InFlight: jd.Excluded.InFlight, SkipOrigin: jd.Excluded.SkipOrigin, ReviewedExcluded: jd.Excluded.ReviewedExcluded,
		},
		Coverage:    qualityCoverage(jd.Eligible.Complete.Total, jd.Eligible.Total),
		Buckets:     make([]apitypes.AnalyticsQualityJudgeBucketDTO, 0, len(jd.Buckets)),
		Selection:   qualityJudgeSelection,
		Limitations: slices.Clone(qualityJudgeLimitations),
	}
	for _, bk := range jd.Buckets {
		out.Judge.Buckets = append(out.Judge.Buckets, apitypes.AnalyticsQualityJudgeBucketDTO{
			Index: bk.Index, Eligible: bk.Eligible, Complete: bk.Complete, Coverage: qualityCoverage(bk.Complete, bk.Eligible),
		})
	}
	return out
}
