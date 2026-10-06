package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthCrossCheckWaiting(t *testing.T) {
	for _, mode := range []string{"silent", "no updates", "near budget", "long tool call"} {
		for _, prior := range []pgtype.Timestamptz{{}, ago(time.Minute), ago(time.Hour)} {
			t.Run(mode+"/"+prior.Time.String(), func(t *testing.T) {
				r := stalledRunRow()
				r.Kind = runkind.Issue
				r.HealthNotifiedAt = prior
				r.BudgetWallSeconds = frozenBudget(8)
				r.StartedAt = ago(7 * time.Hour)
				if mode == "no updates" {
					r.LastActivityAt = pgtype.Timestamptz{}
				}
				if mode == "near budget" {
					r.LastActivityAt = ago(time.Minute)
				}
				st := defaultHealthSettings()
				st.toolCall = 60
				fs, svc, b := nudgeSvc(t, r, st)
				fs.liveCrossCheck = map[uuid.UUID]bool{r.ID: true}
				if mode == "long tool call" {
					msg := useMsg(t, 1, "open", "Bash", map[string]any{"command": "true"})
					fs.messages = map[uuid.UUID][]fakeRunMessage{r.ID: {{seq: 1, kind: "tool_use", payload: msg.Payload, createdAt: t0.Add(-10 * time.Minute)}}}
				}
				if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
					t.Fatalf("changed=%d want 1", n)
				}
				w := lastWrite(t, fs, r.ID)
				if w.Health != healthWaitingWorker || w.HealthReason.String != "waiting for plan cross-check" {
					t.Fatalf("health=%s/%s", w.Health, w.HealthReason.String)
				}
				if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
					t.Fatalf("waiting nudged or stamped: write=%+v nudges=%v", w, b.healthNudges)
				}
				if len(fs.liveCrossCheckCalls) != 1 || fs.liveCrossCheckCalls[0] != r.ID {
					t.Fatalf("lookups=%v", fs.liveCrossCheckCalls)
				}
				// Model SetRunHealth's COALESCE: a NULL stamp preserves the old cooldown.
				r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
				fs.active[0] = r
				if n := svc.detectRunHealth(context.Background(), t0.Add(time.Second)); n != 0 ||
					len(fs.writes) != 1 || len(b.healthNudges) != 1 {
					t.Fatalf("repeated health was not a noop: changed=%d writes=%d nudges=%v", n, len(fs.writes), b.healthNudges)
				}

				// A decision/removal makes the live reader return false; ordinary health resumes.
				fs.liveCrossCheck[r.ID] = false
				if n := svc.detectRunHealth(context.Background(), t0.Add(2*time.Second)); n != 1 {
					t.Fatalf("after removal changed=%d want 1", n)
				}
				w = lastWrite(t, fs, r.ID)
				want, reason := healthStalled, reasonStalled
				switch mode {
				case "near budget":
					want, reason = healthSlow, reasonNearTimeout
				case "long tool call":
					reason = reasonLongToolCall
				}
				if w.Health != want || w.HealthReason.String != reason || w.HealthNotifiedAt.Valid || b.healthNudges[1] {
					t.Fatalf("ordinary health=%+v nudges=%v", w, b.healthNudges)
				}
				// Clear the episode on fresh activity, then stall again. The waiting phase
				// must not have burned a cooldown that would suppress this new episode.
				r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
				r.StartedAt, r.LastActivityAt = pgconv.Time(t0), pgconv.Time(t0)
				fs.window = nil
				fs.messages = nil
				fs.active[0] = r
				svc.detectRunHealth(context.Background(), t0.Add(3*time.Second))
				w = lastWrite(t, fs, r.ID)
				if w.Health != healthOK || w.HealthNotifiedAt.Valid {
					t.Fatalf("clear=%+v", w)
				}
				r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
				fs.active[0] = r
				svc.detectRunHealth(context.Background(), t0.Add(6*time.Minute))
				w = lastWrite(t, fs, r.ID)
				wantNudge := !prior.Valid || t0.Add(6*time.Minute).Sub(prior.Time) >= 30*time.Minute
				if w.Health != healthStalled || w.HealthNotifiedAt.Valid != wantNudge ||
					b.healthNudges[len(b.healthNudges)-1] != wantNudge {
					t.Fatalf("cooldown after waiting: write=%+v nudges=%v", w, b.healthNudges)
				}
			})
		}
	}
}

