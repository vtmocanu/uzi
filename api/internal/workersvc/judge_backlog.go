package workersvc

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// JudgeBacklogBuckets is the ?bucket= filter enum for the Judge menu (PRD #98 M1).
// "all" is the unfiltered view; the other four are the #94 ladder's rungs, matched
// against the GROUP rollup (not against a member). Validated by the handler; the
// service treats an unknown value as "all" only because it can never reach it.
var JudgeBacklogBuckets = map[string]bool{
	"todo": true, "filed": true, "done": true, "dismissed": true, "all": true,
}

// bucketRank orders the #94 ladder for the GROUP rollup (PRD #98 Decision 2:
// dismissed > done > filed > todo). It ranks the OUTPUT of BucketOf; it is not a second
// bucketing implementation — nothing here decides which bucket a recommendation is in.
func bucketRank(b string) int {
	switch b {
	case "dismissed":
		return 3
	case "done":
		return 2
	case "filed":
		return 1
	default:
		return 0
	}
}

// RationalePreviewMaxRunes caps the group's rationale_preview (PRD #98 Decision 1: a
// TRUNCATED, length-capped preview). rationale_md is already capped at
// ReviewRationaleMaxBytes (4 KiB) at ingest, but a backlog page carries one per GROUP, so
// the preview is what keeps an all-time payload bounded — the PRD's payload-growth risk.
// Counted in RUNES, not bytes, so a cut can never split a UTF-8 sequence and hand the SPA
// or the CLI a broken glyph.
const RationalePreviewMaxRunes = 280

// rationalePreview truncates a rationale to the preview cap, appending an ellipsis when it
// actually cut. The text is shipped as PLAIN TEXT, deliberately NOT server-side HTML-escaped
// (PRD #98 Decision 1, corrected against the code 2026-07-20): the no-raw-render guarantee
// is CLIENT-side — RunView.tsx renders judge free text as escaped React text — and every
// consumer of this field must do the same. Escaping here would double-escape in the SPA and
// print HTML entities into the terminal from `uzi review backlog`. Secrets and control
// characters were already scrubbed at the review-POST ingest (judge_review.go).
func rationalePreview(s string) string {
	runes := []rune(s)
	if len(runes) <= RationalePreviewMaxRunes {
		return s
	}
	return strings.TrimRight(string(runes[:RationalePreviewMaxRunes]), " \t\r\n") + "…"
}

// coord is the dedup grain (PRD #98 Decision 2): the (category, target) pair. It is a
// DISPLAY key — #68/#94 key filed/disposition state per (review_id, category, target), so
// the same idea in two runs stays two coordinates with independent triage state.
type coord struct {
	category string
	target   string
}

// JudgeRecommendationBacklog is the Judge menu's grouped read (PRD #98 M1, Decision 1):
// every recommendation across every review the caller owns, deduped by (category, target),
// with each occurrence's per-run triage state intact.
//
// Owner-scoped by the query's user_id filter — the same scoping as JudgeTriageStats, so
// there is no ownership oracle to leak and IsAdmin is never consulted (a uza_ admin_ro
// token sees its OWN backlog and nothing else).
//
// bucket filters the returned GROUPS (see filterGroups); runAnchor, when non-nil, keeps
// only groups that recur in that run — the notification deep-link's /judge?run={id}
// (Decision 4). Neither narrows Triage: that tally is always the caller's whole row set,
// bucketed by the SAME BucketTriage that backs GET /me/judge/stats, so the two agree to
// the digit whatever the filter.
func (s *Service) JudgeRecommendationBacklog(ctx context.Context, ownerUserID uuid.UUID, bucket string, runAnchor uuid.UUID) (apitypes.JudgeBacklogDTO, error) {
	rows, err := s.q.ListJudgeRecommendationRowsForUser(ctx, ownerUserID)
	if err != nil {
		return apitypes.JudgeBacklogDTO{}, err
	}
	out := apitypes.JudgeBacklogDTO{
		Bucket: bucket,
		Groups: filterGroups(GroupJudgeRecommendations(rows), bucket, runAnchor),
		Triage: BucketTriage(triageRowsOf(rows)),
	}
	if runAnchor != uuid.Nil {
		out.Run = runAnchor.String()
	}
	return out, nil
}

// triageRowsOf projects the wide backlog rows onto the narrow TriageRow the shared
// bucketer consumes. This is the single-ladder guarantee in one line: the backlog page
// and the /stats strip tally the identical facts through the identical helper, so a
// wider projection can never grow a second bucketing rule.
func triageRowsOf(rows []store.ListJudgeRecommendationRowsForUserRow) []TriageRow {
	tr := make([]TriageRow, 0, len(rows))
	for _, r := range rows {
		tr = append(tr, TriageRow{
			Status:       r.DispositionStatus.String, // "" when the LEFT JOIN found no disposition
			Reason:       r.DismissReason.String,
			FiledSettled: r.FiledSettled,
		})
	}
	return tr
}

