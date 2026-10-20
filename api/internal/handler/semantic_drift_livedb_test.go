package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// semantic_drift is owner-only like completion_pending: the owner gets the stored
// observations with the derived contradiction flag, an admin viewing the same run gets
// no key at all, and a run with no observation omits it.
func TestGetRunSemanticDriftOwnerOnlyLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc = workersvc.New(store.New(pool), h.box, workersvc.Params{})
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 1995, true)
	id := rmSeedRun(t, pool, owner, repo, "running")
	cliMustExec(t, pool, `UPDATE runs SET completion_contract_version=1 WHERE id=$1`, id)
	cliMustExec(t, pool, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, id)
	route := "/api/runs/" + id.String()
	get := func(user uuid.UUID, scope string) (int, apitypes.RunDTO, []byte) {
		t.Helper()
		rec := bearerReq(router, http.MethodGet, route, cliMintToken(t, pool, user, scope))
		var envelope struct {
			Run apitypes.RunDTO `json:"run"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
		return rec.Code, envelope.Run, rec.Body.Bytes()
	}
	if code, dto, raw := get(owner, clitoken.ScopeUser); code != http.StatusOK || dto.SemanticDrift != nil || bytes.Contains(raw, []byte(`"semantic_drift"`)) {
		t.Fatalf("a run without observations must omit semantic_drift: %d %s", code, raw)
	}
	record := apitypes.SemanticDriftObservation{Phase: "publication", PushedSHA: "0123456789abcdef0123456789abcdef01234567",
		Issue:      apitypes.SemanticDriftSource{SourceID: "issue:1", FrozenHash: "aa", Outcome: "changed", CheckboxOnly: true},
		PRDs:       []apitypes.SemanticDriftSource{{SourceID: "prd:prds/1-x.md", FrozenHash: "bb", Outcome: "unchanged"}},
		LinksAdded: []string{"prds/2-y.md"}, LinksRemoved: []string{}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.New(pool).AppendSemanticObservation(t.Context(), store.AppendSemanticObservationParams{RunID: id, Observation: raw}); err != nil {
		t.Fatal(err)
	}
	code, dto, body := get(owner, clitoken.ScopeUser)
	if code != http.StatusOK || dto.SemanticDrift == nil || !dto.SemanticDrift.Contradiction || len(dto.SemanticDrift.Observations) != 1 || dto.SemanticDrift.Dropped != 0 {
		t.Fatalf("owner view: %d %s", code, body)
	}
	if got := dto.SemanticDrift.Observations[0]; !got.Issue.CheckboxOnly || got.Issue.Outcome != "changed" || len(got.LinksAdded) != 1 {
		t.Fatalf("owner observation: %+v", got)
	}
	if code, dto, body = get(admin, clitoken.ScopeAdminRO); code != http.StatusOK || dto.SemanticDrift != nil || bytes.Contains(body, []byte(`"semantic_drift"`)) {
		t.Fatalf("a non-owner admin observer must not see semantic_drift: %d %s", code, body)
	}
}

// The worker route over real worker authentication: generation fence (409), strict
// request decoding and server validation (400), and the success shape. No forge is wired
// into this handler, so the issue read fails and the observation records unknown/unreadable
// with a 200 rather than failing the request.
func TestWorkerSemanticObservationRouteLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 1996, true)
	run := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "semantic-worker-" + uuid.NewString()
	tokenHash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'semantic',$3,'online')`, worker, owner, tokenHash[:])
	const issue = "implement behavior"
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,claim_generation=7,completion_contract_version=1,issue_description=$3 WHERE id=$1`, run, worker, issue)
	cliMustExec(t, pool, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, run)
	structural := workersvc.PlanCrossCheckCandidate{PlanMd: "implement behavior", Milestones: json.RawMessage(`[{"id":"m1","title":"Behavior"}]`), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	issueHash := sha256.Sum256([]byte(issue))
	proposal, err := json.Marshal(apitypes.SemanticProposal{Criteria: []apitypes.ProposedSemanticCriterion{{
		MilestoneID: "m1", Kind: "code", Text: "behavior", DoneWhen: "behavior works",
		Citations:            []apitypes.SemanticCitation{{SourceID: "issue", Hash: hex.EncodeToString(issueHash[:]), Start: 0, End: len(issue)}},
		EvidenceRequirements: []apitypes.SemanticEvidence{{Description: "test output"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sources := []apitypes.SemanticSourceCapture{
		{ID: "issue", Kind: "issue", Provenance: "worker:x", Completeness: "complete", Bytes: []byte(issue)},
		{ID: "missing-prd", Kind: "prd", Provenance: "repo:missing", Completeness: "unknown", Bytes: []byte{}},
	}
	if _, err = h.wsvc.SubmitSemanticCandidate(t.Context(), worker, run, 7, structural, proposal, sources); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+"/semantic/observation", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	prd := `{"source_id":"missing-prd","ref":"` + sha + `","outcome":"unknown","reason":"missing"}`
	ok := func(generation string) string {
		return `{"claim_generation":` + generation + `,"phase":"publication","pushed_sha":"` + sha + `","prd_sources":[` + prd + `]}`
	}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"stale generation":         {ok("6"), http.StatusConflict},
		"missing generation":       {`{"phase":"publication","pushed_sha":"` + sha + `","prd_sources":[]}`, http.StatusBadRequest},
		"unknown field":            {`{"claim_generation":7,"phase":"publication","pushed_sha":"` + sha + `","prd_sources":[],"issue_text":"x"}`, http.StatusBadRequest},
		"foreign source":           {strings.Replace(ok("7"), "missing-prd", "prd:prds/other.md", 1), http.StatusBadRequest},
		"outcome without a reason": {strings.Replace(ok("7"), `,"reason":"missing"`, "", 1), http.StatusBadRequest},
	} {
		if rec := call(tc.body); rec.Code != tc.want {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	var stored int
	if err = pool.QueryRow(t.Context(), `SELECT jsonb_array_length(observations->'items') FROM semantic_planning WHERE run_id=$1`, run).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("a refused request stored %d observations (err=%v)", stored, err)
	}
	rec := call(ok("7"))
	var record apitypes.SemanticDriftObservation
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &record) != nil {
		t.Fatalf("observe: %d %s", rec.Code, rec.Body.String())
	}
	if record.Phase != "publication" || record.PushedSHA != sha || record.Issue.SourceID != "issue" || record.Issue.Outcome != "unknown" || record.Issue.Reason != "unreadable" ||
		len(record.PRDs) != 1 || record.PRDs[0].Outcome != "unknown" || record.LinksAdded == nil || record.LinksRemoved == nil {
		t.Fatalf("record: %+v", record)
	}
	if strings.Contains(rec.Body.String(), issue) {
		t.Fatalf("response carries issue text: %s", rec.Body.String())
	}
	if err = pool.QueryRow(t.Context(), `SELECT jsonb_array_length(observations->'items') FROM semantic_planning WHERE run_id=$1`, run).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored=%d err=%v", stored, err)
	}
}
