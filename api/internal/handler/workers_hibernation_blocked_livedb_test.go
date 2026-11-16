package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// hibBlockedSeedWorker inserts a hosted worker owned by userID and returns its id.
func hibBlockedSeedWorker(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	tokenHash := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, version, last_heartbeat_at)
		 VALUES ($1, $2, $3, $4, 'online', 'hosted', 'm', 'base', '0.83.0', now())`,
		id, userID, name, tokenHash[:])
	return id
}

func hibBlockedFindWorker(t *testing.T, workers []map[string]any, id uuid.UUID) map[string]any {
	t.Helper()
	for _, w := range workers {
		if w["id"] == id.String() {
			return w
		}
	}
	return nil
}

// hibBlockedAssertTime asserts the JSON field is a non-null RFC3339 string equal to want.
func hibBlockedAssertTime(t *testing.T, label string, w map[string]any, key string, want time.Time) {
	t.Helper()
	v, present := w[key]
	if !present {
		t.Errorf("%s: key %q missing", label, key)
		return
	}
	s, ok := v.(string)
	if !ok {
		t.Errorf("%s: %s = %#v, want an RFC3339 string", label, key, v)
		return
	}
	got, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Errorf("%s: %s = %q is not RFC3339: %v", label, key, s, err)
		return
	}
	if !got.Equal(want) {
		t.Errorf("%s: %s = %s, want %s", label, key, got, want)
	}
}

// hibBlockedAssertBlocked checks the three blocker keys (and optionally hibernation_reason).
func hibBlockedAssertBlocked(t *testing.T, label string, w map[string]any, reason string, at time.Time) {
	t.Helper()
	if got := w["hibernation_blocked_reason"]; got != reason {
		t.Errorf("%s: hibernation_blocked_reason = %#v, want %q", label, got, reason)
	}
	hibBlockedAssertTime(t, label, w, "hibernation_blocked_since", at)
	hibBlockedAssertTime(t, label, w, "hibernation_blocked_at", at)
}

func hibBlockedAssertNulls(t *testing.T, label string, w map[string]any) {
	t.Helper()
	for _, k := range []string{"hibernation_blocked_reason", "hibernation_blocked_since", "hibernation_blocked_at"} {
		v, present := w[k]
		if !present {
			t.Errorf("%s: key %q missing (want JSON null)", label, k)
		} else if v != nil {
			t.Errorf("%s: %s = %#v, want null", label, k, v)
		}
	}
}

// TestWorkersHibernationBlockedRoutesLiveDB proves, through the real router, that the owner
// GET /api/workers and the admin GET /api/admin/workers JSON carry hibernation_blocked_reason,
// hibernation_blocked_since and hibernation_blocked_at (written by the real
// store.SetWorkerHibernationBlocked query) when set and JSON null when unset, and that one
// owner's list never exposes another owner's worker or blocker. There is no single-worker
// owner GET route (only PATCH/DELETE/cordon on /workers/{id}), so the list is the owner view.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestWorkersHibernationBlockedRoutesLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	ctx := context.Background()
	q := store.New(pool)

	admin := cliSeedUser(t, pool, true)
	userA := cliSeedUser(t, pool, false)
	userB := cliSeedUser(t, pool, false)
	tokA := cliMintToken(t, pool, userA, clitoken.ScopeUser)
	adminTok := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	blockedA := hibBlockedSeedWorker(t, pool, userA, "a-blocked")
	cleanA := hibBlockedSeedWorker(t, pool, userA, "a-clean")
	blockedB := hibBlockedSeedWorker(t, pool, userB, "b-blocked")

	atA := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Microsecond)
	atB := atA.Add(37 * time.Minute)
	if err := q.SetWorkerHibernationBlocked(ctx, store.SetWorkerHibernationBlockedParams{
		ID: blockedA, Reason: "controller_refused", At: pgconv.Time(atA),
	}); err != nil {
		t.Fatalf("set blocker A: %v", err)
	}
	if err := q.SetWorkerHibernationBlocked(ctx, store.SetWorkerHibernationBlockedParams{
		ID: blockedB, Reason: "manual_cordon", At: pgconv.Time(atB),
	}); err != nil {
		t.Fatalf("set blocker B: %v", err)
	}
	cliMustExec(t, pool, `UPDATE workers SET hibernation_reason = 'roll_reservation' WHERE id = $1`, blockedA)

	decode := func(label, path, tok string) ([]map[string]any, string) {
		rec := bearerReq(router, http.MethodGet, path, tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET %s = %d, want 200\nbody: %s", label, path, rec.Code, rec.Body.String())
		}
		var body struct {
			Workers []map[string]any `json:"workers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s decode: %v (body %s)", label, err, rec.Body.String())
		}
		return body.Workers, rec.Body.String()
	}

	t.Run("owner list", func(t *testing.T) {
		workers, raw := decode("owner A", "/api/workers", tokA)
		a := hibBlockedFindWorker(t, workers, blockedA)
		if a == nil {
			t.Fatalf("A's blocked worker %s absent from owner list", blockedA)
		}
		hibBlockedAssertBlocked(t, "owner A blocked", a, "controller_refused", atA)
		if got := a["hibernation_reason"]; got != "roll_reservation" {
			t.Errorf("owner A blocked: hibernation_reason = %#v, want roll_reservation", got)
		}
		c := hibBlockedFindWorker(t, workers, cleanA)
		if c == nil {
			t.Fatalf("A's unblocked worker %s absent from owner list", cleanA)
		}
		hibBlockedAssertNulls(t, "owner A clean", c)
		if hibBlockedFindWorker(t, workers, blockedB) != nil {
			t.Errorf("owner A's list includes B's worker %s", blockedB)
		}
		// Quoted so the unrelated manual_cordon_* key names do not match, only the value.
		if strings.Contains(raw, `"manual_cordon"`) {
			t.Errorf("owner A's list body leaks B's blocker value: %s", raw)
		}
	})

	t.Run("admin list", func(t *testing.T) {
		workers, _ := decode("admin", "/api/admin/workers", adminTok)
		a := hibBlockedFindWorker(t, workers, blockedA)
		b := hibBlockedFindWorker(t, workers, blockedB)
		c := hibBlockedFindWorker(t, workers, cleanA)
		if a == nil || b == nil || c == nil {
			t.Fatalf("admin list missing workers: A=%v B=%v clean=%v", a != nil, b != nil, c != nil)
		}
		hibBlockedAssertBlocked(t, "admin A", a, "controller_refused", atA)
		if got := a["hibernation_reason"]; got != "roll_reservation" {
			t.Errorf("admin A: hibernation_reason = %#v, want roll_reservation", got)
		}
		hibBlockedAssertBlocked(t, "admin B", b, "manual_cordon", atB)
		hibBlockedAssertNulls(t, "admin clean", c)
	})
}
