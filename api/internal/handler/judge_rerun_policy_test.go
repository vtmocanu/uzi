package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestRerunJudgeLanePolicy drives POST /api/runs/{id}/rejudge through the public handler with the
// owner's judge lane policy (PRD #4322 M1a) in the fake store. The fake store is not a live
// *store.Queries, so the vendor lane reaches the insert (here, the index conflict), a Codex judge
// reaches the fake's explicit-Codex credential refusal, and the provider-bound Claude lane, which
// needs the create transaction to freeze under the secret lock, fails closed (500) with nothing
// created. Each case is one synchronous request.
func TestRerunJudgeLanePolicy(t *testing.T) {
	providerID := pgtype.UUID{Bytes: uuid.MustParse("65070921-4487-44f4-8630-378c76e42098"), Valid: true}
	secretID := pgtype.UUID{Bytes: uuid.MustParse("0f1c2a54-7a52-4a5b-9a07-27a1ec2f10a4"), Valid: true}
	const (
		activeMsg = "a judge run is already in progress for this run"
		codexMsg  = "the judge's harness (codex) has no usable credential"
	)
	for _, tc := range []struct {
		name   string
		policy store.GetUserJudgeLanePolicyRow
		lane   string
		// want
		status       int
		reason, code string
		message      string
		presence     int
		created      int
	}{
		{name: "neither/claude vendor", lane: "claude", status: http.StatusConflict, message: activeMsg, presence: 1, created: 1},
		{name: "neither/codex vendor", lane: "codex", status: http.StatusUnprocessableEntity, code: "no_credential_for_harness", message: codexMsg},
		{name: "claude source/claude is provider-bound", policy: store.GetUserJudgeLanePolicyRow{ClaudeProviderID: providerID}, lane: "claude", status: http.StatusInternalServerError, message: "internal error"},
		{name: "claude source/codex is vendor", policy: store.GetUserJudgeLanePolicyRow{ClaudeProviderID: providerID}, lane: "codex", status: http.StatusUnprocessableEntity, code: "no_credential_for_harness", message: codexMsg},
		{name: "codex source/claude is vendor", policy: store.GetUserJudgeLanePolicyRow{CodexProviderID: providerID}, lane: "claude", status: http.StatusConflict, message: activeMsg, presence: 1, created: 1},
		{name: "codex source/codex refused", policy: store.GetUserJudgeLanePolicyRow{CodexProviderID: providerID}, lane: "codex", status: http.StatusConflict, reason: "provider_lane_unsupported", message: "this run type does not support a model-provider source yet"},
		{name: "both sources/claude is provider-bound", policy: store.GetUserJudgeLanePolicyRow{ClaudeProviderID: providerID, CodexProviderID: providerID}, lane: "claude", status: http.StatusInternalServerError, message: "internal error"},
		{name: "both sources/codex refused", policy: store.GetUserJudgeLanePolicyRow{ClaudeProviderID: providerID, CodexProviderID: providerID}, lane: "codex", status: http.StatusConflict, reason: "provider_lane_unsupported", message: "this run type does not support a model-provider source yet"},
		{name: "pinned with a credential beats a claude source", policy: store.GetUserJudgeLanePolicyRow{JudgeAnthropicBindMode: workersvc.BindModePinned, JudgeAnthropicSecretID: secretID, ClaudeProviderID: providerID}, lane: "claude", status: http.StatusConflict, message: activeMsg, presence: 1, created: 1},
		{name: "pinned without a credential is not a pin", policy: store.GetUserJudgeLanePolicyRow{JudgeAnthropicBindMode: workersvc.BindModePinned, ClaudeProviderID: providerID}, lane: "claude", status: http.StatusInternalServerError, message: "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds, runID, _ := oneRecStore()
			ds.run.Harness = string(workersvc.HarnessClaude)
			st := &rerunStore{
				dispStore:      ds,
				judgeRunsOn:    pgtype.Text{String: tc.lane, Valid: true},
				judgePolicy:    tc.policy,
				createJudgeErr: &pgconn.PgError{Code: "23505", ConstraintName: "uq_runs_one_active_judge_per_target"},
			}
			h := newRunsHandler(t, st)
			h.wsvc.SetSettings(judgeSwitch{enabled: true})
			rec := httptest.NewRecorder()
			h.RerunJudge(rec, rerunReq(store.User{ID: st.ownerID}, runID))

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if rec.Code != tc.status || body["error"] != tc.message || body["reason"] != tc.reason || body["code"] != tc.code {
				t.Fatalf("response = %d %s, want %d error=%q reason=%q code=%q", rec.Code, rec.Body.String(), tc.status, tc.message, tc.reason, tc.code)
			}
			if len(st.judgePolicyOwners) != 1 || st.judgePolicyOwners[0] != st.ownerID || st.ciphertextReads != tc.presence || st.created != tc.created {
				t.Fatalf("policy reads %v / presence reads %d / inserts %d, want 1 (owner) / %d / %d", st.judgePolicyOwners, st.ciphertextReads, st.created, tc.presence, tc.created)
			}
		})
	}

	for _, lane := range []string{"claude", "codex"} {
		t.Run("policy query error/"+lane, func(t *testing.T) {
			ds, runID, _ := oneRecStore()
			ds.run.Harness = string(workersvc.HarnessClaude)
			st := &rerunStore{dispStore: ds, judgeRunsOn: pgtype.Text{String: lane, Valid: true}, judgePolicyErr: errors.New("policy query failed")}
			h := newRunsHandler(t, st)
			h.wsvc.SetSettings(judgeSwitch{enabled: true})
			rec := httptest.NewRecorder()
			h.RerunJudge(rec, rerunReq(store.User{ID: st.ownerID}, runID))
			if rec.Code != http.StatusInternalServerError || errorMessage(t, rec) != "internal error" {
				t.Fatalf("policy failure = %d %s, want 500 internal error", rec.Code, rec.Body.String())
			}
			if st.ciphertextReads != 0 || st.created != 0 {
				t.Fatalf("policy failure must stop before presence/create: presence=%d inserts=%d", st.ciphertextReads, st.created)
			}
		})
	}
}

