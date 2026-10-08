package recovery

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func reconcileRequest(body []byte) apitypes.RecoveryReconcileRequest {
	m := manifestOf(body)
	return apitypes.RecoveryReconcileRequest{Generation: 1, SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64), Checksum: m.Checksum, ByteSize: m.ByteSize}
}

func TestRecoveryReconcileDecisionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason, expiry, outcome, retained    string
		unbound, mismatchSize, mismatchChecksum, nullSize bool
	}{
		{name: "preparing", state: "preparing", outcome: "replaceable", unbound: true},
		{name: "uploading", state: "uploading", outcome: "replaceable", unbound: true},
		{name: "stalled", state: "needs_action", reason: "upload_retry_window_exhausted", outcome: "replaceable", unbound: true},
		{name: "quota", state: "needs_action", reason: "storage quota exceeded", outcome: "replaceable"},
		{name: "transport", state: "needs_action", reason: "upload failed; retry available", outcome: "replaceable"},
		{name: "discarded", state: "discarded", outcome: "replaceable"},
		{name: "expired", state: "expired", outcome: "replaceable"},
		{name: "expiry-only", state: "available", expiry: "past", outcome: "replaceable"},
		{name: "healthy", state: "available", outcome: "retained", retained: "capture_available"},
		{name: "available-unbound", state: "available", unbound: true, outcome: "retained", retained: "available_capture_manifest_unbound"},
		{name: "available-unbound-past", state: "available", unbound: true, expiry: "past", outcome: "retained", retained: "available_capture_manifest_unbound"},
		{name: "available-no-expiry", state: "available", expiry: "null", outcome: "retained", retained: "available_capture_expiry_invalid"},
		{name: "integrity", state: "needs_action", reason: "archive integrity check failed", expiry: "past", outcome: "retained", retained: "capture_failure_requires_attention"},
		{name: "unknown-failure", state: "needs_action", reason: "unknown", expiry: "past", outcome: "retained", retained: "capture_failure_requires_attention"},
		{name: "absent-failure", state: "needs_action", outcome: "retained", retained: "capture_failure_requires_attention"},
		{name: "preparing-failure", state: "preparing", reason: "unknown", expiry: "past", outcome: "retained", retained: "capture_failure_requires_attention"},
		{name: "uploading-failure", state: "uploading", reason: "unknown", outcome: "retained", retained: "capture_failure_requires_attention"},
		{name: "discarded-failure", state: "discarded", reason: "unknown", outcome: "retained", retained: "capture_failure_requires_attention"},
		{name: "expired-integrity", state: "expired", reason: "archive integrity check failed", outcome: "retained", retained: "capture_failure_requires_attention"},
		{name: "malformed-bound", state: "preparing", nullSize: true, outcome: "retained", retained: "capture_manifest_invalid"},
		{name: "expiry-size", state: "available", expiry: "past", mismatchSize: true, outcome: "retained", retained: "capture_manifest_integrity_mismatch"},
		{name: "expired-size", state: "expired", mismatchSize: true, outcome: "retained", retained: "capture_manifest_integrity_mismatch"},
		{name: "discarded-size", state: "discarded", mismatchSize: true, outcome: "retained", retained: "capture_manifest_integrity_mismatch"},
		{name: "expiry-checksum", state: "available", expiry: "past", mismatchChecksum: true, outcome: "retained", retained: "capture_manifest_integrity_mismatch"},
		{name: "expired-checksum", state: "expired", mismatchChecksum: true, outcome: "retained", retained: "capture_manifest_integrity_mismatch"},
		{name: "discarded-checksum", state: "discarded", mismatchChecksum: true, outcome: "retained", retained: "capture_manifest_integrity_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInventoryEnv(t)
			id := e.reserve(tc.name)
			body := e.upload(id)
			req := reconcileRequest(body)
			e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
			e.exec("UPDATE recovery_captures SET state=$2,reason=NULLIF($3,''),reserved_bytes=123 WHERE id=$1", id, tc.state, tc.reason)
			if tc.unbound {
				e.exec("UPDATE recovery_captures SET manifest_bound=false,checksum=NULL,byte_size=NULL,chunk_count=NULL WHERE id=$1", id)
			}
			if tc.expiry == "past" {
				e.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", id)
			}
			if tc.expiry == "null" {
				e.exec("UPDATE recovery_captures SET expires_at=NULL WHERE id=$1", id)
			}
			if tc.nullSize {
				e.exec("UPDATE recovery_captures SET byte_size=NULL WHERE id=$1", id)
			}
			if tc.mismatchSize {
				req.ByteSize++
			}
			if tc.mismatchChecksum {
				req.Checksum = strings.Repeat("b", 64)
			}
			get := func() store.RecoveryCapture {
				t.Helper()
				c, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			budget := func() store.SumStoredFileBytesRow {
				t.Helper()
				sums, err := e.q.SumStoredFileBytes(e.ctx, store.SumStoredFileBytesParams{UserID: e.w.UserID})
				if err != nil {
					t.Fatal(err)
				}
				return sums
			}
			before := get()
			budgetBefore := budget()
			hold, err := e.q.GetFinalInventoryHold(e.ctx, store.GetFinalInventoryHoldParams{RunID: e.run, UserID: e.w.UserID, WorkerID: e.w.ID, Generation: 1})
			if err != nil {
				t.Fatal(err)
			}
			chunksBefore, err := e.q.ListCaptureChunks(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			res, err := e.svc.Reconcile(e.ctx, e.w, e.run, id, req)
			if err != nil || res.Outcome != tc.outcome || res.Reason != tc.retained || res.FinalReceipt != nil {
				t.Fatalf("reconcile: %+v %v", res, err)
			}
			after := get()
			budgetAfter := budget()
			chunks, err := e.q.ListCaptureChunks(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			afterHold, err := e.q.GetFinalInventoryHold(e.ctx, store.GetFinalInventoryHoldParams{RunID: e.run, UserID: e.w.UserID, WorkerID: e.w.ID, Generation: 1})
			if err != nil || !reflect.DeepEqual(hold, afterHold) {
				t.Fatalf("hold changed: %+v %v", afterHold, err)
			}
			if tc.outcome == "retained" {
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(chunksBefore, chunks) || budgetBefore != budgetAfter {
					t.Fatal("retained mutated capture, chunks, reservation, or stored budget")
				}
				return
			}
			if after.State != "expired" || after.ReservedBytes.Valid || after.Reason.String != "unaccepted_capture_replaced" || len(chunks) != 0 {
				t.Fatalf("fence did not reclaim bytes: %+v chunks=%d", after, len(chunks))
			}
			var charged int64
			switch before.State {
			case "available":
				charged = before.ByteSize.Int64
			case "preparing", "uploading":
				charged = before.ReservedBytes.Int64
			}
			if budgetBefore.OwnerRecoveryBytes-budgetAfter.OwnerRecoveryBytes != charged ||
				budgetBefore.InstanceRecoveryBytes-budgetAfter.InstanceRecoveryBytes != charged ||
				budgetBefore.OwnerJobBytes != budgetAfter.OwnerJobBytes ||
				budgetBefore.InstanceJobBytes != budgetAfter.InstanceJobBytes || afterHold.State != "open" {
				t.Fatalf("fence budget: before=%+v after=%+v charged=%d hold=%s", budgetBefore, budgetAfter, charged, afterHold.State)
			}
			// Lost response: the identical retry returns replacement authority.
			retry, err := e.svc.Reconcile(e.ctx, e.w, e.run, id, req)
			if err != nil || retry.Outcome != "replaceable" {
				t.Fatalf("retry: %+v %v", retry, err)
			}
			if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, manifestOf(body), bytes.NewReader(body)); !errors.Is(err, ErrNotAvailable) {
				t.Fatalf("delayed upload: %v", err)
			}
			e.reject(e.w, e.request(id), ErrManifestConflict)
			failed, err := e.q.MarkCaptureFailed(e.ctx, store.MarkCaptureFailedParams{ID: id})
			if err == nil {
				t.Fatalf("delayed failure revived fenced capture: %+v", failed)
			}
			// Reconsidering an old request never deletes on a changed byte identity.
			if !tc.unbound {
				req.ByteSize++
				res, err = e.svc.Reconcile(e.ctx, e.w, e.run, id, req)
				if err != nil || res.Outcome != "retained" || res.Reason != "capture_manifest_integrity_mismatch" {
					t.Fatalf("changed retry: %+v %v", res, err)
				}
			}
		})
	}
}

