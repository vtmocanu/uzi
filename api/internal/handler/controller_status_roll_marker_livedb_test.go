package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Roll marker persistence and projection against a real database (issue #4385).
func TestControllerStatusRollMarkerLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	ctx := context.Background()
	user := cliSeedUser(t, pool, false)
	receipt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h.now = func() time.Time { return receipt }

	seed := func(name string) uuid.UUID {
		id := uuid.New()
		cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared) VALUES($1,$2,$3,$4,'offline','hosted','1.0.0','m','base')`, id, user, name, []byte(id.String()))
		return id
	}
	post := func(kind string, entries ...map[string]any) {
		t.Helper()
		rows := make([]any, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, e)
		}
		data, err := json.Marshal(map[string]any{"kind": kind, "reported_at": receipt.Add(365 * 24 * time.Hour), "workers": rows})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status %d", rec.Code)
		}
	}
	entry := func(id uuid.UUID, marker any) map[string]any {
		e := map[string]any{"id": id.String(), "phase": "settled"}
		if marker != nil {
			e["roll_marker"] = marker
		}
		return e
	}
	// adminDTO reads the worker through the admin list, after JSON round trip, as a client sees it.
	adminDTO := func(id uuid.UUID) *apitypes.RollMarkerDTO {
		t.Helper()
		all, err := h.q.ListAllWorkers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range all {
			if row.Worker.ID == id {
				raw, err := json.Marshal(workerDTOFromAdminRow(row, "dev", "", h.now(), h.now()))
				if err != nil {
					t.Fatal(err)
				}
				var out apitypes.WorkerDTO
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatal(err)
				}
				return out.RollMarker
			}
		}
		t.Fatal("worker missing from admin list")
		return nil
	}
	userDTO := func(id uuid.UUID) *apitypes.RollMarkerDTO {
		t.Helper()
		rows, err := h.q.ListWorkersByUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == id {
				return workerDTOFromRow(row, "dev", "", h.now(), h.now()).RollMarker
			}
		}
		t.Fatal("worker missing from user list")
		return nil
	}
	present := map[string]any{"state": "present", "canary": true, "completed": true, "target_version": "0.11.7", "coverage_group": "0123456789abcdef", "reservation": true}

	t.Run("present preserved by readiness, cleared by markerless fleet, expires", func(t *testing.T) {
		id := seed("marker-a")
		post("fleet", entry(id, present))
		for name, got := range map[string]*apitypes.RollMarkerDTO{"admin": adminDTO(id), "user": userDTO(id)} {
			if got == nil || got.State != "present" || got.Canary == nil || !*got.Canary || got.Completed == nil || !*got.Completed ||
				got.CoverageGroup == nil || *got.CoverageGroup != "0123456789abcdef" || got.ObservedAt == nil || !got.ObservedAt.Equal(receipt) {
				t.Fatalf("%s: %+v", name, got)
			}
		}
		observed := receipt
		receipt = receipt.Add(20 * time.Second)
		post("readiness", entry(id, nil))
		got := adminDTO(id)
		if got == nil || got.State != "present" || !got.ObservedAt.Equal(observed) {
			t.Fatalf("readiness must preserve the marker and its receipt: %+v", got)
		}
		// TTL: advance the clock past the freshness window with no new fleet report.
		receipt = observed.Add(workersvc.DefaultControllerSignalTTL + time.Second)
		got = adminDTO(id)
		if got == nil || got.State != "unknown" || got.Canary != nil || got.ObservedAt == nil || !got.ObservedAt.Equal(observed) {
			t.Fatalf("expired marker must read unknown but keep its receipt: %+v", got)
		}
		post("fleet", entry(id, nil))
		var isNull bool
		if err := pool.QueryRow(ctx, `SELECT roll_marker IS NULL AND roll_marker_observed_at IS NULL FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&isNull); err != nil || !isNull {
			t.Fatalf("markerless fleet report must store NULL: null=%v err=%v", isNull, err)
		}
		if got := adminDTO(id); got == nil || got.State != "unknown" || got.ObservedAt != nil {
			t.Fatalf("cleared marker: %+v", got)
		}
	})

	t.Run("unavailable and absent are distinct, groups distinct", func(t *testing.T) {
		a, b, c := seed("marker-b"), seed("marker-c"), seed("marker-d")
		other := map[string]any{"state": "present", "coverage_group": "fedcba9876543210", "reservation": true}
		post("fleet", entry(a, map[string]any{"state": "unavailable", "reservation": false}),
			entry(b, map[string]any{"state": "absent", "reservation": false}), entry(c, present))
		post("fleet", entry(seed("marker-e"), other))
		if ga, gb := adminDTO(a), adminDTO(b); ga.State != "unavailable" || gb.State != "absent" {
			t.Fatalf("unavailable=%+v absent=%+v", ga, gb)
		}
		d := seed("marker-f")
		post("fleet", entry(d, other))
		if gc, gd := adminDTO(c), adminDTO(d); *gc.CoverageGroup == *gd.CoverageGroup {
			t.Fatalf("coverage groups collapsed: %q", *gc.CoverageGroup)
		}
	})

	t.Run("invalid marker keeps the entry and stores no marker", func(t *testing.T) {
		id := seed("marker-g")
		post("fleet", entry(id, map[string]any{"state": "present", "canary": "yes"}))
		var n int
		var isNull bool
		if err := pool.QueryRow(ctx, `SELECT count(*), bool_and(roll_marker IS NULL) FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&n, &isNull); err != nil || n != 1 || !isNull {
			t.Fatalf("entry rows=%d marker null=%v err=%v", n, isNull, err)
		}
	})

	t.Run("CHECK rejects an oversized marker and a non-object", func(t *testing.T) {
		id := seed("marker-h")
		post("fleet", entry(id, nil))
		big := `{"state":"present","target_version":"` + strings.Repeat("x", 1100) + `"}`
		if _, err := pool.Exec(ctx, `UPDATE worker_upgrade_reports SET roll_marker=$2::jsonb WHERE worker_id=$1`, id, big); err == nil {
			t.Fatal("oversized marker accepted")
		}
		if _, err := pool.Exec(ctx, `UPDATE worker_upgrade_reports SET roll_marker='[1]'::jsonb WHERE worker_id=$1`, id); err == nil {
			t.Fatal("non-object marker accepted")
		}
	})

	t.Run("legacy statement leaves the marker unchanged", func(t *testing.T) {
		id := seed("marker-i")
		post("fleet", entry(id, present))
		legacy, err := os.ReadFile("../store/testdata/worker_roll_health_legacy.sql")
		if err != nil {
			t.Fatal(err)
		}
		var before, after []byte
		var beforeAt, afterAt pgtype.Timestamptz
		q := `SELECT roll_marker, roll_marker_observed_at FROM worker_upgrade_reports WHERE worker_id=$1`
		if err := pool.QueryRow(ctx, q, id).Scan(&before, &beforeAt); err != nil || len(before) == 0 {
			t.Fatalf("before: %v %s", err, before)
		}
		now := pgtype.Timestamptz{Time: receipt, Valid: true}
		res, err := pool.Exec(ctx, string(legacy), "settled", pgtype.Timestamptz{}, pgtype.Text{}, pgtype.Text{},
			pgtype.Text{}, pgtype.Text{}, int32(0), pgtype.Int4{}, now, now, true, pgtype.Text{String: "1.0.0", Valid: true},
			pgtype.Int4{}, false, []byte(nil), id)
		if err != nil || res.RowsAffected() != 1 {
			t.Fatalf("legacy rows=%d err=%v", res.RowsAffected(), err)
		}
		if err := pool.QueryRow(ctx, q, id).Scan(&after, &afterAt); err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) || !beforeAt.Time.Equal(afterAt.Time) {
			t.Fatalf("legacy statement changed the marker: %s -> %s", before, after)
		}
	})
}
