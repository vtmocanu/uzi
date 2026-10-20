package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Use the full frozen contract at the public projection seam, including all
// independently bounded fields, rather than a marshal-only request fixture.
func semanticCompletionTransportFixture(t *testing.T, criterionCount ...int) (store.Run, completionDecisionRequest) {
	t.Helper()
	body := semanticOwnerMaxRequest(t)
	var proposal apitypes.SemanticProposal
	if err := json.Unmarshal(body.Proposal, &proposal); err != nil {
		t.Fatal(err)
	}
	if len(criterionCount) == 1 {
		proposal.Criteria = proposal.Criteria[:criterionCount[0]]
	}
	candidate := apitypes.SemanticCandidate{CandidateIdentityVersion: apitypes.SemanticIdentityVersion, Revision: 1, Digest: "frozen"}
	criteria := []map[string]any{}
	maintainers := []apitypes.SemanticMaintainerApproval{}
	auth := []map[string]any{}
	for _, c := range proposal.Criteria {
		id := uuid.NewString()
		candidate.Criteria = append(candidate.Criteria, apitypes.SemanticCriterion{ID: id, ProposedSemanticCriterion: c})
		criteria = append(criteria, map[string]any{"id": id, "milestone_id": c.MilestoneID, "text": c.Text, "kind": c.Kind, "done_when": c.DoneWhen, "citations": c.Citations, "evidence_requirements": c.EvidenceRequirements, "maintainer": c.Maintainer, "audit": nil, "finding_ids": []string{}})
		m := apitypes.SemanticMaintainerApproval{ID: id, SemanticMaintainer: *c.Maintainer}
		maintainers = append(maintainers, m)
		auth = append(auth, map[string]any{"id": id, "reason": m.Reason, "conditions": m.Conditions, "verification_steps": m.VerificationSteps, "revision": 1})
	}
	candidate.Sources = []apitypes.SemanticSourceRef{{ID: body.Sources[0].ID, Kind: "issue", Provenance: "issue", Hash: proposal.Criteria[0].Citations[0].Hash}}
	binding := workersvc.SemanticCoverageBinding(candidate)
	p := apitypes.SemanticGatePresentation{Candidate: candidate, Binding: binding, Maintainers: maintainers}
	a := apitypes.SemanticGateApproval{PresentationRevision: 1, PresentationDigest: "frozen", CandidateIdentityVersion: apitypes.SemanticIdentityVersion, CandidateRevision: 1, SemanticCoverageBinding: binding, Maintainers: maintainers}
	contract, err := json.Marshal(map[string]any{"profile": "semantic", "revision": 1, "criteria": criteria, "sources": candidate.Sources, "candidate_identity_version": apitypes.SemanticIdentityVersion, "semantic_presentation": p, "semantic_approval": a, "maintainer_authorizations": auth})
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{ID: uuid.New(), UserID: uuid.New(), Status: "paused", CompletionContract: contract, CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true}, ContractRevision: pgtype.Int4{Int32: 1, Valid: true}, MilestonesFrozen: body.Structural.Milestones}
	if _, err := workersvc.CompletionPending(run); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	return run, completionDecisionRequest{Decision: "authorize_maintainer", ContractRevision: 1, Maintainers: maintainers}
}

// Main executes this maximum write/read proof in the serial LiveDB sweep.
func TestSemanticCompletionMaximumRenewalLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc = workersvc.New(store.New(pool), h.box, workersvc.Params{RunTimeout: 2 * time.Hour})
	h.wsvc.SetTxBeginner(pool)
	h.wsvc.SetBackground(func(func()) {})
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 1996, true)
	id := rmSeedRun(t, pool, owner, repo, "paused")
	frozen, input := semanticCompletionTransportFixture(t)
	cliMustExec(t, pool, `UPDATE runs SET completion_contract_version=1,contract_revision=1,completion_contract=$2,milestones_frozen=$3,hold_reason='completion_blocked' WHERE id=$1`, id, frozen.CompletionContract, frozen.MilestonesFrozen)
	input.Reason = "renew exact owner steps\n\u202eas frozen"
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(router)
	if !semanticOwnerRace {
		srv.Config.ReadTimeout = 15 * time.Second
		srv.Config.WriteTimeout = 15 * time.Second
	}
	srv.Start()
	defer srv.Close()
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	client := &http.Client{Timeout: 30 * time.Second}
	if semanticOwnerRace {
		client.Timeout = 2 * time.Minute
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/runs/"+id.String()+"/completion/decision", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apitypes.SemanticIdentityHeader, apitypes.SemanticIdentityVersion)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	result, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("renew status=%d err=%v body=%.500s", response.StatusCode, readErr, result)
	}
	stored, err := store.New(pool).GetRunByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := workersvc.CompletionPending(stored)
	if err != nil || pending == nil || pending.ContractRevision != 2 || len(pending.Criteria) != 256 {
		t.Fatalf("pending round trip: %v", err)
	}
	for _, c := range pending.Criteria {
		if c.AuthorizationRevision != 2 || c.Status != "unverified" {
			t.Fatal("renewal changed pending meaning")
		}
	}
	get, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/runs/"+id.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	get.Header.Set("Authorization", "Bearer "+token)
	response, err = client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Run apitypes.RunDTO `json:"run"`
	}
	err = json.NewDecoder(response.Body).Decode(&envelope)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !reflect.DeepEqual(envelope.Run.CompletionPending, pending) {
		t.Fatalf("owner GET pending: status=%d err=%v", response.StatusCode, err)
	}
	want := map[string]apitypes.SemanticMaintainer{}
	for _, m := range input.Maintainers {
		want[m.ID] = m.SemanticMaintainer
	}
	for _, c := range pending.Criteria {
		if !reflect.DeepEqual(c.SemanticMaintainer, want[c.CriterionID]) {
			t.Fatal("stored maximum fields changed")
		}
	}
}

