package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// codeCrossCheckWireDir holds the worker status bodies this handler serves, recorded byte for byte.
// agent/test/code-cross-check-wire.test.ts feeds the same files through the production decoder and
// gate (issue #2870: a pending row's NULL findings once serialized as null and the worker dropped
// the wait). Recorded, not authored: on a mismatch this test prints the body to copy in.
const codeCrossCheckWireDir = "../../../fixtures/code-cross-check"

// TestCodeCrossCheckStatusWireFixtures serves a pending row exactly as the store returns it after
// InsertCodeCrossCheck (findings never written, so NULL), then the same row decided with one finding.
func TestCodeCrossCheckStatusWireFixtures(t *testing.T) {
	worker := store.Worker{ID: uuid.MustParse("00000000-0000-4000-8000-0000000000a1"), UserID: uuid.MustParse("00000000-0000-4000-8000-0000000000a2")}
	lead := store.Run{ID: uuid.MustParse("00000000-0000-4000-8000-0000000000a3"), UserID: worker.UserID, Status: "running", ClaimGeneration: 7}
	commit := func(c byte) pgtype.Text { return pgtype.Text{String: string(bytes.Repeat([]byte{c}, 40)), Valid: true} }
	pending := store.CrossCheck{Stage: "code", Round: 1, LeadClaimGeneration: 7,
		BaseCommit: commit('a'), HeadCommit: commit('b'), CandidateDigest: bytes.Repeat([]byte{0xcc}, 32),
		GuidanceDigest: bytes.Repeat([]byte{0xdd}, 32), RepoInstructionsDigest: bytes.Repeat([]byte{0xee}, 32),
		CheckerRunID:   pgtype.UUID{Bytes: uuid.MustParse("00000000-0000-4000-8000-0000000000a4"), Valid: true},
		CheckerHarness: pgtype.Text{String: "codex", Valid: true},
		Outcome:        pgtype.Text{String: "pending", Valid: true},
		DeadlineAt:     pgtype.Timestamptz{Time: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}}
	completed := pending
	completed.Outcome = pgtype.Text{String: "completed", Valid: true}
	completed.Findings = []byte(`[{"id": "F_1", "severity": "major", "path": "a.ts", "line": 2, "title": "Verify guard", "detail": "untrusted advice"}]`)

	for name, row := range map[string]store.CrossCheck{"status-pending.json": pending, "status-completed.json": completed} {
		t.Run(name, func(t *testing.T) {
			svc := workersvc.New(nil, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
			svc.SetTxBeginner(&codeStatusTx{lead: lead, check: row})
			h := &Handler{wsvc: svc}
			router := chi.NewRouter()
			router.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(mw.ContextWithWorker(r.Context(), worker)))
				})
			})
			router.Get("/runs/{id}/code", h.WorkerCodeCrossCheckStatus)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest("GET", "/runs/"+lead.ID.String()+"/code?claim_generation=7", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			want, err := os.ReadFile(filepath.Join(codeCrossCheckWireDir, name)) //nolint:gosec // G304: fixed fixture directory
			if err != nil {
				t.Fatalf("fixture unreadable: %s: %v -- skipping would look identical to passing", name, err)
			}
			if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(rec.Body.Bytes(), "\n")) {
				t.Errorf("fixture %s is stale -- re-record it from this exact body (no -update flag):\n%s", name, rec.Body.Bytes())
			}
		})
	}
}
