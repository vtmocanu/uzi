package issuetracker

import (
	"context"
	"net/url"
	"slices"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// ListIssueLabelEvents implements forge.IssueTracker from the issue's tag
// activities. Events are oldest first, one per added or removed tag; ID is the
// 1-based position in that order. A tag that has the eligibility name but is not
// the eligibility tag is left out.
func (y *youTrack) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]forge.LabelEvent, error) {
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return nil, err
	}
	acts, err := ytList[ytActivity](ctx, y, []string{"issues", w.ID, "activities"},
		url.Values{"categories": {"TagsCategory"}, "fields": {activityFields}})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(acts, func(a, b ytActivity) int {
		switch {
		case a.Timestamp < b.Timestamp:
			return -1
		case a.Timestamp > b.Timestamp:
			return 1
		}
		return 0
	})
	out := []forge.LabelEvent{}
	emit := func(a ytActivity, action string, tags []ytTag) {
		for _, t := range tags {
			if y.foreignEligibility(t) {
				continue
			}
			out = append(out, forge.LabelEvent{
				ID:        int64(len(out) + 1),
				Action:    action,
				LabelName: t.Name,
				Username:  login(a.Author),
				CreatedAt: time.UnixMilli(a.Timestamp).UTC(),
			})
		}
	}
	for _, a := range acts {
		emit(a, "add", a.Added)
		emit(a, "remove", a.Removed)
	}
	return out, nil
}
