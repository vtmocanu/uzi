package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeDockerAllowlist is a stand-in DockerAllowlistReader. Returning err lets a test
// prove the STRICT read contract (a settings read error leaves the run unclaimed).
type fakeDockerAllowlist struct {
	list []uuid.UUID
	err  error
}

func (f fakeDockerAllowlist) DockerRepoAllowlist(context.Context) ([]uuid.UUID, error) {
	return f.list, f.err
}

func dockerWorker() store.Worker {
	return store.Worker{ID: uuid.New(), UserID: uuid.New(), DockerEnabled: pgtype.Bool{Bool: true, Valid: true}}
}

// PRD #89 M-allow. The claim gate's load-bearing decision is WHICH params reach
// ClaimRun: the SQL predicate can only fence a docker worker to the allowlist if the
// service passes is_docker_worker=true and the resolved repo id set. These tests pin
// that wiring; the SQL predicate itself is covered by the store integration path.
//
// Each uses claimErr=pgx.ErrNoRows so Claim reports idle straight after ClaimRun
// (the fake captures claimParams first), keeping the assertions on the params alone
// and off full payload assembly.

// A DOCKER worker passes is_docker_worker=true and the allowlist the reader returned.
func TestClaimDockerWorkerPassesAllowlistPredicate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	svc.SetDockerAllowlist(fakeDockerAllowlist{list: []uuid.UUID{a, b}})

	if _, err := svc.Claim(context.Background(), dockerWorker()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if fs.claimParams == nil {
		t.Fatal("ClaimRun was not called")
	}
	if !fs.claimParams.IsDockerWorker {
		t.Error("IsDockerWorker = false for a docker-enabled worker, want true")
	}
	if !reflect.DeepEqual(fs.claimParams.DockerRepoAllowlist, []uuid.UUID{a, b}) {
		t.Errorf("DockerRepoAllowlist = %v, want [%s %s]", fs.claimParams.DockerRepoAllowlist, a, b)
	}
}

// A NON-docker worker passes is_docker_worker=false, and — proven by the reader being
// wired to error — never consults the allowlist reader at all, so its claim behavior
// is unchanged.
func TestClaimNonDockerWorkerUnaffectedAndSkipsReader(t *testing.T) {
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	// If a non-docker worker wrongly consulted this reader, Claim would surface errBoom.
	svc.SetDockerAllowlist(fakeDockerAllowlist{err: errors.New("boom: reader must not be consulted")})

	if _, err := svc.Claim(context.Background(), worker()); err != nil {
		t.Fatalf("Claim (non-docker) = %v, want nil (reader must not be consulted)", err)
	}
	if fs.claimParams == nil {
		t.Fatal("ClaimRun was not called")
	}
	if fs.claimParams.IsDockerWorker {
		t.Error("IsDockerWorker = true for a non-docker worker, want false")
	}
}

// Fail-closed with no reader wired: a docker worker still claims (is_docker_worker=
// true) but with an EMPTY, non-nil allowlist — so the SQL predicate lets it pick up
// only repo-less runs, never an unvetted repo's run.
func TestClaimDockerWorkerFailsClosedWithoutReader(t *testing.T) {
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams()) // no SetDockerAllowlist

	if _, err := svc.Claim(context.Background(), dockerWorker()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if fs.claimParams == nil {
		t.Fatal("ClaimRun was not called")
	}
	if !fs.claimParams.IsDockerWorker {
		t.Error("IsDockerWorker = false, want true even without a reader")
	}
	if fs.claimParams.DockerRepoAllowlist == nil {
		t.Error("DockerRepoAllowlist = nil, want a non-nil empty slice (encodes as {}, fail-closed)")
	}
	if len(fs.claimParams.DockerRepoAllowlist) != 0 {
		t.Errorf("DockerRepoAllowlist = %v, want empty (fail-closed)", fs.claimParams.DockerRepoAllowlist)
	}
}

// STRICT read: a settings read error for a docker worker leaves the run unclaimed —
// ClaimRun is never reached — rather than claiming against an unknown allowlist.
func TestClaimDockerWorkerStrictOnAllowlistReadError(t *testing.T) {
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	boom := errors.New("settings cold-cache blip")
	svc.SetDockerAllowlist(fakeDockerAllowlist{err: boom})

	_, err := svc.Claim(context.Background(), dockerWorker())
	if !errors.Is(err, boom) {
		t.Fatalf("Claim err = %v, want the settings read error surfaced", err)
	}
	if fs.claimParams != nil {
		t.Error("ClaimRun was called despite an allowlist read error — must not claim when the allowlist is unknown")
	}
}
