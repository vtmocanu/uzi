package handler

import (
	"context"
	"errors"
	"fmt"
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

// Issue #4685: the Reliability section of the scoped analytics API. The request runs the same
// admission and one REPEATABLE READ snapshot as Spend; inside it the reads are, in order,
// authorize, the history start, the cohort statement and the latest-failure statement, all on
// the snapshot's queries, so every figure of the response describes one database state.

const (
	// analyticsReasonSupportingOnly is the reason of an inapplicable response: the kind filter
	// selected only supporting kinds (judge, cross_check, chat), which have no delivery outcome.
	analyticsReasonSupportingOnly = "supporting_kinds_only"
	// analyticsNeedsLandingCaveat qualifies needs_landing: the work may have landed by other means.
	analyticsNeedsLandingCaveat = "work_may_already_have_landed"
)

// analyticsRollHarnesses are the harnesses that get their own rolling failure-rate series.
func analyticsRollHarnesses() []string { return slices.Clone(analyticsHarnesses) }

// reliabilityKinds is the request's kind filter restricted to the Delivery kinds. The store's
// cohort does not filter fn_outcome_kind itself, so this intersection is what keeps judge,
// cross_check and chat runs out of every figure.
func reliabilityKinds(kinds []string) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range runkind.Delivery() {
		if slices.Contains(kinds, k) {
			out = append(out, k)
		}
	}
	return out
}

// analyticsFilterArgs converts the request's repo and harness cohort filters to statement
// arguments; a NULL argument means the filter is off.
func analyticsFilterArgs(req analyticsRequest) (pgtype.UUID, pgtype.Text) {
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

func (d *analyticsDeps) reliability(ctx context.Context, s *store.AnalyticsSnapshot, req analyticsRequest, now time.Time) (apitypes.AnalyticsReliabilityDTO, error) {
	loc, err := d.authorize(ctx, s, req)
	if err != nil {
		return apitypes.AnalyticsReliabilityDTO{}, err
	}

	kinds := reliabilityKinds(req.kinds)
	if len(kinds) == 0 {
		// No delivery kind selected: authorize has run, but no history, cohort or latest-failure
		// statement runs, and the response says so instead of reporting a zero failure rate.
		bounds, err := analytics.ComputeBounds(now, loc, req.window, nil)
		if err != nil {
			return apitypes.AnalyticsReliabilityDTO{}, fmt.Errorf("compute bounds: %w", err)
		}
		return mapAnalyticsReliabilityInapplicable(req, bounds), nil
	}

	repo, harness := analyticsFilterArgs(req)
	var hist pgtype.Timestamptz
	if req.allUsers {
		hist, err = s.Q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{RepoID: repo, Harness: harness, Kinds: kinds})
	} else {
		hist, err = s.Q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: req.scopeUser, RepoID: repo, Harness: harness, Kinds: kinds})
	}
	if err != nil {
		return apitypes.AnalyticsReliabilityDTO{}, err
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
		return apitypes.AnalyticsReliabilityDTO{}, fmt.Errorf("compute bounds: %w", err)
	}

	var raw []byte
	if req.allUsers {
		raw, err = s.Q.AnalyticsAllReliability(ctx, buildAllReliabilityParams(req, bounds, kinds))
	} else {
		raw, err = s.Q.AnalyticsOwnerReliability(ctx, buildOwnerReliabilityParams(req, bounds, kinds))
	}
	if err != nil {
		return apitypes.AnalyticsReliabilityDTO{}, err
	}
	if d.afterCohortRead != nil {
		d.afterCohortRead(ctx)
	}

	var latest store.AnalyticsOwnerLatestFailureRow
	if req.allUsers {
		// The all-scope row type has the owner row's fields, so it converts directly.
		var row store.AnalyticsAllLatestFailureRow
		row, err = s.Q.AnalyticsAllLatestFailure(ctx, allLatestFailureParams(req, kinds))
		latest = store.AnalyticsOwnerLatestFailureRow(row)
	} else {
		latest, err = s.Q.AnalyticsOwnerLatestFailure(ctx, ownerLatestFailureParams(req, kinds))
	}
	if err != nil {
		return apitypes.AnalyticsReliabilityDTO{}, err
	}

	doc, err := store.DecodeAnalyticsReliabilityDoc(raw)
	if err != nil {
		return apitypes.AnalyticsReliabilityDTO{}, err
	}
	return mapAnalyticsReliability(req, bounds, doc, latest)
}

