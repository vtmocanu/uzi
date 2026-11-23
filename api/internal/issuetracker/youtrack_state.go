package issuetracker

import (
	"context"
	"net/http"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// SetIssueState implements forge.IssueTracker. It writes the explicit state
// value Options names for the requested state and never guesses one; with that
// value unset it returns ErrStateMappingUnset. The value is checked against the
// project's State bundle first (checkTargetState), before the issue is read.
func (y *youTrack) SetIssueState(ctx context.Context, projectID, issueIID int64, state forge.IssueState) error {
	if _, err := y.requireProject(projectID); err != nil {
		return err
	}
	var value string
	var closing bool
	switch state {
	case forge.StateOpened:
		value = y.openState
	case forge.StateClosed:
		value, closing = y.closedState, true
	default:
		return y.fail("youtrack: state must be opened or closed")
	}
	if value == "" {
		return y.red.sentinel(ErrStateMappingUnset, 0, "no value configured for "+string(state))
	}
	target, err := y.checkTargetState(ctx, value, closing)
	if err != nil {
		return err
	}
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return err
	}
	return y.writeState(ctx, w, target)
}

// MoveToColumn moves the issue to a board column by writing the State value the
// connection maps to it. An empty column is the implicit Open column and writes
// the open state. A column with no mapped State is ErrColumnNotMapped, and
// neither case sends a request before the check. The target is then checked
// against the project's State bundle and the issue against the bound project, as
// SetIssueState does. An issue the issue tracker already shows as resolved is
// ErrIssueResolved with no write unless move.Reopen is set: the move refuses when
// the issue tracker already shows the issue resolved. The resolved read (GET) and
// the State write (POST) are separate requests, so a resolution landing between
// them is still overwritten; that window is accepted, like the check-then-write
// window of checkTargetState.
func (y *youTrack) MoveToColumn(ctx context.Context, projectID, issueIID int64, move forge.ColumnMove) error {
	return y.moveToColumn(ctx, projectID, issueIID, move.Column, move.Reopen)
}

func (y *youTrack) moveToColumn(ctx context.Context, projectID, issueIID int64, column string, reopen bool) error {
	if _, err := y.requireProject(projectID); err != nil {
		return err
	}
	var value string
	if column == "" {
		if y.openState == "" {
			return y.red.sentinel(ErrStateMappingUnset, 0, "no value configured for the open column")
		}
		value = y.openState
	} else {
		v, ok := y.columnStates[column]
		if !ok || v == "" {
			return y.red.sentinel(ErrColumnNotMapped, 0, "")
		}
		value = v
	}
	target, err := y.checkTargetState(ctx, value, false)
	if err != nil {
		return err
	}
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return err
	}
	if w.Resolved != nil && !reopen {
		return y.red.sentinel(ErrIssueResolved, 0, "")
	}
	return y.writeState(ctx, w, target)
}

// writeState sets the State field of issue w to s, the value checkTargetState
// approved, addressed by its id so the write cannot land on another value.
func (y *youTrack) writeState(ctx context.Context, w ytIssue, s State) error {
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodPost,
		path:   []string{"issues", w.ID},
		query:  url.Values{"fields": {"id"}},
		body: map[string]any{"customFields": []any{map[string]any{
			"name":  y.stateField,
			"$type": "StateIssueCustomField",
			"value": map[string]string{"id": s.ID},
		}}},
		limit: objectBytes,
		prov:  forge.ResponseSuccess,
	}, nil)
	return err
}

// checkTargetState reads the bound project's State bundle (the ListStates code
// path) and refuses, with ErrStateNotAllowed, a target that the bundle lacks, or
// that is not closable when closing, or not openable otherwise. It runs before
// every State write, so a bundle edited after the mapping was saved is caught.
// It returns the matched State, which is the one to write.
// The error quotes the name through quoteName, which bounds it.
func (y *youTrack) checkTargetState(ctx context.Context, name string, closing bool) (State, error) {
	states, err := y.ListStates(ctx, y.project.ID)
	if err != nil {
		return State{}, err
	}
	for _, s := range states {
		if s.Name != name {
			continue
		}
		if closing && ClosableState(s) || !closing && OpenableState(s) {
			return s, nil
		}
		return State{}, y.red.sentinel(ErrStateNotAllowed, 0, "the state is not allowed in this role: "+quoteName(name))
	}
	return State{}, y.red.sentinel(ErrStateNotAllowed, 0, "the project's state bundle has no such state: "+quoteName(name))
}
