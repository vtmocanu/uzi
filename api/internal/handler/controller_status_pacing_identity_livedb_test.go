package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pacingLiveRow is the persisted slice of worker_upgrade_reports these tests assert on.
type pacingLiveRow struct {
	ObservedAt         time.Time
	Phase              string
	BlockingContainer  *string
	BlockingReason     *string
	RestartCount       int32
	MarkerState        *string
	MarkerObservedAt   *time.Time
	PacingNull         bool
	PacingState        *string
	PacingReason       *string
	PacingObservedAt   *time.Time
	HasBlockingWorker  bool
	HasCanary          bool
	CanaryID           *string
	BlockingWorkerID   *string
	PacingObservedNull bool
}

func pacingLiveSeedHosted(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared) VALUES($1,$2,$3,$4,'offline','hosted','1.0.0','m','base')`, id, user, name, []byte(id.String()))
	return id
}

func pacingLivePost(t *testing.T, h *Handler, kind string, rows ...any) int {
	t.Helper()
	data, err := json.Marshal(map[string]any{"kind": kind, "reported_at": h.now().Add(365 * 24 * time.Hour), "workers": rows})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
	return rec.Code
}

// pacingLiveFetch returns the row, or nil when no worker_upgrade_reports row exists.
func pacingLiveFetch(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) *pacingLiveRow {
	t.Helper()
	var r pacingLiveRow
	rows, err := pool.Query(context.Background(), `SELECT observed_at, phase, blocking_container, blocking_reason, restart_count,
		roll_marker->>'state', roll_marker_observed_at,
		pacing IS NULL, pacing->>'state', pacing->>'blocking_reason', pacing_observed_at,
		COALESCE(pacing ? 'blocking_worker_id', false), COALESCE(pacing ? 'canary_id', false),
		pacing->>'canary_id', pacing->>'blocking_worker_id', pacing_observed_at IS NULL
		FROM worker_upgrade_reports WHERE worker_id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	if err := rows.Scan(&r.ObservedAt, &r.Phase, &r.BlockingContainer, &r.BlockingReason, &r.RestartCount,
		&r.MarkerState, &r.MarkerObservedAt, &r.PacingNull, &r.PacingState, &r.PacingReason, &r.PacingObservedAt,
		&r.HasBlockingWorker, &r.HasCanary, &r.CanaryID, &r.BlockingWorkerID, &r.PacingObservedNull); err != nil {
		t.Fatal(err)
	}
	return &r
}

func pacingLiveStuckRow(id uuid.UUID, pacing any) map[string]any {
	e := map[string]any{
		"id": id.String(), "phase": "stuck", "target_image": "repo/agent-base:0.11.7", "pod_phase": "Pending",
		"blocking_container": "seed-nix", "blocking_reason": "CrashLoopBackOff", "restart_count": 6,
		"roll_marker": map[string]any{"state": "present", "canary": true, "completed": true, "target_version": "0.11.7", "coverage_group": "0123456789abcdef", "reservation": true},
	}
	if pacing != nil {
		e["pacing"] = pacing
	}
	return e
}

const pacingLiveReason = "participant reservation without canary; explicit recovery required"

