package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestSemanticCoverageReportBounds(t *testing.T) {
	structural, proposal, sources := semanticFixture()
	candidate, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Digest = "candidate"
	candidate.Criteria[0].ID = uuid.NewString()
	report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: SemanticCoverageBinding(candidate), Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "atomic behavior is represented", Citations: candidate.Criteria[0].Citations, CriterionIDs: []string{candidate.Criteria[0].ID}}}, Criteria: []apitypes.SemanticCoverageAtomicity{{CriterionID: candidate.Criteria[0].ID, Atomic: true, Explanation: "atomic behavior"}}}
	decode := func(t *testing.T, r apitypes.SemanticCoverageReport, wantErr bool) {
		t.Helper()
		_, err := DecodeSemanticCoverageReport(semanticJSON(t, r), candidate, sources)
		if (err != nil) != wantErr {
			t.Fatalf("error=%v wantErr=%v", err, wantErr)
		}
	}
	decode(t, report, false)
	nonAtomic := report
	nonAtomic.Criteria = append([]apitypes.SemanticCoverageAtomicity(nil), report.Criteria...)
	nonAtomic.Criteria[0].Atomic = false
	parsed, err := DecodeSemanticCoverageReport(semanticJSON(t, nonAtomic), candidate, sources)
	if err != nil || len(parsed.Criteria) != 1 || parsed.Criteria[0].Atomic {
		t.Fatal("non-atomic assessment lost")
	}
	for _, tc := range []struct {
		name string
		edit func(*apitypes.SemanticCoverageReport)
	}{
		{"missing atomicity", func(r *apitypes.SemanticCoverageReport) { r.Criteria = nil }},
		{"foreign atomicity", func(r *apitypes.SemanticCoverageReport) { r.Criteria[0].CriterionID = uuid.NewString() }},
		{"duplicate requirement", func(r *apitypes.SemanticCoverageReport) { r.Requirements = append(r.Requirements, r.Requirements[0]) }},
		{"noncanonical requirement", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].RequirementID = "r01" }},
		{"duplicate criterion reference", func(r *apitypes.SemanticCoverageReport) {
			r.Requirements[0].CriterionIDs = append(r.Requirements[0].CriterionIDs, r.Requirements[0].CriterionIDs[0])
		}},
		{"binding", func(r *apitypes.SemanticCoverageReport) { r.CriteriaDigest = "foreign" }},
		{"outcome", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].Status = "approve" }},
		{"explanation", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].Explanation = strings.Repeat("x", 2049) }},
		{"criterion", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].CriterionIDs = []string{uuid.NewString()} }},
		{"no criterion", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].CriterionIDs = nil }},
		{"citation count", func(r *apitypes.SemanticCoverageReport) {
			r.Requirements[0].Citations = make([]apitypes.SemanticCitation, 9)
		}},
		{"citation hash", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].Citations[0].Hash = "foreign" }},
		{"citation range", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].Citations[0].End = 999 }},
		{"citation UTF8", func(r *apitypes.SemanticCoverageReport) { r.Requirements[0].Citations[0].End = 3 }},
		{"result count", func(r *apitypes.SemanticCoverageReport) {
			r.Requirements = make([]apitypes.SemanticCoverageResult, 513)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var copy apitypes.SemanticCoverageReport
			if err := strictSemanticJSON(semanticJSON(t, report), &copy); err != nil {
				t.Fatal(err)
			}
			tc.edit(&copy)
			decode(t, copy, true)
		})
	}
	for _, raw := range []string{
		strings.Repeat(" ", 65537),
		`{"candidate_digest":"x","candidate_digest":"y","results":[]}`,
		`{"Candidate_digest":"x","results":[]}`,
		`{"results":[]} {}`,
		`{"results":[{"outcome":"uncertain","explanation":"\ud800","citations":[],"criterion_ids":[]}]}`,
		`{"results":[],"path":"../escape"}`,
	} {
		if _, err := DecodeSemanticCoverageReport([]byte(raw), candidate, sources); err == nil {
			t.Fatalf("accepted invalid report: %.80s", raw)
		}
	}
}

