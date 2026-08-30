package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestTriggerSourceThreadedByCreateRunFamily pins issue #857 M2: each of the four
// createRun-family public entrypoints threads the correct trigger_source LITERAL into
// store.CreateRunParams. This is the fast (no-DB) tier — it uses the same fakeStore
// harness the other create tests use (f.createRunParams captures the exact
// CreateRunParams the entrypoint built), so it proves the Go-side threading
// independently of the DB. The DB-stamped value for the CreateRun SQL query itself is
// proved separately by TestTriggerSourceStampedLiveDB.
//
// The four entrypoints and their expected stamp (per the shared createRun signature in
// service.go): CreateRun→manual, CreateScheduledRun→schedule, CreateAutopilotRun and
// CreateScheduledAutopilotRun→autopilot. A regression that dropped or crossed a literal
// (e.g. a scheduled run stamped "manual") is caught here at the param boundary.
func TestTriggerSourceThreadedByCreateRunFamily(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	ctx := context.Background()

	// newFS returns a fresh fakeStore staged for a successful createRun: a uzi-labelled
	// (eligible) issue and a non-nil createRunResult so createRun returns cleanly.
	newFS := func() *fakeStore {
		return &fakeStore{
			issueByID:       store.Issue{Title: "T", Labels: uziLabels()},
			createRunResult: store.Run{ID: uuid.New()},
		}
	}

	assertStamp := func(t *testing.T, fs *fakeStore, want string) {
		t.Helper()
		if fs.createRunParams == nil {
			t.Fatal("CreateRun was not called on the store")
		}
		if got := fs.createRunParams.TriggerSource; got != want {
			t.Fatalf("TriggerSource = %q, want %q", got, want)
		}
	}

	t.Run("CreateRun stamps manual", func(t *testing.T) {
		fs := newFS()
		if _, err := New(fs, newBox(t), testParams()).CreateRun(ctx, user, repo, 1, "desc", nil, nil, nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		assertStamp(t, fs, "manual")
	})

	t.Run("CreateScheduledRun stamps schedule", func(t *testing.T) {
		fs := newFS()
		if _, err := New(fs, newBox(t), testParams()).CreateScheduledRun(ctx, user, repo, 1, "desc", nil, nil, nil, false, nil); err != nil {
			t.Fatalf("CreateScheduledRun: %v", err)
		}
		assertStamp(t, fs, "schedule")
	})

	t.Run("CreateAutopilotRun stamps autopilot", func(t *testing.T) {
		fs := newFS()
		if _, err := New(fs, newBox(t), testParams()).CreateAutopilotRun(ctx, user, repo, 1, "desc"); err != nil {
			t.Fatalf("CreateAutopilotRun: %v", err)
		}
		assertStamp(t, fs, "autopilot")
	})

	t.Run("CreateScheduledAutopilotRun stamps autopilot", func(t *testing.T) {
		fs := newFS()
		if _, err := New(fs, newBox(t), testParams()).CreateScheduledAutopilotRun(ctx, user, repo, 1, "desc", nil, nil, nil, false); err != nil {
			t.Fatalf("CreateScheduledAutopilotRun: %v", err)
		}
		assertStamp(t, fs, "autopilot")
	})
}
