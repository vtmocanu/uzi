package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// This exercises the API seam with the existing Loop fallback wire shape, not
// controller execution. Unknown desired targets must remain diagnostic-only.
func TestControllerStatusPacingUnknownTargetLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, tag string
		arms      bool
	}{
		{"different release", "2.0.0", true},
		{"invalid same-base tag", "1.0.0+_x", true},
		{"same registered release", "1.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, pool := cliLiveDB(t)
			ctx := context.Background()
			user := cliSeedUser(t, pool, false)
			ids := []uuid.UUID{uuid.New(), uuid.New()}
			if ids[1].String() < ids[0].String() {
				ids[0], ids[1] = ids[1], ids[0]
			}
			for _, id := range ids {
				cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared,draining_since,draining_reason) VALUES($1,$2,'unknown-pacing',$3,'offline','hosted','1.0.0','m','unresolvable',now(),'image_roll')`, id, user, []byte(id.String()))
			}
			before := make([]any, len(ids))
			for i, id := range ids {
				w, err := h.q.GetWorkerByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				before[i] = w
			}
			// Exact select/finalize diagnostic for unresolved workers in one
			// affected owner/lane: one complete sample, no inferred target.
			reason := "fleet: multiple migration-held drains; intake risk; operator decision; layout_migration_custody: retained drain; held drains=2; sample=" + ids[0].String()
			if len(reason) > 192 {
				t.Fatal("fixture exceeds controller reason budget")
			}
			want := workersvc.PacingReport{State: "held", BlockingReason: &reason}
			first := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
			receipt := first
			h.now = func() time.Time { return receipt }
			future := first.Add(365 * 24 * time.Hour)
			post := func() {
				t.Helper()
				rows := make([]any, 0, len(ids))
				for _, id := range ids {
					rows = append(rows, map[string]any{
						"id": id.String(), "phase": "rolling", "target_image": "",
						"pacing": map[string]any{"state": "held", "target_image": "", "target_version": "", "blocking_reason": reason},
					})
				}
				body, err := json.Marshal(map[string]any{
					"kind": "fleet", "reported_at": future, "worker_image_tag": tc.tag,
					"poll_interval_seconds": 10, "workers": rows,
				})
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(body))))
				if rec.Code != http.StatusNoContent {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			}
			assertPacing := func(raw []byte, at, anchor pgtype.Timestamptz) {
				t.Helper()
				var got workersvc.PacingReport
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("exact unknown-target diagnostic changed: got=%+v want=%+v JSON=%s", got, want, raw)
				}
				if !at.Valid || !at.Time.Equal(receipt) {
					t.Fatalf("pacing receipt=%v want API time %v (controller time %v)", at, receipt, future)
				}
				if anchor.Valid != tc.arms || (tc.arms && !anchor.Time.Equal(first)) {
					t.Fatalf("first rolling anchor=%v want armed=%v at %v", anchor, tc.arms, first)
				}
			}
			for report := 0; report < 2; report++ {
				receipt = first.Add(time.Duration(report) * time.Minute)
				post()
				for _, id := range ids {
					var raw []byte
					var at, anchor, observed, reported pgtype.Timestamptz
					var target, pod pgtype.Text
					var phase string
					if err := pool.QueryRow(ctx, `SELECT pacing,pacing_observed_at,upgrading_since,observed_at,controller_reported_at,target_image,pod_phase,phase FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&raw, &at, &anchor, &observed, &reported, &target, &pod, &phase); err != nil {
						t.Fatal(err)
					}
					assertPacing(raw, at, anchor)
					if target.Valid || pod.Valid || phase != "rolling" || !observed.Time.Equal(receipt) || !reported.Time.Equal(future) {
						t.Fatalf("fallback row invented pod/target or used controller freshness: target=%v pod=%v phase=%s observed=%v reported=%v", target, pod, phase, observed, reported)
					}
				}
				owned, err := h.q.ListWorkersByUser(ctx, user)
				if err != nil || len(owned) != len(ids) {
					t.Fatalf("owned projection rows=%d err=%v", len(owned), err)
				}
				for _, row := range owned {
					assertPacing(row.RollPacing, row.RollPacingObservedAt, row.RollUpgradingSince)
					dto := workerDTOFromRow(row, "dev", "", receipt, receipt)
					assertPacing(dto.RollPacing, row.RollPacingObservedAt, row.RollUpgradingSince)
				}
				all, err := h.q.ListAllWorkers(ctx)
				if err != nil {
					t.Fatal(err)
				}
				found := 0
				for _, row := range all {
					if row.Worker.UserID != user {
						continue
					}
					found++
					assertPacing(row.RollPacing, row.RollPacingObservedAt, row.RollUpgradingSince)
					dto := workerDTOFromAdminRow(row, "dev", "", receipt, receipt)
					assertPacing(dto.RollPacing, row.RollPacingObservedAt, row.RollUpgradingSince)
				}
				if found != len(ids) {
					t.Fatalf("admin projection missing workers: got=%d want=%d", found, len(ids))
				}
			}
			for i, id := range ids {
				after, err := h.q.GetWorkerByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before[i], after) {
					t.Fatal("diagnostic report changed worker registration or drain authority")
				}
			}
		})
	}
}

func TestControllerStatusPacingPersistenceLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared) VALUES($1,$2,'pacing-handler',$3,'offline','hosted','1.0.0','m','base')`, id, user, []byte(id.String()))
	receipt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h.now = func() time.Time { return receipt }
	post := func(kind string, pacing any) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"kind": kind, "reported_at": receipt.Add(365 * 24 * time.Hour), "workers": []any{map[string]any{"id": id.String(), "phase": "settled", "pacing": pacing}}})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
		if rec.Code != 204 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	hold := map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2.0.0"}
	post("fleet", hold)
	original := receipt
	receipt = receipt.Add(time.Hour)
	post("readiness", nil)
	post("", nil)
	post("fleet", map[string]any{"state": "completed"})
	var state string
	var pacingAt, ordinary time.Time
	if err := pool.QueryRow(context.Background(), `SELECT pacing->>'state',pacing_observed_at,observed_at FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&state, &pacingAt, &ordinary); err != nil {
		t.Fatal(err)
	}
	if state != "held" || !pacingAt.Equal(original) || !ordinary.Equal(receipt) {
		t.Fatalf("state=%s pacingAt=%v ordinary=%v", state, pacingAt, ordinary)
	}
	post("fleet", nil)
	var cleared bool
	if err := pool.QueryRow(context.Background(), `SELECT pacing IS NULL AND pacing_observed_at IS NULL FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("clear=%v err=%v", cleared, err)
	}
}
