package issuetrackersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Bounds the repo_issue_tracker_states and repo_issue_tracker_column_states CHECKs
// (migration 00508) enforce on every stored name, and the most columns one mapping may
// carry (the board's own column cap).
const (
	maxStateNameLen     = 200
	maxMappedColumns    = 10
	stateMappingNoLink  = "this repo is not linked to an issue tracker project"
	stateMappingNoConn  = "the repo's issue tracker connection is missing; it cannot be read"
	stateMappingBadName = "a state or column name is empty, too long (max 200), or holds a control, invisible, blank-filler or variation-selector character (or, in a column name, a brace)"
)

// ColumnState maps one board column, by its label name, to the State it stands for.
type ColumnState struct {
	LabelName string
	State     string
}

// StateMapping is the open and closed State of a linked repo and the State of each
// mapped board column (PRD #4173 M5). A board column that is absent from Columns is
// unmapped.
type StateMapping struct {
	OpenState   string
	ClosedState string
	Columns     []ColumnState
}

// StateInfo is one State of the linked project with what uzi may do with it: write it as
// the closed state (Closable), or as the open or a column state (Openable).
type StateInfo struct {
	Name     string
	Resolved bool
	Archived bool
	Closable bool
	Openable bool
}

// StateMappingView is the saved mapping (nil when none is saved yet) and the States the
// linked project offers.
type StateMappingView struct {
	Mapping *StateMapping
	States  []StateInfo
}

// linkedSource reads the owner-scoped repo row and refuses a repo that is not linked.
func (s *Service) linkedSource(ctx context.Context, userID, repoID uuid.UUID) (store.GetRepoIssueSourceForUserRow, error) {
	src, err := s.q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repoID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return src, &Error{Kind: KindNotFound, Message: "repo not found"}
	}
	if err != nil {
		s.log.Error("read repo for issue tracker state mapping", "error", err)
		return src, internal()
	}
	if !src.IssueTrackerConnectionID.Valid || !src.IssueTrackerProjectID.Valid {
		return src, conflict(stateMappingNoLink)
	}
	if !src.LinkedConnectionID.Valid {
		return src, conflict(stateMappingNoConn)
	}
	return src, nil
}

// listCheckedStates refuses a linked project whose State field is run by a state machine
// (PRD #4173, StateMachineRefusal), then lists its States. The check comes first so nothing
// is saved or shown for a project uzi cannot write.
func (s *Service) listCheckedStates(ctx context.Context, userID uuid.UUID, src store.GetRepoIssueSourceForUserRow) ([]issuetracker.State, error) {
	// The stored project id goes into every driver request, so it is checked before a
	// driver is built.
	if !issuetracker.ValidProjectID(src.IssueTrackerProjectID.String) {
		return nil, &Error{Kind: KindBadRequest, Message: "invalid project id"}
	}
	connID := uuid.UUID(src.LinkedConnectionID.Bytes)
	d, err := s.DriverFor(ctx, userID, connID, issuetracker.Binding{}, issuetracker.StateMapping{})
	if err != nil {
		return nil, err
	}
	if err := checkStateField(ctx, d, issuetracker.ProjectRef{ID: src.IssueTrackerProjectID.String, ShortName: src.IssueTrackerProjectShort.String}); err != nil {
		return nil, err
	}
	return listStates(ctx, d, src.IssueTrackerProjectID.String)
}

// listStates lists the issue states of one project through d; a driver failure is a bad
// gateway. The project id check repeats the caller's, as a backstop for a new caller.
func listStates(ctx context.Context, d issuetracker.Driver, projectID string) ([]issuetracker.State, error) {
	if !issuetracker.ValidProjectID(projectID) {
		return nil, &Error{Kind: KindBadRequest, Message: "invalid project id"}
	}
	states, err := d.ListStates(ctx, projectID)
	if err != nil {
		return nil, &Error{Kind: KindBadGateway, Message: "could not list states: " + err.Error()}
	}
	return states, nil
}

