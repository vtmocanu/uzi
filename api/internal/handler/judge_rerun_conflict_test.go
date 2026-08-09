package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// rerunStore is dispStore carried past the RerunJudge gates: the review read path stops
// at ListDispositionsForReview, but the re-run path keeps going into the owner-token
// lookup and the enqueue INSERT. Everything else — the owner-scoped run resolve, the
// judged/unjudged review — is inherited unchanged, and the embedded workersvc.Store is
// still NIL, so any method neither struct implements PANICS if the path reaches it.
type rerunStore struct {
	*dispStore

	// createJudgeErr is what the enqueue INSERT returns. A *pgconn.PgError with code
	// 23505 is the real database's answer when uq_runs_one_active_judge_per_target
	// already holds an active judge for this target — the ONLY way ErrJudgeAlreadyActive
	// is produced (workersvc/judge_read.go: errors.As + Code == "23505").
	createJudgeErr error
	created        int
}

func (s *rerunStore) GetUserSecretCiphertext(_ context.Context, _ store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	// The owner has a token to spend, so the gate above the enqueue passes and the 409
	// under test is reached rather than the 422 for a missing token.
	return store.GetUserSecretCiphertextRow{Ciphertext: []byte{0x1}}, nil
}

func (s *rerunStore) CreateJudgeRun(_ context.Context, _ store.CreateJudgeRunParams) (store.Run, error) {
	s.created++
	if s.createJudgeErr != nil {
		return store.Run{}, s.createJudgeErr
	}
	return store.Run{ID: uuid.New(), Kind: "judge"}, nil
}

// judgeSwitch is the instance judge kill-switch (workersvc.SettingsReader) for these
// tests: enabled=false is the ErrJudgeDisabled 409, enabled=true lets the re-run reach
// the enqueue.
type judgeSwitch struct{ enabled bool }

func (j judgeSwitch) JudgeEnabled(context.Context) (bool, error) { return j.enabled, nil }
func (j judgeSwitch) JudgeModel(context.Context) (string, error) { return "", nil }
func (j judgeSwitch) PRDLabel(context.Context) (string, error)   { return "", nil }
func (j judgeSwitch) RunEligibleLabels(context.Context) ([]string, error) {
	return nil, nil
}
func (j judgeSwitch) EligibleLabelWaivesPRDLink(context.Context) (bool, error) { return false, nil }
func (j judgeSwitch) PrdlessEnabled(context.Context) (bool, error)             { return false, nil }
func (j judgeSwitch) PrdlessLabel(context.Context) (string, error)             { return "", nil }

// rerunReq builds POST /api/runs/{id}/rejudge authenticated as user. The path is inert
// (chi params are injected by hand below, and RerunJudge is called directly), but it is
// the real route — see handler.go's /runs group — so nobody greps for one that never
// existed.
func rerunReq(user store.User, runID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID.String()+"/rejudge", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	return req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), user), chi.RouteCtxKey, rctx))
}

// clientConflictMatch is a Go transcription of the ONE regex the web client uses to tell
// this route's two 409s apart — the 409 branch of the `rerun` handler inside JudgePanel
// (web/src/pages/RunView.tsx):
//
//	if (e instanceof ApiError && e.status === 409 && /already in progress/i.test(e.message))
//
// Cited by symbol and by the regex literal, never by line number: the line moves every
// time a comment lands above it, and a stale citation sends the next reader to the wrong
// place.
//
// It is duplicated here on purpose. The assertion below is not "the strings are these
// strings" for its own sake — it is "the client's discriminator still sorts the two 409s
// the way the client assumes", which is the property that breaks.
var clientConflictMatch = regexp.MustCompile(`(?i)already in progress`)

