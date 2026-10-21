package workersvc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthCodeCrossCheckWaitingRegression(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			r := stalledRunRow()
			r.Kind = runkind.Issue
			r.Harness = harness
			fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
			deadline := t0.Add(7 * time.Minute)
			fs.liveCodeCrossCheckDeadlines = map[uuid.UUID]pgtype.Timestamptz{
				r.ID: pgconv.Time(deadline),
			}

			if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
				t.Fatalf("changed=%d want 1", n)
			}
			w := lastWrite(t, fs, r.ID)
			const wantReason = "waiting until 2026-07-12T12:07:00Z for code cross-check"
			if w.Health != healthWaitingWorker || w.HealthReason.String != wantReason {
				t.Fatalf("health=%s/%q, want %s/%q", w.Health, w.HealthReason.String, healthWaitingWorker, wantReason)
			}
			if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
				t.Fatalf("waiting nudged or stamped: write=%+v nudges=%v", w, b.healthNudges)
			}
			if len(fs.liveCodeCrossCheckCalls) != 1 {
				t.Fatalf("deadline lookups=%v, want one", fs.liveCodeCrossCheckCalls)
			}
			call := fs.liveCodeCrossCheckCalls[0]
			if call.LeadRunID != r.ID || !call.ServerNow.Valid || !call.ServerNow.Time.Equal(t0) {
				t.Fatalf("deadline lookup=%+v, want lead %s at %s", call, r.ID, t0.Format(time.RFC3339))
			}
		})
	}
}

func TestHealthCodeCrossCheckKinds(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		reads int
	}{
		{kind: "issue", reads: 1},
		{kind: "prompt", reads: 1},
		{kind: "self_improve", reads: 1},
		{kind: "ci_fix", reads: 1},
		{kind: "mr_rework", reads: 1},
		{kind: "task", reads: 1},
		{kind: "chat"},
		{kind: "judge"},
		{kind: "job"},
		{kind: "cross_check"},
		{kind: "unknown"},
		{kind: ""},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			r := stalledRunRow()
			r.Kind = tc.kind
			fs, svc, _ := nudgeSvc(t, r, defaultHealthSettings())
			fs.liveCodeCrossCheckDeadlines = map[uuid.UUID]pgtype.Timestamptz{
				r.ID: pgconv.Time(t0.Add(time.Minute)),
			}
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			want, reason := healthStalled, reasonStalled
			if tc.reads == 1 {
				want, reason = healthWaitingWorker, "waiting until 2026-07-12T12:01:00Z for code cross-check"
			}
			if w.Health != want || w.HealthReason.String != reason {
				t.Errorf("health=%s/%q, want %s/%q", w.Health, w.HealthReason.String, want, reason)
			}
			if len(fs.liveCodeCrossCheckCalls) != tc.reads {
				t.Errorf("deadline lookups=%d, want %d", len(fs.liveCodeCrossCheckCalls), tc.reads)
			}
		})
	}
}

func TestHealthCodeCrossCheckPriority(t *testing.T) {
	for _, name := range []string{"task", "silent", "no updates", "long tool", "near budget", "plan wins", "persistence wins", "tool loop wins"} {
		t.Run(name, func(t *testing.T) {
			r := stalledRunRow()
			r.Kind = runkind.Issue
			st := defaultHealthSettings()
			st.toolCall = 60
			fs, svc, _ := nudgeSvc(t, r, st)
			fs.eligibilityErr = errors.New("capacity unavailable")
			fs.onlineErr = errors.New("workers unavailable")
			fs.liveCodeCrossCheckDeadlines = map[uuid.UUID]pgtype.Timestamptz{r.ID: pgconv.Time(t0.Add(time.Minute))}
			want, reason, reads := healthWaitingWorker, "waiting until 2026-07-12T12:01:00Z for code cross-check", 1
			switch name {
			case "task":
				r.Kind = runkind.Task
			case "no updates":
				r.LastActivityAt = pgtype.Timestamptz{}
			case "long tool":
				row := useMsg(t, 1, "open", "Bash", map[string]any{"command": "status"})
				fs.messages = map[uuid.UUID][]fakeRunMessage{r.ID: {{seq: 1, kind: "tool_use", payload: row.Payload, createdAt: t0.Add(-10 * time.Minute)}}}
			case "near budget":
				r.BudgetWallSeconds = pgtype.Int4{Int32: 1200, Valid: true}
				r.LastActivityAt = pgconv.Time(t0)
			case "plan wins":
				fs.liveCrossCheck = map[uuid.UUID]bool{r.ID: true}
				reason = reasonPlanCrossCheckWaiting
				reads = 0
			case "persistence wins":
				wedge(svc, r.ID, persistFlagStreak, persistFlagWindow+time.Second)
				want, reason, reads = healthLooping, reasonPersistFailing, 0
			case "tool loop wins":
				fs.window = map[uuid.UUID][]store.ListRunToolWindowRow{r.ID: loopPairs(t, 5)}
				want, reason, reads = healthLooping, reasonLooping, 0
			}
			fs.active[0] = r
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != want || w.HealthReason.String != reason || len(fs.liveCodeCrossCheckCalls) != reads {
				t.Fatalf("write=%+v reads=%d want %s/%q reads=%d", w, len(fs.liveCodeCrossCheckCalls), want, reason, reads)
			}
			if len(fs.eligibilityCalls) != 0 || len(fs.crossCheckWorkerCalls) != 0 {
				t.Fatalf("unexpected child capacity reads: %+v %+v", fs.eligibilityCalls, fs.crossCheckWorkerCalls)
			}
		})
	}
}

