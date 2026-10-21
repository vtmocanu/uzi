package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Keep every field and array at the compiler maxima, using printable HTML
// identifiers so Go's encoder expands them sixfold without invalid JSONB NULs.
// An optional count keeps only that many maximum criteria, spread evenly over
// the eight milestones (LiveDB round trips use semanticLiveDBCriteria).
func semanticOwnerMaxRequest(t *testing.T, criterionCount ...int) semanticCandidateRequest {
	t.Helper()
	body := semanticEscapedRequest(t)
	body.Sources[0].ID = strings.Repeat("<", 512)
	body.Sources[0].Bytes = []byte(strings.Repeat("i", 64<<10))
	hash := sha256.Sum256(body.Sources[0].Bytes)
	var proposal apitypes.SemanticProposal
	if err := json.Unmarshal(body.Proposal, &proposal); err != nil {
		t.Fatal(err)
	}
	var milestones []workersvc.Milestone
	for i := range 8 {
		id := fmt.Sprintf("m%d", i) + strings.Repeat("x", 62)
		milestones = append(milestones, workersvc.Milestone{ID: id, Title: "Milestone"})
	}
	if len(criterionCount) == 1 {
		proposal.Criteria = proposal.Criteria[:criterionCount[0]]
	}
	for i := range proposal.Criteria {
		c := &proposal.Criteria[i]
		c.MilestoneID = milestones[i*len(milestones)/len(proposal.Criteria)].ID
		for j := range c.Citations {
			c.Citations[j].SourceID = body.Sources[0].ID
			c.Citations[j].Hash = hex.EncodeToString(hash[:])
			c.EvidenceRequirements[j].Path = strings.Repeat("<", 512)
		}
	}
	var err error
	body.Proposal, err = json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	body.Structural.Milestones, err = json.Marshal(milestones)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestOwnerRunNormativeTransport(t *testing.T) {
	body := semanticOwnerMaxRequest(t)
	var proposal apitypes.SemanticProposal
	if err := json.Unmarshal(body.Proposal, &proposal); err != nil {
		t.Fatal(err)
	}
	gate := &apitypes.SemanticGateDTO{Plan: []byte(body.Structural.PlanMd), Sources: body.Sources}
	gate.Presentation.Candidate.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
	gate.Approval.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
	for _, c := range proposal.Criteria {
		id := uuid.NewString()
		gate.Presentation.Candidate.Criteria = append(gate.Presentation.Candidate.Criteria, apitypes.SemanticCriterion{ID: id, ProposedSemanticCriterion: c})
		m := apitypes.SemanticMaintainerApproval{ID: id, SemanticMaintainer: *c.Maintainer}
		gate.Presentation.Maintainers = append(gate.Presentation.Maintainers, m)
		gate.Approval.Maintainers = append(gate.Approval.Maintainers, m)
	}
	dto := apitypes.RunDTO{ID: uuid.NewString(), SemanticGate: gate}
	rec := httptest.NewRecorder()
	writeOwnerRun(rec, dto)
	if rec.Code != http.StatusOK || rec.Body.Len() <= 64<<20 || rec.Body.Len() > apitypes.OwnerRunMaxBytes {
		t.Fatalf("normative owner response status=%d bytes=%d", rec.Code, rec.Body.Len())
	}
	payload, _ := json.Marshal(map[string]any{"semantic": gate.Presentation})
	owner := uuid.New()
	id := uuid.MustParse(dto.ID)
	st := &runsStore{ownerID: owner, run: store.Run{ID: id, UserID: owner, Status: "awaiting_approval", GatePresentedPayload: payload}}
	h := newRunsHandler(t, st)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeOwnerRun(w, dto)
			return
		}
		if r.Header.Get(apitypes.SemanticIdentityHeader) != apitypes.SemanticIdentityVersion {
			t.Error("missing approval marker")
		}
		var input apitypes.RunInputRequest
		if err := h.decodeOwnerRunInput(r, owner, id, &input); err != nil {
			t.Errorf("full echo: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !reflect.DeepEqual(input.SemanticApproval, &gate.Approval) {
			t.Error("changed full approval")
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	client := uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL})
	got, err := client.GetRun(t.Context(), dto.ID)
	if err != nil || !reflect.DeepEqual(got.SemanticGate, gate) {
		t.Fatalf("normative typed GET changed fields: %v", err)
	}
	if _, err := client.SubmitRunInputWithOptions(t.Context(), dto.ID, apitypes.RunInputRequest{Kind: "approve_plan", SemanticApproval: &got.SemanticGate.Approval}); err != nil {
		t.Fatal(err)
	}
	approvalRaw, _ := json.Marshal(gate.Approval)
	if len(approvalRaw) <= 1<<20 || len(approvalRaw) > apitypes.SemanticApprovalMaxBytes {
		t.Fatalf("approval bytes=%d", len(approvalRaw))
	}
	t.Logf("normative owner GET=%d approval=%d", rec.Body.Len(), len(approvalRaw))
}

