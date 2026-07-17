package uzicli

import "context"

// FakeClient is an in-memory Client for command tests. It makes no network
// calls: set the canned fields, or set Err to have every method fail with it.
// GetRun/RunReview return an ExitNotFound error when the id is absent, so tests
// can exercise the exit-4 path.
type FakeClient struct {
	User         User
	Runs         []Run
	RunByID      map[string]Run
	Reviews      map[string]Review
	Workers      []Worker
	Repos        []Repo
	AdminUsers   []User
	AdminRuns    []Run
	AdminWorkers []Worker
	AdminUsageV  Usage
	RateLimits   []RateLimit

	// Err, when non-nil, is returned by every method (before any lookup).
	Err error
}

var _ Client = (*FakeClient)(nil)

func (f *FakeClient) Whoami(context.Context) (User, error) {
	if f.Err != nil {
		return User{}, f.Err
	}
	return f.User, nil
}

func (f *FakeClient) ListRuns(context.Context) ([]Run, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Runs, nil
}

func (f *FakeClient) GetRun(_ context.Context, id string) (Run, error) {
	if f.Err != nil {
		return Run{}, f.Err
	}
	if r, ok := f.RunByID[id]; ok {
		return r, nil
	}
	return Run{}, Exitf(ExitNotFound, "run %s not found", id)
}

func (f *FakeClient) RunReview(_ context.Context, id string) (Review, error) {
	if f.Err != nil {
		return Review{}, f.Err
	}
	if r, ok := f.Reviews[id]; ok {
		return r, nil
	}
	return Review{}, Exitf(ExitNotFound, "run %s not found", id)
}

func (f *FakeClient) ListWorkers(context.Context) ([]Worker, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Workers, nil
}

func (f *FakeClient) ListRepos(context.Context) ([]Repo, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Repos, nil
}

func (f *FakeClient) AdminListUsers(context.Context) ([]User, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminUsers, nil
}

func (f *FakeClient) AdminListRuns(context.Context) ([]Run, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminRuns, nil
}

func (f *FakeClient) AdminListWorkers(context.Context) ([]Worker, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminWorkers, nil
}

func (f *FakeClient) AdminUsage(context.Context) (Usage, error) {
	if f.Err != nil {
		return Usage{}, f.Err
	}
	return f.AdminUsageV, nil
}

func (f *FakeClient) AdminRateLimits(context.Context) ([]RateLimit, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.RateLimits, nil
}
