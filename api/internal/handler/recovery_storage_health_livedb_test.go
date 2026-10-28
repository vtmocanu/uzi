package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func storageCheck(t *testing.T, h *Handler) apitypes.HealthCheckDTO {
	t.Helper()
	d, err := h.healthService().Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Checks {
		if c.ID == "recovery.storage" {
			return c
		}
	}
	t.Fatal("missing recovery.storage")
	return apitypes.HealthCheckDTO{}
}

func storageEvidence(c apitypes.HealthCheckDTO, label string) string {
	for _, e := range c.Evidence {
		if e.Label == label {
			return e.Value
		}
	}
	return ""
}

// Executes the generated aggregate on PostgreSQL, including owners outside the example cutoff.
func TestRecoveryStorageAccountingLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	e.mustExec("DELETE FROM job_files")
	e.mustExec("DELETE FROM recovery_captures")
	h := e.handler()
	h.settings = settings.New(&settingsStore{}, time.Minute)
	for i := 0; i < 10; i++ {
		owner := uuid.New()
		e.mustExec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@storage.e2e")
		for _, state := range []string{"available", "preparing", "uploading", "needs_action", "expired", "discarded"} {
			reason := "storage quota exceeded"
			e.mustExec(`INSERT INTO recovery_captures(hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,byte_size,reserved_bytes,reason)
   VALUES($1,$2,$3,'fixed-worker','abcd',$4,$5,11,7,$6)`, e.holdID, e.run, owner, fmt.Sprintf("%d-%s", i, state), state, reason)
		}
		// Neither an inexact reason nor a different needs_action reason is a refusal marker.
		for j, reason := range []string{"storage quota exceeded ", "upload_retry_window_exhausted"} {
			e.mustExec(`INSERT INTO recovery_captures(hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,reason)
   VALUES($1,$2,$3,'fixed-worker','abcd',$4,'needs_action',$5)`, e.holdID, e.run, owner, fmt.Sprintf("negative-%d-%d", i, j), reason)
		}
		for _, f := range []struct {
			state  string
			expiry any
		}{
			{"reserved", nil}, {"attached", nil}, {"unattached", time.Now().Add(time.Hour)},
			{"unattached", time.Now().Add(-time.Hour)}, {"available", nil}, {"expired", nil},
		} {
			id := uuid.New()
			e.mustExec(`INSERT INTO job_files(id,user_id,direction,display_name,content_type,byte_size,sha256,storage_name,chunk_count,state,expires_at,run_id)
   VALUES($1,$2,'input','fixed.txt','text/plain',13,$3,$4,0,$5,$6,$7)`, id, owner, sha256hex([]byte("fixed")), sha256hex([]byte("fixed"))+".txt", f.state, f.expiry, e.run)
		}
	}
	rows, err := h.q.RecoveryStorageHealth(e.ctx, store.RecoveryStorageHealthParams{Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, ExampleLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatalf("examples: %d", len(rows))
	}
	for _, r := range rows {
		if r.OwnerCount != 10 || r.OmittedCount != 2 || r.AvailableCount != 10 || r.AvailableBytes != 110 ||
			r.PreparingCount != 10 || r.PreparingBytes != 70 || r.UploadingCount != 10 || r.UploadingBytes != 70 ||
			r.JobCount != 50 || r.JobBytes != 650 || r.ReclaimableBytes != 260 || r.RefusedCount != 10 || r.MaxOwnerRecoveryBytes != 25 {
			t.Errorf("global accounting: %+v", r)
		}
		if !r.OwnerID.Valid || r.OwnerAvailableCount != 1 || r.OwnerAvailableBytes != 11 ||
			r.OwnerPreparingCount != 1 || r.OwnerPreparingBytes != 7 || r.OwnerUploadingCount != 1 || r.OwnerUploadingBytes != 7 ||
			r.OwnerJobCount != 5 || r.OwnerJobBytes != 65 || r.OwnerReclaimableBytes != 26 || r.OwnerRefusedCount != 1 {
			t.Errorf("exact owner accounting: %+v", r)
		}
	}
	c := storageCheck(t, h)
	if c.Severity != "warn" || storageEvidence(c, "Shared stored bytes") != "900" || storageEvidence(c, "Omitted owner examples") != "2" {
		t.Errorf("health accounting: %+v", c)
	}
	e.mustExec("DELETE FROM job_files")
	e.mustExec("DELETE FROM recovery_captures")
	c = storageCheck(t, h)
	if c.Severity != "ok" || storageEvidence(c, "Owners") != "0" || storageEvidence(c, "Maximum owner recovery bytes") != "0" {
		t.Errorf("empty accounting: %+v", c)
	}
}

