package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestManualCordonHealthEntryMetadataAndPrerequisites(t *testing.T) {
	actor, worker := uuid.New(), uuid.New()
	label, reason := strings.Repeat("😀", 200), strings.Repeat("🚀", 500)
	evidence, err := json.Marshal(map[string]any{
		"worker_id": worker, "actor_id": actor, "at": t0,
		"actor_label": "\x1b]0;hidden\a" + label + "overflow",
		"reason":      "\x1b[31m" + reason + "overflow\x1b[0m\x1b]8;;https://example.com\x1b\\\x1b]8;;\x1b\\",
	})
	if err != nil {
		t.Fatal(err)
	}
	manual := fmt.Sprintf("waiting for workers cordoned (manual): %s (%s) at %s: %s", label, actor, t0.Format(time.RFC3339), reason)
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		for _, mode := range []string{"manual", "advisory peer", "hard fence", "claimable continuation", "disabled", "custody", "vault", "account", "generic"} {
			t.Run(age.String()+"/"+mode, func(t *testing.T) {
				r := ownerHeldRun()
				r.StatusSince = ago(age)
				counts := store.CountOnlineWorkersClaimableForRunRow{ManualSuitable: 1, ManualCordoned: 1, ManualContext: evidence}
				fs := &healthFakeStore{eligibility: counts}
				svc, _ := pinSvc(fs, defaultHealthSettings())
				svc.p.WorkerAffinityCeiling = 2 * time.Hour
				th := healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true}
				want := manual
				switch mode {
				case "advisory peer":
					fs.eligibility.Claimable = 1
				case "hard fence":
					fs.eligibility.AutomaticOwnerHold = true
					want += "; " + ownerHoldReason(r.UpdatedAt.Time.Add(svc.p.WorkerAffinityCeiling), r.StaleRequeueWorkerName)
				case "claimable continuation":
					fs.eligibility.Claimable = 1
					fs.eligibility.AssignedClaimable = 1
					fs.eligibility.AutomaticOwnerHold = true
					// Generic lack-of-capacity text is outside this assertion; it must
					// never become an owner hold or manual block.
					want = ""
				case "disabled":
					th.queued = 0
					want = ""
				case "vault":
					svc.SetVault(newMemoryVault(newBox(t), newMemVaultStore()))
					want = reasonVaultLocked
				case "custody":
					fs.custodyHolds = int64(testCustodyLimit)
					want = reasonCustodyLimit
				case "account":
					r.CodexAccountGated = true
					want = reasonCodexAccountUnavailable
				case "generic":
					r.OwnerHoldPinnable = false
					fs.eligibility = store.CountOnlineWorkersClaimableForRunRow{}
					want = reasonNoWorker
				}
				if age < th.queued && (mode == "vault" || mode == "custody" || mode == "account" || mode == "generic" || mode == "claimable continuation") {
					want = ""
				}
				flag, got := svc.healthTargetFor(context.Background(), t0, r, th)
				if mode == "claimable continuation" && age >= th.queued {
					if strings.Contains(got, "cordoned (manual)") || strings.Contains(got, "draining or maintenance") {
						t.Fatalf("claimable continuation: %q", got)
					}
					return
				}
				wantFlag := healthWaitingWorker
				if want == "" {
					wantFlag = healthOK
				}
				if got != want || flag != wantFlag {
					t.Fatalf("health=(%q,%q), want=(%q,%q)", flag, got, wantFlag, want)
				}
			})
		}
	}
}

