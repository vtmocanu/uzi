package recovery

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRecoveryUploadForeignWorkerRefusalLiveDB(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "guarded"}[guarded], func(t *testing.T) {
			e := newInventoryEnv(t)
			if !guarded {
				e.exec("DELETE FROM recovery_custody_holds WHERE id=$1", e.hold)
				e.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state)
     VALUES($1,$2,(SELECT repo_id FROM runs WHERE id=$3),$3,1,$4,$5,$4,$3,false,'open')`, e.hold, e.w.UserID, e.run, e.w.ID, e.w.Name)
			}
			id := e.reserve("worker-refusal")
			other := e.w
			other.ID = uuid.New()
			e.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", other.ID, other.UserID, other.ID.String(), other.ID[:])
			body := []byte("not this worker's inventory")
			_, err := e.svc.Upload(e.ctx, other, e.run, id, manifestOf(body), bytes.NewReader(body))
			if !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("same-owner foreign worker refusal=%v; want not authorized", err)
			}
			other.UserID = uuid.New()
			_, err = e.svc.Upload(e.ctx, other, e.run, id, manifestOf(body), bytes.NewReader(body))
			if !errors.Is(err, ErrCaptureNotFound) {
				t.Fatalf("foreign owner exposed capture: %v", err)
			}
		})
	}
}
