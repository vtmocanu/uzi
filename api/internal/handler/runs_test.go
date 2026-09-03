package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// runsStore is a minimal workersvc.Store for the run REST + WS authz tests: it
// owns a single run and answers the viewer/list queries the tested paths reach.
type runsStore struct {
	// PRD #98 M4 judge badge: scripted per-rec triage rows for the page's runs, and the
	// params the handler passed (owner scoping + the bound).
	judgeTriageRunRows []store.ListJudgeTriageRowsForRunsRow
	judgeTriageRunArg  *store.ListJudgeTriageRowsForRunsParams
	judgeTriageRunErr  error

	// issue #750 plan-revise flag: scripted plan-ish message rows for the page's runs,
	// and the run ids the handler passed.
	planRevisionRunRows []store.ListPlanRevisionStateForRunsRow
	planRevisionRunArg  []uuid.UUID
	planRevisionRunErr  error

	// PRD #1064 M2 current_activity: scripted newest-tool_use rows for the page's runs,
	// and the (non-terminal) run ids the handler passed.
	latestToolUseRows []store.LatestToolUseForRunsRow
	latestToolUseArg  []uuid.UUID
	latestToolUseErr  error

	workersvc.Store
	ownerID uuid.UUID
	run     store.Run
	msgs    []store.RunMessage
	// lastPageLim records the Lim (page size) the handler passed through to
	// ListRunMessagesAfterPage on the bounded ?limit= path, so TestListRunMessagesLimit
	// can assert the handler clamped/forwarded the requested size BEFORE the store call —
	// the fixture size alone can't distinguish a clamped page from an unclamped one.
	lastPageLim int32
	userRuns    []store.ListRunsForUserRow
	lastRunsArg *store.ListRunsForUserParams
	allWorkers  []store.ListAllWorkersRow
	activeRuns  []store.ListActiveRunsAllRow
	// claimTemplates backs GetRun's own-roster resolution (PRD #37 M4-fix): the
	// owner's allocation-resolved templates, lead included so the handler's strip is
	// exercised.
	claimTemplates []store.AgentTemplate
	// PRD #40 usage reads. hasRunUsage=false (default) makes GetRunUsageTotal return
	// pgx.ErrNoRows so a run shows no usage — existing GetRun tests are unaffected.
	hasRunUsage   bool
	runUsageTotal store.GetRunUsageTotalRow
	selfUsage     store.SelfUsageRow
	adminTotals   store.AdminUsageTotalsRow
	adminPerUser  []store.AdminUsagePerUserRow
	// PRD #95 steer queue: the follow_up rows ListFollowUpInputsForRun returns, and the
	// row CreateRunInput echoes (so the richer follow-up write's id/created_at are
	// assertable).
	followUpInputs []store.RunUserInput
	createInputRow store.RunUserInput
	// reviseCount is what CountRunReviseInputs returns (PRD #41 plan-revision cap).
	reviseCount int64
	// PRD #84 M4 4c capability approval gate: the run's owning worker (returned by
	// GetWorkerByID), and the capture/return of the override clear. clearCapsRows is the
	// RowsAffected ClearRunRequiredCapabilities returns; on a clear the fake also empties the
	// run's required set so a subsequent GetRunByIDForUser reload observes the override.
	worker          store.Worker
	clearedCapsArg  *store.ClearRunRequiredCapabilitiesParams
	clearCapsRows   int64
	createdApproval *store.CreateApprovePlanInputParams
}