func TestManualCordonHealthEntryAssignedFenceLiveDB(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		t.Run(fmt.Sprint(fenced), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			svc.p.WorkerAffinityCeiling = time.Hour
			worker := e.seedHeartbeatWorker(t, nil)
			run := e.seedQueuedRun(t, nil, nil)
			e.exec(t, "UPDATE runs SET worker_id=$2,status_since=now(),updated_at=now() WHERE id=$1", run, worker)
			e.exec(t, "UPDATE workers SET maintenance_fenced=$2 WHERE id=$1", worker, fenced)
			cordoned, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "😀 operator", pgtype.Text{String: "🚀 investigate", Valid: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			th := healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true}
			row := e.healthRowFor(t, run)
			flag, reason := svc.healthTargetFor(e.ctx, time.Now(), row, th)
			if !fenced {
				if flag != healthOK || reason != "" {
					t.Fatalf("claimable continuation health=(%q,%q)", flag, reason)
				}
				return
			}
			manual := fmt.Sprintf("waiting for workers cordoned (manual): 😀 operator (%s) at %s: 🚀 investigate", e.userID, cordoned.ManualCordonedAt.Time.UTC().Format(time.RFC3339))
			want := manual + "; " + ownerHoldReason(row.UpdatedAt.Time.Add(svc.p.WorkerAffinityCeiling), row.StaleRequeueWorkerName)
			if flag != healthWaitingWorker || reason != want {
				t.Fatalf("fenced health=(%q,%q), want %q", flag, reason, want)
			}
		})
	}
}

func TestManualCordonHealthLeaseAvailabilityLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	svc.ephemeralLease = 2 * time.Hour
	run := e.seedQueuedRun(t, nil, nil)
	bound := e.seedQueuedRun(t, nil, nil)
	worker := e.seedHeartbeatWorker(t, nil)
	e.exec(t, "UPDATE runs SET worker_id=$2,branch=NULL WHERE id=$1", run, worker)
	e.exec(t, `UPDATE workers SET ephemeral=true,ephemeral_run_id=$2,
        lease_since=now(),lease_repo_id=(SELECT repo_id FROM runs WHERE id=$3),
        lease_branch=(SELECT 'agent/issue-' || issue_iid::text FROM runs WHERE id=$3),draining_since=now() WHERE id=$1`, worker, bound, run)
	for _, draining := range []bool{true, false} {
		e.exec(t, "UPDATE workers SET draining_since=CASE WHEN $2 THEN now() ELSE NULL END WHERE id=$1", worker, draining)
		counts, err := svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(1)
		if draining {
			want = 0
		}
		if counts.StrictEligible != want || counts.Claimable != want || counts.ManualSuitable != 1 {
			t.Fatalf("draining=%v counts=%+v want strict and claimable=%d manual suitable=1", draining, counts, want)
		}
	}
}

func TestManualCordonHealthYoungUnicodeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	run := e.seedQueuedRun(t, nil, nil)
	worker := e.seedHeartbeatWorker(t, nil)
	label, reason := strings.Repeat("😀", 200), strings.Repeat("🚀", 500)
	cordoned, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, label, pgtype.Text{String: reason, Valid: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	row := e.healthRowFor(t, run)
	flag, got := svc.healthTargetFor(e.ctx, time.Now(), row, healthThresholds{queued: time.Hour, custodyLimit: testCustodyLimit, custodyLimitOK: true})
	want := fmt.Sprintf("waiting for workers cordoned (manual): %s (%s) at %s: %s", label, e.userID, cordoned.ManualCordonedAt.Time.UTC().Format(time.RFC3339), reason)
	if flag != healthWaitingWorker || got != want {
		t.Fatalf("young health=(%q,%q), want %q", flag, got, want)
	}
}

