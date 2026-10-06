package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestEphemeralDockerHealthEligibilityAllowlistError(t *testing.T) {
	fs := &healthFakeStore{}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.SetEffectiveDockerTier(true)
	svc.SetDockerAllowlist(fakeAllowlistReader{ids: []uuid.UUID{uuid.New()}, err: errFakeAllowlist})
	_, err := svc.WorkerEligibilityForHealth(context.Background(), t0, uuid.New())
	if !errors.Is(err, errFakeAllowlist) || len(fs.eligibilityCalls) != 0 {
		t.Fatalf("eligibility error=%v queries=%d, want error without query", err, len(fs.eligibilityCalls))
	}
}

func TestEphemeralDockerHealthReleasedAllowlistFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []uuid.UUID
		err  error
	}{
		{"values with error", []uuid.UUID{uuid.New()}, errFakeAllowlist},
		{"nil success", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := queuedRunPastThreshold()
			r.Kind = "issue"
			r.RepoID = pgtype.UUID{} // Skip the separate repo-eligibility rung.
			r.ReleasedWorkerID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			fs := &healthFakeStore{onlineWorkers: 1, freeSlotWorkers: 1, eligibility: store.CountOnlineWorkersClaimableForRunRow{Claimable: 1}}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.SetEffectiveDockerTier(true)
			svc.SetDockerAllowlist(fakeAllowlistReader{ids: tc.ids, err: tc.err})
			if reason := svc.queuedReason(context.Background(), t0, r); reason != reasonWaitingWorker {
				t.Fatalf("reason=%q", reason)
			}
			wantQueries := 1
			if tc.err == nil {
				wantQueries = 2
			}
			if len(fs.eligibilityCalls) != wantQueries {
				t.Fatalf("queries=%d want=%d", len(fs.eligibilityCalls), wantQueries)
			}
			args := fs.eligibilityCalls[len(fs.eligibilityCalls)-1]
			if !args.WorkerDockerEnabled || args.DockerRepoAllowlist == nil || len(args.DockerRepoAllowlist) != 0 {
				t.Fatalf("fallback query args=%+v, want tier and non-nil empty list", args)
			}
		})
	}
}

func TestEphemeralDockerHealthEligibilityThreadsTier(t *testing.T) {
	fs := &healthFakeStore{}
	svc := healthSvc(fs, defaultHealthSettings())
	id := uuid.New()
	svc.SetEffectiveDockerTier(true)
	svc.SetDockerAllowlist(fakeAllowlistReader{ids: []uuid.UUID{id}})
	if _, err := svc.WorkerEligibilityForHealth(context.Background(), t0, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if len(fs.eligibilityCalls) != 1 || !fs.eligibilityCalls[0].WorkerDockerEnabled || len(fs.eligibilityCalls[0].DockerRepoAllowlist) != 1 || fs.eligibilityCalls[0].DockerRepoAllowlist[0] != id {
		t.Fatalf("query args=%+v", fs.eligibilityCalls)
	}
}
