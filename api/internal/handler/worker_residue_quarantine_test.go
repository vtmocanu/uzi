package handler

// Issue #2213: handler coverage for the heartbeat's `residue_quarantine` member. The
// fixture under fixtures/worker-heartbeat-residue-quarantine is the shared seam with the
// agent: its test asserts the body the agent builds, and TestHeartbeatFixtureOverlaysQuarantine
// feeds the same bytes through the REAL strict heartbeat decoder (WorkerHeartbeat) and asserts
// the overlaid DTO fields.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const quarantineFixturePath = "../../../fixtures/worker-heartbeat-residue-quarantine/latched.json"

func readQuarantineFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(quarantineFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// heartbeatAs posts body as worker id and returns the decoded worker DTO's two fields.
func heartbeatAs(t *testing.T, h *Handler, id uuid.UUID, body string) (at, cause *string, code int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", strings.NewReader(body))
	req = req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: id, UserID: uuid.New()}))
	rec := httptest.NewRecorder()
	h.WorkerHeartbeat(rec, req)
	if rec.Code != http.StatusOK {
		return nil, nil, rec.Code
	}
	var resp struct {
		Worker struct {
			At    *string `json:"residue_quarantined_at"`
			Cause *string `json:"residue_quarantine_cause"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode heartbeat response: %v (%s)", err, rec.Body.String())
	}
	return resp.Worker.At, resp.Worker.Cause, rec.Code
}

func TestHeartbeatFixtureOverlaysQuarantine(t *testing.T) {
	raw := readQuarantineFixture(t)
	var fx struct {
		Q struct {
			Cause string `json:"cause"`
			RunID string `json:"run_id"`
			Site  string `json:"site"`
		} `json:"residue_quarantine"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	h := newProtocolHandler(t, &protocolStore{})
	id := uuid.New()
	at, cause, code := heartbeatAs(t, h, id, string(raw))
	if code != http.StatusOK {
		t.Fatalf("the strict heartbeat decoder rejected the fixture: %d", code)
	}
	if at == nil || *at != "2026-10-06T16:42:57Z" {
		t.Fatalf("residue_quarantined_at = %v, want 2026-10-06T16:42:57Z", at)
	}
	if cause == nil || *cause != fx.Q.Cause {
		t.Fatalf("residue_quarantine_cause = %v, want %q", cause, fx.Q.Cause)
	}
	got, ok := h.wsvc.ResidueQuarantineFor(id)
	if !ok || got.RunID == nil || got.RunID.String() != fx.Q.RunID || got.Site != fx.Q.Site {
		t.Fatalf("tracker entry = %+v ok=%v", got, ok)
	}
	// A heartbeat that omits the member clears the latch (control: the overlay is the member, not a constant).
	at, cause, _ = heartbeatAs(t, h, id, `{"version":"1"}`)
	if at != nil || cause != nil {
		t.Fatalf("heartbeat without the member must clear: at=%v cause=%v", at, cause)
	}
	if _, ok := h.wsvc.ResidueQuarantineFor(id); ok {
		t.Fatal("tracker still holds the latch after a heartbeat without the member")
	}
}

func TestHeartbeatQuarantineCauseIsSanitizedAndBounded(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	hostile := "pid 1 \x1b[31mred\x1b]0;title\x07 \u202eevil\nline2" + strings.Repeat("x", 1000)
	body, _ := json.Marshal(map[string]any{"residue_quarantine": map[string]any{
		"cause": hostile, "latched_at": "2026-10-06T16:42:57Z", "run_id": nil,
		"site": "pre\x1b[0m_clone\u202e" + strings.Repeat("s", 500),
	}})
	id := uuid.New()
	_, cause, _ := heartbeatAs(t, h, id, string(body))
	if cause == nil {
		t.Fatal("a valid member with a hostile cause must still latch")
	}
	if len(*cause) > 200 {
		t.Fatalf("cause is %d bytes, want <= 200", len(*cause))
	}
	for _, r := range *cause {
		if r < 0x20 || r == 0x7f || r == 0x202e {
			t.Fatalf("cause %q carries control or bidi rune %U", *cause, r)
		}
	}
	got, _ := h.wsvc.ResidueQuarantineFor(id)
	if got.RunID != nil {
		t.Fatalf("null run_id must stay nil, got %v", got.RunID)
	}
	if len(got.Site) > 64 || strings.ContainsAny(got.Site, "\x1b\u202e") {
		t.Fatalf("site %q not sanitized/bounded", got.Site)
	}
}

func TestHeartbeatInvalidQuarantineMemberDropsAndClears(t *testing.T) {
	for name, member := range map[string]string{
		"not an object":    `"latched"`,
		"oversized member": `{"cause":"` + strings.Repeat("a", 5000) + `","latched_at":"2026-10-06T16:42:57Z","run_id":null,"site":"s"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newProtocolHandler(t, &protocolStore{})
			id := uuid.New()
			h.wsvc.RecordResidueQuarantine(id, &workersvc.ResidueQuarantine{Cause: "old", LatchedAt: time.Now()})
			at, cause, code := heartbeatAs(t, h, id, `{"residue_quarantine":`+member+`}`)
			if code != http.StatusOK {
				t.Fatalf("an invalid member must never fail liveness, got %d", code)
			}
			if at != nil || cause != nil {
				t.Fatalf("invalid member must read as not latched: at=%v cause=%v", at, cause)
			}
		})
	}
}

// A member that proves a latch exists but has a bad field keeps the latch (visibility
// must not fail open): a bad or missing latched_at takes the api receive time, a bad
// run_id drops only run_id.
func TestParseQuarantineBadFieldsKeepLatch(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		member string
		wantAt time.Time
		wantID bool
	}{
		"bad latched_at": {`{"cause":"c","latched_at":"yesterday","run_id":null,"site":"s"}`, now, false},
		"missing latch":  {`{"cause":"c","run_id":null,"site":"s"}`, now, false},
		"bad run_id":     {`{"cause":"c","latched_at":"2026-10-06T10:00:00Z","run_id":"not-a-uuid","site":"s"}`, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), false},
		"good run_id":    {`{"cause":"c","latched_at":"2026-10-06T10:00:00Z","run_id":"` + uuid.Nil.String() + `","site":"s"}`, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), true},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseWorkerResidueQuarantine(json.RawMessage(tc.member), uuid.New(), now)
			if got == nil {
				t.Fatal("latch must be kept")
			}
			if !got.LatchedAt.Equal(tc.wantAt) || got.Cause != "c" || (got.RunID != nil) != tc.wantID {
				t.Fatalf("got %+v, want at=%v runID=%v", got, tc.wantAt, tc.wantID)
			}
		})
	}
}

func TestHeartbeatQuarantineFutureLatchIsClamped(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	got := parseWorkerResidueQuarantine(json.RawMessage(`{"cause":"c","latched_at":"2099-01-01T00:00:00Z","run_id":null,"site":"s"}`), uuid.New(), now)
	if got == nil || !got.LatchedAt.Equal(now) {
		t.Fatalf("future latched_at = %+v, want clamped to %v", got, now)
	}
}

func TestRegisterClearsQuarantine(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	id := uuid.New()
	h.wsvc.RecordResidueQuarantine(id, &workersvc.ResidueQuarantine{Cause: "old", LatchedAt: time.Now()})
	req := httptest.NewRequest(http.MethodPost, "/api/worker/register", strings.NewReader(`{"name":"laptop","version":"1"}`))
	req = req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: id, UserID: uuid.New()}))
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := h.wsvc.ResidueQuarantineFor(id); ok {
		t.Fatal("register must clear a latch from the previous process")
	}
}

func TestRegisterAdvertisesQuarantineFeature(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1"}`, uuid.Nil))
	if !strings.Contains(rec.Body.String(), `"worker_residue_quarantine"`) {
		t.Fatalf("register response lacks the worker_residue_quarantine feature: %s", rec.Body.String())
	}
}

