package workersvc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// health_long_tool_call_test.go pins issue #2046: a running run whose oldest open LEAD
// tool call has been in flight longer than health_tool_call_seconds, on a run that has
// also gone quiet for the stall window, is flagged stalled with reasonLongToolCall.
// Every case drives detectRunHealth through healthFakeStore.messages, so the lead-lane
// window, the created_at ageing and the arm's guards are exercised together.

// longCallSettings is the default shape: stall 5m, tool call 20m, near-timeout off so
// the wall-clock arm cannot interfere unless a case opts in.
func longCallSettings() fakeHealthSettings {
	return fakeHealthSettings{enabled: true, stall: 300, toolCall: 1200}
}

// lcRun is a running run started well before the cases' activity, last active `quiet` ago.
func lcRun(quiet time.Duration) store.ListActiveRunsForHealthRow {
	r := runRow("running")
	r.StartedAt = ago(3 * time.Hour)
	r.LastActivityAt = ago(quiet)
	return r
}

// lcUse is an open-or-not lead tool_use created `age` before t0.
func lcUse(t *testing.T, seq int32, id, name string, age time.Duration) fakeRunMessage {
	m := fakeMsg(t, seq, "tool_use", "", map[string]any{"id": id, "name": name, "input": map[string]any{"command": "x"}})
	m.createdAt = t0.Add(-age)
	return m
}

func lcRes(t *testing.T, seq int32, id string) fakeRunMessage {
	return fakeMsg(t, seq, "tool_result", "", map[string]any{"tool_use_id": id})
}

// lcFiller returns n completed lead pairs (2n rows) starting at seq, with no created_at.
func lcFiller(t *testing.T, seq int32, n int) ([]fakeRunMessage, int32) {
	var out []fakeRunMessage
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("filler-%d", seq)
		out = append(out,
			fakeMsg(t, seq, "tool_use", "", map[string]any{"id": id, "name": "Read", "input": map[string]any{"file_path": fmt.Sprintf("f%d.go", seq)}}),
			lcRes(t, seq+1, id))
		seq += 2
	}
	return out, seq
}

// lcDetect runs one pass and returns the written (health, reason); a pass that wrote
// nothing returns the row's current flag (an unchanged ok stays ok).
func lcDetect(t *testing.T, r store.ListActiveRunsForHealthRow, msgs []fakeRunMessage, st fakeHealthSettings) (string, string) {
	t.Helper()
	fs := &healthFakeStore{
		active:   []store.ListActiveRunsForHealthRow{r},
		messages: map[uuid.UUID][]fakeRunMessage{r.ID: msgs},
	}
	svc := healthSvc(fs, st)
	svc.detectRunHealth(context.Background(), t0)
	if len(fs.writes) == 0 {
		return r.Health, r.HealthReason.String
	}
	w := lastWrite(t, fs, r.ID)
	return w.Health, w.HealthReason.String
}

func wantHealth(t *testing.T, name, gotH, gotR, wantH, wantR string) {
	t.Helper()
	if gotH != wantH || gotR != wantR {
		t.Errorf("%s: (health, reason) = (%q, %q), want (%q, %q)", name, gotH, gotR, wantH, wantR)
	}
}

func TestHealthLongToolCallFlagsOldOpenCall(t *testing.T) {
	m := []fakeRunMessage{lcUse(t, 1, "a", "Bash", 25*time.Minute)}
	h, why := lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
	wantHealth(t, "25m open call on a 25m-quiet run", h, why, healthStalled, reasonLongToolCall)

	// The reason is the fixed string: no tool name, input or duration.
	if reasonLongToolCall != "a tool call has been in progress longer than the configured threshold" {
		t.Errorf("reasonLongToolCall = %q", reasonLongToolCall)
	}
}

func TestHealthLongToolCallYoungCallIsOK(t *testing.T) {
	m := []fakeRunMessage{lcUse(t, 1, "a", "Bash", 10*time.Minute)}
	h, why := lcDetect(t, lcRun(10*time.Minute), m, longCallSettings())
	wantHealth(t, "10m open call", h, why, healthOK, "")
}

