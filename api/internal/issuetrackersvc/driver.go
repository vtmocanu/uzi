package issuetrackersvc

import (
	"context"

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
// or malformed one is refused before anything is read.
func (s *Service) DriverFor(ctx context.Context, userID, connID uuid.UUID, binding issuetracker.Binding, stateField, open, closed string) (issuetracker.Driver, error) {
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
		StateField:     stateField,
		OpenState:      open,
		ClosedState:    closed,
	})
	if err != nil {
		s.log.Error("build issue tracker driver", "kind", row.Kind, "error", err)
		return nil, internal()
	}
	return d, nil
}

// ListProjects lists the projects the connection's token can see.
func (s *Service) ListProjects(ctx context.Context, userID, connID uuid.UUID) ([]issuetracker.Project, error) {
	d, err := s.DriverFor(ctx, userID, connID, issuetracker.Binding{}, "", "", "")
	if err != nil {
		return nil, err
	}
	projects, err := d.ListProjects(ctx)
	if err != nil {
		return nil, &Error{Kind: KindBadGateway, Message: "could not list projects: " + err.Error()}
	}
	return projects, nil
}

// ListStates lists the issue states of one project of the connection.
func (s *Service) ListStates(ctx context.Context, userID, connID uuid.UUID, projectID string) ([]issuetracker.State, error) {
	if !issuetracker.ValidProjectID(projectID) {
		return nil, &Error{Kind: KindBadRequest, Message: "invalid project id"}
	}
	d, err := s.DriverFor(ctx, userID, connID, issuetracker.Binding{}, "", "", "")
	if err != nil {
		return nil, err
	}
	states, err := d.ListStates(ctx, projectID)
	if err != nil {
		return nil, &Error{Kind: KindBadGateway, Message: "could not list states: " + err.Error()}
	}
	return states, nil
}

// ListTags lists the tags the connection's token can see, with their owners and
// sharing settings.
func (s *Service) ListTags(ctx context.Context, userID, connID uuid.UUID) ([]issuetracker.Tag, error) {
	d, err := s.DriverFor(ctx, userID, connID, issuetracker.Binding{}, "", "", "")
	if err != nil {
		return nil, err
	}
	tags, err := d.ListTags(ctx)
	if err != nil {
		return nil, &Error{Kind: KindBadGateway, Message: "could not list tags: " + err.Error()}
	}
	return tags, nil
}