// The only pressured owner sorts ninth, beyond the fixed eight owner examples.
func TestRecoveryStorageUnsampledOwnerPressureLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	e.mustExec("DELETE FROM job_files")
	e.mustExec("DELETE FROM recovery_captures")
	h := e.handler()
	h.settings = settings.New(&settingsStore{}, time.Minute)
	h.cfg.RecoveryReadyPayloadPerOwner = 100
	h.cfg.RecoveryInstanceBytes = 0
	h.cfg.StoredFilesBudgetBytes = 0
	var owners []uuid.UUID
	for i := 1; i <= 9; i++ {
		owner := uuid.MustParse(fmt.Sprintf("35840000-0000-4000-8000-%012d", i))
		owners = append(owners, owner)
		// Repeated live-DB runs can retain users, but all their contributions were cleared above.
		e.mustExec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x') ON CONFLICT (id) DO NOTHING", owner, owner.String()+"@storage.example.com")
		available := int64(10)
		if i == 9 {
			available = 30
		}
		for _, capture := range []struct {
			state           string
			bytes, reserved int64
		}{
			{"available", available, 999},
			{"preparing", 999, 20},
			{"uploading", 999, 30},
		} {
			e.mustExec(`INSERT INTO recovery_captures(hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,byte_size,reserved_bytes)
   VALUES($1,$2,$3,'fixed-worker','abcd',$4,$5,$6,$7)`,
				e.holdID, e.run, owner, fmt.Sprintf("pressure-%d-%s", i, capture.state), capture.state, capture.bytes, capture.reserved)
		}
		// Reclaimable files still consume the shared budget, and never the recovery budgets.
		id := uuid.New()
		e.mustExec(`INSERT INTO job_files(id,user_id,direction,display_name,content_type,byte_size,sha256,storage_name,chunk_count,state,run_id)
   VALUES($1,$2,'input','fixed.txt','text/plain',100,$3,$4,0,'available',$5)`,
			id, owner, sha256hex([]byte("fixed")), sha256hex([]byte("fixed"))+".txt", e.run)
	}
	rows, err := h.q.RecoveryStorageHealth(e.ctx, store.RecoveryStorageHealthParams{Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, ExampleLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatalf("examples: %d, want 8", len(rows))
	}
	for i, r := range rows {
		if r.MaxOwnerRecoveryBytes != 80 {
			t.Fatalf("maximum owner recovery bytes=%d, want 80 including unsampled owner", r.MaxOwnerRecoveryBytes)
		}
		if r.OwnerCount != 9 || r.OmittedCount != 1 || r.RefusedCount != 0 ||
			r.AvailableCount != 9 || r.AvailableBytes != 110 || r.PreparingCount != 9 || r.PreparingBytes != 180 ||
			r.UploadingCount != 9 || r.UploadingBytes != 270 || r.JobCount != 9 || r.JobBytes != 900 || r.ReclaimableBytes != 900 {
			t.Errorf("global accounting: %+v", r)
		}
		if !r.OwnerID.Valid || r.OwnerID.Bytes != [16]byte(owners[i]) || r.OwnerID.Bytes == [16]byte(owners[8]) ||
			r.OwnerAvailableCount != 1 || r.OwnerAvailableBytes != 10 || r.OwnerPreparingCount != 1 || r.OwnerPreparingBytes != 20 ||
			r.OwnerUploadingCount != 1 || r.OwnerUploadingBytes != 30 || r.OwnerJobCount != 1 || r.OwnerJobBytes != 100 ||
			r.OwnerReclaimableBytes != 100 || r.OwnerRefusedCount != 0 {
			t.Errorf("ordered unpressured example %d: %+v", i, r)
		}
	}
	c := storageCheck(t, h)
	if c.Severity != "warn" || c.Scope != "owner" ||
		c.Summary != "At least 80% of budget: per-owner recovery; 0 captures currently marked quota-refused." {
		t.Errorf("unsampled pressure: %+v", c)
	}
	for label, want := range map[string]string{
		"Maximum owner recovery bytes": "80", "Recovery bytes": "560", "Shared stored bytes": "1460",
		"Reclaimable job bytes": "900", "Quota-refused captures": "0", "Omitted owner examples": "1",
		"Recovery per-owner limit": "100 bytes", "Recovery instance limit": "disabled", "Shared stored-files limit": "disabled",
	} {
		if got := storageEvidence(c, label); got != want {
			t.Errorf("%s=%q, want %q", label, got, want)
		}
	}
	for _, evidence := range c.Evidence {
		if evidence.Label == "Owner "+owners[8].String() {
			t.Errorf("ninth owner leaked into health examples: %+v", evidence)
		}
	}
}