func TestHealthLongToolCallClearsWhenResultArrives(t *testing.T) {
	r := lcRun(25 * time.Minute)
	r.Health = healthStalled
	r.HealthReason = pgconv.Text(reasonLongToolCall)
	m := []fakeRunMessage{lcUse(t, 1, "a", "Bash", 25*time.Minute), lcRes(t, 2, "a")}
	fs := &healthFakeStore{
		active:   []store.ListActiveRunsForHealthRow{r},
		messages: map[uuid.UUID][]fakeRunMessage{r.ID: m},
	}
	svc := healthSvc(fs, longCallSettings())
	svc.detectRunHealth(context.Background(), t0)
	// The call returned, so the run is no longer in flight: the quiet 25m now reads as the
	// plain stall (the result bumped nothing in this fixture), never the long-call reason.
	w := lastWrite(t, fs, r.ID)
	if w.Health == healthStalled && w.HealthReason.String == reasonLongToolCall {
		t.Fatalf("flag still long-call after the result landed: %+v", w)
	}
	if w.HealthReason.String != reasonStalled {
		t.Fatalf("reason = %q, want %q once the call completed", w.HealthReason.String, reasonStalled)
	}

	// With recent activity (the result bumps last_activity_at) the flag clears to ok.
	r2 := lcRun(time.Minute)
	r2.Health = healthStalled
	r2.HealthReason = pgconv.Text(reasonLongToolCall)
	fs2 := &healthFakeStore{
		active:   []store.ListActiveRunsForHealthRow{r2},
		messages: map[uuid.UUID][]fakeRunMessage{r2.ID: m},
	}
	healthSvc(fs2, longCallSettings()).detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs2, r2.ID); w.Health != healthOK {
		t.Fatalf("health = %q, want ok after the result", w.Health)
	}
}

func TestHealthLongToolCallOutOfOrderCompletion(t *testing.T) {
	// Old A open, newer B used and completed: the OLDEST open call (A) ages the run.
	m := []fakeRunMessage{
		lcUse(t, 1, "a", "Bash", 25*time.Minute),
		lcUse(t, 2, "b", "Bash", 7*time.Minute),
		lcRes(t, 3, "b"),
	}
	h, why := lcDetect(t, lcRun(6*time.Minute), m, longCallSettings())
	wantHealth(t, "old A open behind completed B", h, why, healthStalled, reasonLongToolCall)

	// A completed, young B open: the oldest OPEN call is now 10m old.
	m = []fakeRunMessage{
		lcUse(t, 1, "a", "Bash", 25*time.Minute),
		lcUse(t, 2, "b", "Bash", 10*time.Minute),
		lcRes(t, 3, "a"),
	}
	h, why = lcDetect(t, lcRun(10*time.Minute), m, longCallSettings())
	wantHealth(t, "A completed, young B open", h, why, healthOK, "")
}

// TestHealthLongToolCallAgesTheOldestOfSeveralOpenCalls pins "oldest", not "newest" or
// "first seen": two ordinary lead calls are open at once (concurrent Codex calls, see
// leadInFlight), one past the threshold and one well under it.
func TestHealthLongToolCallAgesTheOldestOfSeveralOpenCalls(t *testing.T) {
	m := []fakeRunMessage{
		lcUse(t, 1, "a", "Bash", 25*time.Minute),
		lcUse(t, 2, "b", "Bash", 7*time.Minute),
	}
	h, why := lcDetect(t, lcRun(7*time.Minute), m, longCallSettings())
	wantHealth(t, "old A and young B both open", h, why, healthStalled, reasonLongToolCall)
}

// TestHealthLongToolCallUnknownCreatedAtBesideAnAgedCall: a row with no created_at is
// skipped, never treated as the zero time, so it cannot mask a genuinely old open call
// beside it, in either row order.
func TestHealthLongToolCallUnknownCreatedAtBesideAnAgedCall(t *testing.T) {
	for _, unknownOlderSeq := range []bool{true, false} {
		old := lcUse(t, 1, "a", "Bash", 25*time.Minute)
		unknown := lcUse(t, 2, "b", "Bash", 0)
		unknown.createdAt = time.Time{}
		if unknownOlderSeq {
			old.seq, unknown.seq = 2, 1
		}
		h, why := lcDetect(t, lcRun(7*time.Minute), []fakeRunMessage{old, unknown}, longCallSettings())
		wantHealth(t, fmt.Sprintf("unknown created_at beside an aged call (unknownOlderSeq=%v)", unknownOlderSeq), h, why, healthStalled, reasonLongToolCall)
	}
}