func TestOwnerRunRefusesBeforeMarker(t *testing.T) {
	for _, largeRemainder := range []bool{false, true} {
		gate := &apitypes.SemanticGateDTO{}
		gate.Presentation.Candidate.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
		gate.Approval.CandidateIdentityVersion = "future"
		dto := apitypes.RunDTO{SemanticGate: gate}
		if largeRemainder {
			gate.Approval.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
			plan := strings.Repeat("p", apitypes.OwnerRunLegacyMaxBytes)
			dto.PlanMd = &plan
		}
		rec := httptest.NewRecorder()
		writeOwnerRun(rec, dto)
		if rec.Code != http.StatusInternalServerError || rec.Header().Get(apitypes.SemanticIdentityHeader) != "" {
			t.Fatalf("refused envelope was marked: status=%d marker=%q", rec.Code, rec.Header().Get(apitypes.SemanticIdentityHeader))
		}
	}
}

func TestOwnerInputLegacyLimit(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{`{"kind":"cancel"}`, true},
		{`{"kind":"cancel"} {}`, true},
		{`{"kind":"cancel","unknown":true}`, false},
		{`{"kind":"cancel","body":"` + strings.Repeat("a", 1<<20) + `"}`, false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.raw))
		var input apitypes.RunInputRequest
		err := (&Handler{}).decodeOwnerRunInput(req, uuid.New(), uuid.New(), &input)
		if (err == nil) != tc.ok {
			t.Fatalf("valid=%v err=%v", tc.ok, err)
		}
	}
}

func TestOwnerInputMarkedAdmission(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	id := uuid.New()
	presentation := apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{CandidateIdentityVersion: apitypes.SemanticIdentityVersion}}
	payload, _ := json.Marshal(map[string]any{"semantic": presentation})
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: id, UserID: owner.ID, Status: "awaiting_approval", GatePresentedPayload: payload}}
	h := newRunsHandler(t, st)
	_, stored := workersvc.SemanticGateBindingFromRun(st.run)
	if stored == nil {
		t.Fatal("fixture has no semantic binding")
	}
	approval := *stored
	changed := approval
	changed.PresentationDigest = "changed"
	base, _ := json.Marshal(apitypes.RunInputRequest{Kind: "approve_plan", SemanticApproval: &approval, Body: "x"})
	echo, _ := json.Marshal(approval)
	ordinaryFill := apitypes.RunInputLegacyMaxBytes - (len(base) - len(echo) + len("null")) + 1
	for _, tc := range []struct {
		name, kind, marker string
		owner              uuid.UUID
		echo               *apitypes.SemanticGateApproval
		body               string
		ok                 bool
	}{
		{"exact", "approve_plan", apitypes.SemanticIdentityVersion, owner.ID, &approval, "", true},
		{"exact ordinary remainder", "approve_plan", apitypes.SemanticIdentityVersion, owner.ID, &approval, strings.Repeat("x", ordinaryFill), true},
		{"overflow ordinary remainder", "approve_plan", apitypes.SemanticIdentityVersion, owner.ID, &approval, strings.Repeat("x", ordinaryFill+1), false},
		{"nonowner", "approve_plan", apitypes.SemanticIdentityVersion, uuid.New(), &approval, "", false},
		{"other kind", "follow_up", apitypes.SemanticIdentityVersion, owner.ID, &approval, "", false},
		{"null", "approve_plan", apitypes.SemanticIdentityVersion, owner.ID, nil, "", false},
		{"changed echo", "approve_plan", apitypes.SemanticIdentityVersion, owner.ID, &changed, "", false},
		{"wrong marker large", "approve_plan", "future", owner.ID, &approval, strings.Repeat("a", 1<<20), false},
		{"ordinary overflow", "approve_plan", apitypes.SemanticIdentityVersion, owner.ID, &approval, strings.Repeat("a", 1<<20), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(apitypes.RunInputRequest{Kind: tc.kind, SemanticApproval: tc.echo, Body: tc.body})
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
			req.Header.Set(apitypes.SemanticIdentityHeader, tc.marker)
			var input apitypes.RunInputRequest
			err := h.decodeOwnerRunInput(req, tc.owner, id, &input)
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
