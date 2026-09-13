package workersvc

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// AdminJudgeRecommendationBacklog is the admin "All users" grouped read (PRD #1184 M1): every
// recommendation across EVERY user's runs, deduped by (category, target), with attribution
// hidden at four layers. It is the cross-user twin of JudgeRecommendationBacklog.
//
// It reads a SEPARATE all-users query (ListJudgeRecommendationRowsAll) with no user predicate
// and no ?run= anchor — the owner path is untouched, and IsAdmin is authorized on the route,
// not here. The cap+1 / truncated / bucket-filter / triage-from-a-separate-query semantics are
// identical to the owner backlog, so the two share their invariants; only the grouping differs
// (it counts distinct users, and drops every identifier).
//
// categories is the pushed-down ?category= label filter (nil = all labels, the NULL-sentinel
// slice — never []string{}), validated by the handler against the shared taxonomy. bucket
// filters the resulting GROUPS in Go by the shared BucketOf rollup, exactly as the owner path
// does.
func (s *Service) AdminJudgeRecommendationBacklog(ctx context.Context, bucket string, categories []string) (apitypes.JudgeAdminBacklogDTO, error) {
	rows, err := s.q.ListJudgeRecommendationRowsAll(ctx, store.ListJudgeRecommendationRowsAllParams{
		Categories: categories,
		// One over the cap, so a full page is distinguished from an exactly-full one without a
		// second COUNT — the same convention as the owner backlog.
		Lim: JudgeBacklogMaxRows + 1,
	})
	if err != nil {
		return apitypes.JudgeAdminBacklogDTO{}, err
	}
	truncated := len(rows) > JudgeBacklogMaxRows
	if truncated {
		rows = rows[:JudgeBacklogMaxRows]
	}
	triage, err := s.AdminJudgeTriageStats(ctx)
	if err != nil {
		return apitypes.JudgeAdminBacklogDTO{}, err
	}
	return apitypes.JudgeAdminBacklogDTO{
		Bucket:    bucket,
		Groups:    filterAdminGroups(GroupJudgeRecommendationsAll(rows), bucket),
		Truncated: truncated,
		Triage:    triage,
	}, nil
}

// AdminJudgeTriageStats is the admin "All users" triage strip (PRD #1184 M1): the cross-user
// tally, read from ListJudgeTriageRowsAll and bucketed through the SAME shared BucketTriage
// ladder as the owner strip, so the two cannot drift. It is the canonical all-users tally the
// admin backlog and category chips scope against; never tallied from the grouped page.
func (s *Service) AdminJudgeTriageStats(ctx context.Context) (apitypes.TriageDTO, error) {
	rows, err := s.q.ListJudgeTriageRowsAll(ctx)
	if err != nil {
		return apitypes.TriageDTO{}, err
	}
	tr := make([]TriageRow, 0, len(rows))
	for _, r := range rows {
		tr = append(tr, TriageRow{
			Status:       r.DispositionStatus.String, // "" when the LEFT JOIN found no disposition
			Reason:       r.DismissReason.String,
			FiledSettled: r.FiledSettled,
		})
	}
	return BucketTriage(tr), nil
}

// AdminJudgeCategoryStats is the admin "All users" filter-chip counts (PRD #1184 M1): the
// cross-user bucket → category → count matrix, computed the SAME way as the owner
// JudgeCategoryStats — the shared Go rollup GroupJudgeRecommendationsAll over an UNCAPPED
// whole-backlog row load (Lim: 0, the LIMIT NULLIF sentinel), then each group's rollup Bucket
// tallied into the matrix. Categories is nil (facet independence — the chip counts must never
// apply the category filter) and there is no ?run= anchor on the admin path.
func (s *Service) AdminJudgeCategoryStats(ctx context.Context) (apitypes.JudgeCategoryStatsDTO, error) {
	rows, err := s.q.ListJudgeRecommendationRowsAll(ctx, store.ListJudgeRecommendationRowsAllParams{
		Categories: nil, // facet independence — never filter the counts by category
		Lim:        0,   // UNCAPPED — the LIMIT NULLIF(@lim, 0) sentinel means no limit
	})
	if err != nil {
		return apitypes.JudgeCategoryStatsDTO{}, err
	}
	// Pre-initialize all five bucket keys, so an absent bucket serializes as {} rather than
	// null and the frontend can index CountsByBucket[tab][cat] uniformly — identical to the
	// owner JudgeCategoryStats.
	matrix := map[string]map[string]int{
		BucketTodo:      {},
		BucketFiled:     {},
		BucketDone:      {},
		BucketDismissed: {},
		BucketAll:       {},
	}
	for _, g := range GroupJudgeRecommendationsAll(rows) {
		matrix[g.Bucket][g.Category]++
		matrix[BucketAll][g.Category]++
	}
	return apitypes.JudgeCategoryStatsDTO{CountsByBucket: matrix}, nil
}