// GetStateMapping returns the saved state mapping of a linked repo and the States of its
// project. A repo that is not the user's is NotFound and one that is not linked is a
// Conflict.
func (s *Service) GetStateMapping(ctx context.Context, userID, repoID uuid.UUID) (StateMappingView, error) {
	src, err := s.linkedSource(ctx, userID, repoID)
	if err != nil {
		return StateMappingView{}, err
	}
	states, err := s.listCheckedStates(ctx, userID, src)
	if err != nil {
		return StateMappingView{}, err
	}
	view := StateMappingView{States: make([]StateInfo, 0, len(states))}
	for _, st := range states {
		view.States = append(view.States, StateInfo{
			Name: st.Name, Resolved: st.Resolved, Archived: st.Archived,
			Closable: issuetracker.ClosableState(st), Openable: issuetracker.OpenableState(st),
		})
	}
	if src.OpenState.Valid && src.ClosedState.Valid {
		m := StateMapping{OpenState: src.OpenState.String, ClosedState: src.ClosedState.String}
		var cols map[string]string
		if len(src.ColumnStates) > 0 {
			if err := json.Unmarshal(src.ColumnStates, &cols); err != nil {
				s.log.Error("decode stored column states", "error", err)
				return StateMappingView{}, internal()
			}
		}
		for label, state := range cols {
			m.Columns = append(m.Columns, ColumnState{LabelName: label, State: state})
		}
		slices.SortFunc(m.Columns, func(a, b ColumnState) int { return strings.Compare(a.LabelName, b.LabelName) })
		view.Mapping = &m
	}
	return view, nil
}

