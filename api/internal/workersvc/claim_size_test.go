package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimSizeLimitMatchesSharedFixture pins claimResponseMaxBytes to the figure the worker
// client reads from the same fixture (agent/test/client-claim-response-bound.test.ts).
func TestClaimSizeLimitMatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/claim-response-limit.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Max int `json:"claim_response_max_bytes"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Max != claimResponseMaxBytes {
		t.Fatalf("claimResponseMaxBytes = %d, fixture = %d", claimResponseMaxBytes, fx.Max)
	}
}

const claimSizeTemplateMarker = "TEMPLATE-BODY-MARKER"

// claimSizeStore is an issue-run fake store whose user templates encode at 6 bytes per '<'.
func claimSizeStore(t *testing.T) *fakeStore {
	t.Helper()
	fs := scheduleModelStore(t, pgtype.Text{}, pgtype.Text{})
	fs.templates = []store.AgentTemplate{
		{Name: "big", Description: "d", PromptBody: claimSizeTemplateMarker + strings.Repeat("<", 4096)},
	}
	return fs
}

func TestOversizeRunClaimFailsTerminallyClaimSize(t *testing.T) {
	fs := claimSizeStore(t)
	svc := newOffModeService(fs, newBox(t), testParams())
	svc.claimSizeLimit = 8192
	payload, err := svc.assembleAndFinishRunClaim(context.Background(), worker(), fs.claimRun, false)
	if payload != nil || err != nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v), want idle", payload, err)
	}
	got := fs.claimFailed
	if got == nil {
		t.Fatal("FailClaimAssemblyExact not called")
	}
	if got.FailOrigin.String != "provisioning_failed" {
		t.Fatalf("FailOrigin = %q", got.FailOrigin.String)
	}
	if !strings.HasPrefix(got.FailureReason.String, "claim too large:") {
		t.Fatalf("FailureReason = %q", got.FailureReason.String)
	}
	if strings.Contains(got.FailureReason.String, claimSizeTemplateMarker) {
		t.Fatal("failure reason leaked template content")
	}
}

func TestUnderLimitRunClaimClaimSizeDelivers(t *testing.T) {
	fs := claimSizeStore(t)
	svc := newOffModeService(fs, newBox(t), testParams())
	svc.claimSizeLimit = 1 << 20
	payload, err := svc.assembleAndFinishRunClaim(context.Background(), worker(), fs.claimRun, false)
	if err != nil || payload == nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v), want payload", payload, err)
	}
	if fs.claimFailed != nil {
		t.Fatal("under-limit claim failed the run")
	}
}

func TestOversizeRunClaimBoundaryMatchesWireBody(t *testing.T) {
	payload := &ClaimPayload{Kind: "issue", IssueTitle: "t<>&"}
	rec := httptest.NewRecorder()
	if err := httpx.WriteJSON(rec, http.StatusOK, payload); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	wire := rec.Body.Len()
	if wire != len(raw)+1 {
		t.Fatalf("wire body %d bytes, json.Marshal+1 = %d", wire, len(raw)+1)
	}
	svc := &Service{claimSizeLimit: wire}
	if err := svc.refuseOversizeRunClaim(payload); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	svc.claimSizeLimit = wire - 1
	if err := svc.refuseOversizeRunClaim(payload); !errors.Is(err, errClaimTooLarge) {
		t.Fatalf("one byte over: %v", err)
	}
}

func TestOversizeRunClaimTypedAndCrossCheckExempt(t *testing.T) {
	payload := &ClaimPayload{Kind: "issue", IssueTitle: strings.Repeat("x", 512)}
	svc := &Service{claimSizeLimit: 64}
	err := svc.refuseOversizeRunClaim(payload)
	if !errors.Is(err, errClaimTooLarge) {
		t.Fatalf("not errClaimTooLarge: %v", err)
	}
	if got := claimAssemblyOrigin(err); got != "provisioning_failed" {
		t.Fatalf("origin = %q", got)
	}
	if !strings.HasPrefix(err.Error(), "claim too large:") {
		t.Fatalf("reason = %q", err.Error())
	}

	payload.Kind = runkind.CrossCheck
	if err := svc.refuseOversizeRunClaim(payload); err != nil {
		t.Fatalf("cross_check must be exempt: %v", err)
	}
}
