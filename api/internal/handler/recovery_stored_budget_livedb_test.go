package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/recovery"
)

// PRD #1909 M1 (D2): recovery-archive admission counts job-file bytes against
// UZI_STORED_FILES_BUDGET_BYTES, and an archive that still does not fit is refused exactly as
// today (507). This drives the real WORKER HTTP handler through the real recoveryLimits mapping.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRecoveryUploadStoredFilesBudgetHTTPLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	// Instance-wide sums: start from empty file stores (safe under the sweep's -p 1 on a
	// throwaway database; the other tests seed their own rows).
	e.mustExec(`DELETE FROM job_files`)
	e.mustExec(`DELETE FROM recovery_captures`)

	h := e.handler()
	h.cfg.StoredFilesBudgetBytes = 500
	if got := h.recoveryLimits().StoredFilesBudgetBytes; got != 500 {
		t.Fatalf("recoveryLimits().StoredFilesBudgetBytes = %d, want the configured 500", got)
	}
	svc := recovery.New(h.q, h.pool, h.box, h.recoveryLimits(), nil)
	h.recoverySvc = svc

	// 400 bytes of a fresh unattached job input: counted, and not reclaimable inside its TTL.
	e.mustExec(`INSERT INTO job_files (id, user_id, direction, display_name, content_type, byte_size, sha256, storage_name, chunk_count, state, expires_at)
	            VALUES ($1, $2, 'input', 'held.txt', 'text/plain', 400, $3, $4, 0, 'unattached', now() + interval '1 hour')`,
		uuid.New(), e.user, sha256hex([]byte("held")), sha256hex([]byte("held"))+".txt")

	upload := func(key string, data []byte) int {
		res := e.reserve(svc, e.worker, key, "dddd4444")
		m, _ := json.Marshal(manifestFor(data))
		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/worker/runs/%s/archives/%s/upload", e.run, res.CaptureID), bytes.NewReader(data))
		req.Header.Set(recoveryManifestHeader, string(m))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", e.run.String())
		rctx.URLParams.Add("captureID", res.CaptureID)
		ctx := mw.ContextWithWorker(req.Context(), e.worker)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		h.WorkerRecoveryUpload(rec, req.WithContext(ctx))
		return rec.Code
	}
	if code := upload("k-over", bytes.Repeat([]byte("o"), 101)); code != http.StatusInsufficientStorage {
		t.Fatalf("101-byte archive with 100 of the shared budget left: status %d, want 507", code)
	}
	if code := upload("k-fit", bytes.Repeat([]byte("f"), 100)); code != http.StatusOK {
		t.Fatalf("100-byte archive into the 100 left: status %d, want 200", code)
	}
}
