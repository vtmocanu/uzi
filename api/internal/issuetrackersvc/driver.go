package issuetrackersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

// DriverFor builds a driver for a stored connection. It re-checks the strict
// base URL form and the allowlist against the stored row BEFORE opening the
// token, so removing a host from the allowlist stops use of existing rows
// (fail closed). The token is opened with the row's user, kind and base URL as
// AAD, used only to build the driver, and never returned or logged. binding is
// the empty issuetracker.Binding for a connection-level driver, or all of the
// project, the repository's forge project id and the eligibility tag; a partial
// or malformed one is refused before anything is read. states is the repo's
// stored state mapping, or the zero StateMapping for a connection-level driver.
func (s *Service) DriverFor(ctx context.Context, userID, connID uuid.UUID, binding issuetracker.Binding, states issuetracker.StateMapping) (issuetracker.Driver, error) {
	if issuetracker.ValidBinding(binding) != nil {
		return nil, &Error{Kind: KindBadRequest, Message: "invalid project binding"}
	}
	row, err := s.getRow(ctx, userID, connID)
	if err != nil {
		return nil, err
	}
	kind, ok := issuetracker.ParseKind(row.Kind)
	if !ok {
		s.log.Error("stored issue tracker connection has an unknown kind", "connection", connID.String(), "kind", row.Kind)
		return nil, internal()
	}
	norm, err := strictBaseURL(row.BaseUrl)
	if err != nil || norm != row.BaseUrl {
		return nil, &Error{Kind: KindBadRequest, Message: "base URL is not valid for an issue tracker connection"}
	}
	if !s.allowed(norm) {
		return nil, &Error{Kind: KindBadRequest, Message: "base URL is not in the allowed issue tracker list"}
	}
	token, err := openToken(s.box, userID, kind, norm, row.TokenCiphertext)
	if err != nil {
		s.log.Error("open issue tracker token", "connection", connID.String(), "error", err)
		return nil, internal()
	}
	d, err := s.newDriver(kind, issuetracker.Options{
		BaseURL:        norm,
		Token:          token,
		Project:        binding.Project,
		ProjectID:      binding.ProjectID,
		EligibilityTag: binding.EligibilityTag,
		StateField:     states.Field,
		OpenState:      states.Open,
		ClosedState:    states.Closed,
		ColumnStates:   states.Columns,
	})
	if err != nil {
		s.log.Error("build issue tracker driver", "kind", row.Kind, "error", err)
		return nil, internal()
	}
	return d, nil
}

// ListProjects lists the projects the connection's token can see.
func (s *Service) ListProjects(ctx context.Context, userID, connID uuid.UUID) ([]issuetracker.Project, error) {
	d, err := s.DriverFor(ctx, userID, connID, issuetracker.Binding{}, issuetracker.StateMapping{})
	if err != nil {
		return nil, err
	}
	projects, err := d.ListProjects(ctx)
	if err != nil {
		return nil, &Error{Kind: KindBadGateway, Message: "could not list projects: " + err.Error()}
	}
	return projects, nil
}

// ListTags lists the tags the connection's token can see, with their owners and
// sharing settings.
func (s *Service) ListTags(ctx context.Context, userID, connID uuid.UUID) ([]issuetracker.Tag, error) {
	d, err := s.DriverFor(ctx, userID, connID, issuetracker.Binding{}, issuetracker.StateMapping{})
	if err != nil {
		return nil, err
	}
	tags, err := d.ListTags(ctx)
	if err != nil {
		return nil, &Error{Kind: KindBadGateway, Message: "could not list tags: " + err.Error()}
	}
	return tags, nil
}

// StateMachineRefusal is the reason a project whose State field is run by a state machine
// is refused: at link, on the state mapping, and on a board move of a linked repo.
func StateMachineRefusal(short string) string {
	return "project " + short + " uses a state-machine workflow on its State field; uzi needs a plain State field. " +
		"Link a project with a plain State field, or remove the state-machine rule from the State field in YouTrack."
}

// StateFieldUnreadableRefusal is the reason a project whose State field uzi cannot read as
// a plain State field is refused: at link, on the states GET and PUT, and on a board move
// of a linked repo. cause is the driver's error, which names the issue.
func StateFieldUnreadableRefusal(short string, cause error) string {
	return "uzi could not read project " + short + "'s State field as a plain State field (" + cause.Error() + "). " +
		"The connection's account may not be allowed to see the State field, or the project has no field by that name. " +
		"Let that account see the State field in YouTrack, or link a project whose State field it can read."
}

// checkStateField runs the driver's State field check: a state machine is a Conflict
// with StateMachineRefusal, a State field the connection cannot read as a plain one is a
// Conflict with StateFieldUnreadableRefusal (the read succeeded and what YouTrack returned
// refuses it), and any other failure, a failed read included, a BadGateway naming the project.
func checkStateField(ctx context.Context, d issuetracker.Driver, project issuetracker.ProjectRef) error {
	err := d.CheckStateField(ctx, project)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, issuetracker.ErrStateMachineField):
		return &Error{Kind: KindConflict, Message: StateMachineRefusal(project.ShortName)}
	case errors.Is(err, issuetracker.ErrStateFieldUnreadable):
		return &Error{Kind: KindConflict, Message: StateFieldUnreadableRefusal(project.ShortName, err)}
	default:
		return &Error{Kind: KindBadGateway, Message: "could not check project " + project.ShortName + "'s State field: " + err.Error()}
	}
}