// GroupJudgeRecommendationsAll dedups the flat all-users rows by (category, target) (PRD #1184
// M1) — the attribution-hidden cousin of GroupJudgeRecommendations. It expects the query's
// order (most-recently-JUDGED first), so a group's FIRST row supplies rationale_preview.
//
// Per group: OpenCount counts todo members, RunCount counts DISTINCT run_ids, UserCount counts
// DISTINCT user_ids (the "K users" signal this view exists for). Bucket is todo when OpenCount
// >= 1, else the highest member rung. The distinct-count inputs (run_id, user_id) are read
// here and DROPPED — this layer is where the second attribution-hiding layer lives: no
// identifier reaches an emitted occurrence, only {judged_at, verdict, bucket, set_via?}.
//
// Groups come out ranked by cross-user recurrence — UserCount, then RunCount, then OpenCount,
// all descending — because how many people hit a pattern is the strongest priority signal;
// ties keep the most-recent-first order the query established (SliceStable).
func GroupJudgeRecommendationsAll(rows []store.ListJudgeRecommendationRowsAllRow) []apitypes.JudgeAdminGroupDTO {
	groups := []apitypes.JudgeAdminGroupDTO{}
	index := map[coord]int{}
	runsSeen := map[coord]map[uuid.UUID]bool{}
	usersSeen := map[coord]map[uuid.UUID]bool{}
	topRung := map[coord]int{}

	for _, r := range rows {
		key := coord{category: r.Category, target: r.Target}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			runsSeen[key] = map[uuid.UUID]bool{}
			usersSeen[key] = map[uuid.UUID]bool{}
			groups = append(groups, apitypes.JudgeAdminGroupDTO{
				Category: r.Category,
				Target:   r.Target,
				// The first row of a group is its most-recent occurrence (query order).
				RationalePreview: rationalePreview(r.RationaleMd),
				Occurrences:      []apitypes.JudgeAdminOccurrenceDTO{},
			})
		}
		b := BucketOf(r.DispositionStatus.String, r.FiledSettled)
		g := &groups[i]
		// The emitted occurrence carries NO run_id and NO user_id — the two opaque UUIDs the
		// query projects are counted just below and never leave this function (PRD #1184
		// Success Criterion #2, hiding layer 2).
		g.Occurrences = append(g.Occurrences, apitypes.JudgeAdminOccurrenceDTO{
			JudgedAt: r.JudgedAt.Time,
			Verdict:  r.Verdict,
			Bucket:   b,
			// Passed through, never interpreted: a NULL set_via yields "" and the DTO omits it.
			SetVia: r.SetVia.String,
		})
		if b == BucketTodo {
			g.OpenCount++
		}
		if !runsSeen[key][r.RunID] {
			runsSeen[key][r.RunID] = true
			g.RunCount++
		}
		if !usersSeen[key][r.UserID] {
			usersSeen[key][r.UserID] = true
			g.UserCount++
		}
		if rank := bucketRank(b); rank > topRung[key] {
			topRung[key] = rank
		}
	}

	// Roll up: any open member makes the whole group To triage; a fully-settled group shows at
	// its highest member state — the same rollup as the owner grouper.
	for i := range groups {
		key := coord{category: groups[i].Category, target: groups[i].Target}
		if groups[i].OpenCount > 0 {
			groups[i].Bucket = BucketTodo
			continue
		}
		groups[i].Bucket = bucketName(topRung[key])
	}

	sort.SliceStable(groups, func(a, b int) bool {
		if groups[a].UserCount != groups[b].UserCount {
			return groups[a].UserCount > groups[b].UserCount
		}
		if groups[a].RunCount != groups[b].RunCount {
			return groups[a].RunCount > groups[b].RunCount
		}
		return groups[a].OpenCount > groups[b].OpenCount
	})
	return groups
}

// filterAdminGroups applies the ?bucket= filter to the admin grouped rows, matching the GROUP
// ROLLUP exactly as filterGroups does for the owner path (the one Go bucket filter, because the
// rollup is computed from the shared BucketOf — a SQL bucket filter would be the forbidden
// second ladder).
func filterAdminGroups(groups []apitypes.JudgeAdminGroupDTO, bucket string) []apitypes.JudgeAdminGroupDTO {
	out := make([]apitypes.JudgeAdminGroupDTO, 0, len(groups))
	for _, g := range groups {
		if bucket != BucketAll && bucket != "" && g.Bucket != bucket {
			continue
		}
		out = append(out, g)
	}
	return out
}