// Embedding only Store deliberately hides the optional deadline reader.
type healthWithoutCodeReader struct{ Store }

func TestHealthCodeCrossCheckFallback(t *testing.T) {
	for _, tc := range []struct {
		name                string
		deadline            pgtype.Timestamptz
		err                 error
		unavailable, logged bool
	}{
		{name: "absent", err: pgx.ErrNoRows},
		{name: "wrapped no rows", err: errors.Join(errors.New("wrapped"), pgx.ErrNoRows)},
		{name: "reader unavailable", unavailable: true},
		{name: "query error", err: errors.New("deadline query failed"), logged: true},
		{name: "invalid"},
		{name: "equal", deadline: pgconv.Time(t0)},
		{name: "past", deadline: pgconv.Time(t0.Add(-time.Second))},
		{name: "infinity", deadline: pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}},
		{name: "negative infinity", deadline: pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			r := stalledRunRow()
			r.Kind = runkind.Issue
			fs, svc, _ := nudgeSvc(t, r, defaultHealthSettings())
			fs.liveCodeCrossCheckErr = tc.err
			fs.liveCodeCrossCheckDeadlines = map[uuid.UUID]pgtype.Timestamptz{r.ID: tc.deadline}
			if tc.unavailable {
				svc.q = &healthWithoutCodeReader{Store: fs}
			}
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthStalled || w.HealthReason.String != reasonStalled {
				t.Fatalf("write=%+v", w)
			}
			reads := 1
			if tc.unavailable {
				reads = 0
			}
			if len(fs.liveCodeCrossCheckCalls) != reads {
				t.Fatalf("deadline lookups=%d, want %d", len(fs.liveCodeCrossCheckCalls), reads)
			}
			if got := strings.Contains(logs.String(), "health: read live code cross-check"); got != tc.logged {
				t.Fatalf("logged=%v want=%v logs=%s", got, tc.logged, &logs)
			}
		})
	}
}

