package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestStatusCellHibernation pins the PRD #4205 precedence: hibernation state wins over
// offline, cordon and quarantine, and the wake_error text never reaches the cell.
func TestStatusCellHibernation(t *testing.T) {
	ts := time.Now()
	werr := "pod \x1b[31mnever became ready"
	for _, tc := range []struct {
		name string
		w    apitypes.WorkerDTO
		want string
	}{
		{"none passes through", apitypes.WorkerDTO{Status: "online", HibernationState: "none"}, "online"},
		{"preparing", apitypes.WorkerDTO{Status: "online", HibernationState: "preparing"}, "hibernating"},
		{"hibernated beats offline", apitypes.WorkerDTO{Status: "offline", HibernationState: "hibernated"}, "hibernated"},
		{"hibernated beats cordon", apitypes.WorkerDTO{Status: "offline", DrainingSince: &ts, EffectiveCordoned: true, HibernationState: "hibernated"}, "hibernated"},
		{"waking", apitypes.WorkerDTO{Status: "offline", HibernationState: "waking"}, "waking"},
		{"waking with wake error", apitypes.WorkerDTO{Status: "offline", HibernationState: "waking", WakeError: &werr}, "waking (wake error)"},
		{"empty wake error ignored", apitypes.WorkerDTO{Status: "offline", HibernationState: "waking", WakeError: strPtrT("")}, "waking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusCell(tc.w); got != tc.want {
				t.Errorf("statusCell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkerStateHibernationPrecedence(t *testing.T) {
	now := time.Now()
	for state, want := range map[string]string{"preparing": "hibernating", "hibernated": "hibernated", "waking": "waking"} {
		w := apitypes.WorkerDTO{Status: "offline", DrainingSince: &now, Busy: true, HibernationState: state}
		if got := workerState(workerRow{w: w}); got != want {
			t.Errorf("workerState(%s) = %q, want %q", state, got, want)
		}
	}
}

func TestTUIWorkersHibernation(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.splitMode = "off"
	m = press(t, m, "2")
	rows := []workerRow{
		{w: apitypes.WorkerDTO{ID: "a", Name: "sleeper", Status: "offline", HibernationState: "hibernated"}},
		{w: apitypes.WorkerDTO{ID: "b", Name: "napper", Status: "offline", HibernationState: "waking", WakeError: sp("bad \x1b[31mwake \u202eevil")}},
		{w: apitypes.WorkerDTO{ID: "c", Name: "awake", Status: "online"}},
	}
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	frame := stripANSI(m.View().Content)
	for _, want := range []string{"hibernated", "waking", "idle"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame missing %q\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "offline") {
		t.Errorf("hibernating worker rendered as offline\n%s", frame)
	}
	raw := m.View().Content
	if strings.Contains(raw, "\x1b[31mwake") || strings.Contains(raw, "\u202e") {
		t.Error("wake_error reached the frame unsanitized")
	}
	for i, line := range strings.Split(frame, "\n") {
		if visualWidth(line) > m.width {
			t.Errorf("line %d width=%d > %d", i, visualWidth(line), m.width)
		}
	}
}

// The worker DETAIL view draws wake_error through renderer.Plain; hostile control bytes
// and bidi overrides must not reach it.
func TestTUIWorkerDetailWakeErrorSanitized(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 200, 70
	w := apitypes.WorkerDTO{ID: "b", Name: "napper", Status: "offline", HibernationState: "waking", WakeError: sp("bad \x1b[31mwake \u202eevil \x1b]0;pwn\x07")}
	m.workers.rows = []workerRow{{w: w}}
	m.workerDetail = workerDetailState{workerID: w.ID}
	lines, _ := m.workerDetailLines(time.Now())
	out := strings.Join(lines, "\n")
	if !strings.Contains(stripANSI(out), "wake error") {
		t.Fatalf("detail lacks the wake error line:\n%s", out)
	}
	if strings.Contains(out, "\x1b]") || strings.Contains(out, "\u202e") || strings.Contains(out, "\x07") || strings.Contains(out, "\x1b[31mwake") {
		t.Fatalf("wake_error reached the detail view unsanitized:\n%q", out)
	}
}

func TestWorkerIntakeLabelHibernating(t *testing.T) {
	for _, state := range []string{"preparing", "hibernated", "waking"} {
		w := apitypes.WorkerDTO{HibernationState: state, EffectiveCordoned: true}
		if got := workerIntakeLabel(w); got != "" {
			t.Errorf("%s: intake label %q, want empty", state, got)
		}
	}
	now := time.Now()
	w := apitypes.WorkerDTO{HibernationState: "hibernated", ManualCordonedAt: &now}
	if got := workerIntakeLabel(w); got != "cordoned (manual)" {
		t.Errorf("manual cordon lost: %q", got)
	}
}
