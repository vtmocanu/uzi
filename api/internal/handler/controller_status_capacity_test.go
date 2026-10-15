package handler

import (
	"encoding/json"

	"fmt"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCapacityRawCurrentRowRequired(t *testing.T) {
	var ws hostedsvc.WorkerStatus
	valid := `{"phase":"stuck","pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu","blocking_kind":"scheduler_capacity"}`
	if err := json.Unmarshal([]byte(valid), &ws); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, "null",
		`{"phase":"stuck","pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu"}`,
		`{"phase":"stuck","pod_phase":"Pending","blocking_kind":"scheduler_capacity"}`,
		`{"phase":"stuck","blocking_reason":"scheduler capacity: insufficient cpu","blocking_kind":"scheduler_capacity"}`,
		`{"pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu","blocking_kind":"scheduler_capacity"}`,
	} {
		if got := rawBlockingKind(ws, json.RawMessage(raw)); got != "" {
			t.Fatalf("inherited capacity for %s: %s", raw, got)
		}
	}
	if got := rawBlockingKind(ws, json.RawMessage(valid)); got != "scheduler_capacity" {
		t.Fatalf("valid current row = %q", got)
	}
}

func TestCapacityDuplicateWorkerArraysCurrentRow(t *testing.T) {
	first := `{"id":"11111111-1111-1111-1111-111111111111","phase":"stuck","pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu","blocking_kind":"scheduler_capacity"}`
	sibling := strings.Replace(first, "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", 1)
	for _, later := range []string{`{}`, "null",
		`{"phase":"stuck","pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu","blocking_kind":"scheduler_capacity"}`,
		`{"id":"11111111-1111-1111-1111-111111111111","phase":"stuck","pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu"}`,
		`{"id":"11111111-1111-1111-1111-111111111111","phase":"stuck","pod_phase":"Pending","blocking_kind":"scheduler_capacity"}`,
		`{"id":"11111111-1111-1111-1111-111111111111","phase":"rolling","pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu"}`,
	} {
		t.Run(later, func(t *testing.T) {
			st := &statusStore{rows: 1}
			h := newProtocolHandler(t, st)
			body := `{"workers":[` + first + `],"workers":[` + later + "," + sibling + "]}"
			rec := httptest.NewRecorder()
			h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
			var current hostedsvc.WorkerStatus
			if err := json.Unmarshal([]byte(later), &current); err != nil {
				t.Fatal(err)
			}
			if current.ID == "" {
				if rec.Code != 204 || len(st.upserts) != 1 || st.upserts[0].WorkerID.String() != "22222222-2222-2222-2222-222222222222" || !st.upserts[0].BlockingKind.Valid {
					t.Fatalf("invalid identity affected valid sibling: status=%d rows=%+v", rec.Code, st.upserts)
				}
				return
			}
			if rec.Code != 204 || len(st.upserts) != 2 {
				t.Fatalf("status=%d rows=%d", rec.Code, len(st.upserts))
			}
			wantPhase := "stuck"
			if strings.Contains(later, "rolling") {
				wantPhase = "rolling"
			}
			if st.upserts[0].BlockingKind.Valid || st.upserts[0].Phase != wantPhase ||
				!st.upserts[1].BlockingKind.Valid || st.upserts[1].Phase != "stuck" {
				t.Fatalf("current row/sibling = %+v", st.upserts)
			}
		})
	}
}