// TestWorkerSurfacesOverlayQuarantine covers the user list, the admin list and the PATCH
// response. The heartbeat response is covered by TestHeartbeatFixtureOverlaysQuarantine
// and the register response (a fresh process, never latched) by TestRegisterClearsQuarantine.
func TestWorkerSurfacesOverlayQuarantine(t *testing.T) {
	id, other := uuid.New(), uuid.New()
	q := &workersvc.ResidueQuarantine{Cause: "unattributed pid 7", LatchedAt: time.Date(2026, 10, 6, 16, 42, 57, 0, time.UTC)}
	check := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		var body struct {
			Workers []map[string]json.RawMessage `json:"workers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		rows := body.Workers
		if len(rows) != 2 {
			t.Fatalf("rows = %s", rec.Body.String())
		}
		if string(rows[0]["residue_quarantined_at"]) != `"2026-10-06T16:42:57Z"` || string(rows[0]["residue_quarantine_cause"]) != `"unattributed pid 7"` {
			t.Fatalf("latched row = %v", rows[0])
		}
		if string(rows[1]["residue_quarantined_at"]) != "null" || string(rows[1]["residue_quarantine_cause"]) != "null" {
			t.Fatalf("clean row = %v", rows[1])
		}
	}
	withUser := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/api/workers", nil).WithContext(mw.ContextWithUser(context.Background(), store.User{ID: uuid.New()}))
	}
	t.Run("user list", func(t *testing.T) {
		st := &custodyListStore{ownerRows: []store.ListWorkersByUserRow{{ID: id}, {ID: other}}}
		h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}
		h.wsvc.RecordResidueQuarantine(id, q)
		rec := httptest.NewRecorder()
		h.ListWorkers(rec, withUser())
		check(t, rec)
	})
	t.Run("admin list", func(t *testing.T) {
		st := &custodyListStore{adminRows: []store.ListAllWorkersRow{{Worker: store.Worker{ID: id}}, {Worker: store.Worker{ID: other}}}}
		h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}
		h.wsvc.RecordResidueQuarantine(id, q)
		rec := httptest.NewRecorder()
		h.AdminListWorkers(rec, withUser())
		check(t, rec)
	})
	t.Run("patch", func(t *testing.T) {
		owner := store.User{ID: uuid.New(), IsActive: true}
		h := newBindHandler(t, &bindStore{})
		h.wsvc.RecordResidueQuarantine(id, q)
		rec := httptest.NewRecorder()
		h.PatchWorker(rec, patchWorkerReq(t, owner, id, `{"anthropic_token":null}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Worker map[string]json.RawMessage `json:"worker"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if string(env.Worker["residue_quarantined_at"]) != `"2026-10-06T16:42:57Z"` {
			t.Fatalf("patch response = %v", env.Worker)
		}
	})
}
