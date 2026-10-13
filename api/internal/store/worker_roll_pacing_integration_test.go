package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerRollPacingPersistenceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	svc := workersvc.New(q, nil, workersvc.Params{})
	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`, user, fmt.Sprintf("%s@pacing.test", user))
	seed := func(kind string) uuid.UUID {
		id := uuid.New()
		var size any
		if kind == "hosted" {
			size = "m"
		}
		mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared) VALUES($1,$2,'pacing',$3,'offline',$4,'1.0.0',$5,'base')`, id, user, []byte(id.String()), kind, size)
		return id
	}
	hosted, external := seed("hosted"), seed("external")
	t0 := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	reason := "bad\n\u001bpod"
	hold := &workersvc.PacingReport{State: "held", TargetImage: "repo:v2", TargetVersion: "2.0.0", BlockingReason: &reason}
	write := func(id uuid.UUID, kind string, p *workersvc.PacingReport, at time.Time) {
		t.Helper()
		n, err := svc.RecordRollHealth(ctx, workersvc.RollHealthReport{WorkerID: id, Kind: kind, Pacing: p, Phase: "settled", ObservedAt: at, ControllerReportedAt: at.Add(365 * 24 * time.Hour)})
		if err != nil || n != 1 {
			t.Fatalf("write kind=%q rows=%d err=%v", kind, n, err)
		}
	}
	assertHeld := func(observed time.Time) {
		t.Helper()
		var data []byte
		var receipt, ordinary time.Time
		var fresh bool
		if err := pool.QueryRow(ctx, `SELECT pacing,pacing_observed_at,observed_at,pacing_observed_at > $2::timestamptz - interval '5 minutes' FROM worker_upgrade_reports WHERE worker_id=$1`, hosted, t0.Add(time.Hour)).Scan(&data, &receipt, &ordinary, &fresh); err != nil {
			t.Fatal(err)
		}
		var p workersvc.PacingReport
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		if p.State != "held" || p.BlockingReason == nil || *p.BlockingReason != "badpod" || !receipt.Equal(t0) || !ordinary.Equal(observed) || fresh {
			t.Fatalf("pacing=%s receipt=%v ordinary=%v fresh=%v", data, receipt, ordinary, fresh)
		}
		rows, err := q.ListWorkersByUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.ID == hosted {
				found = true
				if string(row.RollPacing) != string(data) || !row.RollPacingObservedAt.Time.Equal(t0) {
					t.Fatal("user projection lost pacing")
				}
			}
		}
		if !found {
			t.Fatal("hosted missing")
		}
		all, err := q.ListAllWorkers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found = false
		for _, row := range all {
			if row.Worker.ID == hosted {
				found = true
				if string(row.RollPacing) != string(data) || !row.RollPacingObservedAt.Time.Equal(t0) {
					t.Fatal("all projection lost pacing")
				}
			}
		}
		if !found {
			t.Fatal("hosted missing in all")
		}
	}
	write(hosted, "fleet", hold, t0)
	assertHeld(t0)
	write(hosted, "readiness", nil, t0.Add(time.Hour))
	assertHeld(t0.Add(time.Hour))
	write(hosted, "readiness", &workersvc.PacingReport{State: "override"}, t0.Add(2*time.Hour))
	assertHeld(t0.Add(2 * time.Hour))
	write(hosted, "", nil, t0.Add(3*time.Hour))
	assertHeld(t0.Add(3 * time.Hour))
	for _, id := range []uuid.UUID{external, uuid.New()} {
		n, err := svc.RecordRollHealth(ctx, workersvc.RollHealthReport{WorkerID: id, Kind: "fleet", Pacing: hold, Phase: "stuck", ObservedAt: t0})
		if err != nil || n != 0 {
			t.Fatalf("confinement n=%d err=%v", n, err)
		}
	}
	var unchanged bool
	if err := pool.QueryRow(ctx, `SELECT status='offline' AND version='1.0.0' AND last_heartbeat_at IS NULL AND draining_since IS NULL AND token_hash=$2 FROM workers WHERE id=$1`, hosted, []byte(hosted.String())).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("worker changed: %v %v", unchanged, err)
	}
	for _, bad := range []string{`[]`, `{"state":"held","padding":"` + strings.Repeat("x", 4097) + `"}`} {
		p := rollReport(hosted, "settled", t0)
		p.UpdatePacing = true
		p.Pacing = []byte(bad)
		if _, err := q.UpsertWorkerRollHealth(ctx, p); err == nil {
			t.Fatal("DB accepted unbounded/nonobject pacing")
		}
	}
	assertHeld(t0.Add(3 * time.Hour))
	write(hosted, "fleet", nil, t0.Add(4*time.Hour))
	var cleared bool
	if err := pool.QueryRow(ctx, `SELECT pacing IS NULL AND pacing_observed_at IS NULL FROM worker_upgrade_reports WHERE worker_id=$1`, hosted).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("fleet clear: %v %v", cleared, err)
	}
	write(hosted, "", hold, t0)
	assertHeld(t0)
}
