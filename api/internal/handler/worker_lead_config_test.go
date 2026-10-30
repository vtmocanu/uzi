package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/hub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// leadConfigStore is a minimal workersvc.Store for the lead-config worker route (PRD #3244): it
// overrides only the ownership read and the fenced two-statement write.
type leadConfigStore struct {
	workersvc.Store
	ownedErr error

	recordRes   store.RecordRunLeadConfigResult
	recordErr   error
	recordCalls int
	recordArg   store.RecordRunLeadConfigParams
}

func (m *leadConfigStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return store.Run{ID: uuid.New(), Kind: "issue", ClaimGeneration: 1}, m.ownedErr
}

func (m *leadConfigStore) RecordRunLeadConfig(_ context.Context, arg store.RecordRunLeadConfigParams) (store.RecordRunLeadConfigResult, error) {
	m.recordCalls++
	m.recordArg = arg
	return m.recordRes, m.recordErr
}

// recordingBroadcaster stands in for the Slack side of the MultiBroadcaster: it counts every call.
type recordingBroadcaster struct {
	mu    sync.Mutex
	calls int
}

func (b *recordingBroadcaster) bump() { b.mu.Lock(); b.calls++; b.mu.Unlock() }
func (b *recordingBroadcaster) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
	b.bump()
}
func (b *recordingBroadcaster) PublishState(uuid.UUID, string)                { b.bump() }
func (b *recordingBroadcaster) PublishHealth(uuid.UUID, string, string, bool) { b.bump() }
func (b *recordingBroadcaster) PublishInput(uuid.UUID)                        { b.bump() }

// leadConfigBody is a valid Claude default-case report; mutate overrides keys (nil deletes one).
func leadConfigBody(t *testing.T, mutate map[string]any) string {
	t.Helper()
	m := map[string]any{
		"claim_generation":         1,
		"phase":                    "plan",
		"phase_entry":              1,
		"seq":                      1,
		"truncated":                false,
		"requested_model":          "opus",
		"requested_model_source":   "user_default",
		"requested_effort":         "medium",
		"requested_effort_mode":    "explicit",
		"requested_effort_source":  "builtin",
		"observed_model":           nil,
		"observed_model_evidence":  nil,
		"observed_effort":          nil,
		"observed_effort_evidence": nil,
	}
	for k, v := range mutate {
		if v == nil && (k == "claim_generation") {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func leadConfigPost(h *Handler, runID uuid.UUID, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.WorkerRecordLeadConfig(rec, workerReq(http.MethodPost, body, runID))
	return rec
}

// leadConfigHandler builds a handler over st with a live hub and a recording broadcaster, and
// subscribes to runID's hub frames.
func leadConfigHandler(t *testing.T, st *leadConfigStore, runID uuid.UUID) (*Handler, *hub.Subscription, *recordingBroadcaster) {
	t.Helper()
	h := newProtocolHandler(t, st)
	hb := hub.New()
	h.hub = hb
	slack := &recordingBroadcaster{}
	h.wsvc.SetBroadcaster(workersvc.MultiBroadcaster{slack})
	sub := hb.Subscribe(runID)
	t.Cleanup(sub.Close)
	return h, sub, slack
}

// drainFrames returns the frames currently buffered on sub (publishing is synchronous).
func drainFrames(sub *hub.Subscription) []string {
	var out []string
	for {
		select {
		case f := <-sub.Events():
			out = append(out, string(f))
		default:
			return out
		}
	}
}

func TestLeadConfigValidationRejections(t *testing.T) {
	type tc struct {
		name   string
		body   string
		status int
		reason string
		field  string // must appear in the message ("" = not asserted)
		secret string // must NOT appear in the response
	}
	bad := func(mut map[string]any) string { return leadConfigBody(t, mut) }
	valid := bad(nil)
	cases := []tc{
		{name: "unknown field", body: strings.TrimSuffix(valid, "}") + `,"surprise":"leak-unknown"}`, status: 400, reason: "invalid_lead_config", secret: "leak-unknown"},
		{name: "trailing value", body: valid + ` {"x":"leak-trailing"}`, status: 400, reason: "invalid_lead_config", secret: "leak-trailing"},
		{name: "4097-byte body", body: valid + strings.Repeat(" ", 4097-len(valid)), status: 400, reason: "invalid_lead_config"},
		{name: "malformed json", body: `{"claim_generation":`, status: 400, reason: "invalid_lead_config"},
		{name: "model with control char", body: bad(map[string]any{"requested_model": "op\x1bus-leak"}), status: 400, reason: "invalid_lead_config", field: "requested_model", secret: "us-leak"},
		{name: "model with bidi char", body: bad(map[string]any{"requested_model": "op‮us-leak"}), status: 400, reason: "invalid_lead_config", field: "requested_model", secret: "us-leak"},
		{name: "101-byte model", body: bad(map[string]any{"requested_model": strings.Repeat("m", 101)}), status: 400, reason: "invalid_lead_config", field: "requested_model", secret: "mmmmmmmm"},
		{name: "bad phase enum", body: bad(map[string]any{"phase": "review-leak"}), status: 400, reason: "invalid_lead_config", field: "phase", secret: "review-leak"},
		{name: "bad source enum", body: bad(map[string]any{"requested_model_source": "wat-leak"}), status: 400, reason: "invalid_lead_config", field: "requested_model_source", secret: "wat-leak"},
		{name: "bad effort", body: bad(map[string]any{"requested_effort": "turbo-leak"}), status: 400, reason: "invalid_lead_config", field: "requested_effort", secret: "turbo-leak"},
		{name: "bad observed evidence", body: bad(map[string]any{"observed_model": "claude-x", "observed_model_evidence": "guess-leak"}), status: 400, reason: "invalid_lead_config", field: "observed_model_evidence", secret: "guess-leak"},
		{name: "generation missing", body: bad(map[string]any{"claim_generation": nil}), status: 400, reason: "claim_generation_required"},
		{name: "generation zero", body: bad(map[string]any{"claim_generation": 0}), status: 400, reason: "claim_generation_required"},
		{name: "generation negative", body: bad(map[string]any{"claim_generation": -2}), status: 400, reason: "claim_generation_required"},
		{name: "phase_entry 9", body: bad(map[string]any{"phase_entry": 9}), status: 400, reason: "invalid_lead_config", field: "phase_entry"},
		{name: "phase_entry 0", body: bad(map[string]any{"phase_entry": 0}), status: 400, reason: "invalid_lead_config", field: "phase_entry"},
		{name: "seq 33", body: bad(map[string]any{"seq": 33}), status: 400, reason: "invalid_lead_config", field: "seq"},
		{name: "truncated with seq 31", body: bad(map[string]any{"truncated": true, "seq": 31}), status: 400, reason: "invalid_lead_config", field: "truncated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runID := uuid.New()
			st := &leadConfigStore{recordRes: store.RecordRunLeadConfigResult{Inserted: true}}
			h, sub, slack := leadConfigHandler(t, st, runID)
			rec := leadConfigPost(h, runID, c.body)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.status, rec.Body.String())
			}
			out := decodeMap(t, rec)
			if out["reason"] != c.reason {
				t.Fatalf("reason = %v, want %s (%s)", out["reason"], c.reason, rec.Body.String())
			}
			if c.field != "" && !strings.Contains(fmt.Sprint(out["error"]), c.field) {
				t.Errorf("message %q must name the field %q", out["error"], c.field)
			}
			if c.secret != "" && strings.Contains(rec.Body.String(), c.secret) {
				t.Errorf("response %q echoes the rejected value %q", rec.Body.String(), c.secret)
			}
			if st.recordCalls != 0 {
				t.Errorf("a rejected body reached the store %d times", st.recordCalls)
			}
			if f := drainFrames(sub); len(f) != 0 || slack.calls != 0 {
				t.Errorf("a rejected body published frames %v / broadcasts %d", f, slack.calls)
			}
		})
	}
}

