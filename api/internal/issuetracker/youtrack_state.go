package issuetracker

import (
	"context"
	"net/http"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// SetIssueState implements forge.IssueTracker. It writes the explicit state
// value Options names for the requested state and never guesses one; with that
// value unset it returns ErrStateMappingUnset.
func (y *youTrack) SetIssueState(ctx context.Context, projectID, issueIID int64, state forge.IssueState) error {
	if _, err := y.requireProject(projectID); err != nil {
		return err
	}
	var value string
	switch state {
	case forge.StateOpened:
		value = y.openState
	case forge.StateClosed:
		value = y.closedState
	default:
		return y.fail("youtrack: state must be opened or closed")
	}
	if value == "" {
		return y.red.sentinel(ErrStateMappingUnset, 0, "no value configured for "+string(state))
	}
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return err
	}
	_, _, err = y.do(ctx, ytCall{
		method: http.MethodPost,
		path:   []string{"issues", w.ID},
		query:  url.Values{"fields": {"id"}},
		body: map[string]any{"customFields": []any{map[string]any{
			"name":  y.stateField,
			"$type": "StateIssueCustomField",
			"value": map[string]string{"name": value},
		}}},
		limit: objectBytes,
		prov:  forge.ResponseSuccess,
	}, nil)
	return err
}
