package handler

import (
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCodexAccountHTTPReasonAndPrecedence(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		body   string
	}{
		{workersvc.ErrCodexAccountUnavailable, 409, "{\"error\":\"codex credential is not available\",\"reason\":\"codex_account_unavailable\"}\n"},
		{workersvc.ErrCodexAccountQuarantined, 409, "{\"error\":\"codex credential is not available\"}\n"},
		{workersvc.ErrCodexRefreshUnrecoverable, 409, "{\"error\":\"codex refresh is unavailable\"}\n"},
	} {
		rec := httptest.NewRecorder()
		(&Handler{}).writeCodexError(rec, "refresh", tc.err)
		if rec.Code != tc.status || rec.Body.String() != tc.body {
			t.Fatalf("error %v: %d %q want %d %q", tc.err, rec.Code, rec.Body.String(), tc.status, tc.body)
		}
	}
	for _, parent := range []error{workersvc.ErrRunNotOwned, workersvc.ErrCodexRunNotBound, workersvc.ErrCodexWorkerMismatch, workersvc.ErrCodexCapabilityEpoch, workersvc.ErrCodexVaultLocked} {
		status, message, reason := codexHTTPError(errors.Join(workersvc.ErrCodexAccountUnavailable, parent))
		wantStatus, wantMessage, wantReason := codexHTTPError(parent)
		if status != wantStatus || message != wantMessage || reason != wantReason {
			t.Fatalf("precedence: %v", parent)
		}
	}
	status, _, _ := codexHTTPError(workersvc.ErrCodexAccountUnavailable)
	if status != http.StatusConflict {
		t.Fatal(status)
	}
}

func TestCodexRouteErrorClasses(t *testing.T) {
	logs := installTimingCapture(t)
	for _, tc := range []struct {
		err     error
		success bool
		want    string
	}{
		{nil, true, ""}, {nil, false, "internal"},
		{errors.New("private provider text"), false, "internal"},
		{workersvc.ErrRunNotOwned, false, "not_found"},
		{workersvc.ErrCodexRunNotBound, false, "not_found"},
		{workersvc.ErrCodexWorkerMismatch, false, "not_found"},
		{workersvc.ErrCodexCapabilityMismatch, false, "capability"},
		{workersvc.ErrCodexCapabilityEpoch, false, "capability_epoch"},
		{workersvc.ErrCodexScopeNotApplicable, false, "scope_not_applicable"},
		{workersvc.ErrCodexKindModeMismatch, false, "kind_mode_mismatch"},
		{workersvc.ErrCodexVaultLocked, false, "vault_locked"},
		{workersvc.ErrCodexRefreshContended, false, "contended"},
		{workersvc.ErrCodexRefreshQuarantined, false, "refresh_quarantined"},
		{workersvc.ErrCodexRefreshNoToken, false, "refresh_no_token"},
		{workersvc.ErrCodexRefreshNoClient, false, "refresh_no_client"},
		{workersvc.ErrCodexRunNotActivelyClaimed, false, "run_not_actively_claimed"},
		{workersvc.ErrCodexAccountKeyUnfrozen, false, "account_key_unfrozen"},
		{workersvc.ErrCodexAccountTupleMismatch, false, "account_tuple_mismatch"},
		{workersvc.ErrCodexAccountRevisionStale, false, "account_revision_stale"},
		{workersvc.ErrCodexBindingConflict, false, "binding_conflict"},
		{workersvc.ErrCodexAccountQuarantined, false, "account_quarantined"},
		{workersvc.ErrCodexMaterialRevisionStale, false, "material_revision_stale"},
		{errors.Join(workersvc.ErrCodexRefreshRejected, workersvc.ErrCodexRefreshUnrecoverable), false, "refresh_rejected"},
		{workersvc.ErrCodexRefreshUnrecoverable, false, "refresh_unrecoverable"},
		{errors.Join(workersvc.ErrCodexVaultLocked, workersvc.ErrCodexRefreshQuarantined), false, "vault_locked"},
		{errors.Join(workersvc.ErrCodexCapabilityEpoch, workersvc.ErrCodexVaultLocked), false, "capability_epoch"},
	} {
		if got := codexRouteErrorClass(tc.err, tc.success); got != tc.want {
			t.Fatalf("class=%s want=%s", got, tc.want)
		}
		result := "error"
		if tc.success {
			result = "ok"
		}
		logCodexRouteTiming(uuid.New(), time.Now(), result, tc.err)
		records := logs.snapshot()
		record := records[len(records)-1]
		if record.attrs["error_class"].String() != tc.want {
			t.Fatalf("logged class=%v want=%s", record.attrs, tc.want)
		}
		if _, ok := record.attrs["account_hold"]; ok {
			t.Fatal("absent verdict logged")
		}
		for _, key := range []string{"error", "secret_id", "account_id", "user_id"} {
			if _, ok := record.attrs[key]; ok {
				t.Fatalf("diagnostic leaked %s", key)
			}
		}
	}
}