func (s *runsStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.run.ID && arg.UserID == s.ownerID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *runsStore) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	if id == s.run.ID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *runsStore) ListRunMessagesAfter(context.Context, store.ListRunMessagesAfterParams) ([]store.RunMessage, error) {
	return s.msgs, nil
}
func (s *runsStore) ListRunMessagesAfterPage(_ context.Context, arg store.ListRunMessagesAfterPageParams) ([]store.RunMessage, error) {
	s.lastPageLim = arg.Lim
	out := s.msgs
	if arg.Lim >= 0 && int(arg.Lim) < len(out) {
		out = out[:arg.Lim]
	}
	return out, nil
}
func (s *runsStore) ListClaimAgentTemplates(context.Context, pgtype.UUID) ([]store.AgentTemplate, error) {
	return s.claimTemplates, nil
}
func (s *runsStore) ListRunsForUser(_ context.Context, arg store.ListRunsForUserParams) ([]store.ListRunsForUserRow, error) {
	s.lastRunsArg = &arg
	return s.userRuns, nil
}
func (s *runsStore) ListAllWorkers(context.Context) ([]store.ListAllWorkersRow, error) {
	return s.allWorkers, nil
}
func (s *runsStore) ListActiveRunsAll(context.Context, pgtype.Timestamptz) ([]store.ListActiveRunsAllRow, error) {
	return s.activeRuns, nil
}
func (s *runsStore) CreateRunInput(context.Context, store.CreateRunInputParams) (store.RunUserInput, error) {
	return s.createInputRow, nil
}
func (s *runsStore) CreateApprovePlanInput(_ context.Context, arg store.CreateApprovePlanInputParams) (store.RunUserInput, error) {
	s.createdApproval = &arg
	return store.RunUserInput{}, nil
}
func (s *runsStore) GetRunMilestoneFreezeSnapshot(_ context.Context, id uuid.UUID) (store.GetRunMilestoneFreezeSnapshotRow, error) {
	return store.GetRunMilestoneFreezeSnapshotRow{ID: id}, nil
}
func (s *runsStore) GetWorkerByID(context.Context, uuid.UUID) (store.Worker, error) {
	return s.worker, nil
}
func (s *runsStore) ClearRunRequiredCapabilities(_ context.Context, arg store.ClearRunRequiredCapabilitiesParams) (int64, error) {
	s.clearedCapsArg = &arg
	// Mirror the real owner+status-guarded UPDATE: empty the run's required set so a
	// subsequent GetRunByIDForUser reload (inside SubmitInput) observes the override.
	s.run.RequiredCapabilities = nil
	return s.clearCapsRows, nil
}
func (s *runsStore) CountRunReviseInputs(context.Context, uuid.UUID) (int64, error) {
	return s.reviseCount, nil
}
func (s *runsStore) CreateRunReviseInputIfUnderCap(_ context.Context, arg store.CreateRunReviseInputIfUnderCapParams) (store.RunUserInput, error) {
	// Emulate the atomic cap: insert only while the persisted count is under the cap.
	if s.reviseCount >= int64(arg.MaxRevisions) {
		return store.RunUserInput{}, pgx.ErrNoRows
	}
	return s.createInputRow, nil
}
func (s *runsStore) ListFollowUpInputsForRun(_ context.Context, runID uuid.UUID) ([]store.RunUserInput, error) {
	if runID != s.run.ID {
		return nil, nil
	}
	return s.followUpInputs, nil
}
func (s *runsStore) CreateStopVerdictInput(context.Context, store.CreateStopVerdictInputParams) (store.RunUserInput, error) {
	return store.RunUserInput{}, nil
}
func (s *runsStore) GetRunUsageTotal(_ context.Context, _ uuid.UUID) (store.GetRunUsageTotalRow, error) {
	if !s.hasRunUsage {
		return store.GetRunUsageTotalRow{}, pgx.ErrNoRows
	}
	return s.runUsageTotal, nil
}
func (s *runsStore) SelfUsage(_ context.Context, _ uuid.UUID) (store.SelfUsageRow, error) {
	return s.selfUsage, nil
}
func (s *runsStore) AdminUsageTotals(context.Context) (store.AdminUsageTotalsRow, error) {
	return s.adminTotals, nil
}
func (s *runsStore) AdminUsagePerUser(context.Context) ([]store.AdminUsagePerUserRow, error) {
	return s.adminPerUser, nil
}

func newRunsHandler(t *testing.T, st workersvc.Store) *Handler {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &Handler{wsvc: workersvc.New(st, box, workersvc.Params{}), hub: hub.New()}
}

