package schedsvc

import (
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Connect the actual scheduler collection/admission transaction to the actual
// claim assembly and input download fence, rather than seeding a repair directly.
func TestCIArtifactsSchedulerClaimFencedDownloadLiveDB(t *testing.T) {
	l := newCILive(t, ciRed("wf", 100, 0))
	l.enableArtifacts(t)
	svc := l.sc.runs.(*workersvc.Service)
	t.Cleanup(func() {
		for _, query := range []string{
			"DELETE FROM recovery_custody_holds WHERE user_id=$1",
			"DELETE FROM runs WHERE user_id=$1",
			"DELETE FROM workers WHERE user_id=$1",
		} {
			if _, err := l.pool.Exec(l.ctx, query, l.user); err != nil {
				t.Errorf("claim fixture cleanup: %v", err)
			}
		}
	})
	out := l.fire(t)
	if len(out.Started) != 1 {
		t.Fatalf("scheduler: %+v", out)
	}
	runID := out.Started[0].RunID
	worker := func(caps ...string) store.Worker {
		t.Helper()
		id, a, b := uuid.New(), uuid.New(), uuid.New()
		if _, err := l.pool.Exec(l.ctx, `INSERT INTO workers
			(id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities,max_concurrent_runs)
			VALUES ($1,$2,$3,$4,'online',now(),$5,1)`,
			id, l.user, "artifact-"+id.String(), append(a[:], b[:]...), caps); err != nil {
			t.Fatal(err)
		}
		w, err := l.q.GetWorkerByID(l.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	guardOnly := worker(capability.CIConfigGuardV1)
	artifactsOnly := worker(capability.CIArtifactsV1)
	for _, w := range []store.Worker{guardOnly, artifactsOnly} {
		p, err := svc.Claim(l.ctx, w, nil)
		if err != nil || p != nil {
			t.Fatalf("incapable worker claimed: payload=%+v err=%v", p, err)
		}
		var state string
		var owner pgtype.UUID
		if err := l.pool.QueryRow(l.ctx, "SELECT status,worker_id FROM runs WHERE id=$1", runID).Scan(&state, &owner); err != nil {
			t.Fatal(err)
		}
		if state != "queued" || owner.Valid {
			t.Fatalf("refused claim mutated run: state=%s owned=%v", state, owner.Valid)
		}
	}
	w := worker(capability.CIConfigGuardV1, capability.CIArtifactsV1)
	p, err := svc.Claim(l.ctx, w, nil)
	if err != nil || p == nil {
		t.Fatalf("capable claim: %+v %v", p, err)
	}
	if p.RunID != runID.String() || p.Job != nil || !p.Config.CIConfigGuard ||
		p.CIRepair == nil || len(p.CIRepair.InputFiles) != 1 || len(p.CIRepair.Manifest.Entries) != 1 {
		t.Fatalf("claim lost scheduler artifacts or guard: %+v", p.CIRepair)
	}
	file := p.CIRepair.InputFiles[0]
	entry := p.CIRepair.Manifest.Entries[0]
	if entry.ManifestID != file.ManifestID || entry.SHA256 != file.SHA256 || file.Size != 13 {
		t.Fatalf("claim identity: file=%+v entry=%+v", file, entry)
	}
	fileID := uuid.MustParse(file.ManifestID)
	read := func(w store.Worker, generation int64, want error) {
		t.Helper()
		_, r, err := svc.OpenJobInputFile(l.ctx, w, runID, fileID, generation)
		if want != nil {
			if !errors.Is(err, want) {
				t.Fatalf("download fence: %v want %v", err, want)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil || string(data) != "live evidence" {
			t.Fatalf("download: %q %v", data, err)
		}
	}
	read(w, p.ClaimGeneration, nil)
	read(guardOnly, p.ClaimGeneration, workersvc.ErrRunNotFound)
	read(w, p.ClaimGeneration-1, workersvc.ErrStaleClaim)
	if _, err := l.pool.Exec(l.ctx, "UPDATE runs SET claim_released_at=now() WHERE id=$1", runID); err != nil {
		t.Fatal(err)
	}
	read(w, p.ClaimGeneration, workersvc.ErrStaleClaim)
	if _, err := l.pool.Exec(l.ctx, "UPDATE runs SET claim_released_at=NULL,status='completed',finished_at=now() WHERE id=$1", runID); err != nil {
		t.Fatal(err)
	}
	read(w, p.ClaimGeneration, workersvc.ErrRunTerminal)
}