func TestControllerStatusCapacityRawFallbackAndSiblings(t *testing.T) {
	for _, cause := range []string{
		`"scheduler_capacity"`, `42`, `{}`, `[]`, `null`, `"unknown"`, `"scheduler_\u202ecapacity"`,
	} {
		t.Run(cause, func(t *testing.T) {
			st := &statusStore{rows: 1}
			h := newProtocolHandler(t, st)
			now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
			h.now = func() time.Time { return now }
			body := `{"workers":[{"id":"11111111-1111-1111-1111-111111111111","phase":"stuck","pod_phase":"Pending","blocking_reason":"scheduler capacity: insufficient cpu","blocking_kind":` + cause +
				`},{"id":"22222222-2222-2222-2222-222222222222","phase":"rolling"}]}`
			rec := httptest.NewRecorder()
			h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
			if rec.Code != 204 || len(st.upserts) != 2 {
				t.Fatalf("status=%d rows=%d body=%s", rec.Code, len(st.upserts), rec.Body.String())
			}
			if got := st.upserts[0]; got.BlockingKind.Valid != (cause == `"scheduler_capacity"`) || got.Phase != "stuck" || !got.ObservedAt.Time.Equal(now) {
				t.Fatalf("raw fallback/freshness = %+v", got)
			}
			if st.upserts[1].BlockingKind.Valid || st.upserts[1].Phase != "rolling" {
				t.Fatal("sibling lost")
			}
		})
	}
}

func TestControllerStatusCapacityDuplicatesAndRawDiagnostics(t *testing.T) {
	fields := []struct{ key, good, bad string }{
		{"blocking_kind", `"scheduler_capacity"`, `"unknown"`},
		{"phase", `"stuck"`, `"rolling"`},
		{"pod_phase", `"Pending"`, `"Running"`},
		{"blocking_reason", `"scheduler capacity: insufficient cpu"`, `"ImagePullBackOff"`},
		{"blocking_container", `null`, `"worker"`},
		{"restart_count", `0`, `1`},
		{"last_exit_code", `null`, `0`},
	}
	for _, field := range fields {
		for _, reverse := range []bool{false, true} {
			for _, folded := range []string{field.key, strings.ToUpper(field.key), strings.ReplaceAll(field.key, "s", "\u017f")} {
				t.Run(field.key+fmt.Sprint(reverse, folded), func(t *testing.T) {
					values := make([]string, 0, len(fields)+1)
					for _, f := range fields {
						if f.key == field.key {
							continue
						}
						values = append(values, `"`+f.key+`":`+f.good)
					}
					first, second := field.good, field.bad
					if reverse {
						first, second = second, first
					}
					key := folded
					values = append(values, `"`+field.key+`":`+first, `"`+key+`":`+second)
					st := &statusStore{rows: 1}
					h := newProtocolHandler(t, st)
					body := `{"workers":[{"id":"11111111-1111-1111-1111-111111111111",` + strings.Join(values, ",") + `},{"id":"22222222-2222-2222-2222-222222222222","phase":"settled"}]}`
					rec := httptest.NewRecorder()
					h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
					if rec.Code != 204 || len(st.upserts) != 2 {
						t.Fatalf("status=%d rows=%d", rec.Code, len(st.upserts))
					}
					if st.upserts[0].BlockingKind.Valid {
						t.Fatal("duplicate cause survived")
					}
					wantPhase := "stuck"
					if field.key == "phase" && !reverse {
						wantPhase = "rolling"
					}
					if st.upserts[0].Phase != wantPhase || st.upserts[1].Phase != "settled" {
						t.Fatal("ordinary phase/sibling changed")
					}
				})
			}
		}
	}
	for _, field := range []string{
		`"blocking_reason":"scheduler capacity: insufficient c\u202epu"`,
		`"pod_phase":"Pen\u202eding"`,
		`"blocking_container":"\u202e"`,
	} {
		st := &statusStore{rows: 1}
		h := newProtocolHandler(t, st)
		// The last value is what ordinary JSON decoding reads, but the duplicate is
		// deliberately avoided here to exercise original unsanitized diagnostics.
		body := `{"workers":[{"id":"11111111-1111-1111-1111-111111111111","phase":"stuck","blocking_kind":"scheduler_capacity",`
		if !strings.HasPrefix(field, `"pod_phase"`) {
			body += `"pod_phase":"Pending",`
		}
		if !strings.HasPrefix(field, `"blocking_reason"`) {
			body += `"blocking_reason":"scheduler capacity: insufficient cpu",`
		}
		body += field + `}]}`
		rec := httptest.NewRecorder()
		h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
		if rec.Code != 204 || len(st.upserts) != 1 || st.upserts[0].BlockingKind.Valid {
			t.Fatalf("raw diagnostic accepted: %s", field)
		}
	}
}
