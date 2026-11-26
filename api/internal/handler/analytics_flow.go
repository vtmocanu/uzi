package handler

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729: the Flow section of the scoped analytics API (GET /api/analytics/flow and
// GET /api/admin/analytics/flow). The request flow is the Spend one in serveAnalytics; only
// the statement and the mapping below differ.

// flow runs the Flow statement inside the snapshot: authorize, history start, the
// afterFirstRead hook, bounds, then the one owner or all-scope statement.
func (d *analyticsDeps) flow(ctx context.Context, s *store.AnalyticsSnapshot, req analyticsRequest, now time.Time) (apitypes.AnalyticsFlowDTO, error) {
	loc, err := d.authorize(ctx, s, req)
	if err != nil {
		return apitypes.AnalyticsFlowDTO{}, err
	}

	repo, harness := analyticsRepoHarness(req)
	var hist pgtype.Timestamptz
	if req.allUsers {
		hist, err = s.Q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{RepoID: repo, Harness: harness, Kinds: req.kinds})
	} else {
		hist, err = s.Q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: req.scopeUser, RepoID: repo, Harness: harness, Kinds: req.kinds})
	}
	if err != nil {
		return apitypes.AnalyticsFlowDTO{}, err
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
		return apitypes.AnalyticsFlowDTO{}, fmt.Errorf("compute bounds: %w", err)
	}

	stmt := buildFlowStatement(req, bounds)
	var raw []byte
	if stmt.owner != nil {
		raw, err = s.Q.AnalyticsOwnerFlow(ctx, *stmt.owner)
	} else {
		raw, err = s.Q.AnalyticsAllFlow(ctx, *stmt.all)
	}
	if err != nil {
		return apitypes.AnalyticsFlowDTO{}, err
	}
	doc, err := store.DecodeAnalyticsFlowDoc(raw)
	if err != nil {
		return apitypes.AnalyticsFlowDTO{}, err
	}
	return mapAnalyticsFlow(req, bounds, doc)
}

// analyticsFlowStatement is the one Flow statement a request runs: owner-shaped (always
// filtered to one user) or all-scope.
type analyticsFlowStatement struct {
	owner *store.AnalyticsOwnerFlowParams
	all   *store.AnalyticsAllFlowParams
}

// analyticsRepoHarness converts the optional repo and harness selectors to statement params.
func analyticsRepoHarness(req analyticsRequest) (pgtype.UUID, pgtype.Text) {
	var repo pgtype.UUID
	if req.repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: req.repo, Valid: true}
	}
	var harness pgtype.Text
	if req.harness != "" {
		harness = pgtype.Text{String: req.harness, Valid: true}
	}
	return repo, harness
}

// buildFlowStatement picks the statement and fills its parameters. Only an admin request
// spanning every user (req.allUsers) runs the all-scope statement; the owner route and an
// admin user=<uuid> request always run the owner statement for req.scopeUser.
func buildFlowStatement(req analyticsRequest, b analytics.Bounds) analyticsFlowStatement {
	repo, harness := analyticsRepoHarness(req)
	if req.allUsers {
		return analyticsFlowStatement{all: &store.AnalyticsAllFlowParams{
			Now: analyticsTS(b.Now), From: analyticsTS(b.From),
			BucketStarts: analyticsTSList(b.BucketStarts), BucketEnds: analyticsTSList(b.BucketEnds),
			HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN,
			RepoID: repo, Harness: harness, Kinds: req.kinds,
		}}
	}
	return analyticsFlowStatement{owner: &store.AnalyticsOwnerFlowParams{
		Now: analyticsTS(b.Now), From: analyticsTS(b.From),
		BucketStarts: analyticsTSList(b.BucketStarts), BucketEnds: analyticsTSList(b.BucketEnds),
		HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN,
		UserID: req.scopeUser, RepoID: repo, Harness: harness, Kinds: req.kinds,
	}}
}

// flowMapper keeps the first mapping error so a malformed document is a 500, never a
// silently dropped or zeroed value.
type flowMapper struct{ err error }