func TestLeadConfigBodyAtCapIsNotRejectedForSize(t *testing.T) {
	// A body of exactly 4096 bytes decodes (the 4097-byte case above is the over-cap side).
	runID := uuid.New()
	st := &leadConfigStore{recordRes: store.RecordRunLeadConfigResult{Inserted: true}}
	h, _, _ := leadConfigHandler(t, st, runID)
	valid := leadConfigBody(t, nil)
	body := valid + strings.Repeat(" ", 4096-len(valid))
	if rec := leadConfigPost(h, runID, body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

func TestLeadConfigWriteOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		ownedErr  error
		recordRes store.RecordRunLeadConfigResult
		recordErr error
		status    int
		reason    string
		field     string
		wantCalls int
		refresh   int
	}{
		{name: "inserted", recordRes: store.RecordRunLeadConfigResult{Inserted: true}, status: 204, wantCalls: 1, refresh: 1},
		{name: "identical duplicate", recordRes: store.RecordRunLeadConfigResult{Inserted: false}, status: 204, wantCalls: 1},
		{name: "not owned", ownedErr: pgx.ErrNoRows, status: 404},
		{name: "claim not current", recordErr: store.ErrLeadConfigClaimNotCurrent, status: 409, reason: "claim_not_current", wantCalls: 1},
		{name: "claim not current, wrapped", recordErr: fmt.Errorf("x: %w", store.ErrLeadConfigClaimNotCurrent), status: 409, reason: "claim_not_current", wantCalls: 1},
		{name: "key conflict", recordErr: store.ErrLeadConfigKeyConflict, status: 409, reason: "lead_config_key_conflict", wantCalls: 1},
		{
			name: "requested_model check maps to 400", status: 400, reason: "invalid_lead_config", field: "requested_model", wantCalls: 1,
			recordErr: fmt.Errorf("insert run lead config: %w", &pgconn.PgError{Code: "23514", ConstraintName: "run_lead_configs_requested_model_check", Detail: "Failing row contains (leak-detail)"}),
		},
		{name: "other check constraint stays 500", status: 500, wantCalls: 1,
			recordErr: &pgconn.PgError{Code: "23514", ConstraintName: "some_other_check"}},
		{name: "invariant breach is 500", recordErr: store.ErrLeadConfigInvariant, status: 500, wantCalls: 1},
		{name: "generic error is 500", recordErr: errors.New("boom leak-err"), status: 500, wantCalls: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runID := uuid.New()
			st := &leadConfigStore{ownedErr: c.ownedErr, recordRes: c.recordRes, recordErr: c.recordErr}
			h, sub, slack := leadConfigHandler(t, st, runID)
			rec := leadConfigPost(h, runID, leadConfigBody(t, nil))
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.status, rec.Body.String())
			}
			if c.status == 204 && rec.Body.Len() != 0 {
				t.Errorf("204 carries a body %q", rec.Body.String())
			}
			if c.reason != "" {
				if got := decodeMap(t, rec)["reason"]; got != c.reason {
					t.Errorf("reason = %v, want %s", got, c.reason)
				}
			}
			if c.field != "" && !strings.Contains(rec.Body.String(), c.field) {
				t.Errorf("body %q must name field %q", rec.Body.String(), c.field)
			}
			for _, leak := range []string{"leak-detail", "leak-err"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("body %q leaks %q", rec.Body.String(), leak)
				}
			}
			if st.recordCalls != c.wantCalls {
				t.Errorf("store calls = %d, want %d", st.recordCalls, c.wantCalls)
			}
			frames := drainFrames(sub)
			if len(frames) != c.refresh {
				t.Fatalf("hub frames = %v, want %d refresh frame(s)", frames, c.refresh)
			}
			for _, f := range frames {
				var ev map[string]any
				if err := json.Unmarshal([]byte(f), &ev); err != nil || ev["type"] != "refresh" || len(ev) != 1 {
					t.Errorf("frame %s must be a data-less refresh frame", f)
				}
			}
			if slack.calls != 0 {
				t.Errorf("the Slack-side broadcaster saw %d calls, want 0", slack.calls)
			}
		})
	}
}