func TestHealthCodeCrossCheckStableTicksAndTransitions(t *testing.T) {
	for _, exit := range []string{"expiry", "removal", "error", "long tool", "near budget", "other waiting", "healthy"} {
		t.Run(exit, func(t *testing.T) {
			r := stalledRunRow()
			r.Kind = runkind.Issue
			// An expired cooldown makes a mistaken leftPin classification observable.
			r.HealthNotifiedAt = ago(24 * time.Hour)
			originalNotifiedAt := r.HealthNotifiedAt
			st := defaultHealthSettings()
			st.toolCall = 60
			fs, svc, b := nudgeSvc(t, r, st)
			deadline := pgconv.Time(t0.Add(time.Minute).In(time.FixedZone("offset", 3600)))
			fs.liveCodeCrossCheckDeadlines = map[uuid.UUID]pgtype.Timestamptz{r.ID: deadline}
			svc.detectRunHealth(context.Background(), t0)
			first := lastWrite(t, fs, r.ID)
			if first.HealthReason.String != "waiting until 2026-07-12T12:01:00Z for code cross-check" || first.HealthNotifiedAt.Valid || b.healthNudges[0] {
				t.Fatalf("first=%+v nudges=%v", first, b.healthNudges)
			}
			r.Health, r.HealthReason, r.HealthSince = first.Health, first.HealthReason, first.HealthSince
			// Model SetRunHealth's COALESCE(new stamp, persisted stamp).
			if first.HealthNotifiedAt.Valid {
				r.HealthNotifiedAt = first.HealthNotifiedAt
			}
			fs.active[0] = r
			if n := svc.detectRunHealth(context.Background(), t0.Add(30*time.Second)); n != 0 || len(fs.writes) != 1 {
				t.Fatalf("stable tick changed=%d writes=%d", n, len(fs.writes))
			}
			if !fs.active[0].HealthNotifiedAt.Valid || !fs.active[0].HealthNotifiedAt.Time.Equal(originalNotifiedAt.Time) {
				t.Fatal("prior notification lost")
			}
			now := t0.Add(time.Minute)
			if exit == "removal" || exit == "error" {
				now = t0.Add(45 * time.Second)
				if !deadline.Time.After(now) {
					t.Fatal("removal/error must terminate waiting before deadline expiry")
				}
			}
			want, reason := healthStalled, reasonStalled
			switch exit {
			case "expiry":
				if !deadline.Time.Equal(now) {
					t.Fatal("expiry must terminate waiting at deadline equality")
				}
			case "removal":
				delete(fs.liveCodeCrossCheckDeadlines, r.ID)
			case "error":
				fs.liveCodeCrossCheckErr = errors.New("deadline unavailable")
			case "long tool":
				row := useMsg(t, 1, "open", "Bash", map[string]any{"command": "status"})
				fs.messages = map[uuid.UUID][]fakeRunMessage{r.ID: {{seq: 1, kind: "tool_use", payload: row.Payload, createdAt: t0.Add(-10 * time.Minute)}}}
				reason = reasonLongToolCall
			case "near budget":
				r.LastActivityAt = pgconv.Time(now)
				r.BudgetWallSeconds = pgtype.Int4{Int32: 1200, Valid: true}
				fs.active[0] = r
				want, reason = healthSlow, reasonNearTimeout
			case "other waiting":
				fs.liveCrossCheck = map[uuid.UUID]bool{r.ID: true}
				want, reason = healthWaitingWorker, reasonPlanCrossCheckWaiting
			case "healthy":
				r.LastActivityAt = pgconv.Time(now)
				fs.active[0] = r
				want, reason = healthOK, ""
			}
			svc.detectRunHealth(context.Background(), now)
			w := lastWrite(t, fs, r.ID)
			if w.Health != want || (reason != "" && w.HealthReason.String != reason) || w.HealthNotifiedAt.Valid || b.healthNudges[len(b.healthNudges)-1] {
				t.Fatalf("transition=%+v nudges=%v", w, b.healthNudges)
			}
			r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
			if w.HealthNotifiedAt.Valid {
				r.HealthNotifiedAt = w.HealthNotifiedAt
			}
			if !r.HealthNotifiedAt.Valid || !r.HealthNotifiedAt.Time.Equal(originalNotifiedAt.Time) {
				t.Fatalf("transition notification=%+v want preserved %+v", r.HealthNotifiedAt, originalNotifiedAt)
			}
			switch want {
			case healthWaitingWorker:
				if !w.HealthSince.Time.Equal(t0) {
					t.Fatalf("same enum reset since: %+v", w)
				}
			case healthOK:
				if w.HealthSince.Valid {
					t.Fatalf("healthy since=%+v", w.HealthSince)
				}
			default:
				if !w.HealthSince.Time.Equal(now) {
					t.Fatalf("ordinary episode since=%+v want %s", w.HealthSince, now)
				}
			}
		})
	}
}

func TestHealthCodeCrossCheckDoesNotBurnCooldown(t *testing.T) {
	r := stalledRunRow()
	r.Kind = runkind.Issue
	fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	fs.liveCodeCrossCheckDeadlines = map[uuid.UUID]pgtype.Timestamptz{r.ID: pgconv.Time(t0.Add(time.Minute))}
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker || w.HealthReason.String != "waiting until 2026-07-12T12:01:00Z for code cross-check" || len(fs.liveCodeCrossCheckCalls) != 1 {
		t.Fatalf("code wait=%+v deadline lookups=%d", w, len(fs.liveCodeCrossCheckCalls))
	}
	if w.HealthNotifiedAt.Valid || b.healthNudges[0] {
		t.Fatal("code wait burned cooldown")
	}
	// Model the persisted wait and then clear it on fresh activity.
	r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
	r.LastActivityAt = pgconv.Time(t0.Add(time.Minute))
	fs.active[0] = r
	fs.liveCodeCrossCheckDeadlines = nil
	svc.detectRunHealth(context.Background(), t0.Add(time.Minute))
	w = lastWrite(t, fs, r.ID)
	if w.Health != healthOK || w.HealthNotifiedAt.Valid {
		t.Fatalf("clear=%+v", w)
	}
	r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
	fs.active[0] = r
	// A later ordinary ok -> flagged episode remains eligible to notify.
	svc.detectRunHealth(context.Background(), t0.Add(7*time.Minute))
	w = lastWrite(t, fs, r.ID)
	if !w.HealthNotifiedAt.Valid || !b.healthNudges[len(b.healthNudges)-1] {
		t.Fatalf("ordinary episode did not notify: %+v %v", w, b.healthNudges)
	}
}
