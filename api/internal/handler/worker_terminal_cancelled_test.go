package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type terminalInputsStore struct{ *protocolStore }

func (*terminalInputsStore) ConsumeRunInputs(context.Context, uuid.UUID) ([]store.ConsumeRunInputsRow, error) {
	return nil, nil
}

// The API returns the current stored generation through the authenticated worker route.
func TestWorkerRunInputsTerminalCancelledLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), time.Now().UnixNano()&0x3fffffff, true)
	id := rmSeedRun(t, pool, owner, repo, "running")
	workerID := uuid.New()
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,'cancel-control',$3)", workerID, owner, hash)
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=31,status='cancelled' WHERE id=$1", id, workerID)
	for _, capable := range []bool{false, true} {
		caps := []string{}
		if capable {
			caps = []string{capability.InputReceiptsV1}
		}
		cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", workerID, caps)
		for _, generation := range []int64{31, 32} {
			cliMustExec(t, pool, "UPDATE runs SET claim_generation=$2 WHERE id=$1", id, generation)
			req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/"+id.String()+"/inputs", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Inputs            []json.RawMessage                  `json:"inputs"`
				TerminalCancelled *workersvc.TerminalCancelledSignal `json:"terminal_cancelled"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Inputs == nil || len(body.Inputs) != 0 || body.TerminalCancelled == nil || body.TerminalCancelled.RunID != id || body.TerminalCancelled.Generation != generation {
				t.Fatalf("body=%s, want current generation %d", rec.Body.String(), generation)
			}
		}
	}
}

func TestWorkerRunInputsTerminalCancelled(t *testing.T) {
	for _, capable := range []bool{false, true} {
		for _, state := range []string{"cancelled", "running", "failed", "completed", "zero generation", "chat", "foreign worker"} {
			t.Run(state+map[bool]string{false: " legacy", true: " receipts"}[capable], func(t *testing.T) {
				id := uuid.New()
				st := &terminalInputsStore{&protocolStore{ownedRun: store.Run{ID: id, Kind: "issue", Status: state, ClaimGeneration: 23}}}
				if state == "zero generation" {
					st.ownedRun.Status = "cancelled"
					st.ownedRun.ClaimGeneration = 0
				}
				if state == "chat" {
					st.ownedRun.Status = "cancelled"
					st.ownedRun.Kind = "chat"
				}
				if state == "foreign worker" {
					st.ownedErr = pgx.ErrNoRows
				}
				h := newProtocolHandler(t, st.protocolStore)
				h.wsvc = workersvc.New(st, newHandlerTestBox(t), workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
				req := workerReq(http.MethodGet, "", id)
				if capable {
					w := store.Worker{ID: uuid.New(), ProtocolCapabilities: []string{capability.InputReceiptsV1}}
					req = req.WithContext(mw.ContextWithWorker(req.Context(), w))
				}
				for poll := 0; poll < 2; poll++ {
					rec := httptest.NewRecorder()
					h.WorkerRunInputs(rec, req)
					if state == "foreign worker" {
						if rec.Code != http.StatusNotFound {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
						}
						continue
					}
					if rec.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					var body map[string]json.RawMessage
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if state == "cancelled" {
						if string(body["inputs"]) != "[]" || len(body) != 2 {
							t.Fatalf("body=%s", rec.Body.String())
						}
						var control map[string]json.RawMessage
						if err := json.Unmarshal(body["terminal_cancelled"], &control); err != nil {
							t.Fatal(err)
						}
						if len(control) != 2 || string(control["run_id"]) != `"`+id.String()+`"` || string(control["generation"]) != "23" {
							t.Fatalf("control=%s", body["terminal_cancelled"])
						}
						if len(st.replayRunIDs) != 0 {
							t.Fatal("terminal poll replayed inputs")
						}
					} else if _, ok := body["terminal_cancelled"]; ok {
						t.Fatalf("unexpected control: %s", rec.Body.String())
					}
				}
			})
		}
	}
}