// SetStateMapping replaces the whole state mapping of a linked repo (PRD #4173 M5).
//
// Every name must be a State of the repo's linked project, by exact name: the open state
// and every column state must be unresolved and not archived, the closed state must be one
// that means done (issuetracker.ClosableState, so never "Won't fix"), and no two mapped
// States may be equal. Every column label must be a current board column of the repo. The
// States are read from the issue tracker before the transaction; the transaction takes the
// repo row lock that a board column reconfigure of a linked repo also takes, then reads the
// board columns and replaces the mapping, so the mapping never lands against columns that a
// concurrent reconfigure is deleting.
func (s *Service) SetStateMapping(ctx context.Context, userID, repoID uuid.UUID, in StateMapping) (StateMapping, error) {
	if err := validateMappingShape(in); err != nil {
		return StateMapping{}, err
	}
	src, err := s.linkedSource(ctx, userID, repoID)
	if err != nil {
		return StateMapping{}, err
	}
	states, err := s.listCheckedStates(ctx, userID, src)
	if err != nil {
		return StateMapping{}, err
	}
	if err := checkMappingStates(in, states); err != nil {
		return StateMapping{}, err
	}

	err = s.inTx(ctx, func(q Querier) error {
		lock, err := q.LockRepoForStateMappingForUser(ctx, store.LockRepoForStateMappingForUserParams{ID: repoID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Kind: KindNotFound, Message: "repo not found"}
		}
		if err != nil {
			return err
		}
		if !lock.IssueTrackerConnectionID.Valid {
			return conflict(stateMappingNoLink)
		}
		cols, err := q.ListBoardColumns(ctx, repoID)
		if err != nil {
			return err
		}
		have := make(map[string]bool, len(cols))
		for _, c := range cols {
			have[c.LabelName] = true
		}
		for _, c := range in.Columns {
			if !have[c.LabelName] {
				return &Error{Kind: KindBadRequest, Message: fmt.Sprintf("%q is not a board column of this repo", c.LabelName)}
			}
		}
		_, err = q.UpsertRepoIssueTrackerStates(ctx, store.UpsertRepoIssueTrackerStatesParams{
			OpenState: in.OpenState, ClosedState: in.ClosedState, RepoID: repoID, UserID: userID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict(stateMappingNoLink)
		}
		if err != nil {
			return err
		}
		if err := q.DeleteRepoIssueTrackerColumnStates(ctx, repoID); err != nil {
			return err
		}
		for _, c := range in.Columns {
			if err := q.InsertRepoIssueTrackerColumnState(ctx, store.InsertRepoIssueTrackerColumnStateParams{
				RepoID: repoID, LabelName: c.LabelName, StateName: c.State,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return StateMapping{}, s.txError("save issue tracker state mapping", err)
	}
	return in, nil
}

// validStateName holds a State name to what migration 00508 checks (1 to 200 characters, no
// control character) and to the rule ListStates applies (issuetracker.StateNameProblem: no
// invisible, blank-filler or variation-selector character). A brace is allowed: a State is
// written by id and never goes into a query.
func validStateName(name string) bool {
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= maxStateNameLen && !issuetracker.StateNameProblem(name)
}

// validColumnLabel holds a board column label to the rule a linked repo's ConfigureColumns
// applies (issuetracker.LabelNameProblem: at most 200 bytes, no brace, no invisible,
// blank-filler or variation-selector character), which is stricter than the 1 to 200
// character CHECK of migration 00508.
func validColumnLabel(name string) bool { return !issuetracker.LabelNameProblem(name) }

// validateMappingShape is the part of the validation that needs no issue tracker read:
// name bounds, the column cap, and distinct column labels and distinct States.
func validateMappingShape(in StateMapping) *Error {
	bad := func(msg string) *Error { return &Error{Kind: KindBadRequest, Message: msg} }
	if len(in.Columns) > maxMappedColumns {
		return bad(fmt.Sprintf("too many mapped columns (max %d)", maxMappedColumns))
	}
	if !validStateName(in.OpenState) || !validStateName(in.ClosedState) {
		return bad(stateMappingBadName)
	}
	labels := map[string]bool{}
	used := map[string]bool{in.OpenState: true}
	if used[in.ClosedState] {
		return bad("the open and the closed state must be different states")
	}
	used[in.ClosedState] = true
	for _, c := range in.Columns {
		if !validColumnLabel(c.LabelName) || !validStateName(c.State) {
			return bad(stateMappingBadName)
		}
		if labels[c.LabelName] {
			return bad("a column is mapped more than once")
		}
		labels[c.LabelName] = true
		if used[c.State] {
			return bad(fmt.Sprintf("the state %q is mapped more than once; the open state, the closed state and every column need different states", c.State))
		}
		used[c.State] = true
	}
	return nil
}

// checkMappingStates checks the mapping against the project's States as the issue tracker
// lists them.
func checkMappingStates(in StateMapping, states []issuetracker.State) *Error {
	byName := make(map[string]issuetracker.State, len(states))
	for _, st := range states {
		byName[st.Name] = st
	}
	bad := func(msg string, args ...any) *Error {
		return &Error{Kind: KindBadRequest, Message: fmt.Sprintf(msg, args...)}
	}
	find := func(name string) (issuetracker.State, *Error) {
		st, ok := byName[name]
		if !ok {
			return st, bad("the state %q is not a state of the linked project", name)
		}
		return st, nil
	}
	open, err := find(in.OpenState)
	if err != nil {
		return err
	}
	if !issuetracker.OpenableState(open) {
		return bad("the state %q cannot be the open state: it must be an unresolved, unarchived state", in.OpenState)
	}
	closed, err := find(in.ClosedState)
	if err != nil {
		return err
	}
	if !issuetracker.ClosableState(closed) {
		return bad("the state %q cannot be the closed state: it must be a resolved state that means done (for example Fixed or Done), never one such as Won't fix", in.ClosedState)
	}
	for _, c := range in.Columns {
		st, err := find(c.State)
		if err != nil {
			return err
		}
		if !issuetracker.OpenableState(st) {
			return bad("the state %q cannot be a column state: it must be an unresolved, unarchived state", c.State)
		}
	}
	return nil
}