func (m *flowMapper) fail(format string, args ...any) {
	if m.err == nil {
		m.err = fmt.Errorf("analytics flow: "+format, args...)
	}
}

func (m *flowMapper) ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		m.fail("unparsable timestamp %q", s)
		return time.Time{}
	}
	return t.UTC()
}

func (m *flowMapper) tsPtr(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t := m.ts(*s)
	return &t
}

// checkFlowBuckets requires a per-bucket series to cover the calendar exactly once: want
// cells, every index in [0, want), no index repeated. A violation fails the mapping.
func checkFlowBuckets[T any](m *flowMapper, series string, cells []T, index func(T) int, want int) bool {
	if len(cells) != want {
		m.fail("%s has %d buckets, the calendar has %d", series, len(cells), want)
		return false
	}
	seen := make(map[int]bool, want)
	for _, c := range cells {
		i := index(c)
		if i < 0 || i >= want {
			m.fail("%s bucket index %d outside the calendar's %d buckets", series, i, want)
			return false
		}
		if seen[i] {
			m.fail("%s bucket index %d appears more than once", series, i)
			return false
		}
		seen[i] = true
	}
	return true
}

// nonNil returns an empty slice for nil so the wire value is [] and never null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func mapFlowOutcomes(c store.AnalyticsFlowOutcomeCounts) apitypes.AnalyticsFlowOutcomeCountsDTO {
	return apitypes.AnalyticsFlowOutcomeCountsDTO{
		Completed: c.Completed, Cancelled: c.Cancelled, PlanRejected: c.PlanRejected,
		BaseDiverged: c.BaseDiverged, Failed: c.Failed, Total: c.Total,
	}
}

