package healthsvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestEvaluateScopes(t *testing.T) {
	want := map[string]string{
		"fleet.roll": "instance", "controller.report": "instance", "db": "instance", "db.size": "instance", "loops": "instance",
		"fleet.capacity": "owner", "fleet.disk": "owner", "fleet.rundisk": "owner",
		"fleet.quarantine": "owner",
		"queue.waiting":    "owner", "queue.undispatched": "owner", "forge.sync": "owner",
		"forge.ciwatch": "owner", "slack.socket": "owner", "schedules.paused": "owner",
		"board.drift": "owner", "custody.holds": "owner", "recovery.storage": "owner", "release.check": "owner",
		"pricing.codex": "instance",
	}
	svc := newSvc(&fakeStore{}, &fakeSettings{})
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Checks) != len(want) || len(checkMeta) != len(want) {
		t.Fatal("scope coverage differs from registry")
	}
	seen := map[string]bool{}
	for _, c := range doc.Checks {
		if c.Scope != "instance" && c.Scope != "owner" {
			t.Fatalf("%s has empty/invalid scope %q", c.ID, c.Scope)
		}
		expected, ok := want[c.ID]
		if !ok || seen[c.ID] || c.Scope != expected {
			t.Fatalf("unexpected scope/ID: %+v", c)
		}
		seen[c.ID] = true
	}
}

func TestEvaluateBlocking(t *testing.T) {
	for _, tc := range []struct {
		name            string
		owner, db, roll bool
	}{
		{name: "no danger"}, {name: "owner only", owner: true},
		{name: "db", db: true}, {name: "fleet.roll", roll: true},
		{name: "mixed", owner: true, db: true, roll: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{controller: pgtype.Timestamptz{Time: fixedNow, Valid: true}}
			if tc.owner {
				fs.waitingRows = []store.ListWaitingWorkerRunsRow{{HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}}}
			}
			if tc.roll {
				fs.workers = []store.ListAllWorkersRow{hostedRow("worker", "stuck", time.Second, "ImagePullBackOff", "worker", "0.84.0")}
			}
			svc := newSvc(fs, &fakeSettings{healthEnabled: true})
			svc.probeDB = func(context.Context) dbStat {
				if tc.db {
					return dbStat{pingErr: errors.New("down")}
				}
				return dbStat{schemaAtHead: true}
			}
			doc, err := svc.Evaluate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if doc.Blocking != (tc.db || tc.roll) {
				t.Fatalf("blocking=%v checks=%+v", doc.Blocking, doc.Checks)
			}
			wantCount := 0
			for _, danger := range []bool{tc.owner, tc.db, tc.roll} {
				if danger {
					wantCount++
				}
			}
			if doc.Counts.Danger != wantCount {
				t.Fatalf("danger tally=%d want %d", doc.Counts.Danger, wantCount)
			}
			if wantCount > 0 && doc.Status != sevDanger {
				t.Fatalf("overall=%s want danger", doc.Status)
			}
		})
	}
}

// The server-provided scope, rather than the ID, selects notification checks.
// Missing and invalid scopes cannot produce a notice even with a blocking document.
func TestEpisodeNoticeScopeSelection(t *testing.T) {
	for _, scope := range []string{"instance", "owner", "", "invalid"} {
		t.Run("scope="+scope, func(t *testing.T) {
			st := &fakeEpisodeStore{open: store.GetOpenHealthEpisodeRow{ID: uuid.New()}, admins: []uuid.UUID{uuid.New()}}
			nf := &fakeEpisodeNotifier{}
			checks := []apitypes.HealthCheckDTO{
				{ID: "future.instance", Scope: "instance", Severity: sevDanger, Title: "future", Summary: "danger"},
				{ID: "db", Scope: scope, Severity: sevDanger, Title: "db", Summary: "danger"},
			}
			ev := &mutEvaluator{doc: noticeDoc(sevDanger, checks)}
			newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true}).Reconcile(context.Background())
			if len(nf.sent) != 1 {
				t.Fatalf("notices=%d", len(nf.sent))
			}
			got := nf.sent[0].Payload.(map[string]any)["checks"].([]dangerCheck)
			wantLen := 1
			if scope == "instance" {
				wantLen = 2
			}
			if len(got) != wantLen || got[0].ID != "future.instance" {
				t.Fatalf("selected=%+v", got)
			}
			if wantLen == 2 && got[1].ID != "db" {
				t.Fatalf("order=%+v", got)
			}
		})
	}
	for _, id := range []string{"queue.waiting", "fleet.capacity"} {
		t.Run(id+"/owner-only", func(t *testing.T) {
			st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: uuid.New(), admins: []uuid.UUID{uuid.New()}}
			nf := &fakeEpisodeNotifier{}
			ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO(id, id, "danger")})}
			r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})
			r.Reconcile(context.Background())
			r.Reconcile(context.Background())
			if st.opened != 0 || len(st.claims) != 0 || len(nf.sent) != 0 {
				t.Fatal("owner danger opened/claimed/notified")
			}
		})
	}
}