func pacingLiveHeld(extra map[string]any) map[string]any {
	p := map[string]any{"state": "held", "target_image": "repo/agent-base:0.11.7", "target_version": "0.11.7", "blocking_reason": pacingLiveReason}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// A malformed optional pacing diagnostic must never cost the row its health, blocking
// detail or roll marker (issue #4467).
func TestControllerStatusInvalidPacingIdentityPreservesHealthLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	receipt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h.now = func() time.Time { return receipt }

	// assertHealth checks everything that must land regardless of the pacing outcome.
	assertHealth := func(t *testing.T, r *pacingLiveRow) {
		t.Helper()
		if r == nil {
			t.Fatal("no worker_upgrade_reports row: the entry was dropped")
		}
		if !r.ObservedAt.Equal(receipt) || r.Phase != "stuck" || r.RestartCount != 6 ||
			r.BlockingContainer == nil || *r.BlockingContainer != "seed-nix" ||
			r.BlockingReason == nil || *r.BlockingReason != "CrashLoopBackOff" {
			t.Fatalf("health not persisted: %+v", r)
		}
		if r.MarkerState == nil || *r.MarkerState != "present" || r.MarkerObservedAt == nil || !r.MarkerObservedAt.Equal(receipt) {
			t.Fatalf("roll marker not persisted: %+v", r)
		}
	}
	post := func(t *testing.T, id uuid.UUID, pacing any) *pacingLiveRow {
		t.Helper()
		if code := pacingLivePost(t, h, "fleet", pacingLiveStuckRow(id, pacing)); code != http.StatusNoContent {
			t.Fatalf("status %d", code)
		}
		r := pacingLiveFetch(t, pool, id)
		assertHealth(t, r)
		return r
	}
	keptHeld := func(t *testing.T, r *pacingLiveRow) {
		t.Helper()
		if r.PacingNull || r.PacingState == nil || *r.PacingState != "held" || r.PacingReason == nil || *r.PacingReason != pacingLiveReason ||
			r.PacingObservedAt == nil || !r.PacingObservedAt.Equal(receipt) {
			t.Fatalf("pacing state/reason not retained: %+v", r)
		}
	}

	t.Run("invalid blocking_worker_id is omitted alone", func(t *testing.T) {
		id := pacingLiveSeedHosted(t, pool, user, "id-main")
		r := post(t, id, pacingLiveHeld(map[string]any{"blocking_worker_id": "missing-canary"}))
		keptHeld(t, r)
		if r.HasBlockingWorker {
			t.Fatal("synthetic blocking_worker_id persisted")
		}
	})

	t.Run("invalid canary_id is omitted alone", func(t *testing.T) {
		id := pacingLiveSeedHosted(t, pool, user, "id-canary")
		blocker := uuid.NewString()
		r := post(t, id, pacingLiveHeld(map[string]any{"canary_id": "missing-canary", "blocking_worker_id": blocker}))
		keptHeld(t, r)
		if r.HasCanary || r.BlockingWorkerID == nil || *r.BlockingWorkerID != blocker {
			t.Fatalf("canary must be omitted and the valid blocker kept: %+v", r)
		}
	})

	t.Run("non-string identifiers are omitted", func(t *testing.T) {
		for name, v := range map[string]any{"number": 5, "object": map[string]any{}, "array": []any{}, "bool": true} {
			id := pacingLiveSeedHosted(t, pool, user, "id-type-"+name)
			r := post(t, id, pacingLiveHeld(map[string]any{"canary_id": v, "blocking_worker_id": v}))
			keptHeld(t, r)
			if r.HasCanary || r.HasBlockingWorker {
				t.Fatalf("%s: non-string identifier persisted: %+v", name, r)
			}
		}
	})

	t.Run("both identifiers invalid", func(t *testing.T) {
		id := pacingLiveSeedHosted(t, pool, user, "id-both")
		r := post(t, id, pacingLiveHeld(map[string]any{"canary_id": "x", "blocking_worker_id": 7}))
		keptHeld(t, r)
		if r.HasCanary || r.HasBlockingWorker {
			t.Fatalf("identifiers persisted: %+v", r)
		}
	})

	t.Run("valid canary kept exactly when the blocker is invalid", func(t *testing.T) {
		id := pacingLiveSeedHosted(t, pool, user, "id-canary-kept")
		canary := uuid.NewString()
		r := post(t, id, pacingLiveHeld(map[string]any{"canary_id": canary, "blocking_worker_id": "missing-canary"}))
		keptHeld(t, r)
		if r.CanaryID == nil || *r.CanaryID != canary || r.HasBlockingWorker {
			t.Fatalf("canary must be kept exactly and the blocker omitted: %+v", r)
		}
	})

	t.Run("invalid content discards pacing and clears the fleet value", func(t *testing.T) {
		cases := map[string]any{
			"string":            "str",
			"array":             []any{},
			"number":            5,
			"state number":      map[string]any{"state": 5},
			"image object":      pacingLiveHeld(map[string]any{"target_image": map[string]any{}}),
			"unknown enum":      map[string]any{"state": "completed"},
			"oversize reason":   pacingLiveHeld(map[string]any{"blocking_reason": strings.Repeat("x", 193)}),
			"oversize image":    pacingLiveHeld(map[string]any{"target_image": strings.Repeat("x", 513)}),
			"oversize version":  pacingLiveHeld(map[string]any{"target_version": strings.Repeat("x", 129)}),
			"retained oversize": pacingLiveHeld(map[string]any{"retained_image": strings.Repeat("x", 513)}),
		}
		for name, pacing := range cases {
			id := pacingLiveSeedHosted(t, pool, user, "content-"+name)
			// A previously stored pacing must be cleared by the fleet report.
			if code := pacingLivePost(t, h, "fleet", pacingLiveStuckRow(id, pacingLiveHeld(nil))); code != http.StatusNoContent {
				t.Fatalf("%s: seed status %d", name, code)
			}
			if r := pacingLiveFetch(t, pool, id); r == nil || r.PacingNull {
				t.Fatalf("%s: seed pacing missing", name)
			}
			r := post(t, id, pacing)
			if !r.PacingNull || !r.PacingObservedNull {
				t.Fatalf("%s: pacing must be NULL with NULL pacing_observed_at: %+v", name, r)
			}
		}
	})

	t.Run("a malformed row does not block valid siblings", func(t *testing.T) {
		bad := pacingLiveSeedHosted(t, pool, user, "sib-bad")
		ok1 := pacingLiveSeedHosted(t, pool, user, "sib-1")
		ok2 := pacingLiveSeedHosted(t, pool, user, "sib-2")
		code := pacingLivePost(t, h, "fleet",
			pacingLiveStuckRow(ok1, pacingLiveHeld(nil)),
			pacingLiveStuckRow(bad, "garbage"),
			pacingLiveStuckRow(ok2, pacingLiveHeld(map[string]any{"blocking_worker_id": "missing-canary"})))
		if code != http.StatusNoContent {
			t.Fatalf("status %d", code)
		}
		for _, id := range []uuid.UUID{bad, ok1, ok2} {
			assertHealth(t, pacingLiveFetch(t, pool, id))
		}
		if r := pacingLiveFetch(t, pool, bad); !r.PacingNull {
			t.Fatal("malformed pacing persisted")
		}
		keptHeld(t, pacingLiveFetch(t, pool, ok1))
		keptHeld(t, pacingLiveFetch(t, pool, ok2))
	})

	t.Run("invalid base identity or phase still produces no row", func(t *testing.T) {
		unknownPhase := pacingLiveSeedHosted(t, pool, user, "base-phase")
		row := pacingLiveStuckRow(unknownPhase, pacingLiveHeld(nil))
		row["phase"] = "bogus-phase"
		badID := pacingLiveStuckRow(uuid.New(), pacingLiveHeld(nil))
		badID["id"] = "not-a-uuid"
		if code := pacingLivePost(t, h, "fleet", row, badID); code != http.StatusNoContent {
			t.Fatalf("status %d", code)
		}
		if pacingLiveFetch(t, pool, unknownPhase) != nil {
			t.Fatal("entry with an unmodelled phase was persisted")
		}
	})
}