func ownerLatestFailureParams(req analyticsRequest, kinds []string) store.AnalyticsOwnerLatestFailureParams {
	repo, harness := analyticsFilterArgs(req)
	return store.AnalyticsOwnerLatestFailureParams{UserID: req.scopeUser, RepoID: repo, Harness: harness, Kinds: kinds}
}

func allLatestFailureParams(req analyticsRequest, kinds []string) store.AnalyticsAllLatestFailureParams {
	repo, harness := analyticsFilterArgs(req)
	return store.AnalyticsAllLatestFailureParams{RepoID: repo, Harness: harness, Kinds: kinds}
}

// buildOwnerReliabilityParams fills the owner-shaped cohort statement. RevealRepos comes only
// from which route was mounted (req.admin), never from the query: the owner route always masks
// repos the caller has no connection to.
func buildOwnerReliabilityParams(req analyticsRequest, b analytics.Bounds, kinds []string) store.AnalyticsOwnerReliabilityParams {
	repo, harness := analyticsFilterArgs(req)
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = analyticsTS(*b.PreviousFrom)
	}
	return store.AnalyticsOwnerReliabilityParams{
		From: analyticsTS(b.From), PreviousFrom: prev, Breakdown: req.breakdown,
		Now: analyticsTS(b.Now), FoldFrom: analyticsTS(b.FoldFrom),
		BucketStarts: analyticsTSList(b.BucketStarts), BucketEnds: analyticsTSList(b.BucketEnds),
		RollStarts: analyticsTSList(b.RollStarts), RollEnds: analyticsTSList(b.RollEnds),
		RollHarnesses: analyticsRollHarnesses(), LandableOrigins: workersvc.AllHumanLandableFailOrigins(),
		RevealRepos: req.admin, UserID: req.scopeUser,
		RepoID: repo, Harness: harness, Kinds: kinds,
	}
}

// buildAllReliabilityParams fills the all-scope cohort statement (admin, every user).
func buildAllReliabilityParams(req analyticsRequest, b analytics.Bounds, kinds []string) store.AnalyticsAllReliabilityParams {
	repo, harness := analyticsFilterArgs(req)
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = analyticsTS(*b.PreviousFrom)
	}
	return store.AnalyticsAllReliabilityParams{
		From: analyticsTS(b.From), PreviousFrom: prev, Breakdown: req.breakdown,
		Now: analyticsTS(b.Now), FoldFrom: analyticsTS(b.FoldFrom),
		BucketStarts: analyticsTSList(b.BucketStarts), BucketEnds: analyticsTSList(b.BucketEnds),
		RollStarts: analyticsTSList(b.RollStarts), RollEnds: analyticsTSList(b.RollEnds),
		RollHarnesses: analyticsRollHarnesses(), LandableOrigins: workersvc.AllHumanLandableFailOrigins(),
		RepoID: repo, Harness: harness, Kinds: kinds,
	}
}

func mapAnalyticsOutcomes(o store.AnalyticsOutcomes) apitypes.AnalyticsOutcomesDTO {
	return apitypes.AnalyticsOutcomesDTO{
		Completed: o.Completed, Cancelled: o.Cancelled, PlanRejected: o.PlanRejected,
		BaseDiverged: o.BaseDiverged, Failed: o.Failed,
	}
}

func mapAnalyticsReliabilityBlock(b store.AnalyticsReliabilityBlock) apitypes.AnalyticsReliabilityBlockDTO {
	return apitypes.AnalyticsReliabilityBlockDTO{
		Finished: b.Finished, Outcomes: mapAnalyticsOutcomes(b.Outcomes),
		FailureRate: mapAnalyticsNumber(b.FailureRate), RequeuedRuns: b.RequeuedRuns,
	}
}