// runReq builds a GET request authenticated as user with a chi {id} route param.
func runReq(user store.User, runID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/runs/x", nil)
	ctx := mw.ContextWithUser(req.Context(), user)
	if runID != uuid.Nil {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

// inputReq builds a POST /runs/{id}/inputs authenticated as user, carrying a JSON
// steering body.
func inputReq(user store.User, runID uuid.UUID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/runs/x/inputs", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	ctx := context.WithValue(mw.ContextWithUser(req.Context(), user), chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

// TestRunToDTOStopKind pins that the PRD #33 deliberate-stop signal actually reaches the
// RunDTO that GET /api/runs/{id} serves. Added by PRD #97 M4, which dropped the e2e leg
// asserting `.run.stop_kind == plan_rejected` over the wire.
//
// The e2e leg was NOT redundant with the citation the PRD gave: store/stop_kind_integration_test.go
// proves the COLUMN is stamped, apitypes/wire_test.go pins the JSON KEY (shape only), and
// board_latestrun_test.go asserts a stop_kind VALUE — but through mapLatestRun, a different
// DTO built by a different call site. Nothing covered runToDTO's own mapping (runs_dto.go).
//
// Both directions, so the test can discriminate: a stamped value surfaces, and an unstamped
// (NULL) column becomes nil rather than the empty string a naive `.String` read would yield.
func TestRunToDTOStopKind(t *testing.T) {
	stamped := runToDTO(store.Run{
		Status:   "failed",
		StopKind: pgtype.Text{String: "plan_rejected", Valid: true},
	}, "normal")
	if stamped.StopKind == nil {
		t.Fatal("a stamped stop_kind must reach the RunDTO, got nil")
	}
	if *stamped.StopKind != "plan_rejected" {
		t.Errorf("stop_kind = %q, want plan_rejected", *stamped.StopKind)
	}

	// NULL column ⇒ nil pointer (omitted from JSON), never "".
	unstamped := runToDTO(store.Run{Status: "completed"}, "normal")
	if unstamped.StopKind != nil {
		t.Errorf("an unstamped stop_kind must map to nil, got %q", *unstamped.StopKind)
	}
}

// TestRunToDTOStopReason pins that the operator's free-text cancel reason (issue #525)
// reaches the RunDTO runToDTO builds, mirroring the StopKind mapping above. Both
// directions: a stamped reason surfaces, and a NULL column becomes nil rather than "".
func TestRunToDTOStopReason(t *testing.T) {
	stamped := runToDTO(store.Run{
		Status:     "cancelled",
		StopReason: pgtype.Text{String: "wrong approach, restarting", Valid: true},
	}, "normal")
	if stamped.StopReason == nil {
		t.Fatal("a stamped stop_reason must reach the RunDTO, got nil")
	}
	if *stamped.StopReason != "wrong approach, restarting" {
		t.Errorf("stop_reason = %q, want %q", *stamped.StopReason, "wrong approach, restarting")
	}

	// NULL column ⇒ nil pointer (omitted from JSON), never "".
	unstamped := runToDTO(store.Run{Status: "completed"}, "normal")
	if unstamped.StopReason != nil {
		t.Errorf("an unstamped stop_reason must map to nil, got %q", *unstamped.StopReason)
	}
}

// TestRunToDTORequirementSet pins that PRD #84's inferred/hinted requirement set reaches the
// RunDTO runToDTO builds: required_capabilities and required_tools surface their values and
// are normalized to a non-nil empty slice ([] over null) when the column is empty, and
// size_class surfaces the NOT NULL DEFAULT ” string. Covers the M4 4c DTO exposure the
// web/CLI (4d) derive the readiness/mismatch display from.
func TestRunToDTORequirementSet(t *testing.T) {
	populated := runToDTO(store.Run{
		Status:               "awaiting_approval",
		RequiredCapabilities: []string{"docker"},
		RequiredTools:        []string{"go", "node"},
		SizeClass:            "m",
	}, "normal")
	if len(populated.RequiredCapabilities) != 1 || populated.RequiredCapabilities[0] != "docker" {
		t.Errorf("required_capabilities = %v, want [docker]", populated.RequiredCapabilities)
	}
	if len(populated.RequiredTools) != 2 || populated.RequiredTools[0] != "go" {
		t.Errorf("required_tools = %v, want [go node]", populated.RequiredTools)
	}
	if populated.SizeClass != "m" {
		t.Errorf("size_class = %q, want m", populated.SizeClass)
	}

	// Empty columns ⇒ non-nil empty slices ([] over null) and "" for size_class.
	empty := runToDTO(store.Run{Status: "queued"}, "normal")
	if empty.RequiredCapabilities == nil || len(empty.RequiredCapabilities) != 0 {
		t.Errorf("required_capabilities = %v, want non-nil empty", empty.RequiredCapabilities)
	}
	if empty.RequiredTools == nil || len(empty.RequiredTools) != 0 {
		t.Errorf("required_tools = %v, want non-nil empty", empty.RequiredTools)
	}
	if empty.SizeClass != "" {
		t.Errorf("size_class = %q, want empty", empty.SizeClass)
	}
}

func TestGetRunOwnerNonOwnerAdmin(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)

	// Owner sees.
	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GetRun = %d, want 200", rec.Code)
	}

	// Non-owner, non-admin: 404 (indistinguishable from unknown id).
	other := store.User{ID: uuid.New()}
	rec = httptest.NewRecorder()
	h.GetRun(rec, runReq(other, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner GetRun = %d, want 404", rec.Code)
	}

	// Admin sees any run.
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	rec = httptest.NewRecorder()
	h.GetRun(rec, runReq(admin, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GetRun = %d, want 200 (admin sees all)", rec.Code)
	}
}

// TestGetRunPopulatesOwnAgents proves the run-detail DTO carries the owner's
// allocation-resolved OWN roster (name + description), with the lead stripped: the
// plan-gate picker's "My agent templates" chips come from exactly what
// ListClaimAgentTemplates delivers, not the broader visible-template list (PRD #37
// M4-fix — the allocation gap that could 400 an excluded-but-undelivered chip).
func TestGetRunPopulatesOwnAgents(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{
		ownerID: owner.ID,
		run:     store.Run{ID: runID, UserID: owner.ID, Status: "awaiting_approval"},
		claimTemplates: []store.AgentTemplate{
			{Name: "lead", Description: "orchestrates"},
			{Name: "coder", Description: "implements features"},
			{Name: "reviewer", Description: "reviews changes"},
		},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetRun = %d, want 200", rec.Code)
	}
	var body struct {
		Run struct {
			OwnAgents []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"own_agents"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	got := body.Run.OwnAgents
	if len(got) != 2 {
		t.Fatalf("own_agents len = %d, want 2 (lead stripped): %+v", len(got), got)
	}
	if got[0].Name != "coder" || got[0].Description != "implements features" {
		t.Errorf("own_agents[0] = %+v, want {coder, implements features}", got[0])
	}
	if got[1].Name != "reviewer" {
		t.Errorf("own_agents[1].Name = %q, want reviewer", got[1].Name)
	}
	for _, a := range got {
		if a.Name == "lead" {
			t.Fatalf("lead must be stripped from own_agents, got %+v", got)
		}
	}
}

func TestListRunMessagesViewerAuthz(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{
		ownerID: owner.ID,
		run:     store.Run{ID: runID, UserID: owner.ID, Status: "running"},
		msgs:    []store.RunMessage{{Seq: 1, Kind: "text", Payload: []byte(`{}`)}},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.ListRunMessages(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"seq":1`) {
		t.Fatalf("owner messages = %d %q, want 200 with seq 1", rec.Code, rec.Body.String())
	}

	other := store.User{ID: uuid.New()}
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReq(other, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner messages = %d, want 404", rec.Code)
	}

	admin := store.User{ID: uuid.New(), IsAdmin: true}
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReq(admin, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin messages = %d, want 200", rec.Code)
	}
}

// TestListRunMessagesPagedViewerAuthz is the ?limit= twin of TestListRunMessagesViewerAuthz:
// it proves the owner-or-admin gate holds on the BOUNDED path too, so a future edit that
// forgets the GetRunForViewer check on ListRunMessagesForViewerPage is caught. Modeled on
// the unbounded authz test's harness (same runsStore fake, same three viewers), but every
// request carries ?limit=5 so it drives the paged service method.
func TestListRunMessagesPagedViewerAuthz(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{
		ownerID: owner.ID,
		run:     store.Run{ID: runID, UserID: owner.ID, Status: "running"},
		msgs:    []store.RunMessage{{Seq: 1, Kind: "text", Payload: []byte(`{}`)}},
	}
	h := newRunsHandler(t, st)

	// Owner sees the bounded page.
	rec := httptest.NewRecorder()
	h.ListRunMessages(rec, runReqQuery(owner, runID, "limit=5"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"seq":1`) {
		t.Fatalf("owner paged messages = %d %q, want 200 with seq 1", rec.Code, rec.Body.String())
	}

	// A non-owner is denied the SAME way as on the unbounded path: 404, no messages.
	other := store.User{ID: uuid.New()}
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReqQuery(other, runID, "limit=5"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner paged messages = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"seq"`) {
		t.Fatalf("non-owner paged messages must leak nothing, got %q", rec.Body.String())
	}

	// An admin sees any run through the bounded path.
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReqQuery(admin, runID, "limit=5"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin paged messages = %d, want 200", rec.Code)
	}
}

// runReqQuery is runReq with a raw query string (e.g. "limit=2") on the URL so the
// ListRunMessages handler's ?limit=/?after= parsing is exercised.
func runReqQuery(user store.User, runID uuid.UUID, rawQuery string) *http.Request {
	req := runReq(user, runID)
	req.URL.RawQuery = rawQuery
	return req
}

// TestListRunMessagesLimit covers the opt-in bounded ?limit= page (issue #160 M2):
// a valid limit returns a bounded slice, invalid limits are 400, an absent limit
// stays on the unbounded legacy path, and an over-max limit is clamped down.
func TestListRunMessagesLimit(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	msgs := make([]store.RunMessage, 0, 5)
	for i := 0; i < 5; i++ {
		msgs = append(msgs, store.RunMessage{Seq: int32(i + 1), Kind: "text", Payload: []byte(`{}`)})
	}
	st := &runsStore{
		ownerID: owner.ID,
		run:     store.Run{ID: runID, UserID: owner.ID, Status: "running"},
		msgs:    msgs,
	}
	h := newRunsHandler(t, st)

	decodeSeqs := func(t *testing.T, body string) []int32 {
		t.Helper()
		var env struct {
			Messages []struct {
				Seq int32 `json:"seq"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decode messages: %v (body %q)", err, body)
		}
		seqs := make([]int32, 0, len(env.Messages))
		for _, m := range env.Messages {
			seqs = append(seqs, m.Seq)
		}
		return seqs
	}

	// (a) ?limit=2 returns exactly the first 2 of 5 messages, in seq order.
	rec := httptest.NewRecorder()
	h.ListRunMessages(rec, runReqQuery(owner, runID, "limit=2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("limit=2 status = %d, want 200", rec.Code)
	}
	if got := decodeSeqs(t, rec.Body.String()); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("limit=2 seqs = %v, want [1 2]", got)
	}
	// The handler must forward the exact requested page size on the non-clamped path.
	if st.lastPageLim != 2 {
		t.Fatalf("limit=2 forwarded Lim = %d, want 2", st.lastPageLim)
	}

	// (b) invalid limits are rejected with 400.
	for _, raw := range []string{"limit=0", "limit=-1", "limit=abc"} {
		rec = httptest.NewRecorder()
		h.ListRunMessages(rec, runReqQuery(owner, runID, raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", raw, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "limit must be a positive integer") {
			t.Fatalf("%s body = %q, want positive-integer error", raw, rec.Body.String())
		}
	}

	// (c) absent limit stays on the unbounded path: all 5 messages come back.
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReq(owner, runID))
	if got := decodeSeqs(t, rec.Body.String()); len(got) != 5 {
		t.Fatalf("no-limit seqs = %v, want all 5", got)
	}

	// (d) a limit above maxRunMessagesPage is clamped down to it BEFORE the store call.
	// The 5-message fixture can't distinguish clamped from unclamped by response size, so
	// this asserts on the Lim the handler actually forwarded to the store: the handler
	// must have reduced maxRunMessagesPage+50 to exactly maxRunMessagesPage. Deleting the
	// clamp block in runs_lifecycle.go reddens this assertion (the fake would record the raw
	// over-max Lim), which is what gives the subcase its gating power.
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReqQuery(owner, runID, fmt.Sprintf("limit=%d", maxRunMessagesPage+50)))
	if rec.Code != http.StatusOK {
		t.Fatalf("over-max limit status = %d, want 200", rec.Code)
	}
	if st.lastPageLim != maxRunMessagesPage {
		t.Fatalf("over-max limit forwarded Lim = %d, want it clamped to %d", st.lastPageLim, maxRunMessagesPage)
	}
	if got := decodeSeqs(t, rec.Body.String()); len(got) > maxRunMessagesPage {
		t.Fatalf("over-max limit returned %d messages, want <= %d", len(got), maxRunMessagesPage)
	}
}

// gzipAuthDB is a store.DBTX that lets a request clear RequireUser's cookie branch
// without a live database: it answers getUserByID ("FROM users") with a single active
// user and returns no rows for anything else. It exists so TestListRunMessagesGzip can
// drive its request through the REAL h.Routes() router — including the Compress
// middleware handler.go mounts on /runs/{id}/messages — rather than a hand-built one.
type gzipAuthDB struct{ user store.User }

func (gzipAuthDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (gzipAuthDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (d gzipAuthDB) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
	if strings.Contains(sql, "FROM users") {
		return gzipUserRow{u: d.user}
	}
	return gzipNoRow{}
}

type gzipNoRow struct{}

func (gzipNoRow) Scan(...any) error { return pgx.ErrNoRows }

// gzipUserRow scans the stored user positionally into getUserByID's destinations.
// sqlc generates the Scan targets in User's field-declaration order, so a positional
// copy is faithful; the field-count guard reddens loudly if that ceases to hold.
type gzipUserRow struct{ u store.User }

func (r gzipUserRow) Scan(dest ...any) error {
	v := reflect.ValueOf(r.u)
	if v.NumField() != len(dest) {
		return fmt.Errorf("gzipUserRow: user has %d fields but scan wants %d", v.NumField(), len(dest))
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(v.Field(i))
	}
	return nil
}

// TestListRunMessagesGzip proves the run-messages route is wired through chi's
// Compress middleware (handler.go:1097): a request advertising `Accept-Encoding: gzip`
// gets a `Content-Encoding: gzip` body that, once inflated, is byte-identical to the
// uncompressed response. It drives the request through the REAL h.Routes() router, so
// deleting `r.With(chimw.Compress(5))` in handler.go makes this test fail — a
// hand-built router registering the middleware itself could not catch that regression.
//
// chi v5's Compress negotiates on Accept-Encoding and Content-Type only (there is no
// size threshold), so a one-message fixture would already exercise it; the larger,
// repetitive fixture just makes the inflate-equals-plain assertion cover a body big
// enough that compression is non-degenerate.
func TestListRunMessagesGzip(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	msgs := make([]store.RunMessage, 0, 200)
	for i := 0; i < 200; i++ {
		msgs = append(msgs, store.RunMessage{
			Seq:     int32(i + 1),
			Kind:    "text",
			Payload: []byte(`{"role":"assistant","content":"a repetitive message body that compresses well"}`),
		})
	}
	st := &runsStore{
		ownerID: owner.ID,
		run:     store.Run{ID: runID, UserID: owner.ID, Status: "running"},
		msgs:    msgs,
	}

	// Real router, real RequireUser: authenticate over the cookie branch as the owner,
	// backed by gzipAuthDB so getUserByID resolves without a live database.
	secret := []byte("0123456789abcdef0123456789abcdef")
	h := &Handler{
		q:    store.New(gzipAuthDB{user: store.User{ID: owner.ID, IsActive: true}}),
		cfg:  config.Config{JWTSecret: secret, AuthTokenTTL: time.Hour},
		wsvc: workersvc.New(st, newHandlerTestBox(t), workersvc.Params{}),
		hub:  hub.New(),
	}
	noLimit := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit)

	jwt, err := auth.IssueToken(secret, owner.ID.String(), 0, time.Hour)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	newReq := func(acceptGzip bool) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/messages", nil)
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request; Secure/HttpOnly/SameSite are response-side attributes irrelevant to a cookie a unit test sends.
		if acceptGzip {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		return req
	}

	// Uncompressed baseline.
	plainRec := httptest.NewRecorder()
	router.ServeHTTP(plainRec, newReq(false))
	if plainRec.Code != http.StatusOK {
		t.Fatalf("plain messages = %d, want 200", plainRec.Code)
	}
	if ce := plainRec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("plain request must not be gzip-encoded, got Content-Encoding %q", ce)
	}

	// gzip-negotiated request.
	gzRec := httptest.NewRecorder()
	router.ServeHTTP(gzRec, newReq(true))
	if gzRec.Code != http.StatusOK {
		t.Fatalf("gzip messages = %d, want 200", gzRec.Code)
	}
	if ce := gzRec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip (Compress middleware must wrap this route)", ce)
	}

	gr, err := gzip.NewReader(bytes.NewReader(gzRec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	inflated, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip inflate: %v", err)
	}
	if !bytes.Equal(inflated, plainRec.Body.Bytes()) {
		t.Fatalf("inflated gzip body differs from uncompressed body:\ngot  %q\nwant %q", inflated, plainRec.Body.Bytes())
	}
}

func TestCreateRunInputIsOwnerOnly(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)

	// Owner may steer their own run.
	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID, `{"kind":"follow_up","body":"keep going"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner steering = %d, want 202", rec.Code)
	}

	// A non-owner is denied (404, indistinguishable from an unknown run).
	rec = httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(store.User{ID: uuid.New()}, runID, `{"kind":"follow_up","body":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner steering = %d, want 404", rec.Code)
	}

	// A non-owner ADMIN is ALSO denied: steering is owner-only, with no admin
	// bypass (admin visibility is read-only — reads + WS are owner-or-admin, but
	// approving/cancelling another user's run is not).
	rec = httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(store.User{ID: uuid.New(), IsAdmin: true}, runID, `{"kind":"cancel"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner admin steering = %d, want 404 (steering is owner-only)", rec.Code)
	}
}

// TestListRunInputsOwnerOnly is the PRD #95 M1 authz matrix for the steer-queue read:
// the owner sees their follow_up queue; a non-owner AND a non-owner admin (admin_ro)
// both get 404 — GET /inputs is strict owner-only (GetRunByIDForUser), not owner-or-
// admin, because follow-ups are never in run_messages and would otherwise leak. The
// DTO carries body + consumed_at so the client derives Queued/Delivered; the handler
// preserves the query's newest-first order (no re-sort, no truncation).
func TestListRunInputsOwnerOnly(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	newer := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	consumed := newer.Add(30 * time.Minute)
	st := &runsStore{
		ownerID: owner.ID,
		run:     store.Run{ID: runID, UserID: owner.ID, Status: "running"},
		// Newest-first, as the query returns them: a Delivered (consumed_at set) then a
		// Queued (consumed_at NULL). The handler must not reorder or drop either.
		followUpInputs: []store.RunUserInput{
			{ID: 2, RunID: runID, Kind: "follow_up", Body: pgtype.Text{String: "and the changelog", Valid: true},
				ConsumedAt: pgtype.Timestamptz{Time: consumed, Valid: true}, CreatedAt: pgtype.Timestamptz{Time: newer, Valid: true}},
			{ID: 1, RunID: runID, Kind: "follow_up", Body: pgtype.Text{String: "focus on the api", Valid: true},
				CreatedAt: pgtype.Timestamptz{Time: older, Valid: true}},
		},
	}
	h := newRunsHandler(t, st)

	// Owner sees the queue, in the returned order, with delivery status derivable.
	rec := httptest.NewRecorder()
	h.ListRunInputs(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner ListRunInputs = %d, want 200", rec.Code)
	}
	var body struct {
		Inputs []struct {
			ID         int64      `json:"id"`
			Body       *string    `json:"body"`
			ConsumedAt *time.Time `json:"consumed_at"`
			CreatedAt  time.Time  `json:"created_at"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Inputs) != 2 {
		t.Fatalf("inputs len = %d, want 2 (no truncation): %s", len(body.Inputs), rec.Body.String())
	}
	// Order preserved (newest id first); the first is Delivered, the second Queued.
	if body.Inputs[0].ID != 2 || body.Inputs[0].ConsumedAt == nil {
		t.Errorf("inputs[0] = %+v, want id 2 delivered (consumed_at set)", body.Inputs[0])
	}
	if body.Inputs[1].ID != 1 || body.Inputs[1].ConsumedAt != nil {
		t.Errorf("inputs[1] = %+v, want id 1 queued (consumed_at nil)", body.Inputs[1])
	}
	if body.Inputs[1].Body == nil || *body.Inputs[1].Body != "focus on the api" {
		t.Errorf("inputs[1].body = %v, want the message text", body.Inputs[1].Body)
	}

	// A non-owner is denied (404, indistinguishable from an unknown run).
	rec = httptest.NewRecorder()
	h.ListRunInputs(rec, runReq(store.User{ID: uuid.New()}, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner ListRunInputs = %d, want 404", rec.Code)
	}

	// A non-owner ADMIN (admin_ro) is ALSO denied: the steer read is owner-only, so an
	// admin cannot read another user's follow-up text even though they can view the run.
	rec = httptest.NewRecorder()
	h.ListRunInputs(rec, runReq(store.User{ID: uuid.New(), IsAdmin: true}, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner admin ListRunInputs = %d, want 404 (steer read is owner-only)", rec.Code)
	}
}

// TestCreateRunInputFollowUpReturnsCreatedRow pins the richer follow-up write (PRD #95
// S2): a follow_up POST returns the created row's id + created_at so the web's
// optimistic queue entry adopts the real id; a non-follow_up input omits them.
func TestCreateRunInputFollowUpReturnsCreatedRow(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	created := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	st := &runsStore{
		ownerID:        owner.ID,
		run:            store.Run{ID: runID, UserID: owner.ID, Status: "running"},
		createInputRow: store.RunUserInput{ID: 42, RunID: runID, Kind: "follow_up", CreatedAt: pgtype.Timestamptz{Time: created, Valid: true}},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID, `{"kind":"follow_up","body":"keep going"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("follow_up = %d, want 202", rec.Code)
	}
	var resp struct {
		ServerSide bool       `json:"server_side"`
		ID         *int64     `json:"id"`
		CreatedAt  *time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.ID == nil || *resp.ID != 42 {
		t.Fatalf("follow_up response id = %v, want 42 (the created row)", resp.ID)
	}
	if resp.CreatedAt == nil || !resp.CreatedAt.Equal(created) {
		t.Fatalf("follow_up response created_at = %v, want %v", resp.CreatedAt, created)
	}

	// An approve_plan (no selection) reports no queue row: id/created_at omitted.
	rec = httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID, `{"kind":"approve_plan"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve_plan = %d, want 202", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"id"`) || strings.Contains(rec.Body.String(), `"created_at"`) {
		t.Fatalf("approve_plan response must omit id/created_at, got %s", rec.Body.String())
	}
}

// TestCreateRunInputRevisePlanCap pins the PRD #41 wiring end-to-end at the handler:
// revise_plan is an accepted kind (not a 400), an accepted revision returns 202, and
// a revision over PLAN_MAX_REVISIONS maps ErrReviseCapReached → 409.
func TestCreateRunInputRevisePlanCap(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	newH := func(st workersvc.Store) *Handler {
		box, err := secretbox.New(make([]byte, secretbox.KeySize))
		if err != nil {
			t.Fatalf("new box: %v", err)
		}
		return &Handler{wsvc: workersvc.New(st, box, workersvc.Params{PlanMaxRevisions: 3}), hub: hub.New()}
	}

	// Under the cap (2 persisted, cap 3 → 3rd revise accepted) → 202.
	stOK := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "awaiting_approval"}, reviseCount: 2}
	rec := httptest.NewRecorder()
	newH(stOK).CreateRunInput(rec, inputReq(owner, runID, `{"kind":"revise_plan","body":"use pgx"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("under-cap revise = %d, want 202", rec.Code)
	}

	// At the cap (3 persisted, cap 3 → 4th revise rejected) → 409.
	stCapped := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "awaiting_approval"}, reviseCount: 3}
	rec = httptest.NewRecorder()
	newH(stCapped).CreateRunInput(rec, inputReq(owner, runID, `{"kind":"revise_plan","body":"again"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("over-cap revise = %d, want 409", rec.Code)
	}
}

// TestCreateRunInputChatFollowUp409 pins the chat-cap hole fix (issue #258 M5): a
// follow_up posted to the generic /inputs endpoint against a CHAT run is rejected at
// the service boundary and the handler maps ErrChatInputNotAllowed → 409. Chat turns
// must ride the chat message endpoint, which enforces CHAT_MAX_TURNS.
func TestCreateRunInputChatFollowUp409(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running", Kind: runkind.Chat}}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID, `{"kind":"follow_up","body":"keep going"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("chat follow_up on /inputs = %d, want 409", rec.Code)
	}
}

func TestListRunsReturnsUsersRuns(t *testing.T) {
	user := store.User{ID: uuid.New()}
	st := &runsStore{userRuns: []store.ListRunsForUserRow{
		{Run: store.Run{ID: uuid.New(), Status: "queued"}, RepoPath: "grp/repo", WorkerName: pgtype.Text{String: "laptop", Valid: true}},
	}}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "grp/repo") || !strings.Contains(rec.Body.String(), "laptop") {
		t.Fatalf("ListRuns body missing display fields: %q", rec.Body.String())
	}
	// No query params → both narrowings NULL (the unchanged full list).
	if st.lastRunsArg == nil || st.lastRunsArg.RepoID.Valid || st.lastRunsArg.IssueIid.Valid {
		t.Fatalf("unfiltered ListRuns must pass NULL narrowings, got %+v", st.lastRunsArg)
	}
	if st.lastRunsArg.UserID != user.ID {
		t.Fatalf("ListRuns must scope to the requesting user")
	}
}

func TestListRunsThreadsRepoIssueFilters(t *testing.T) {
	user := store.User{ID: uuid.New()}
	repoID := uuid.New()
	st := &runsStore{}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs?repo_id="+repoID.String()+"&issue_iid=42", nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns = %d, want 200", rec.Code)
	}
	if st.lastRunsArg == nil {
		t.Fatal("ListRunsForUser not called")
	}
	if !st.lastRunsArg.RepoID.Valid || uuid.UUID(st.lastRunsArg.RepoID.Bytes) != repoID {
		t.Fatalf("repo_id filter not threaded: %+v", st.lastRunsArg.RepoID)
	}
	if !st.lastRunsArg.IssueIid.Valid || st.lastRunsArg.IssueIid.Int64 != 42 {
		t.Fatalf("issue_iid filter not threaded: %+v", st.lastRunsArg.IssueIid)
	}
}

func TestListRunsRejectsMalformedFilters(t *testing.T) {
	user := store.User{ID: uuid.New()}
	for _, q := range []string{"?repo_id=not-a-uuid", "?issue_iid=abc"} {
		st := &runsStore{}
		h := newRunsHandler(t, st)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/runs"+q, nil)
		h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("ListRuns%s = %d, want 400", q, rec.Code)
		}
		if st.lastRunsArg != nil {
			t.Fatalf("a malformed filter must reject before hitting the store (%s)", q)
		}
	}
}