// mapAnalyticsFlow builds the response from the calendar and the store document. The only
// value taken from the calendar rather than the document is issue_elapsed.buckets[i].partial.
func mapAnalyticsFlow(req analyticsRequest, b analytics.Bounds, doc store.AnalyticsFlowDoc) (apitypes.AnalyticsFlowDTO, error) {
	m := &flowMapper{}

	filters := apitypes.AnalyticsFlowFiltersDTO{Kinds: slices.Clone(nonNil(req.kinds))}
	if req.repo != uuid.Nil {
		r := req.repo.String()
		filters.Repo = &r
	}
	if req.harness != "" {
		h := req.harness
		filters.Harness = &h
	}

	out := apitypes.AnalyticsFlowDTO{
		AnalyticsEnvelopeDTO: analyticsEnvelope(req, b),
		SelectedUser:         req.scope().User,
		Filters:              filters,
	}

	w := doc.WaitingNow
	out.WaitingNow = apitypes.AnalyticsFlowWaitingDTO{
		TimeBasis: w.TimeBasis, Population: w.Population, Total: w.Total,
		States: make([]apitypes.AnalyticsFlowWaitStateDTO, 0, len(w.States)),
	}
	for _, st := range w.States {
		out.WaitingNow.States = append(out.WaitingNow.States, apitypes.AnalyticsFlowWaitStateDTO{
			State: st.State, Count: st.Count, OldestStatusSince: m.tsPtr(st.OldestStatusSince), OldestAgeSeconds: st.OldestAgeSeconds,
		})
	}

	cr := doc.Activity.Created
	out.Activity.Created = apitypes.AnalyticsFlowCreatedDTO{
		TimeBasis: cr.TimeBasis, Population: cr.Population, Total: cr.Total,
		Buckets: make([]apitypes.AnalyticsFlowCreatedCellDTO, 0, len(cr.Buckets)),
	}
	for _, c := range cr.Buckets {
		out.Activity.Created.Buckets = append(out.Activity.Created.Buckets, apitypes.AnalyticsFlowCreatedCellDTO{Index: c.Index, Count: c.Count})
	}
	fi := doc.Activity.Finished
	out.Activity.Finished = apitypes.AnalyticsFlowFinishedDTO{
		TimeBasis: fi.TimeBasis, Population: fi.Population, ExcludedKinds: slices.Clone(nonNil(fi.ExcludedKinds)),
		Total:   mapFlowOutcomes(fi.Total),
		Buckets: make([]apitypes.AnalyticsFlowFinishedCellDTO, 0, len(fi.Buckets)),
	}
	for _, c := range fi.Buckets {
		out.Activity.Finished.Buckets = append(out.Activity.Finished.Buckets, apitypes.AnalyticsFlowFinishedCellDTO{
			Index: c.Index, AnalyticsFlowOutcomeCountsDTO: mapFlowOutcomes(c.AnalyticsFlowOutcomeCounts),
		})
	}

	fs := doc.FirstStartDelay
	out.FirstStartDelay = apitypes.AnalyticsFlowFirstStartDTO{
		TimeBasis: fs.TimeBasis, Population: fs.Population, Approximate: fs.Approximate,
		Limitations: slices.Clone(nonNil(fs.Limitations)),
		Observed:    fs.Observed, NotStarted: fs.NotStarted, Invalid: fs.Invalid,
		P50Seconds: mapAnalyticsNumber(fs.P50Seconds), P90Seconds: mapAnalyticsNumber(fs.P90Seconds),
	}

	pr := doc.PlanResponse
	out.PlanResponse = apitypes.AnalyticsFlowPlanResponseDTO{
		TimeBasis: pr.TimeBasis, Population: pr.Population, Approximate: pr.Approximate, N: pr.N,
		P50Seconds: mapAnalyticsNumber(pr.P50Seconds), P90Seconds: mapAnalyticsNumber(pr.P90Seconds),
		Unknown: apitypes.AnalyticsFlowUnknownDTO{
			Total: pr.Unknown.Total,
			Reasons: apitypes.AnalyticsFlowResponseReasonsDTO{
				Ambiguous: pr.Unknown.Reasons.Ambiguous, NoPresentation: pr.Unknown.Reasons.NoPresentation,
				InvalidChronology: pr.Unknown.Reasons.InvalidChronology, ExcludedDisposition: pr.Unknown.Reasons.ExcludedDisposition,
				ReconciliationOnly: pr.Unknown.Reasons.ReconciliationOnly, LegacyUnbound: pr.Unknown.Reasons.LegacyUnbound,
			},
		},
		NotApproved: pr.NotApproved,
	}

	df := doc.DeliveryFunnel
	out.DeliveryFunnel = apitypes.AnalyticsFlowFunnelDTO{
		TimeBasis: df.TimeBasis, Population: df.Population, Applicable: df.Applicable, Reason: df.Reason,
		Stages: apitypes.AnalyticsFlowStagesDTO{
			Created: df.Stages.Created, PlanSettled: df.Stages.PlanSettled, Completed: df.Stages.Completed,
			PRPresent: df.Stages.PRPresent, Merged: df.Stages.Merged,
		},
		Planning: apitypes.AnalyticsFlowPlanningDTO{
			SettledHuman: df.Planning.SettledHuman, SettledAuto: df.Planning.SettledAuto, Skipped: df.Planning.Skipped,
			Rejected: df.Planning.Rejected, InProgress: df.Planning.InProgress, Failed: df.Planning.Failed,
			Cancelled: df.Planning.Cancelled, Unknown: df.Planning.Unknown,
		},
		Outcomes: apitypes.AnalyticsFlowOutcomesDTO{
			Completed: df.Outcomes.Completed, Failed: df.Outcomes.Failed, Cancelled: df.Outcomes.Cancelled,
			PlanRejected: df.Outcomes.PlanRejected, BaseDiverged: df.Outcomes.BaseDiverged, InProgress: df.Outcomes.InProgress,
		},
		Disclosure: apitypes.AnalyticsFlowFunnelDisclosureDTO{
			HistoricalApprovalOnly: df.Disclosure.HistoricalApprovalOnly, AmbiguousCurrent: df.Disclosure.AmbiguousCurrent,
			LegacyEvidence: df.Disclosure.LegacyEvidence,
		},
		PRUnits: apitypes.AnalyticsFlowPRUnitsDTO{
			Present: df.PRUnits.Present, Merged: df.PRUnits.Merged,
			Buckets: make([]apitypes.AnalyticsFlowPRUnitCellDTO, 0, len(df.PRUnits.Buckets)),
		},
	}
	for _, c := range df.PRUnits.Buckets {
		out.DeliveryFunnel.PRUnits.Buckets = append(out.DeliveryFunnel.PRUnits.Buckets, apitypes.AnalyticsFlowPRUnitCellDTO{
			Index: c.Index, Present: c.Present, Merged: c.Merged,
		})
	}

	ap := doc.Approval
	out.Approval = apitypes.AnalyticsFlowApprovalDTO{
		TimeBasis: ap.TimeBasis, Population: ap.Population, Applicable: ap.Applicable, Reason: ap.Reason,
		Total: ap.Total, Human: ap.Human, AutoApproveConfigured: ap.AutoApproveConfigured,
		Unknown: apitypes.AnalyticsFlowApprovalUnknownDTO{
			Total: ap.Unknown.Total,
			Reasons: apitypes.AnalyticsFlowApprovalReasonsDTO{
				Ambiguous: ap.Unknown.Reasons.Ambiguous, LegacyUnbound: ap.Unknown.Reasons.LegacyUnbound,
				HistoricalApprovalOnly: ap.Unknown.Reasons.HistoricalApprovalOnly, NoEvidence: ap.Unknown.Reasons.NoEvidence,
			},
		},
	}

	ie := doc.IssueElapsed
	out.IssueElapsed = apitypes.AnalyticsFlowElapsedDTO{
		TimeBasis: ie.TimeBasis, Population: ie.Population, Applicable: ie.Applicable, Reason: ie.Reason,
		N: ie.N, Unfinished: ie.Unfinished, Invalid: ie.Invalid, Incomplete: ie.Incomplete,
		P50Seconds: mapAnalyticsNumber(ie.P50Seconds), P90Seconds: mapAnalyticsNumber(ie.P90Seconds),
		Histogram: make([]apitypes.AnalyticsFlowHistogramBinDTO, 0, len(ie.Histogram)),
		Buckets:   make([]apitypes.AnalyticsFlowElapsedCellDTO, 0, len(ie.Buckets)),
	}
	for _, h := range ie.Histogram {
		out.IssueElapsed.Histogram = append(out.IssueElapsed.Histogram, apitypes.AnalyticsFlowHistogramBinDTO{
			LowerSeconds: h.LowerSeconds, UpperSeconds: h.UpperSeconds, Count: h.Count,
		})
	}
	elapsedOK := checkFlowBuckets(m, "issue_elapsed", ie.Buckets, func(c store.AnalyticsFlowElapsedCell) int { return c.Index }, len(b.Buckets))
	checkFlowBuckets(m, "activity.created", cr.Buckets, func(c store.AnalyticsFlowCreatedCell) int { return c.Index }, len(b.Buckets))
	checkFlowBuckets(m, "activity.finished", fi.Buckets, func(c store.AnalyticsFlowFinishedCell) int { return c.Index }, len(b.Buckets))
	checkFlowBuckets(m, "delivery_funnel.pr_units", df.PRUnits.Buckets, func(c store.AnalyticsFlowPRUnitCell) int { return c.Index }, len(b.Buckets))
	if !elapsedOK {
		ie.Buckets = nil // the partial flag is looked up by index, so an invalid series maps no cells
	}
	for _, c := range ie.Buckets {
		out.IssueElapsed.Buckets = append(out.IssueElapsed.Buckets, apitypes.AnalyticsFlowElapsedCellDTO{
			Index: c.Index, Partial: b.Buckets[c.Index].Partial,
			N: c.N, Unfinished: c.Unfinished, Invalid: c.Invalid, Incomplete: c.Incomplete,
			P50Seconds: mapAnalyticsNumber(c.P50Seconds), P90Seconds: mapAnalyticsNumber(c.P90Seconds),
		})
	}

	if m.err != nil {
		return apitypes.AnalyticsFlowDTO{}, m.err
	}
	return out, nil
}
