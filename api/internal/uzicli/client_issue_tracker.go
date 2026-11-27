package uzicli

import (
	"context"
	"net/url"
	"time"
)

// IssueTrackerConnection mirrors one entry of GET /api/issue-trackers/connections. The
// handler's type is handler-local, so the CLI keeps its own copy of the wire shape. The
// response never carries a token.
type IssueTrackerConnection struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	BaseURL        string    `json:"base_url"`
	AccountID      string    `json:"account_id"`
	AccountLogin   string    `json:"account_login"`
	CreatedAt      time.Time `json:"created_at"`
	LastVerifiedAt time.Time `json:"last_verified_at"`
}

// IssueTrackerColumnState maps one board column (its label name) to an issue tracker State.
type IssueTrackerColumnState struct {
	LabelName string `json:"label_name"`
	State     string `json:"state"`
}

// IssueTrackerStateMapping is a repo's saved State mapping.
type IssueTrackerStateMapping struct {
	OpenState   string                    `json:"open_state"`
	ClosedState string                    `json:"closed_state"`
	Columns     []IssueTrackerColumnState `json:"columns"`
}

// IssueTrackerState is one State of the linked project with what uzi may do with it.
type IssueTrackerState struct {
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
	Archived bool   `json:"archived"`
	Closable bool   `json:"closable"`
	Openable bool   `json:"openable"`
}

// IssueTrackerStates is the GET /api/repos/{id}/issue-tracker-link/states reply: the saved
// mapping (nil until one is saved) and the project's States.
type IssueTrackerStates struct {
	Mapping *IssueTrackerStateMapping `json:"mapping"`
	States  []IssueTrackerState       `json:"states"`
}

func (c *HTTPClient) ListIssueTrackerConnections(ctx context.Context) ([]IssueTrackerConnection, error) {
	var env struct {
		Connections []IssueTrackerConnection `json:"connections"`
	}
	if err := c.get(ctx, "/api/issue-trackers/connections", &env); err != nil {
		return nil, err
	}
	return env.Connections, nil
}

func (c *HTTPClient) GetIssueTrackerStates(ctx context.Context, repoID string) (IssueTrackerStates, error) {
	var out IssueTrackerStates
	if err := c.get(ctx, "/api/repos/"+url.PathEscape(repoID)+"/issue-tracker-link/states", &out); err != nil {
		return IssueTrackerStates{}, err
	}
	return out, nil
}