// TestRerunJudgeConflictMessages pins BOTH 409s POST /api/runs/{id}/rejudge can return —
// status AND exact body — because the web client discriminates between them BY MESSAGE
// TEXT and nothing else.
//
// 🔴 THE ALREADY-ACTIVE MESSAGE IS LOAD-BEARING WIRE CONTRACT, NOT PROSE. httpx.Error
// emits {"error": "<message>"} with NO machine-readable code, so the two conflicts are
// indistinguishable on the wire except by their text. JudgePanel's `rerun` handler
// matches /already in progress/i to decide whether to ABSORB the 409 (re-fetch, converge
// on the pending-judge state, no banner — the TOCTOU race a judge enqueued between the
// panel's fetch and the click) or to SHOW it (the kill-switch 409, which a user who
// turned judging off must see). Reword "a judge run is already in progress for this run"
// and the disabled path is unaffected while the race path starts surfacing an error
// banner for something that succeeded — silently, because before this test every Go test
// stayed green through the rewording and the only other copy of the string is
// web/src/mocks/mockApi.ts:2500, maintained by whoever did the rewording.
//
// If a machine-readable error code ever lands on httpx.Error, change JudgePanel's rerun
// handler to discriminate on THAT first; this test is then free to relax.
func TestRerunJudgeConflictMessages(t *testing.T) {
	const (
		disabledMsg = "run judging is disabled"
		activeMsg   = "a judge run is already in progress for this run"
	)

	newStore := func() (*rerunStore, uuid.UUID) {
		// A completed issue run owned by the caller: past the visibility, owner, terminal
		// -status and eligible-kind gates, so the only thing left to answer with is one of
		// the two 409s.
		ds, runID, _ := oneRecStore()
		return &rerunStore{dispStore: ds}, runID
	}

	// ---- ErrJudgeDisabled: the instance kill-switch is off ---------------------------
	t.Run("judging disabled", func(t *testing.T) {
		st, runID := newStore()
		h := newRunsHandler(t, st)
		h.wsvc.SetSettings(judgeSwitch{enabled: false})

		rec := httptest.NewRecorder()
		h.RerunJudge(rec, rerunReq(store.User{ID: st.ownerID}, runID))

		if rec.Code != http.StatusConflict {
			t.Fatalf("RerunJudge with the kill-switch off = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if got := errorMessage(t, rec); got != disabledMsg {
			t.Fatalf("error = %q, want %q — the client shows this one verbatim", got, disabledMsg)
		}
		if clientConflictMatch.MatchString(errorMessage(t, rec)) {
			t.Fatalf("the DISABLED message %q matches the client's /already in progress/i "+
				"absorb-the-409 test (the 409 branch of JudgePanel's `rerun` handler in "+
				"web/src/pages/RunView.tsx) — a user who turned judging off would "+
				"get silence instead of the reason", errorMessage(t, rec))
		}
		if st.created != 0 {
			t.Fatalf("CreateJudgeRun ran %d times behind a disabled kill-switch; want 0", st.created)
		}
	})

	// ---- ErrJudgeAlreadyActive: the enqueue lost the race to the unique index --------
	t.Run("a judge is already active", func(t *testing.T) {
		st, runID := newStore()
		// Exactly what the driver returns when uq_runs_one_active_judge_per_target
		// rejects the INSERT; workersvc maps 23505 (and only 23505) to
		// ErrJudgeAlreadyActive.
		st.createJudgeErr = &pgconn.PgError{Code: "23505", ConstraintName: "uq_runs_one_active_judge_per_target"}
		h := newRunsHandler(t, st)
		h.wsvc.SetSettings(judgeSwitch{enabled: true})

		rec := httptest.NewRecorder()
		h.RerunJudge(rec, rerunReq(store.User{ID: st.ownerID}, runID))

		if rec.Code != http.StatusConflict {
			t.Fatalf("RerunJudge onto an active judge = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if st.created != 1 {
			t.Fatalf("CreateJudgeRun ran %d times; want 1 — the 409 must come from the index, "+
				"not from a pre-flight check that could go stale", st.created)
		}
		got := errorMessage(t, rec)
		if got != activeMsg {
			t.Fatalf("error = %q, want EXACTLY %q — the 409 branch of JudgePanel's `rerun` "+
				"handler (web/src/pages/RunView.tsx) matches this text with "+
				"/already in progress/i to absorb the TOCTOU 409; reword it there first "+
				"(and in web/src/mocks/mockApi.ts) or the click starts showing an error "+
				"banner for a judge that is genuinely running", got, activeMsg)
		}
		if !clientConflictMatch.MatchString(got) {
			t.Fatalf("%q does not match the client's /already in progress/i test "+
				"(the 409 branch of JudgePanel's `rerun` handler in web/src/pages/RunView.tsx), "+
				"so the re-fetch-and-converge path is unreachable", got)
		}
	})
}

// errorMessage reads httpx.Error's {"error": …} envelope. Decoding into a map rather than
// a struct keeps the assertion on the wire bytes the client actually parses.
func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %s: %v", rec.Body.String(), err)
	}
	return body["error"]
}