// TestRerunJudgeNoDefaultTokenIs422 pins the unchanged outcome of a vendor Claude judge with no
// default Anthropic token row: the manual path still answers 422 add-a-token, and nothing is created.
func TestRerunJudgeNoDefaultTokenIs422(t *testing.T) {
	ds, runID, _ := oneRecStore()
	ds.run.Harness = string(workersvc.HarnessClaude)
	st := &rerunStore{dispStore: ds, noToken: true}
	h := newRunsHandler(t, st)
	h.wsvc.SetSettings(judgeSwitch{enabled: true})
	rec := httptest.NewRecorder()
	h.RerunJudge(rec, rerunReq(store.User{ID: st.ownerID}, runID))
	if rec.Code != http.StatusUnprocessableEntity || errorMessage(t, rec) != "add an Anthropic token before running the judge" {
		t.Fatalf("no default token = %d %s, want 422 add-a-token", rec.Code, rec.Body.String())
	}
	if st.created != 0 {
		t.Fatalf("CreateJudgeRun ran %d times for an owner with no token", st.created)
	}
}

// TestWriteRerunJudgeErrorProviderArms pins every provider arm a rerun can answer, including the
// new judge_policy_changed 409 (PRD #4322 M1a), by status, stable reason code and wrapped errors.
func TestWriteRerunJudgeErrorProviderArms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"policy changed", workersvc.ErrJudgePolicyChanged, http.StatusConflict, "judge_policy_changed"},
		{"policy changed wrapped", fmt.Errorf("create: %w", workersvc.ErrJudgePolicyChanged), http.StatusConflict, "judge_policy_changed"},
		{"source unusable typed", &workersvc.ProviderSourceUnusableError{Reason: workersvc.SourceUnusableRetired}, http.StatusConflict, "provider_source_unusable"},
		{"source unusable sentinel", workersvc.ErrProviderSourceUnusable, http.StatusConflict, "provider_source_unusable"},
		{"model unmapped typed", &workersvc.ProviderModelUnmappedError{Model: "opus", Source: "judge"}, http.StatusConflict, "provider_model_unmapped"},
		{"model unmapped sentinel", workersvc.ErrProviderModelUnmapped, http.StatusConflict, "provider_model_unmapped"},
		{"lane unsupported", workersvc.ErrProviderLaneUnsupported, http.StatusConflict, "provider_lane_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeRerunJudgeError(rec, tc.err)
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if rec.Code != tc.status || body["reason"] != tc.reason {
				t.Fatalf("response = %d %s, want %d reason %q", rec.Code, rec.Body.String(), tc.status, tc.reason)
			}
		})
	}
	// The typed unmapped refusal names the model and where it came from.
	rec := httptest.NewRecorder()
	writeRerunJudgeError(rec, &workersvc.ProviderModelUnmappedError{Model: "opus", Source: "judge"})
	if got := errorMessage(t, rec); got != `the provider's model map does not map "opus" (judge); map it or change the model` {
		t.Fatalf("unmapped message = %q", got)
	}
}