func TestRecoveryReconcileReceiptFirstLiveDB(t *testing.T) {
	for _, kind := range []string{"archive", "settled"} {
		t.Run(kind, func(t *testing.T) {
			e := newInventoryEnv(t)
			id := e.reserve("receipt")
			body := e.upload(id)
			e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
			release := e.request(id)
			if kind == "settled" {
				evidence := "publication"
				release.FinalDisposition = &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}
				release.ReleaseEvidence = &evidence
			}
			e.release(release)
			e.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", id)
			before, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
			if err != nil {
				t.Fatal(err)
			}
			req := reconcileRequest(body)
			req.ByteSize++
			req.SourceSha = "bbbb2222"
			req.CoverageDigest = strings.Repeat("b", 64)
			res, err := e.svc.Reconcile(e.ctx, e.w, e.run, id, req)
			if err != nil || res.Outcome != "accepted" || !reflect.DeepEqual(res.FinalReceipt, release.FinalDisposition) {
				t.Fatalf("receipt: %+v %v", res, err)
			}
			if kind == "settled" && res.ReleaseEvidence != "publication" {
				t.Fatal("missing settled evidence")
			}
			after, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("accepted receipt mutated capture")
			}
			e.bytes(id, body)
		})
	}
}

func TestRecoveryReconcileIdentityLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("identity")
	body := e.upload(id)
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	for _, field := range []string{"worker", "user", "run", "capture", "generation", "source", "coverage", "capability", "malformed-sha", "malformed-checksum", "malformed-coverage", "negative-size"} {
		t.Run(field, func(t *testing.T) {
			w, run, capID, req := e.w, e.run, id, reconcileRequest(body)
			want := ErrNotAuthorized
			switch field {
			case "worker":
				w.ID = uuid.New()
			case "user":
				w.UserID = uuid.New()
			case "run":
				run = uuid.New()
			case "capture":
				capID = uuid.New()
			case "generation":
				req.Generation++
			case "source":
				req.SourceSha = "bbbb2222"
				want = ErrManifestConflict
			case "coverage":
				req.CoverageDigest = strings.Repeat("b", 64)
				want = ErrManifestConflict
			case "capability":
				w.ProtocolCapabilities = nil
				want = ErrBadRequest
			case "malformed-sha":
				req.SourceSha = "bad"
				want = ErrBadRequest
			case "malformed-checksum":
				req.Checksum = "bad"
				want = ErrBadRequest
			case "malformed-coverage":
				req.CoverageDigest = "bad"
				want = ErrBadRequest
			case "negative-size":
				req.ByteSize = -1
				want = ErrBadRequest
			}
			res, err := e.svc.Reconcile(e.ctx, w, run, capID, req)
			if !errors.Is(err, want) {
				t.Fatalf("identity: %+v %v want %v", res, err, want)
			}
			e.bytes(id, body)
		})
	}
}
