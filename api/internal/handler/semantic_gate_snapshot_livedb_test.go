package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The real owner query captures N, then the hook commits N+1 before returning N.
// Only this read seam is decorated; semantic compilation, revision, ACK and
// presentation publication use the real service and database.
type semanticSnapshotStore struct {
	*store.Queries
	afterRead func(store.Run)
}

func (s *semanticSnapshotStore) GetRunByIDForUser(ctx context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	run, err := s.Queries.GetRunByIDForUser(ctx, arg)
	if err == nil && s.afterRead != nil {
		hook := s.afterRead
		s.afterRead = nil
		hook(run)
	}
	return run, err
}

func TestGetRunSemanticSnapshotLiveDB(t *testing.T) {
	for _, scenario := range []string{"captured_N", "foreign", "admin_observer", "structural", "missing_source", "corrupt_source", "missing_plan", "corrupt_plan"} {
		t.Run(scenario, func(t *testing.T) {
			h, router, pool := cliLiveDB(t)
			ctx := t.Context()
			q := store.New(pool)
			h.wsvc = workersvc.New(q, h.box, workersvc.Params{PlanMaxRevisions: 10})
			h.wsvc.SetTxBeginner(pool)
			originalService := h.wsvc
			owner := cliSeedUser(t, pool, false)
			repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 1994, true)
			id := rmSeedRun(t, pool, owner, repo, "running")
			worker := uuid.New()
			cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'semantic-snapshot',$3,'online',$4)`,
				worker, owner, worker[:], []string{capability.InputReceiptsV1, capability.GateRevisionV1})
			issue := []byte("frozen issue requirement")
			cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,claim_generation=7,completion_contract_version=1,issue_description=$3 WHERE id=$1`, id, worker, string(issue))
			viewer := owner
			if scenario == "foreign" {
				viewer = cliSeedUser(t, pool, false)
			}
			if scenario == "admin_observer" {
				viewer = cliSeedUser(t, pool, true)
			}
			token := cliMintToken(t, pool, viewer, clitoken.ScopeUser)
			route := "/api/runs/" + id.String()
			if scenario == "structural" {
				rec := bearerReq(router, http.MethodGet, route, token)
				var envelope struct {
					Run apitypes.RunDTO `json:"run"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &envelope) != nil || envelope.Run.ID != id.String() || envelope.Run.Status != "running" || envelope.Run.SemanticGate != nil || bytes.Contains(rec.Body.Bytes(), []byte(`"semantic_gate"`)) {
					t.Fatalf("structural response: %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			// Explicit dormant enrollment is fixture setup, never a production
			// creation path. Both presentations below use the public service.
			cliMustExec(t, pool, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, id)
			generation := int64(7)
			structural := workersvc.PlanCrossCheckCandidate{PlanMd: "plan N", Milestones: json.RawMessage(`[{"id":"m1","title":"Behavior"}]`)}
			hash := sha256.Sum256(issue)
			proposal := apitypes.SemanticProposal{Criteria: []apitypes.ProposedSemanticCriterion{{
				MilestoneID: "m1", Kind: "code", Text: "behavior N", DoneWhen: "observable behavior works",
				Citations:            []apitypes.SemanticCitation{{SourceID: "issue", Hash: hex.EncodeToString(hash[:]), Start: 0, End: len(issue)}},
				EvidenceRequirements: []apitypes.SemanticEvidence{{Description: "test output"}},
			}}}
			sources := []apitypes.SemanticSourceCapture{
				{ID: "issue", Kind: "issue", Provenance: "server:run-issue", Completeness: "complete", Bytes: issue},
				{ID: "prd", Kind: "prd", Provenance: "repo:requirements", Completeness: "complete", Bytes: []byte("source N")},
			}
			publish := func(expected, input *int64) store.Run {
				t.Helper()
				raw, err := json.Marshal(proposal)
				if err != nil {
					t.Fatal(err)
				}
				candidate, err := originalService.SubmitSemanticCandidate(ctx, worker, id, generation, structural, raw, sources)
				if err != nil {
					t.Fatal(err)
				}
				presentation := uuid.New()
				ms := []workersvc.Milestone{{ID: "m1", Title: "Behavior"}}
				_, applied, _, err := originalService.SetStateReport(ctx, store.Worker{ID: worker, UserID: owner}, id, workersvc.StateRequest{
					State: "awaiting_approval", ClaimGeneration: &generation, PresentationID: &presentation,
					CandidateDigest: candidate.Digest, PlanMd: &structural.PlanMd, Milestones: &ms,
					ExpectedGateRevision: expected, RevisionInputID: input,
				})
				if err != nil || !applied {
					t.Fatalf("publish semantic gate: applied=%v err=%v", applied, err)
				}
				// Read the persisted presentation, rather than assuming the state
				// report return includes every newly written gate column.
				run, err := q.GetRunByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return run
			}
			n := publish(nil, nil)
			expected, err := originalService.SemanticGateForOwner(ctx, owner, n)
			if err != nil || expected == nil {
				t.Fatalf("load N: %v", err)
			}
			if scenario == "captured_N" {
				decorated := &semanticSnapshotStore{Queries: q}
				decorated.afterRead = func(captured store.Run) {
					if captured.GateRevision != n.GateRevision {
						t.Fatal("did not capture N")
					}
					// Genuine owner revision, receipt ACK and candidate replacement.
					input, err := originalService.SubmitInputWithOptions(ctx, owner, id, "revise_plan", "clarify", nil, workersvc.SubmitInputOptions{ExpectedGateRevision: &n.GateRevision})
					if err != nil {
						t.Fatal(err)
					}
					wkr, err := q.GetWorkerByID(ctx, worker)
					if err != nil {
						t.Fatal(err)
					}
					if receipt, err := originalService.AckInputs(ctx, wkr, id, generation, []int64{input.ID}); err != nil || !receipt.Active {
						t.Fatalf("revision ACK: %+v %v", receipt, err)
					}
					structural.PlanMd = "distinct plan N+1"
					proposal.Criteria[0].Text = "distinct behavior N+1"
					sources[1].Bytes = []byte("distinct source N+1")
					next := publish(&n.GateRevision, &input.ID)
					if next.GateRevision != n.GateRevision+1 || next.PlanMd.String == n.PlanMd.String {
						t.Fatal("N+1 was not committed")
					}
				}
				h.wsvc = workersvc.New(decorated, h.box, workersvc.Params{})
				h.wsvc.SetTxBeginner(pool)
			}
			switch scenario {
			case "missing_source", "missing_plan":
				target := expected.Presentation.Candidate.Sources[0].Hash
				if scenario == "missing_plan" {
					target = expected.Presentation.Candidate.PlanHash
				}
				cliMustExec(t, pool, "DELETE FROM semantic_snapshots WHERE run_id=$1 AND hash=$2", id, target)
			case "corrupt_source", "corrupt_plan":
				target := expected.Presentation.Candidate.Sources[0].Hash
				if scenario == "corrupt_plan" {
					target = expected.Presentation.Candidate.PlanHash
				}
				cliMustExec(t, pool, "UPDATE semantic_snapshots SET raw=$3 WHERE run_id=$1 AND hash=$2", id, target, []byte("corrupt"))
			}
			rec := bearerReq(router, http.MethodGet, route, token)
			if scenario == "foreign" {
				if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), issue) {
					t.Fatalf("foreign response: %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if scenario == "missing_source" || scenario == "corrupt_source" || scenario == "missing_plan" || scenario == "corrupt_plan" {
				if rec.Code != http.StatusInternalServerError || bytes.Contains(rec.Body.Bytes(), issue) {
					t.Fatalf("blob failure response: %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			var envelope struct {
				Run apitypes.RunDTO `json:"run"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &envelope) != nil || envelope.Run.ID != id.String() {
				t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
			}
			dto := envelope.Run
			if scenario == "admin_observer" {
				if dto.GateRevision != n.GateRevision || dto.SemanticGate != nil || bytes.Contains(rec.Body.Bytes(), []byte(`"semantic_gate"`)) {
					t.Fatal("admin observer received owner semantic bytes")
				}
				return
			}
			if dto.GateRevision != n.GateRevision || dto.PlanMd == nil || *dto.PlanMd != n.PlanMd.String || !reflect.DeepEqual(dto.SemanticGate, expected) {
				t.Fatalf("mixed run/presentation snapshot: %s", rec.Body.String())
			}
			actual, err := q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			actualGate, err := originalService.SemanticGateForOwner(ctx, owner, actual)
			if err != nil || actualGate == nil || actual.GateRevision != n.GateRevision+1 || actual.PlanMd.String != "distinct plan N+1" ||
				actualGate.Presentation.Candidate.Digest == expected.Presentation.Candidate.Digest ||
				actualGate.Approval.PresentationRevision != actual.GateRevision ||
				bytes.Equal(actualGate.Plan, expected.Plan) || bytes.Equal(actualGate.Sources[1].Bytes, expected.Sources[1].Bytes) {
				t.Fatalf("database is not distinct committed N+1: %v", err)
			}
		})
	}
}