func TestAdminListWorkersAndRuns(t *testing.T) {
	revisingRun := uuid.New()
	st := &runsStore{
		allWorkers: []store.ListAllWorkersRow{
			{Worker: store.Worker{ID: uuid.New(), Name: "w1", Status: "online"}, Busy: true, ActiveRuns: 1, OwnerEmail: "u@example.com"},
		},
		activeRuns: []store.ListActiveRunsAllRow{
			{Run: store.Run{ID: revisingRun, Status: "awaiting_approval"}, RepoPath: "grp/repo", OwnerEmail: "u@example.com"},
		},
		// The run is mid-replan: its latest plan-ish message is a plan_revising, so the admin
		// list must enrich is_revising the same way ListRuns does (issue #750).
		planRevisionRunRows: []store.ListPlanRevisionStateForRunsRow{
			{RunID: revisingRun, Seq: 1, Kind: "plan"},
			{RunID: revisingRun, Seq: 2, Kind: "plan_revising"},
		},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.AdminListWorkers(rec, httptest.NewRequest(http.MethodGet, "/api/admin/workers", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "u@example.com") {
		t.Fatalf("AdminListWorkers = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.AdminListRuns(rec, httptest.NewRequest(http.MethodGet, "/api/admin/runs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "grp/repo") {
		t.Fatalf("AdminListRuns = %d %q", rec.Code, rec.Body.String())
	}
	// The admin overview must carry is_revising too, so `uzi admin runs` renders a mid-replan
	// run as "revising" (via effectiveRunStatus) rather than as awaiting_approval.
	if !strings.Contains(rec.Body.String(), `"is_revising":true`) {
		t.Fatalf("AdminListRuns must enrich is_revising for a revising run: %q", rec.Body.String())
	}
	if got := st.planRevisionRunArg; len(got) != 1 || got[0] != revisingRun {
		t.Fatalf("AdminListRuns plan-revising lookup arg = %v, want [%v]", got, revisingRun)
	}
}

// wsTestServer wires ServeWS behind a middleware that injects user into context,
// standing in for the RequireUser mount (PRD #112 M1) so the WS authz + upgrade can
// be exercised end to end over a real (hijackable) connection.
//
// It deliberately proves nothing about WHICH middleware handler.go mounted /ws
// behind — it injects the user directly, so it is green either way. That property is
// pinned separately, over the real router, in ws_bearer_livedb_test.go.
func wsTestServer(h *Handler, user store.User) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r.WithContext(mw.ContextWithUser(r.Context(), user)))
	}))
}