func TestLeadConfigStoreReceivesFencedKey(t *testing.T) {
	runID := uuid.New()
	st := &leadConfigStore{recordRes: store.RecordRunLeadConfigResult{Inserted: true}}
	h, _, _ := leadConfigHandler(t, st, runID)
	rec := httptest.NewRecorder()
	req := workerReq(http.MethodPost, leadConfigBody(t, map[string]any{"claim_generation": 3, "seq": 2, "phase": "implement"}), runID)
	h.WorkerRecordLeadConfig(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	a := st.recordArg
	if a.RunID != runID || a.ClaimGeneration != 3 || a.Seq != 2 || a.Phase != "implement" || a.WorkerID == uuid.Nil {
		t.Fatalf("fence params = %+v", a)
	}
}

func TestLeadConfigOneRefreshPerInsertAcrossRequests(t *testing.T) {
	runID := uuid.New()
	st := &leadConfigStore{recordRes: store.RecordRunLeadConfigResult{Inserted: true}}
	h, sub, slack := leadConfigHandler(t, st, runID)
	leadConfigPost(h, runID, leadConfigBody(t, nil))
	st.recordRes = store.RecordRunLeadConfigResult{Inserted: false} // identical retry
	leadConfigPost(h, runID, leadConfigBody(t, nil))
	st.recordRes, st.recordErr = store.RecordRunLeadConfigResult{}, store.ErrLeadConfigKeyConflict
	leadConfigPost(h, runID, leadConfigBody(t, map[string]any{"requested_model": "sonnet"}))
	st.recordErr = store.ErrLeadConfigClaimNotCurrent
	leadConfigPost(h, runID, leadConfigBody(t, nil))
	st.recordErr = errors.New("db down")
	leadConfigPost(h, runID, leadConfigBody(t, nil))
	st.recordRes, st.recordErr = store.RecordRunLeadConfigResult{Inserted: true}, nil
	leadConfigPost(h, runID, leadConfigBody(t, map[string]any{"seq": 2}))
	if f := drainFrames(sub); len(f) != 2 {
		t.Fatalf("hub frames = %v, want exactly 2 (one per insert)", f)
	}
	if slack.calls != 0 {
		t.Fatalf("Slack-side broadcaster saw %d calls, want 0", slack.calls)
	}
}

func TestLeadConfigNilHubRecordsWithoutPanic(t *testing.T) {
	st := &leadConfigStore{recordRes: store.RecordRunLeadConfigResult{Inserted: true}}
	h := newProtocolHandler(t, st) // no hub, as most handler unit tests build it
	if h.hub != nil {
		t.Fatal("test premise: handler must have a nil hub")
	}
	rec := leadConfigPost(h, uuid.New(), leadConfigBody(t, nil))
	if rec.Code != http.StatusNoContent || st.recordCalls != 1 {
		t.Fatalf("status = %d calls = %d, want 204 and 1 (%s)", rec.Code, st.recordCalls, rec.Body.String())
	}
}

func TestLeadConfigNoWorkerIs401(t *testing.T) {
	h := newProtocolHandler(t, &leadConfigStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/worker/x", strings.NewReader(leadConfigBody(t, nil)))
	h.WorkerRecordLeadConfig(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