type storageInspectReader struct {
	io.Reader
	inspect   func()
	inspected bool
}

func (r *storageInspectReader) Read(p []byte) (int, error) {
	if !r.inspected {
		r.inspected = true
		r.inspect()
	}
	return r.Reader.Read(p)
}

// The prerequisite log precedes the warning assertion so a later branch-removal mutation
// proves that the real refusal and persisted marker survived before classification failed.
func TestRecoveryStorageUploadRefusalHealthLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	e.mustExec("DELETE FROM job_files")
	e.mustExec("DELETE FROM recovery_captures")
	h := e.handler()
	h.settings = settings.New(&settingsStore{}, time.Minute)
	h.cfg.RecoveryReadyPayloadPerOwner = 100
	h.cfg.RecoveryInstanceBytes = 200
	h.cfg.StoredFilesBudgetBytes = 300
	svc := h.recovery()
	data := bytes.Repeat([]byte("q"), 101)
	res := e.reserve(svc, e.worker, "quota-health", "abcd1234")
	manifest := manifestFor(data)
	upload := func(auth bool, body io.Reader, m apitypes.RecoveryUploadManifest) *httptest.ResponseRecorder {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/upload", body)
		req.Header.Set(recoveryManifestHeader, string(raw))
		route := chi.NewRouteContext()
		route.URLParams.Add("id", e.run.String())
		route.URLParams.Add("captureID", res.CaptureID)
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
		if auth {
			ctx = mw.ContextWithWorker(ctx, e.worker)
		}
		rec := httptest.NewRecorder()
		h.WorkerRecoveryUpload(rec, req.WithContext(ctx))
		return rec
	}
	assertState := func(t *testing.T, wantState, wantReason string) {
		t.Helper()
		var state string
		var reason *string
		if err := e.pool.QueryRow(e.ctx, "SELECT state,reason FROM recovery_captures WHERE id=$1", res.CaptureID).Scan(&state, &reason); err != nil {
			t.Fatal(err)
		}
		got := ""
		if reason != nil {
			got = *reason
		}
		if state != wantState || got != wantReason {
			t.Fatalf("persisted state=%s reason=%q, want %s %q", state, got, wantState, wantReason)
		}
	}
	t.Run("auth negative control", func(t *testing.T) {
		rec := upload(false, bytes.NewReader(data), manifest)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if c := storageCheck(t, h); c.Severity != "ok" {
			t.Errorf("auth caused warning: %+v", c)
		}
	})
	verified := false
	t.Run("HTTP refusal and persisted marker prerequisite", func(t *testing.T) {
		rec := upload(true, bytes.NewReader(data), manifest)
		if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), `"reason":"quota"`) {
			t.Fatalf("refusal status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertState(t, "needs_action", "storage quota exceeded")
		verified = true
		t.Log("prerequisite upload refusal 507 reason=quota and persisted needs_action storage quota exceeded verified")
	})
	t.Run("persisted refusal warns", func(t *testing.T) {
		if !verified {
			t.Fatal("HTTP refusal and persisted marker prerequisite failed")
		}
		c := storageCheck(t, h)
		if c.Severity != "warn" || c.Scope != "owner" || c.Summary != "1 captures currently marked quota-refused." {
			t.Errorf("refusal warning: %+v", c)
		}
		if storageEvidence(c, "Recovery per-owner limit") != "100 bytes" || storageEvidence(c, "Recovery instance limit") != "200 bytes" || storageEvidence(c, "Shared stored-files limit") != "300 bytes" {
			t.Errorf("lazy effective limits: %+v", c)
		}
	})
	t.Run("refused capture accounting", func(t *testing.T) {
		c := storageCheck(t, h)
		for label, want := range map[string]string{
			"Quota-refused captures": "1",
			"Available captures":     "0; 0 bytes",
			"Preparing reservations": "0; 0 bytes",
			"Uploading reservations": "0; 0 bytes",
			"Non-expired job files":  "0; 0 bytes",
			"Recovery bytes":         "0",
			"Shared stored bytes":    "0",
		} {
			if got := storageEvidence(c, label); got != want {
				t.Errorf("refusal accounting %s=%q, want %q", label, got, want)
			}
		}
	})
	t.Run("released custody preserves warning", func(t *testing.T) {
		e.mustExec("UPDATE recovery_custody_holds SET state='released',released_at=now(),live_worker_id=NULL,live_run_id=NULL WHERE id=$1", e.holdID)
		if c := storageCheck(t, h); c.Severity != "warn" {
			t.Errorf("released warning: %+v", c)
		}
		e.mustExec("UPDATE recovery_custody_holds SET state='open',released_at=NULL,live_worker_id=$2,live_run_id=$3 WHERE id=$1", e.holdID, e.worker.ID, e.run)
	})
	// Admit the same manifest under a higher ceiling; inspect BEFORE the reader yields bytes.
	previous := h
	h = e.handler()
	h.cfg = previous.cfg
	h.settings = previous.settings
	h.cfg.RecoveryReadyPayloadPerOwner = 1000
	h.recoverySvc = e.service(h.recoveryLimits())
	t.Run("admitted retry clears before success", func(t *testing.T) {
		reader := &storageInspectReader{Reader: bytes.NewReader(data), inspect: func() {
			assertState(t, "uploading", "")
			c := storageCheck(t, h)
			if c.Severity != "ok" || storageEvidence(c, "Uploading reservations") != "1; 101 bytes" {
				t.Errorf("admitted retry: %+v", c)
			}
		}}
		rec := upload(true, reader, manifest)
		if !reader.inspected || rec.Code != 200 {
			t.Errorf("retry status=%d inspected=%v body=%s", rec.Code, reader.inspected, rec.Body.String())
		}
		assertState(t, "available", "")
		c := storageCheck(t, h)
		if c.Severity != "ok" || storageEvidence(c, "Available captures") != "1; 101 bytes" {
			t.Errorf("success: %+v", c)
		}
	})
	t.Run("discard excludes stale marker", func(t *testing.T) {
		e.mustExec("UPDATE recovery_captures SET state='needs_action',reason='storage quota exceeded' WHERE id=$1", res.CaptureID)
		if _, err := svc.Discard(e.ctx, e.user, e.run, uuid.MustParse(res.CaptureID)); err != nil {
			t.Fatal(err)
		}
		if c := storageCheck(t, h); c.Severity != "ok" {
			t.Errorf("discard: %+v", c)
		}
	})
	t.Run("nonquota HTTP integrity refusal", func(t *testing.T) {
		res = e.reserve(h.recoverySvc, e.worker, "integrity-health", "abcd1234")
		rec := upload(true, bytes.NewReader(bytes.Repeat([]byte("x"), 101)), manifest)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"reason":"integrity"`) {
			t.Errorf("integrity status=%d body=%s", rec.Code, rec.Body.String())
		}
		if c := storageCheck(t, h); c.Severity != "ok" {
			t.Errorf("nonquota warning: %+v", c)
		}
	})
	t.Run("stalled retry replaces reason", func(t *testing.T) {
		e.mustExec("UPDATE recovery_captures SET state='needs_action',reason='storage quota exceeded',created_at=now()-interval '2 hours' WHERE id=$1", res.CaptureID)
		if _, err := h.q.StampCaptureReservation(e.ctx, store.StampCaptureReservationParams{ID: uuid.MustParse(res.CaptureID), ReservedBytes: pgtype.Int8{Int64: 101, Valid: true}}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.q.ExpireStalledUploads(e.ctx, pgtype.Interval{Microseconds: int64(time.Hour / time.Microsecond), Valid: true}); err != nil {
			t.Fatal(err)
		}
		assertState(t, "needs_action", "upload_retry_window_exhausted")
		if c := storageCheck(t, h); c.Severity != "ok" {
			t.Errorf("stalled nonquota: %+v", c)
		}
	})
}
