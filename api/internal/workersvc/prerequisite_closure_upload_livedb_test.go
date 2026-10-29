package workersvc

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPrerequisiteUploadKicksBrokerOnceLiveDB(t *testing.T) {
	for _, name := range []string{"ack", "lost_ack", "request_cancelled"} {
		t.Run(name, func(t *testing.T) {
			f := newPrerequisiteExecutorLiveDB(t, 1)
			// Only mutable registration changes: custody/capture provenance stays immutable.
			f.env.exec("UPDATE workers SET name='held-worker' WHERE id=(SELECT worker_id FROM runs WHERE id=$1)", f.m.row.RunID)
			// The forge and smart-HTTP server are synthetic evidence, never a GitHub probe.
			// The production broker derives its local clone URL from repo.web_url + ".git".
			f.env.exec("UPDATE forge_connections SET base_url='https://github.com' WHERE id=$1", f.m.conn.ID)
			r := m3RecoveryFixture(t, f, 16)
			f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", r.worker.ID, []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1, capability.RecoveryPrerequisiteClosureV1})
			var err error
			r.worker, err = f.env.q.GetWorkerByID(f.env.ctx, r.worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			r.manifest.PrerequisiteShas = append([]string(nil), f.m.row.PrerequisiteShas...)
			f.h.refName = pushbroker.PrereqRef(r.capture)
			f.h.lost = name == "lost_ack"
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			f.s.StartPrerequisiteClosures(ctx)
			t.Cleanup(func() {
				if err := f.s.DrainPrerequisiteClosures(context.Background()); err != nil {
					t.Error(err)
				}
			})
			uploadCtx, cancelUpload := context.WithCancel(ctx)
			defer cancelUpload()
			var kicks, admitted atomic.Int32
			r.svc.SetPrerequisiteClosure([]string{"github"}, func(user, capture uuid.UUID) {
				kicks.Add(1)
				if name == "request_cancelled" {
					// The upload transaction is committed; losing the request cannot cancel the lane.
					cancelUpload()
				}
				if f.s.KickPrerequisiteClosure(user, capture) {
					admitted.Add(1)
				}
			})
			// Discard the first receipt as a worker that lost the upload ACK would.
			up, err := r.upload(uploadCtx, f, bytes.NewReader(r.body))
			if name == "request_cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled upload receipt: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				m3UploadAssert(t, r, up)
			}
			joined := make(chan struct{})
			go func() { f.s.prerequisite.wg.Wait(); close(joined) }()
			m3Wait(t, ctx, joined)
			read := func() store.RecoveryPrerequisiteClosure {
				t.Helper()
				row, err := f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{UserID: r.worker.UserID, CaptureID: r.capture})
				if err != nil {
					t.Fatal(err)
				}
				return row
			}
			before := read()
			if before.State != "pinned" || before.Attempts != 1 || !before.InvokedAt.Valid || before.LiveRunID != prerequisiteID(f.m.row.RunID) ||
				before.CaptureID != r.capture || before.HoldID != f.m.row.HoldID || before.RunID != f.m.row.RunID ||
				before.RepoID != f.m.row.RepoID || before.Checksum != r.manifest.Checksum ||
				!reflect.DeepEqual(before.PrerequisiteShas, r.manifest.PrerequisiteShas) ||
				before.PinRef != pushbroker.PrereqRef(r.capture) || before.PinTip.String != prerequisiteTestTip ||
				before.ConnectionID != prerequisiteID(f.m.conn.ID) || before.ForgeKind.String != "github" ||
				before.NormalizedBaseUrl.String != "https://github.com" || before.ProjectID.Int64 != 1 || before.DefaultBranch.String != "main" {
				t.Fatalf("frozen row: %+v", before)
			}
			// Concurrent upload retries are bounded to four calls; every result is joined,
			// and one failed retry does not prevent its siblings from completing.
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := r.upload(ctx, f, bytes.NewReader(r.body)); errs <- err }()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Errorf("concurrent retry: %v", err)
				}
			}
			after := read()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("retry changed frozen closure row")
			}
			var rows, markers int
			if err := f.env.pool.QueryRow(ctx, "SELECT count(*),count(invoked_at) FROM recovery_prerequisite_closures WHERE capture_id=$1", r.capture).Scan(&rows, &markers); err != nil {
				t.Fatal(err)
			}
			f.h.mu.Lock()
			sends, deletes, tip := f.h.sends, f.h.deletes, f.h.tip
			f.h.mu.Unlock()
			if rows != 1 || markers != 1 || sends != 1 || deletes != 0 || tip != prerequisiteTestTip || kicks.Load() != 1 || admitted.Load() != 1 {
				t.Fatalf("rows=%d markers=%d sends=%d deletes=%d tip=%s kicks=%d admitted=%d", rows, markers, sends, deletes, tip, kicks.Load(), admitted.Load())
			}
			// Pinning prerequisites never makes a thin archive a self-contained FINAL.
			if _, err := r.svc.Release(ctx, r.worker, f.m.row.RunID, r.final); !errors.Is(err, recovery.ErrManifestConflict) {
				t.Fatalf("thin FINAL: %v", err)
			}
			var holdState string
			err = f.env.pool.QueryRow(ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", f.m.row.HoldID).Scan(&holdState)
			if err != nil || holdState != "open" {
				t.Fatalf("FINAL changed custody: %s %v", holdState, err)
			}
		})
	}
}