// GroupJudgeRecommendations dedups the flat rows by (category, target) (PRD #98
// Decisions 1/2). It expects the query's order — most-recent review first — so a group's
// FIRST row is its most-recent occurrence and supplies rationale_preview.
//
// Per group: OpenCount counts members whose bucket is todo; RunCount counts DISTINCT runs
// (a judge may emit the same coordinate twice in one review, so occurrences can outnumber
// runs); Bucket is todo when OpenCount >= 1, else the highest member rung. Groups come out
// ranked by recurrence — run_count, then open_count — because frequency, not the judge's
// self-reported confidence, is the trustworthy priority signal (Solution Overview); ties
// keep the most-recent-first order the query established.
func GroupJudgeRecommendations(rows []store.ListJudgeRecommendationRowsForUserRow) []apitypes.JudgeRecommendationGroupDTO {
	groups := []apitypes.JudgeRecommendationGroupDTO{}
	index := map[coord]int{}
	runsSeen := map[coord]map[uuid.UUID]bool{}
	topRung := map[coord]int{}

	for _, r := range rows {
		key := coord{category: r.Category, target: r.Target}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			runsSeen[key] = map[uuid.UUID]bool{}
			groups = append(groups, apitypes.JudgeRecommendationGroupDTO{
				Category: r.Category,
				Target:   r.Target,
				// The first row of a group is its most-recent occurrence (query order).
				RationalePreview: rationalePreview(r.RationaleMd),
				Occurrences:      []apitypes.JudgeOccurrenceDTO{},
			})
		}
		b := BucketOf(r.DispositionStatus.String, r.FiledSettled)
		g := &groups[i]
		g.Occurrences = append(g.Occurrences, apitypes.JudgeOccurrenceDTO{
			RunID:      r.RunID.String(),
			RunTitle:   r.RunTitle,
			ReviewID:   r.ReviewID.String(),
			RecID:      r.RecID.String(),
			Verdict:    r.Verdict,
			Confidence: r.Confidence,
			Bucket:     b,
			FiledIssue: filedIssueRef(r),
		})
		if b == "todo" {
			g.OpenCount++
		}
		if !runsSeen[key][r.RunID] {
			runsSeen[key][r.RunID] = true
			g.RunCount++
		}
		if rank := bucketRank(b); rank > topRung[key] {
			topRung[key] = rank
		}
	}

	// Roll up: any open member makes the whole group To triage; a fully-settled group
	// shows at its highest member state (a documented display quirk — the occurrence
	// expander always carries the per-run truth, Decision 2).
	for i := range groups {
		key := coord{category: groups[i].Category, target: groups[i].Target}
		if groups[i].OpenCount > 0 {
			groups[i].Bucket = "todo"
			continue
		}
		groups[i].Bucket = bucketName(topRung[key])
	}

	sort.SliceStable(groups, func(a, b int) bool {
		if groups[a].RunCount != groups[b].RunCount {
			return groups[a].RunCount > groups[b].RunCount
		}
		return groups[a].OpenCount > groups[b].OpenCount
	})
	return groups
}

// bucketName is bucketRank's inverse, used only to name a fully-settled group's rollup.
func bucketName(rank int) string {
	switch rank {
	case 3:
		return "dismissed"
	case 2:
		return "done"
	case 1:
		return "filed"
	default:
		return "todo"
	}
}

// filedIssueRef carries a SETTLED filed link onto an occurrence, or nil. filed_at is the
// settlement marker (#68): a claimed-but-unfiled coordinate has a row with a NULL
// filed_at, which the ladder already buckets as todo, so it must not render as filed here
// either.
func filedIssueRef(r store.ListJudgeRecommendationRowsForUserRow) *apitypes.JudgeFiledIssueRefDTO {
	if !r.FiledSettled {
		return nil
	}
	return &apitypes.JudgeFiledIssueRefDTO{
		IssueIID: r.FiledIssueIid.Int64,
		IssueURL: r.FiledIssueUrl.String,
		FiledAt:  r.FiledAt.Time,
	}
}

// filterGroups applies the ?bucket= filter and the ?run= anchor to the grouped rows.
//
// bucket matches the GROUP ROLLUP, so "todo" is exactly "open_count >= 1" (Decision 2)
// and the settled rungs are mutually exclusive with it. The run anchor keeps a group if
// ANY of its occurrences is in that run, but never trims the occurrence list — the whole
// point of arriving from a notification is to see that the recommendation also recurs
// elsewhere.
func filterGroups(groups []apitypes.JudgeRecommendationGroupDTO, bucket string, runAnchor uuid.UUID) []apitypes.JudgeRecommendationGroupDTO {
	anchor := ""
	if runAnchor != uuid.Nil {
		anchor = runAnchor.String()
	}
	out := make([]apitypes.JudgeRecommendationGroupDTO, 0, len(groups))
	for _, g := range groups {
		if bucket != "all" && bucket != "" && g.Bucket != bucket {
			continue
		}
		if anchor != "" && !groupHasRun(g, anchor) {
			continue
		}
		out = append(out, g)
	}
	return out
}

func groupHasRun(g apitypes.JudgeRecommendationGroupDTO, runID string) bool {
	for _, o := range g.Occurrences {
		if o.RunID == runID {
			return true
		}
	}
	return false
}
