package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type terminalSnapshotStore struct {
	protocolStore
	row store.TerminalRejectionCustodySnapshotRow
	arg store.TerminalRejectionCustodySnapshotParams
}

func (s *terminalSnapshotStore) TerminalRejectionCustodySnapshot(_ context.Context, arg store.TerminalRejectionCustodySnapshotParams) (store.TerminalRejectionCustodySnapshotRow, error) {
	s.arg = arg
	return s.row, nil
}

func TestTerminalRejectionValidationHTTP(t *testing.T) {
	id := uuid.New().String()
	valid := `{"run_id":"` + id + `","claim_generation":1,"reason":"mac_failure"}`
	for _, body := range []string{
		`{"rejections":[` + valid + `,{"run_id":"bad","claim_generation":1,"reason":"mac_failure"}]}`,
		`{"rejections":[{"run_id":"` + id + `","reason":"mac_failure"}]}`,
		`{"rejections":[{"run_id":"` + id + `","claim_generation":-1,"reason":"mac_failure"}]}`,
		`{"rejections":[{"run_id":"` + id + `","claim_generation":9007199254740992,"reason":"mac_failure"}]}`,
		`{"rejections":[{"run_id":"` + id + `","claim_generation":1.5,"reason":"mac_failure"}]}`,
		`{"rejections":[{"run_id":"` + id + `","claim_generation":1,"reason":"other"}]}`,
		`{"rejections":[` + valid + `],"extra":1}`,
		`{"rejections":null}`,
		`{"rejections":[]} {}`,
		`{"rejections":[` + strings.TrimSuffix(strings.Repeat(valid+",", 257), ",") + `]}`,
		`{"rejections":[]}` + strings.Repeat(" ", 128<<10),
	} {
		h := newProtocolHandler(t, &protocolStore{})
		rec := httptest.NewRecorder()
		h.WorkerTerminalRejections(rec, workerReq(http.MethodPost, body, uuid.Nil))
		if rec.Code != 400 {
			t.Fatalf("status = %d for %.200s", rec.Code, body)
		}
	}
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerTerminalRejections(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"rejections":[]}`)))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated POST = %d", rec.Code)
	}
}

func TestTerminalRejectionCustodyHTTP(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		exact, sibling, open, unsettled int64
		outcome                         string
	}{
		{"absent", 0, 0, 0, 0, "unknown"}, {"released", 1, 0, 0, 0, "settled"},
		{"discarded duplicate", 2, 0, 0, 0, "settled"}, {"open", 2, 0, 1, 1, "retained"},
		{"ambiguous sibling", 1, 1, 0, 0, "unknown"}, {"exact cap", 257, 0, 0, 0, "unknown"},
		{"sibling cap", 1, 257, 0, 0, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &terminalSnapshotStore{row: store.TerminalRejectionCustodySnapshotRow{
				ExactCount: tc.exact, SiblingCount: tc.sibling, OpenCount: tc.open, UnsettledCount: tc.unsettled,
				ExactHolds: []byte("[]"), SiblingHolds: []byte("[]"),
			}}
			h := newProtocolHandler(t, st)
			req := workerReq(http.MethodGet, "", uuid.New())
			req.URL.RawQuery = "generation=1"
			rec := httptest.NewRecorder()
			h.WorkerTerminalRejectionCustody(rec, req)
			if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
			}
			var out workersvc.TerminalRejectionCustody
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Outcome != tc.outcome || out.Complete != (tc.exact <= 256 && tc.sibling <= 256) {
				t.Fatalf("response %+v", out)
			}
			if st.arg.Generation != 1 || st.arg.WorkerID == uuid.Nil || st.arg.UserID == uuid.Nil {
				t.Fatalf("scope %+v", st.arg)
			}
		})
	}
}

func TestTerminalRejectionRegisterIndependent(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		h := newProtocolHandler(t, &protocolStore{})
		h.cfg.ActiveSnapshotDisabled = disabled
		rec := httptest.NewRecorder()
		h.WorkerRegister(rec, workerReq(http.MethodPost, "{}", uuid.Nil))
		if rec.Code != 200 {
			t.Fatalf("register %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Features []string `json:"protocol_features"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, feature := range out.Features {
			if feature == "terminal_rejection_report" {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("features %v", out.Features)
		}
	}
}
