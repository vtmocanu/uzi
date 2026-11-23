package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type providerHealthStore struct {
	*healthFakeStore
	providerCount int64
	providerErr   error
	providerCalls []store.CountOnlineWorkersSatisfyingProviderParams
}

func (f *providerHealthStore) CountOnlineWorkersSatisfyingProvider(_ context.Context, arg store.CountOnlineWorkersSatisfyingProviderParams) (int64, error) {
	f.providerCalls = append(f.providerCalls, arg)
	return f.providerCount, f.providerErr
}
func (f *providerHealthStore) CountOnlineWorkersSatisfyingCodexHarness(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}
func (f *providerHealthStore) CountOnlineWorkersSatisfyingCodexRuntime(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}

func TestHealthProviderEngineCount(t *testing.T) {
	for _, engine := range []string{"claude", "codex", "unknown"} {
		for _, tc := range []struct {
			name  string
			bound bool
			count int64
			err   error
			want  string
		}{
			{"missing", true, 0, nil, reasonNoProviderCapableWorker},
			{"available", true, 1, nil, reasonWaitingWorker},
			{"read-error", true, 0, errors.New("count failed"), reasonWaitingWorker},
			{"vendor", false, 0, nil, reasonWaitingWorker},
		} {
			t.Run(engine+"/"+tc.name, func(t *testing.T) {
				r := queuedRunPastThreshold()
				r.Harness = engine
				r.Kind = "issue"
				r.RepoID = pgtype.UUID{}
				r.ModelProviderKeyMode = pgtype.Text{String: "user_key", Valid: tc.bound}
				fs := &providerHealthStore{healthFakeStore: &healthFakeStore{onlineWorkers: 1, freeSlotWorkers: 1}, providerCount: tc.count, providerErr: tc.err}
				svc := healthSvc(fs, defaultHealthSettings())
				if got := svc.queuedReason(context.Background(), t0, r, thWithLimit(testCustodyLimit)); got != tc.want {
					t.Fatalf("reason=%q want=%q", got, tc.want)
				}
				if !tc.bound {
					if len(fs.providerCalls) != 0 {
						t.Fatal("vendor run queried provider count")
					}
					return
				}
				if len(fs.providerCalls) != 1 || fs.providerCalls[0].UserID != r.UserID || fs.providerCalls[0].Engine != engine {
					t.Fatalf("provider calls=%+v", fs.providerCalls)
				}
			})
		}
	}
}
