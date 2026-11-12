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
// 1-based position in that order. Tags follow readLabel, as in issue reads: the
// eligibility tag is reported under the stored eligibility name, a tag the
// token's account owns under its own name, and any other tag is left out.
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
	emit := func(a ytActivity, action string, tags []ytTag) error {
		for _, t := range tags {
			name, ok, err := y.readLabel(ctx, t)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			out = append(out, forge.LabelEvent{
				ID:        int64(len(out) + 1),
				Action:    action,
				LabelName: name,
				Username:  login(a.Author),
				CreatedAt: time.UnixMilli(a.Timestamp).UTC(),
			})
		}
		return nil
	}
	for _, a := range acts {
		if err := emit(a, "add", a.Added); err != nil {
			return nil, err
		}
		if err := emit(a, "remove", a.Removed); err != nil {
			return nil, err
		}
	}
	return out, nil
}
