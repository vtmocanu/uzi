package uzicli

import "context"

func (f *FakeClient) ListIssueTrackerConnections(context.Context) ([]IssueTrackerConnection, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.IssueTrackerConnections, nil
}

func (f *FakeClient) GetIssueTrackerStates(_ context.Context, repoID string) (IssueTrackerStates, error) {
	f.LastIssueTrackerStatesRepoID = repoID
	if f.Err != nil {
		return IssueTrackerStates{}, f.Err
	}
	return f.IssueTrackerStatesResult, nil
}