func wsURL(t *testing.T, base string, runID uuid.UUID) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(base, "http") + "/?run=" + runID.String()
}

func TestServeWSDeniesNonOwnerBeforeUpgrade(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)

	// A non-owner is refused at authz, before any upgrade — recorder never hijacks.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ws?run="+runID.String(), nil)
	h.ServeWS(rec, req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()})))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner ServeWS = %d, want 404 (denied before upgrade)", rec.Code)
	}
}

func TestServeWSDeliversLiveEventsToOwner(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)
	srv := wsTestServer(h, owner)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(t, srv.URL, runID), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	// Publish repeatedly until the read lands: the subscription is registered a
	// beat after the handshake, so a single early publish could race it.
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				h.hub.PublishMessage(runID, 1, "text", "coder", "", "", json.RawMessage(`{"x":1}`), time.Now())
			}
		}
	}()
	defer close(stop)

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var ev hub.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if ev.Type != "message" || ev.Seq != 1 {
		t.Fatalf("unexpected frame: %+v", ev)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func TestServeWSRejectsCrossOrigin(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)
	srv := wsTestServer(h, owner)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A mismatched Origin (the CSWSH case) must fail the handshake: coder/websocket
	// enforces Origin == Host by default, which ServeWS relies on.
	_, _, err := websocket.Dial(ctx, wsURL(t, srv.URL, runID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		t.Fatal("cross-origin WS handshake must be rejected (CSWSH defense)")
	}
}

// ListJudgeTriageRowsForRuns backs the /runs judge badge count (PRD #98 M4). The default
// is an empty set — an unjudged page — so every pre-existing run-list test keeps its
// meaning; judgeTriageRunRows lets a test script real recommendations.
func (s *runsStore) ListJudgeTriageRowsForRuns(_ context.Context, arg store.ListJudgeTriageRowsForRunsParams) ([]store.ListJudgeTriageRowsForRunsRow, error) {
	s.judgeTriageRunArg = &arg
	return s.judgeTriageRunRows, s.judgeTriageRunErr
}

// ListPlanRevisionStateForRuns backs the /runs plan-revise flag (issue #750). The
// default is an empty set — no run revising — so every pre-existing run-list test keeps
// its meaning; planRevisionRunRows lets a test script plan-ish message rows.
func (s *runsStore) ListPlanRevisionStateForRuns(_ context.Context, runIds []uuid.UUID) ([]store.ListPlanRevisionStateForRunsRow, error) {
	s.planRevisionRunArg = runIds
	return s.planRevisionRunRows, s.planRevisionRunErr
}

// LatestToolUseForRuns backs the current_activity "now" line (PRD #1064 M2). The default
// is no rows — no tool_use frame, so every run reads a null current_activity and every
// pre-existing run-list/get test keeps its meaning; latestToolUseRows lets a test script
// the newest tool_use per run, latestToolUseArg captures the (non-terminal) ids passed.
func (s *runsStore) LatestToolUseForRuns(_ context.Context, runIds []uuid.UUID) ([]store.LatestToolUseForRunsRow, error) {
	s.latestToolUseArg = runIds
	return s.latestToolUseRows, s.latestToolUseErr
}