func TestSemanticCandidatePayload(t *testing.T) {
	for _, mode := range []string{"persisted", "replacement", "empty identity", "foreign worker", "stale generation", "released", "terminal", "not running", "unenrolled", "corrupt source", "missing source", "null source", "corrupt plan"} {
		t.Run(mode, func(t *testing.T) {
			worker, runID := uuid.New(), uuid.New()
			plan, issue, empty := []byte("plan\x00bytes"), []byte("server\x00issue"), []byte{}
			candidate := apitypes.SemanticCandidate{
				CandidateIdentityVersion: semanticIdentityVersion, Revision: 1, Digest: "exact-candidate", PlanHash: semanticHash(plan),
				Sources: []apitypes.SemanticSourceRef{
					{ID: "issue", Kind: "issue", Hash: semanticHash(issue), Provenance: "server:run-issue", Completeness: "complete"},
					{ID: "missing-prd", Kind: "prd", Hash: semanticHash(empty), Provenance: "repo:missing", Completeness: "unknown"},
				},
			}
			enrollment := apitypes.SemanticEnrollment{Profile: "semantic", Policy: apitypes.SemanticPolicy{Version: 1, Model: "sonnet", MaxReviews: 3, MaxInvocations: 2, MaxTokens: 120000, MaxSeconds: 900}}
			planning := store.GetSemanticPlanningRow{Enrollment: semanticJSON(t, enrollment), Revision: 1, Candidate: semanticJSON(t, candidate)}
			run := store.Run{ID: runID, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, ClaimGeneration: 7, Status: "running", CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true}}
			expected := candidate.Digest
			switch mode {
			case "replacement":
				expected = "previous-candidate"
			case "empty identity":
				expected = ""
			case "foreign worker":
				run.WorkerID.Bytes = uuid.New()
			case "stale generation":
				run.ClaimGeneration = 8
			case "released":
				run.ClaimReleasedAt.Valid = true
			case "terminal":
				run.Status = "completed"
			case "not running":
				run.Status = "awaiting_approval"
			case "unenrolled":
				run.CompletionContractVersion.Valid = false
			}
			tx := &crossCheckBoundaryTx{t: t}
			reads := 0
			tx.query = func(sql string, args []any) pgx.Row {
				switch {
				case strings.HasPrefix(sql, "-- name: GetRunByIDForUpdate"):
					return crossCheckBoundaryRow{value: run}
				case strings.HasPrefix(sql, "-- name: GetSemanticPlanning"):
					return crossCheckBoundaryRow{value: planning}
				case strings.HasPrefix(sql, "-- name: GetSemanticSnapshot"):
					reads++
					if args[0] != runID {
						t.Fatal("snapshot escaped run")
					}
					blob := map[string][]byte{candidate.PlanHash: plan, candidate.Sources[0].Hash: issue, candidate.Sources[1].Hash: empty}[args[1].(string)]
					if args[1] == candidate.Sources[0].Hash {
						switch mode {
						case "corrupt source":
							blob = []byte("forged local issue")
						case "missing source":
							return crossCheckBoundaryRow{err: pgx.ErrNoRows}
						case "null source":
							blob = nil
						}
					}
					if mode == "corrupt plan" && args[1] == candidate.PlanHash {
						blob = []byte("wrong plan")
					}
					return crossCheckBoundaryRow{value: []any{blob}}
				default:
					t.Fatalf("unexpected query: %s", sql)
					return nil
				}
			}
			tx.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
				if !strings.HasPrefix(sql, "-- name: UpdateSemanticReviewState") {
					t.Fatalf("unexpected write: %s", sql)
				}
				var ledger semanticCoverageLedger
				if err := json.Unmarshal(args[1].([]byte), &ledger); err != nil || len(ledger.Reviews) != 0 || !ledger.Started.IsZero() {
					t.Fatalf("payload load reserved review capacity: %s", args[1])
				}
				return pgconn.NewCommandTag("UPDATE 1"), nil
			}
			svc := &Service{txBeginner: tx}
			got, err := svc.SemanticCandidatePayload(t.Context(), worker, runID, 7, expected)
			if mode != "persisted" {
				if err == nil || tx.committed || !reflect.DeepEqual(got, apitypes.SemanticCandidateResponse{}) {
					t.Fatalf("refused payload: %+v err=%v committed=%v", got, err, tx.committed)
				}
				if (mode == "replacement" || mode == "empty identity") && reads != 0 {
					t.Fatal("stale identity read snapshots")
				}
				return
			}
			if err != nil || !tx.committed {
				t.Fatalf("payload: %v committed=%v", err, tx.committed)
			}
			if !reflect.DeepEqual(got.Candidate, candidate) || got.Binding != SemanticCoverageBinding(candidate) || !bytes.Equal(got.Plan, plan) ||
				len(got.Sources) != 2 || !bytes.Equal(got.Sources[0].Bytes, issue) || got.Sources[0].Provenance != "server:run-issue" ||
				got.Sources[1].Bytes == nil || len(got.Sources[1].Bytes) != 0 || got.Sources[1].Completeness != "unknown" || got.Sources[1].Provenance != "repo:missing" {
				t.Fatalf("persisted payload changed: %+v", got)
			}
			raw := semanticJSON(t, got)
			if !bytes.Contains(raw, []byte(`"bytes":""`)) || !bytes.Contains(raw, []byte(`"plan":"cGxhbgBieXRlcw=="`)) {
				t.Fatalf("base64/empty wire changed: %s", raw)
			}
			var replay apitypes.SemanticCandidateResponse
			if err := json.Unmarshal(raw, &replay); err != nil || !reflect.DeepEqual(got, replay) {
				t.Fatalf("payload roundtrip: %v", err)
			}
		})
	}
}

func TestSemanticCoverageServiceStrictRequests(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	w, r := uuid.New(), uuid.New()
	for _, raw := range []string{
		`{"request_id":"bad"}`,
		`{"request_id":"x","request_id":"y"}`,
		`{"request_id":"x","path":"../escape"}`,
		`{"request_id":null}`,
	} {
		if _, err := s.ReserveSemanticCoverage(ctx, w, r, 7, []byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{
		`{"status":"approve"}`,
		`{"status":"unknown","tokens":-1}`,
		`{"status":"unknown","tokens":1,"tokens":2}`,
	} {
		if err := s.SettleSemanticCoverage(ctx, w, r, 7, []byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