func TestManualCordonHealthReadFailureRecovery(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(age)
			r.Health = healthWaitingWorker
			r.HealthReason = pgtype.Text{String: manualCordonWaitPrefix + " prior operator evidence", Valid: true}
			r.HealthSince, r.HealthNotifiedAt = ago(30*time.Minute), ago(20*time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, eligibilityErr: errors.New("projection unavailable")}
			svc, b := pinSvc(fs, defaultHealthSettings())
			if n := svc.detectRunHealth(context.Background(), t0); n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 {
				t.Fatalf("failed read changed episode: changes=%d writes=%+v nudges=%v", n, fs.writes, b.healthNudges)
			}
			if len(fs.eligibilityCalls) != 1 || fs.active[0].HealthSince != r.HealthSince || fs.active[0].HealthNotifiedAt != r.HealthNotifiedAt {
				t.Fatal("failed evaluation lost episode stamps or reread projection")
			}
			// A later successful projection proves the manual block ended.
			fs.eligibilityErr = nil
			fs.onlineWorkers, fs.freeSlotWorkers = 1, 1
			if n := svc.detectRunHealth(context.Background(), t0.Add(time.Second)); n != 1 {
				t.Fatalf("recovery changes=%d want 1", n)
			}
			w := lastWrite(t, fs, r.ID)
			if age < 10*time.Minute {
				if w.Health != healthOK || w.HealthReason.Valid || w.HealthSince.Valid {
					t.Fatalf("young recovery did not clear: %+v", w)
				}
			} else if w.Health != healthWaitingWorker || w.HealthReason.String != reasonWaitingWorker || w.HealthSince != r.HealthSince {
				t.Fatalf("aged recovery lost ordinary wait/episode: %+v", w)
			}
			if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] || len(fs.eligibilityCalls) != 2 {
				t.Fatalf("recovery renotified or reread: write=%+v nudges=%v calls=%d", w, b.healthNudges, len(fs.eligibilityCalls))
			}
		})
	}
}

func TestManualCordonOwnerPinnedReadFailureRecovery(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			r := ownerHeldRun()
			r.StatusSince = ago(age)
			r.Health = healthWaitingWorker
			r.HealthReason = pgtype.Text{
				String: fmt.Sprintf("%s prior operator (%s) at %s: investigating capacity", manualCordonWaitPrefix, uuid.New(), t0.Add(-time.Hour).Format(time.RFC3339)),
				Valid:  true,
			}
			r.HealthSince, r.HealthNotifiedAt = ago(30*time.Minute), ago(20*time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, eligibilityErr: errors.New("projection unavailable")}
			svc, b := pinSvc(fs, defaultHealthSettings())
			if n := svc.detectRunHealth(context.Background(), t0); n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 {
				t.Fatalf("failed read changed manual episode: changes=%d writes=%+v nudges=%v", n, fs.writes, b.healthNudges)
			}
			got := fs.active[0]
			if got.Health != r.Health || got.HealthReason != r.HealthReason || got.HealthSince != r.HealthSince || got.HealthNotifiedAt != r.HealthNotifiedAt || len(fs.eligibilityCalls) != 1 {
				t.Fatalf("failed read lost manual evidence or episode: got=%+v calls=%d", got, len(fs.eligibilityCalls))
			}
			// A successful projection proves the assigned continuation can claim.
			fs.eligibilityErr = nil
			fs.eligibility = store.CountOnlineWorkersClaimableForRunRow{Claimable: 1, AssignedClaimable: 1}
			fs.onlineWorkers, fs.freeSlotWorkers = 1, 1
			if n := svc.detectRunHealth(context.Background(), t0.Add(time.Second)); n != 1 {
				t.Fatalf("recovery changes=%d want 1", n)
			}
			w := lastWrite(t, fs, r.ID)
			if age < 10*time.Minute {
				if w.Health != healthOK || w.HealthReason.Valid || w.HealthSince.Valid {
					t.Fatalf("young recovery did not clear: %+v", w)
				}
			} else if w.Health != healthWaitingWorker || w.HealthReason.String != reasonWaitingWorker || w.HealthSince != r.HealthSince {
				t.Fatalf("aged recovery lost ordinary wait/episode: %+v", w)
			}
			if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] || len(fs.eligibilityCalls) != 2 {
				t.Fatalf("recovery renotified or reread: write=%+v nudges=%v calls=%d", w, b.healthNudges, len(fs.eligibilityCalls))
			}
		})
	}
}

