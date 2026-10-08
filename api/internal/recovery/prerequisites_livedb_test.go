package recovery

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Both transaction entry points must compare declaration identity before returning
// an available receipt, including the stream loser after another upload commits.
func TestRecoveryPrerequisiteRetryLiveDB(t *testing.T) {
	for _, stored := range [][]string{nil, {}, {"aaaa1111", "bbbb2222"}} {
		e := newInventoryEnv(t)
		id := e.reserve("retry")
		body := []byte("immutable archive")
		m := manifestOf(body)
		m.PrerequisiteShas = stored
		if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		for _, candidate := range [][]string{nil, {}, {"aaaa1111", "bbbb2222"}, {"bbbb2222", "aaaa1111"}, {"AAAA1111", "bbbb2222"}, {"cccc3333"}} {
			same := len(stored) == 0 && len(candidate) == 0 || len(stored) > 0 && strings.Join(stored, ",") == strings.Join(candidate, ",")
			retry := m
			retry.PrerequisiteShas = candidate
			for _, entry := range []string{"admit", "stream"} {
				var err error
				if entry == "admit" {
					_, done, admitErr := e.svc.admit(e.ctx, e.w, e.run, id, retry)
					err = admitErr
					if same && !done {
						t.Fatal("available retry did not return receipt")
					}
				} else {
					_, err = e.svc.stream(e.ctx, e.w, e.run, id, retry, bytes.NewReader(nil))
				}
				if same && err != nil || !same && !errors.Is(err, ErrManifestConflict) {
					t.Fatalf("%s stored=%v retry=%v: %v", entry, stored, candidate, err)
				}
			}
		}
		// Preserve existing committed-byte receipt authority, without changing its declaration.
		retry := m
		retry.ByteSize++
		retry.Checksum = strings.Repeat("b", 64)
		if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, retry, bytes.NewReader(nil)); err != nil {
			t.Fatalf("available byte receipt: %v", err)
		}
		e.bytes(id, body)
		cap, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
		if err != nil || strings.Join(cap.PrerequisiteShas, ",") != strings.Join(stored, ",") || cap.ByteSize.Int64 != m.ByteSize || cap.Checksum.String != m.Checksum {
			t.Fatalf("receipt retries changed manifest: %+v %v", cap, err)
		}
	}
}

func TestRecoveryPrerequisiteFinalLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("thin")
	body := []byte("thin declaration")
	m := manifestOf(body)
	m.PrerequisiteShas = []string{"bbbb2222"}
	if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	e.reject(e.w, e.request(id), ErrManifestConflict)
	var intact bool
	if err := e.pool.QueryRow(e.ctx, `SELECT h.state='open' AND h.live_worker_id=$2 AND h.live_run_id=$3
 AND c.local_replica_worker_id IS NULL AND c.ready_retention_seconds IS NULL
 AND c.state='available' AND c.prerequisite_shas=ARRAY['bbbb2222']
 FROM recovery_custody_holds h JOIN recovery_captures c ON c.hold_id=h.id WHERE c.id=$1`, id, e.w.ID, e.run).Scan(&intact); err != nil || !intact {
		t.Fatalf("rejected FINAL mutated custody: %v %v", intact, err)
	}
	e.bytes(id, body)
	e.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", id)
	if _, err := e.q.ExpireReadyCaptures(e.ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true}); err != nil {
		t.Fatal(err)
	}
	cap, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
	if err != nil || cap.State != "expired" {
		t.Fatalf("archive did not expire: %+v %v", cap, err)
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("expired bytes retained: %+v %v", chunks, err)
	}
	hold, err := e.q.GetFinalInventoryHold(e.ctx, store.GetFinalInventoryHoldParams{RunID: e.run, UserID: e.w.UserID, WorkerID: e.w.ID, Generation: e.gen})
	if err != nil || hold.State != "open" || !hold.LiveWorkerID.Valid || !hold.LiveRunID.Valid {
		t.Fatalf("archive expiry settled custody: %+v %v", hold, err)
	}
}

func TestRecoveryHistoricalPrerequisiteReceiptLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("historical")
	e.upload(id)
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	req := e.request(id)
	e.release(req)
	// Model an already-committed receipt from before the new admission rule.
	e.exec("UPDATE recovery_captures SET prerequisite_shas=ARRAY['bbbb2222'] WHERE id=$1", id)
	var bound bool
	if err := e.pool.QueryRow(e.ctx, "SELECT manifest_bound AND prerequisite_shas=ARRAY['bbbb2222'] AND state='available' FROM recovery_captures WHERE id=$1", id).Scan(&bound); err != nil || !bound {
		t.Fatalf("historical bound prerequisites: %v %v", bound, err)
	}
	e.release(req)
}