func mapAnalyticsReliabilityPoints(in []store.AnalyticsReliabilityPoint) []apitypes.AnalyticsReliabilityPointDTO {
	out := make([]apitypes.AnalyticsReliabilityPointDTO, 0, len(in))
	for _, p := range in {
		out = append(out, apitypes.AnalyticsReliabilityPointDTO{Index: p.Index, Finished: p.Finished, Failed: p.Failed, Value: p.Value, Reason: p.Reason})
	}
	return out
}

// mapAnalyticsReliability builds the response from the calendar, the cohort document and the
// latest-failure row. A document whose bucket or rolling arrays do not match the calendar it was
// computed against is a 500, never a misaligned chart.
func mapAnalyticsReliability(req analyticsRequest, b analytics.Bounds, doc store.AnalyticsReliabilityDoc, latest store.AnalyticsOwnerLatestFailureRow) (apitypes.AnalyticsReliabilityDTO, error) {
	if len(doc.Buckets) != len(b.Buckets) || len(doc.Rolling.Overall) != len(b.Rolling) {
		return apitypes.AnalyticsReliabilityDTO{}, fmt.Errorf("analytics: reliability document has %d buckets and %d rolling points for a calendar of %d and %d",
			len(doc.Buckets), len(doc.Rolling.Overall), len(b.Buckets), len(b.Rolling))
	}
	if len(doc.Rolling.Harnesses) != len(analyticsHarnesses) {
		return apitypes.AnalyticsReliabilityDTO{}, fmt.Errorf("analytics: reliability document has %d harness series, want %d", len(doc.Rolling.Harnesses), len(analyticsHarnesses))
	}
	for i, h := range doc.Rolling.Harnesses {
		if h.Harness != analyticsHarnesses[i] || len(h.Points) != len(b.Rolling) {
			return apitypes.AnalyticsReliabilityDTO{}, fmt.Errorf("analytics: reliability harness series %d is %q with %d points, want %q with %d",
				i, h.Harness, len(h.Points), analyticsHarnesses[i], len(b.Rolling))
		}
	}
	out := apitypes.AnalyticsReliabilityDTO{
		AnalyticsEnvelopeDTO: analyticsEnvelope(req, b),
		SelectedUser:         req.scope().User,
		Applicable:           true,
		NeedsLanding:         apitypes.AnalyticsReliabilityNeedsLandingDTO{Runs: doc.NeedsLanding, Caveat: analyticsNeedsLandingCaveat},
	}
	out.Totals.Current = mapAnalyticsReliabilityBlock(doc.Totals.Current)
	if doc.Totals.Previous != nil {
		prev := mapAnalyticsReliabilityBlock(*doc.Totals.Previous)
		out.Totals.Previous = &prev
	}

	out.Series = make([]apitypes.AnalyticsReliabilityCellDTO, 0, len(doc.Buckets))
	for _, c := range doc.Buckets {
		out.Series = append(out.Series, apitypes.AnalyticsReliabilityCellDTO{Index: c.Index, Finished: c.Finished, Outcomes: mapAnalyticsOutcomes(c.Outcomes)})
	}

	out.Causes = apitypes.AnalyticsReliabilityCausesDTO{Failed: doc.Causes.Failed, Items: make([]apitypes.AnalyticsReliabilityCauseDTO, 0, len(doc.Causes.Items))}
	for _, c := range doc.Causes.Items {
		out.Causes.Items = append(out.Causes.Items, apitypes.AnalyticsReliabilityCauseDTO{Rank: c.Rank, IsOther: c.IsOther, Origin: c.Origin, Runs: c.Runs, Share: c.Share})
	}

	out.RollingFailureRate = apitypes.AnalyticsReliabilityRollingDTO{
		Overall:   mapAnalyticsReliabilityPoints(doc.Rolling.Overall),
		Harnesses: make([]apitypes.AnalyticsReliabilityHarnessSeriesDTO, 0, len(doc.Rolling.Harnesses)),
	}
	for _, h := range doc.Rolling.Harnesses {
		out.RollingFailureRate.Harnesses = append(out.RollingFailureRate.Harnesses,
			apitypes.AnalyticsReliabilityHarnessSeriesDTO{Harness: h.Harness, Points: mapAnalyticsReliabilityPoints(h.Points)})
	}

	out.Breakdown = apitypes.AnalyticsReliabilityBreakdownDTO{
		Dimension: doc.Breakdown.Dimension,
		Rows:      make([]apitypes.AnalyticsReliabilityRowDTO, 0, len(doc.Breakdown.Rows)),
	}
	for _, r := range doc.Breakdown.Rows {
		out.Breakdown.Rows = append(out.Breakdown.Rows, apitypes.AnalyticsReliabilityRowDTO{
			Rank: r.Rank, IsOther: r.IsOther, Key: r.Key, Label: r.Label, ID: uuidStringPtr(r.ID),
			N: r.N, Outcomes: mapAnalyticsOutcomes(r.Outcomes), FailureRate: mapAnalyticsNumber(r.FailureRate), RequeuedRuns: r.RequeuedRuns,
		})
	}

	if latest.Found {
		if !latest.FailedAt.Valid {
			return apitypes.AnalyticsReliabilityDTO{}, errors.New("analytics: latest failure found without a timestamp")
		}
		out.LatestFailure = &apitypes.AnalyticsReliabilityLatestFailureDTO{
			FailedAt: latest.FailedAt.Time.UTC(), Origin: latest.Origin, RunID: latest.RunID.String(),
			UserID: latest.FailedUserID.String(), CompletedSinceLastFailure: latest.CompletedSinceLastFailure,
		}
	}
	return out, nil
}