func TestSemanticCompletionMaximumAdmission(t *testing.T) {
	run, input := semanticCompletionTransportFixture(t)
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 1<<20 || len(raw) > apitypes.SemanticRunInputMaxBytes {
		t.Fatalf("wire bytes=%d", len(raw))
	}
	h := newRunsHandler(t, &runsStore{ownerID: run.UserID, run: run})
	router := chi.NewRouter()
	router.Post("/api/runs/{id}/completion/decision", h.ContinueCompletionDecision)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/completion/decision", run.ID), bytes.NewReader(raw))
	req.Header.Set(apitypes.SemanticIdentityHeader, apitypes.SemanticIdentityVersion)
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: run.UserID}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	// Empty ordinary decision reason deliberately stops after request admission,
	// before the concrete transactional service.
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reason is required") {
		t.Fatalf("maximum request failed admission: %d %s", rec.Code, rec.Body.String())
	}
	t.Logf("admitted 256 frozen maintainers, wire bytes=%d", len(raw))
	// Keep each rejection above the ordinary 1 MiB boundary, while avoiding
	// repeated maximum-contract validation. The full maximum is admitted above.
	run, input = semanticCompletionTransportFixture(t, 32)
	raw, err = json.Marshal(input)
	if err != nil || len(raw) <= apitypes.RunInputLegacyMaxBytes {
		t.Fatalf("large rejection fixture bytes=%d err=%v", len(raw), err)
	}
	h = newRunsHandler(t, &runsStore{ownerID: run.UserID, run: run})
	for _, tc := range []struct {
		name   string
		raw    []byte
		owner  uuid.UUID
		marker string
	}{
		{"unmarked", raw, run.UserID, ""},
		{"foreign", raw, uuid.New(), apitypes.SemanticIdentityVersion},
		{"admin observer", raw, uuid.New(), apitypes.SemanticIdentityVersion},
		{"duplicate", append([]byte(`{"decision":"authorize_maintainer",`), raw[1:]...), run.UserID, apitypes.SemanticIdentityVersion},
		{"case alias", bytes.Replace(raw, []byte(`"decision"`), []byte(`"Decision"`), 1), run.UserID, apitypes.SemanticIdentityVersion},
		{"unknown", append([]byte(`{"unknown":true,`), raw[1:]...), run.UserID, apitypes.SemanticIdentityVersion},
		{"trailing", append(bytes.Clone(raw), []byte(" {}")...), run.UserID, apitypes.SemanticIdentityVersion},
		{"invalid UTF8", bytes.Replace(raw, []byte("authorize_maintainer"), []byte{'x', 0xff}, 1), run.UserID, apitypes.SemanticIdentityVersion},
		{"other decision", bytes.Replace(raw, []byte("authorize_maintainer"), []byte("accept"), 1), run.UserID, apitypes.SemanticIdentityVersion},
		{"missing revision", bytes.Replace(raw, []byte(`"contract_revision":1`), []byte(`"contract_revision":0`), 1), run.UserID, apitypes.SemanticIdentityVersion},
		{"changed condition", bytes.Replace(raw, []byte(`"conditions":[`), []byte(`"conditions":["changed",`), 1), run.UserID, apitypes.SemanticIdentityVersion},
		{"ordinary remainder", bytes.Replace(raw, []byte(`"guidance":""`), []byte(`"guidance":"`+strings.Repeat("x", apitypes.RunInputLegacyMaxBytes)+`"`), 1), run.UserID, apitypes.SemanticIdentityVersion},
		{"total limit", []byte(strings.Repeat(" ", apitypes.SemanticRunInputMaxBytes+1)), run.UserID, apitypes.SemanticIdentityVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(tc.raw))
			r.Header.Set(apitypes.SemanticIdentityHeader, tc.marker)
			var dst completionDecisionRequest
			if err := h.decodeSemanticCompletionDecision(r, tc.owner, run.ID, &dst); err == nil {
				t.Fatal("invalid large envelope admitted")
			}
		})
	}
	// Unmarked structural clients retain the existing ordinary decoder semantics.
	for _, body := range []string{`{"decision":"continue","guidance":"legacy"}`, `{"decision":"accept","reason":"legacy","contract_revision":1} {}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var dst completionDecisionRequest
		if err := h.decodeSemanticCompletionDecision(r, run.UserID, run.ID, &dst); err != nil {
			t.Fatal(err)
		}
	}

}
