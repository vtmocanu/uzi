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
	w, err := y.resolveIssueForStateWrite(ctx, projectID, issueIID)
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
	w, err := y.resolveIssueForStateWrite(ctx, projectID, issueIID)
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
			"$type": plainStateType,
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

// stateMachineType is the $type YouTrack gives a State field that a
// state-machine workflow runs.
const stateMachineType = "StateMachineIssueCustomField"

// plainStateType is the $type of a State field uzi can write directly.
const plainStateType = "StateIssueCustomField"

// stateKind is how an issue's response describes its State field.
type stateKind int

const (
	// stateFieldUnreadable: no entry of the State field's name has a plain
	// $type, or one has another or no $type. Nothing may be written.
	stateFieldUnreadable stateKind = iota
	// stateFieldPlain: the State field is one uzi can write directly.
	stateFieldPlain
	// stateFieldMachine: a state machine runs the State field.
	stateFieldMachine
)

// stateFieldKind classifies the State field in w, which must have been read
// with customFields(name,$type). Fields of another name are ignored. Any entry
// of the State field's name typed as a state machine decides machine; else at
// least one plain entry and none of another or empty type is plain; anything
// else, including no entry, is unreadable.
func (y *youTrack) stateFieldKind(w ytIssue) stateKind {
	plain := false
	unknown := false
	for _, f := range w.CustomFields {
		if f.Name != y.stateField {
			continue
		}
		switch f.Type {
		case stateMachineType:
			return stateFieldMachine
		case plainStateType:
			plain = true
		default:
			unknown = true
		}
	}
	if plain && !unknown {
		return stateFieldPlain
	}
	return stateFieldUnreadable
}

// unreadableDetail names the issue for ErrStateFieldUnreadable: its readable
// id, or its id when that is empty. quoteName bounds the length.
func unreadableDetail(w ytIssue) string {
	if w.IDReadable == "" {
		return "issue " + quoteName(w.ID)
	}
	return "issue " + quoteName(w.IDReadable)
}

// resolveIssueForStateWrite is resolveIssue for a State write. Its one GET also
// asks for customFields(name,$type). An issue whose State field a state machine
// runs is ErrStateMachineField, and one whose response does not list the State
// field as a plain StateIssueCustomField only (stateFieldKind) is
// ErrStateFieldUnreadable, before any write and with no fallback.
func (y *youTrack) resolveIssueForStateWrite(ctx context.Context, projectID, issueIID int64) (ytIssue, error) {
	w, err := y.resolveIssueFields(ctx, projectID, issueIID, issueFields+","+stateCheckFields)
	if err != nil {
		return ytIssue{}, err
	}
	switch y.stateFieldKind(w) {
	case stateFieldMachine:
		return ytIssue{}, y.red.sentinel(ErrStateMachineField, 0, "")
	case stateFieldUnreadable:
		return ytIssue{}, y.red.sentinel(ErrStateFieldUnreadable, 0, unreadableDetail(w))
	}
	return w, nil
}

// stateCheckFields is what the State-write read adds to issueFields, and what
// CheckStateField reads of each sampled issue besides its id and project.
const stateCheckFields = "customFields(name,$type)"

// CheckStateField implements Driver. It reads the first page of the project's
// issues (pageSize items, the listing's query) asking only for each issue's
// ids, project and its State custom field's name and type, and never writes.
// A sampled issue of the project run by a state machine is
// ErrStateMachineField, which wins; else one that does not list the State
// field as a plain StateIssueCustomField only (stateFieldKind) is
// ErrStateFieldUnreadable, naming the first such issue. An issue of another project is ignored, so the ref's id decides, not
// only its short name. A non-empty page with no issue of the project is an
// error, since nothing was checked.
func (y *youTrack) CheckStateField(ctx context.Context, project ProjectRef) error {
	if err := ValidProjectRef(project); err != nil {
		return err
	}
	if project == (ProjectRef{}) {
		return y.fail("youtrack: no project to check")
	}
	query := url.Values{
		"query":        {issueQuery(project.ShortName, forge.ListIssuesOptions{})},
		"fields":       {"id,idReadable,project(id)," + stateCheckFields},
		"customFields": {y.stateField},
	}
	var sampled, inProject, found bool
	var unreadableAt string
	err := ytPages(ctx, y, []string{"issues"}, query, func(page []ytIssue) bool {
		sampled = len(page) > 0
		for _, w := range page {
			if w.Project == nil || w.Project.ID != project.ID {
				continue
			}
			inProject = true
			switch y.stateFieldKind(w) {
			case stateFieldMachine:
				found = true
			case stateFieldUnreadable:
				if unreadableAt == "" {
					unreadableAt = unreadableDetail(w)
				}
			}
			if found {
				break
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	if found {
		return y.red.sentinel(ErrStateMachineField, 0, "")
	}
	if unreadableAt != "" {
		return y.red.sentinel(ErrStateFieldUnreadable, 0, unreadableAt)
	}
	if sampled && !inProject {
		// A page of issues, none of them in the project: the stored short name
		// no longer names it, so nothing was checked and that is no pass.
		return y.fail("youtrack: the sampled issues are not in the linked project")
	}
	return nil
}