func TestHealthCrossCheckLoopPriority(t *testing.T) {
	for _, persist := range []bool{false, true} {
		t.Run(map[bool]string{false: "tool", true: "persist"}[persist], func(t *testing.T) {
			r := stalledRunRow()
			r.Kind = runkind.Issue
			fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
			fs.liveCrossCheck = map[uuid.UUID]bool{r.ID: true}
			fs.window = map[uuid.UUID][]store.ListRunToolWindowRow{r.ID: {
				useMsg(t, 4, "d", "Bash", map[string]any{"command": "true"}),
				useMsg(t, 3, "c", "Bash", map[string]any{"command": "true"}),
				useMsg(t, 2, "b", "Bash", map[string]any{"command": "true"}),
				useMsg(t, 1, "a", "Bash", map[string]any{"command": "true"}),
			}}
			want := reasonLooping
			if persist {
				wedge(svc, r.ID, persistFlagStreak, persistFlagWindow+time.Second)
				want = reasonPersistFailing
			}
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthLooping || w.HealthReason.String != want ||
				!w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || !b.healthNudges[0] {
				t.Fatalf("looping write=%+v nudges=%v", w, b.healthNudges)
			}
			if len(fs.liveCrossCheckCalls) != 0 {
				t.Fatalf("loop queried live check: %v", fs.liveCrossCheckCalls)
			}
		})
	}
}

// Embedding only Store hides the optional reader while forwarding ordinary health operations.
type healthWithoutCrossCheckReader struct{ Store }

func TestHealthCrossCheckReadFallsThrough(t *testing.T) {
	for _, mode := range []string{"no live row", "error", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			r := stalledRunRow()
			r.Kind = runkind.Issue
			fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
			if mode == "error" {
				fs.liveCrossCheck = map[uuid.UUID]bool{r.ID: true}
				fs.liveCrossCheckErr = errors.New("live check unavailable")
			}
			if mode == "unavailable" {
				svc.q = healthWithoutCrossCheckReader{Store: fs}
			}
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthStalled || w.HealthReason.String != reasonStalled ||
				!w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || !b.healthNudges[0] {
				t.Fatalf("fallthrough write=%+v nudges=%v", w, b.healthNudges)
			}
			wantCalls := 1
			if mode == "unavailable" {
				wantCalls = 0
			}
			if len(fs.liveCrossCheckCalls) != wantCalls {
				t.Fatalf("calls=%v want %d", fs.liveCrossCheckCalls, wantCalls)
			}
		})
	}
}

func TestHealthCrossCheckGuards(t *testing.T) {
	for _, tc := range []struct{ status, kind, want, reason string }{
		{"claimed", runkind.Issue, healthOK, ""},
		{"queued", runkind.Issue, healthWaitingWorker, reasonNoWorker},
		{"awaiting_approval", runkind.Issue, healthApprovalIdle, reasonApprovalIdle},
		{"running", runkind.Chat, healthStalled, reasonStalled},
		{"running", runkind.Judge, healthStalled, reasonStalled},
		{"running", runkind.Job, healthStalled, reasonStalled},
		{"running", runkind.Task, healthStalled, reasonStalled},
		{"running", runkind.CrossCheck, healthStalled, reasonStalled},
	} {
		t.Run(tc.status+"/"+tc.kind, func(t *testing.T) {
			r := stalledRunRow()
			r.Status, r.Kind = tc.status, tc.kind
			r.StatusSince = ago(2 * time.Hour)
			fs, svc, _ := nudgeSvc(t, r, defaultHealthSettings())
			fs.liveCrossCheck = map[uuid.UUID]bool{r.ID: true}
			svc.detectRunHealth(context.Background(), t0)
			if tc.want == healthOK {
				if len(fs.writes) != 0 {
					t.Fatalf("claimed health wrote: %+v", fs.writes)
				}
			} else if w := lastWrite(t, fs, r.ID); w.Health != tc.want || w.HealthReason.String != tc.reason {
				t.Fatalf("health=%+v want %s/%s", w, tc.want, tc.reason)
			}
			if len(fs.liveCrossCheckCalls) != 0 {
				t.Fatalf("excluded arm queried live check: %v", fs.liveCrossCheckCalls)
			}
		})
	}
}