func TestHealthLongToolCallIgnoresCallsBeforeALifecycleBoundary(t *testing.T) {
	for _, event := range []string{"init", "result"} {
		m := []fakeRunMessage{
			lcUse(t, 1, "orphan", "Bash", 25*time.Minute),
			fakeMsg(t, 2, "status", "", map[string]any{"event": event}),
		}
		h, why := lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
		if why == reasonLongToolCall {
			t.Errorf("%s boundary: an orphan before the boundary was aged (%q, %q)", event, h, why)
		}
		wantHealth(t, event+" boundary", h, why, healthStalled, reasonStalled)
	}
}

func TestHealthLongToolCallTailOverflow(t *testing.T) {
	// An ordinary open call older than the 40-row tail is invisible (as in leadInFlight):
	// the run reads as plain stalled, never the long-call reason.
	m := []fakeRunMessage{lcUse(t, 1, "old", "Bash", 25*time.Minute)}
	filler, _ := lcFiller(t, 2, 30)
	m = append(m, filler...)
	h, why := lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
	wantHealth(t, "open call beyond the tail", h, why, healthStalled, reasonStalled)

	// The same call inside the tail is the long call.
	filler, next := lcFiller(t, 1, 5)
	m = append(filler, lcUse(t, next, "old", "Bash", 25*time.Minute))
	h, why = lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
	wantHealth(t, "open call inside the tail", h, why, healthStalled, reasonLongToolCall)
}

func TestHealthLongToolCallSkipsOpenDelegation(t *testing.T) {
	for _, name := range []string{"Agent", "Task"} {
		m := []fakeRunMessage{lcUse(t, 1, "d", name, 25*time.Minute)}
		h, why := lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
		wantHealth(t, "old open "+name, h, why, healthOK, "")
	}
	// A delegation does not launder an ordinary old call next to it.
	m := []fakeRunMessage{
		lcUse(t, 1, "d", "Agent", 25*time.Minute),
		lcUse(t, 2, "b", "Bash", 25*time.Minute),
	}
	h, why := lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
	wantHealth(t, "old Agent plus old Bash", h, why, healthOK, "")
}

func TestHealthLongToolCallDelegationBeyondTailIsCoveredByQuietGuard(t *testing.T) {
	const parent = "parent-agent"
	build := func() []fakeRunMessage {
		m := []fakeRunMessage{lcUse(t, 1, parent, "Agent", 40*time.Minute)}
		filler, next := lcFiller(t, 2, 30) // 60 lead rows: the dispatch leaves the 40-row tail
		m = append(m, filler...)
		m = append(m, lcUse(t, next, "bash", "Bash", 25*time.Minute))
		m = append(m, fakeMsg(t, next+1, "tool_use", parent, map[string]any{"id": "nested", "name": "Read", "input": map[string]any{}}))
		return m
	}
	// Nested frames keep the run active (last activity 1m ago): never flagged.
	h, why := lcDetect(t, lcRun(time.Minute), build(), longCallSettings())
	wantHealth(t, "streaming delegation beyond the tail", h, why, healthOK, "")

	// The subagent stopped 6m ago: the quiet guard passes and the old Bash call flags.
	h, why = lcDetect(t, lcRun(6*time.Minute), build(), longCallSettings())
	wantHealth(t, "quiet delegation beyond the tail", h, why, healthStalled, reasonLongToolCall)

	// stall disabled: the quiet window falls back to the tool-call threshold (20m).
	st := fakeHealthSettings{enabled: true, stall: 0, toolCall: 1200}
	h, why = lcDetect(t, lcRun(10*time.Minute), build(), st)
	wantHealth(t, "stall=0, activity 10m ago", h, why, healthOK, "")
	h, why = lcDetect(t, lcRun(21*time.Minute), build(), st)
	wantHealth(t, "stall=0, activity 21m ago", h, why, healthStalled, reasonLongToolCall)
}

func TestHealthLongToolCallBothGuardsMustHold(t *testing.T) {
	// Age past the threshold but the run is not yet quiet for the stall window.
	m := []fakeRunMessage{lcUse(t, 1, "a", "Bash", 25*time.Minute)}
	h, why := lcDetect(t, lcRun(3*time.Minute), m, longCallSettings())
	wantHealth(t, "old call, recent activity", h, why, healthOK, "")

	// Quiet past the stall window but the call is younger than the threshold.
	m = []fakeRunMessage{lcUse(t, 1, "a", "Bash", 10*time.Minute)}
	h, why = lcDetect(t, lcRun(6*time.Minute), m, longCallSettings())
	wantHealth(t, "young call, quiet run", h, why, healthOK, "")
}