func TestManualCordonYoungProjectionReadOnce(t *testing.T) {
	evidence := mustJSON(t, map[string]any{"worker_id": uuid.New(), "actor_id": uuid.New(), "at": t0})
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{runRow("queued")}, eligibility: store.CountOnlineWorkersClaimableForRunRow{ManualSuitable: 1, ManualCordoned: 1, ManualContext: evidence}}
	fs.active[0].StatusSince = ago(time.Minute)
	svc, _ := pinSvc(fs, defaultHealthSettings())
	if n := svc.detectRunHealth(context.Background(), t0); n != 1 || len(fs.eligibilityCalls) != 1 || !isManualCordonWaitReason(lastWrite(t, fs, fs.active[0].ID).HealthReason.String) {
		t.Fatalf("manual evaluation changes=%d projection reads=%d", n, len(fs.eligibilityCalls))
	}
}

func TestManualCordonReadFailurePrerequisitePriority(t *testing.T) {
	for _, mode := range []string{"vault", "custody", "account", "stale pin"} {
		t.Run(mode, func(t *testing.T) {
			r := ownerHeldRun()
			r.StatusSince = ago(time.Hour)
			r.Health = healthWaitingWorker
			r.HealthReason = pgtype.Text{String: manualCordonWaitPrefix + " prior evidence", Valid: true}
			r.StaleRequeuePinnable = mode == "stale pin"
			if r.StaleRequeuePinnable {
				r.StatusSince = ago(15 * time.Minute)
			}
			fs := &healthFakeStore{eligibilityErr: errors.New("projection unavailable")}
			svc, _ := pinSvc(fs, defaultHealthSettings())
			want := ""
			switch mode {
			case "vault":
				svc.SetVault(newMemoryVault(newBox(t), newMemVaultStore()))
				want = reasonVaultLocked
			case "custody":
				fs.custodyHolds = int64(testCustodyLimit)
				want = reasonCustodyLimit
			case "account":
				r.CodexAccountGated = true
				want = reasonCodexAccountUnavailable
			case "stale pin":
				want = staleRequeuePinReason(r.StatusSince.Time.Add(svc.p.WorkerStaleRequeueGrace), r.StaleRequeueWorkerName)
			}
			fs.active = []store.ListActiveRunsForHealthRow{r}
			if n := svc.detectRunHealth(context.Background(), t0); n != 1 || lastWrite(t, fs, r.ID).HealthReason.String != want {
				t.Fatalf("prerequisite priority: changes=%d writes=%+v want=%q", n, fs.writes, want)
			}
		})
	}
}

func TestManualCordonPersistentHistoricalEligibilityLiveDB(t *testing.T) {
	for _, mode := range []string{"ordinary", "hard fence", "manual only"} {
		t.Run(mode, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			run := e.seedQueuedRun(t, nil, nil)
			worker := e.seedHeartbeatWorker(t, nil)
			at := time.Now().UTC().Truncate(time.Microsecond)
			e.exec(t, `UPDATE workers SET maintenance_fenced=$2,maintenance_activity_floor=$3 WHERE id=$1`, worker, mode == "hard fence", at)
			if _, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "lane operator", pgtype.Text{}, mode == "manual only"); err != nil {
				t.Fatal(err)
			}
			counts, err := svc.workerEligibilityForHealth(e.ctx, at, run, true)
			wantNonDraining, wantClaimable := int64(1), int64(1)
			if mode == "hard fence" {
				wantNonDraining = 0
			}
			if mode != "ordinary" {
				wantClaimable = 0
			}
			wantManual := int64(0)
			if mode == "manual only" {
				wantManual = 1
			}
			if err != nil || counts.StrictEligible != 1 || counts.NonDrainingEligible != wantNonDraining || counts.Claimable != wantClaimable || counts.ManualSuitable != 1 || counts.ManualCordoned != wantManual {
				t.Fatalf("%s counts=%+v err=%v want strict=1 nonDraining=%d claimable=%d manualSuitable=1 manualCordoned=%d", mode, counts, err, wantNonDraining, wantClaimable, wantManual)
			}
		})
	}
}
