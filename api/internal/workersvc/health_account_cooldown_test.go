package workersvc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthAccountCooldown(t *testing.T) {
	r := runRow("running")
	r.AccountCooldownClass = pgconv.TextOrNull("rate_limit")
	r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
	r.LastActivityAt = ago(time.Hour)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingAccountCooldown || w.HealthReason.String != apitypes.AccountCooldownReason || w.HealthNotifiedAt.Valid {
		t.Fatalf("cooldown=%+v", w)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("entry nudges=%v", b.healthNudges)
	}
	// Identical polls, refreshed leases and extended policy leave reason, writes and WS unchanged.
	r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
	for _, extra := range []time.Duration{0, time.Minute, time.Hour} {
		r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute + extra))
		fs.active[0] = r
		before := len(b.healths)
		if n := svc.detectRunHealth(context.Background(), t0); n != 0 || len(b.healths) != before || len(fs.writes) != 1 {
			t.Fatal("unchanged cooldown wrote or broadcast")
		}
	}
	// Expiry clears the expected wait without notifying.
	r.AccountCooldownEligibleAt = pgconv.Time(t0)
	r.LastActivityAt = pgconv.Time(t0)
	fs.active[0] = r
	svc.detectRunHealth(context.Background(), t0)
	w = lastWrite(t, fs, r.ID)
	if w.Health != healthOK || w.HealthNotifiedAt.Valid || b.healthNudges[len(b.healthNudges)-1] {
		t.Fatalf("exit=%+v", w)
	}
	// A later ordinary fault starts a normal episode.
	r.Health, r.HealthReason = w.Health, w.HealthReason
	r.LastActivityAt = ago(time.Hour)
	fs.active[0] = r
	svc.detectRunHealth(context.Background(), t0)
	if !b.healthNudges[len(b.healthNudges)-1] {
		t.Fatal("later fault failed to notify")
	}
}

func TestHealthAccountCooldownPrecedence(t *testing.T) {
	for _, status := range []string{"claimed", "running", "awaiting_approval"} {
		r := runRow(status)
		r.AccountCooldownClass = pgconv.TextOrNull("overload")
		r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
		fs := &healthFakeStore{}
		svc := healthSvc(fs, defaultHealthSettings())
		flag, reason := svc.healthTargetFor(context.Background(), t0, r, svc.healthThresholds(context.Background()))
		if flag != healthWaitingAccountCooldown || reason != apitypes.AccountCooldownReason {
			t.Fatalf("%s=%s/%s", status, flag, reason)
		}
	}
	for _, elapsed := range []time.Duration{95 * time.Second, 101 * time.Second} {
		r := runRow("running")
		r.Kind = "issue"
		r.StartedAt = ago(elapsed)
		r.BudgetWallSeconds = pgtype.Int4{Int32: 100, Valid: true}
		r.AccountCooldownClass = pgconv.TextOrNull("overload")
		r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
		svc := healthSvc(&healthFakeStore{}, defaultHealthSettings())
		flag, reason := svc.healthTargetFor(context.Background(), t0, r, svc.healthThresholds(context.Background()))
		if flag != healthSlow || reason != reasonNearTimeout {
			t.Fatalf("wall elapsed=%s: %s/%s", elapsed, flag, reason)
		}
	}
}

func TestHealthAccountCooldownDirectTransitions(t *testing.T) {
	for _, fault := range []string{"persistence", "tools", "near_wall", "outcome", "silence", "approval_age"} {
		for _, recent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recent_nudge=%t", fault, recent), func(t *testing.T) {
				r := runRow("running")
				r.Health = healthWaitingAccountCooldown
				r.HealthReason = pgconv.TextOrNull(apitypes.AccountCooldownReason)
				r.HealthSince = ago(time.Hour)
				r.AccountCooldownClass = pgconv.TextOrNull("rate_limit")
				r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
				r.LastActivityAt = pgconv.Time(t0)
				if recent {
					r.HealthNotifiedAt = ago(time.Minute)
				}
				fs := &healthFakeStore{}
				svc := healthSvc(fs, defaultHealthSettings())
				want, reason := healthLooping, reasonPersistFailing
				switch fault {
				case "persistence":
					wedge(svc, r.ID, persistFlagStreak, persistFlagWindow+time.Second)
				case "tools":
					reason = reasonLooping
					rows := []store.ListRunToolWindowRow{}
					for i := int32(0); i < 4; i++ {
						id := fmt.Sprintf("call%d", i)
						rows = append(rows, useMsg(t, i*2+1, id, "Read", map[string]any{"file_path": "same"}), store.ListRunToolWindowRow{Seq: i*2 + 2, Kind: "tool_result", Payload: mustJSON(t, map[string]any{"tool_use_id": id, "content": "same result"})})
					}
					fs.window = map[uuid.UUID][]store.ListRunToolWindowRow{r.ID: rows}
				case "near_wall":
					want, reason = healthSlow, reasonNearTimeout
					r.Kind, r.StartedAt = "issue", ago(95*time.Second)
					r.BudgetWallSeconds = pgtype.Int4{Int32: 100, Valid: true}
				case "outcome":
					want, reason = healthStalled, reasonOutcomeUndelivered
					r.TerminalPendingSince = ago(time.Hour)
				case "silence":
					want, reason = healthStalled, reasonStalled
					r.AccountCooldownEligibleAt, r.LastActivityAt = pgconv.Time(t0), ago(time.Hour)
				case "approval_age":
					want, reason = healthApprovalIdle, reasonApprovalIdle
					r.Status, r.StatusSince = "awaiting_approval", ago(2*time.Hour)
					r.AccountCooldownEligibleAt = pgconv.Time(t0)
				}
				fs.active = []store.ListActiveRunsForHealthRow{r}
				b := &fakeBroadcaster{}
				svc.SetBroadcaster(b)
				svc.detectRunHealth(context.Background(), t0)
				w := lastWrite(t, fs, r.ID)
				if w.Health != want || w.HealthReason.String != reason || !w.HealthSince.Time.Equal(t0) {
					t.Fatalf("transition = %+v, want %s/%s restamped", w, want, reason)
				}
				wantNudge := !recent && fault != "silence" && fault != "approval_age"
				if len(b.healthNudges) != 1 || b.healthNudges[0] != wantNudge || w.HealthNotifiedAt.Valid != wantNudge {
					t.Fatalf("nudges=%v stamp=%+v want=%t", b.healthNudges, w.HealthNotifiedAt, wantNudge)
				}
				if wantNudge && !w.HealthNotifiedAt.Time.Equal(t0) {
					t.Fatal("wrong notification stamp")
				}
				r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
				fs.active[0] = r
				if n := svc.detectRunHealth(context.Background(), t0); n != 0 || len(b.healthNudges) != 1 {
					t.Fatal("unchanged fault rebroadcast")
				}
			})
		}
	}
}