func TestHealthLongToolCallEmptyIDIsNeverAged(t *testing.T) {
	m := []fakeRunMessage{lcUse(t, 1, "", "Bash", 25*time.Minute)}
	h, why := lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
	if why == reasonLongToolCall {
		t.Fatalf("an empty-id open use was aged: (%q, %q)", h, why)
	}
	wantHealth(t, "empty id", h, why, healthStalled, reasonStalled)
}

func TestHealthLongToolCallUnknownCreatedAtIsNeverAged(t *testing.T) {
	m := []fakeRunMessage{lcUse(t, 1, "a", "Bash", 0)}
	m[0].createdAt = time.Time{}
	h, why := lcDetect(t, lcRun(25*time.Minute), m, longCallSettings())
	wantHealth(t, "invalid created_at", h, why, healthOK, "")
}

func TestHealthLongToolCallZeroThresholdDisables(t *testing.T) {
	m := []fakeRunMessage{lcUse(t, 1, "a", "Bash", 25*time.Minute)}
	st := fakeHealthSettings{enabled: true, stall: 300, toolCall: 0}
	h, why := lcDetect(t, lcRun(25*time.Minute), m, st)
	wantHealth(t, "toolCall=0", h, why, healthOK, "")
}

func TestHealthLongToolCallOutboxQueuedForOwner(t *testing.T) {
	r := lcRun(25 * time.Minute)
	w := uuid.New()
	r.WorkerID = pgconv.UUID(w)
	fs := &healthFakeStore{
		active:   []store.ListActiveRunsForHealthRow{r},
		messages: map[uuid.UUID][]fakeRunMessage{r.ID: {lcUse(t, 1, "a", "Bash", 25*time.Minute)}},
	}
	svc := healthSvc(fs, longCallSettings())
	svc.outbox.record(w, []OutboxEntry{{RunID: r.ID, PendingMessages: 3, Since: t0.Add(-time.Minute)}}, t0)
	svc.detectRunHealth(context.Background(), t0)
	got := lastWrite(t, fs, r.ID)
	if got.Health != healthStalled || got.HealthReason.String != reasonOutboxQueued {
		t.Fatalf("(health, reason) = (%q, %q), want stalled/%q", got.Health, got.HealthReason.String, reasonOutboxQueued)
	}
}

func TestHealthLongToolCallPrecedence(t *testing.T) {
	// Looping outranks it: the same call repeated past the loop threshold, the last open and old.
	var loop []fakeRunMessage
	for i := int32(0); i < 5; i++ {
		id := fmt.Sprintf("loop-%d", i)
		loop = append(loop, fakeMsg(t, 2*i+1, "tool_use", "", map[string]any{"id": id, "name": "Bash", "input": map[string]any{"command": "same"}}))
		if i < 4 {
			loop = append(loop, lcRes(t, 2*i+2, id))
		}
	}
	loop[len(loop)-1].createdAt = t0.Add(-25 * time.Minute)
	h, why := lcDetect(t, lcRun(25*time.Minute), loop, longCallSettings())
	wantHealth(t, "looping window", h, why, healthLooping, reasonLooping)

	// It outranks near-timeout: a run past 85% of its 2h budget with an old open call.
	m := []fakeRunMessage{lcUse(t, 1, "a", "Bash", 25*time.Minute)}
	r := lcRun(25 * time.Minute)
	r.StartedAt = ago(110 * time.Minute)
	st := fakeHealthSettings{enabled: true, stall: 300, toolCall: 1200, nearTimeoutPct: 85}
	h, why = lcDetect(t, r, m, st)
	wantHealth(t, "past near-timeout with a long call", h, why, healthStalled, reasonLongToolCall)

	// Control: the same run without the open call is the near-timeout flag.
	h, why = lcDetect(t, r, []fakeRunMessage{lcUse(t, 1, "a", "Bash", 25*time.Minute), lcRes(t, 2, "a")}, fakeHealthSettings{enabled: true, stall: 0, toolCall: 1200, nearTimeoutPct: 85})
	wantHealth(t, "near-timeout control", h, why, healthSlow, reasonNearTimeout)
}