// mapAnalyticsReliabilityInapplicable builds the response for a kind filter that selects no
// delivery kind: zero counts, every rate null with reason not_applicable, no latest failure.
func mapAnalyticsReliabilityInapplicable(req analyticsRequest, b analytics.Bounds) apitypes.AnalyticsReliabilityDTO {
	na := func() *string { s := store.AnalyticsReasonNotApplicable; return &s }
	block := func() apitypes.AnalyticsReliabilityBlockDTO {
		return apitypes.AnalyticsReliabilityBlockDTO{FailureRate: apitypes.AnalyticsNullableNumberDTO{Reason: na()}}
	}
	points := func() []apitypes.AnalyticsReliabilityPointDTO {
		pts := make([]apitypes.AnalyticsReliabilityPointDTO, 0, len(b.Rolling))
		for i := range b.Rolling {
			pts = append(pts, apitypes.AnalyticsReliabilityPointDTO{Index: i, Reason: na()})
		}
		return pts
	}
	reason := analyticsReasonSupportingOnly
	out := apitypes.AnalyticsReliabilityDTO{
		AnalyticsEnvelopeDTO: analyticsEnvelope(req, b),
		SelectedUser:         req.scope().User,
		Applicable:           false,
		Reason:               &reason,
		Series:               make([]apitypes.AnalyticsReliabilityCellDTO, 0, len(b.Buckets)),
		Causes:               apitypes.AnalyticsReliabilityCausesDTO{Items: []apitypes.AnalyticsReliabilityCauseDTO{}},
		RollingFailureRate: apitypes.AnalyticsReliabilityRollingDTO{
			Overall:   points(),
			Harnesses: make([]apitypes.AnalyticsReliabilityHarnessSeriesDTO, 0, len(analyticsHarnesses)),
		},
		Breakdown:    apitypes.AnalyticsReliabilityBreakdownDTO{Dimension: req.breakdown, Rows: []apitypes.AnalyticsReliabilityRowDTO{}},
		NeedsLanding: apitypes.AnalyticsReliabilityNeedsLandingDTO{Caveat: analyticsNeedsLandingCaveat},
	}
	out.Totals.Current = block()
	if b.PreviousFrom != nil {
		prev := block()
		out.Totals.Previous = &prev
	}
	for i := range b.Buckets {
		out.Series = append(out.Series, apitypes.AnalyticsReliabilityCellDTO{Index: i})
	}
	for _, h := range analyticsRollHarnesses() {
		out.RollingFailureRate.Harnesses = append(out.RollingFailureRate.Harnesses, apitypes.AnalyticsReliabilityHarnessSeriesDTO{Harness: h, Points: points()})
	}
	return out
}
