package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const recoveryUnlockPassword = "recovery-unlock-password"

type vaultRecoveryEvents struct {
	states, notes map[uuid.UUID]string
}

func (b *vaultRecoveryEvents) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
}
func (b *vaultRecoveryEvents) PublishHealth(uuid.UUID, string, string, bool) {}
func (b *vaultRecoveryEvents) PublishInput(uuid.UUID)                        {}
func (b *vaultRecoveryEvents) PublishState(id uuid.UUID, status string) {
	if _, exists := b.states[id]; exists {
		panic("duplicate state publication")
	}
	b.states[id] = status
}
func (b *vaultRecoveryEvents) Notify(id uuid.UUID, status string) {
	if _, exists := b.notes[id]; exists {
		panic("duplicate lifecycle publication")
	}
	b.notes[id] = status
}

func vaultRecoveryHandler(t *testing.T, e *recoveryEnv, createVault bool) (*Handler, *fakeCodexPoker, *vaultRecoveryEvents) {
	t.Helper()
	h := e.handler()
	h.vault = vault.New(e.box, h.q)
	h.wsvc.SetVault(h.vault)
	poker := &fakeCodexPoker{}
	h.SetCodexUsagePoker(poker)
	events := &vaultRecoveryEvents{states: map[uuid.UUID]string{}, notes: map[uuid.UUID]string{}}
	h.wsvc.SetBroadcaster(events)
	h.wsvc.SetLifecycle(events)
	if createVault {
		if err := h.vault.Unlock(e.ctx, e.user, recoveryUnlockPassword); err != nil {
			t.Fatal(err)
		}
		h.vault.Lock(e.user)
	}
	return h, poker, events
}

func vaultRecoveryUnlock(h *Handler, ctx context.Context, uid uuid.UUID, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/vault/unlock", strings.NewReader(string(body)))
	req = req.WithContext(mw.ContextWithUser(ctx, store.User{ID: uid, IsActive: true}))
	rec := httptest.NewRecorder()
	h.VaultUnlock(rec, req)
	return rec
}

func vaultRecoveryRow(t *testing.T, e *recoveryEnv, id uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(e.ctx, "SELECT to_jsonb(r) FROM runs r WHERE id=$1", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

// Seed distinct old reset values and non-default preserved history. The whole-row
// comparison below also catches accidental resets beyond the named affinity fields.
func seedVaultRecoveryRow(t *testing.T, e *recoveryEnv, uid uuid.UUID, status string, cause any, deadline any, index int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecT(e.ctx, t, e.pool, `INSERT INTO runs
		(id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
		 recovery_wait_cause, recovery_retry_not_before, recovery_wait_count,
		 worker_id, session_id, last_seq, claim_generation, claim_released_at,
		 started_at, budget_paused_seconds, codex_cap_hash, codex_claim_epoch,
		 health, health_reason, health_since, status_since, updated_at)
		VALUES ($1,$2,$3,'issue',$4,'vault recovery','context',$5,$6,$7,$8,
		 $9,$10,$11,$12,'2002-03-04 05:06:07+00',
		 '2003-04-05 06:07:08+00',$13,decode('1234abcd','hex'),$14,
		 'stalled',$15,'2004-05-06 07:08:09+00','2005-06-07 08:09:10+00','2006-07-08 09:10:11+00')`,
		id, uid, e.repo, 2000+index, status, cause, deadline, 100000+index,
		e.worker.ID, fmt.Sprintf("session-%d", index), 41+index, 71+index, 91+index, 111+index, fmt.Sprintf("old-health-%d", index))
	return id
}

func assertVaultRecoveryPromoted(t *testing.T, before, after map[string]any, lower, upper time.Time) {
	t.Helper()
	want := map[string]any{
		"status": "queued", "started_at": nil, "budget_paused_seconds": float64(0),
		"codex_cap_hash": nil, "codex_claim_epoch": before["codex_claim_epoch"].(float64) + 1,
		"health": "ok", "health_reason": nil, "health_since": nil,
	}
	for key, value := range want {
		if !reflect.DeepEqual(after[key], value) {
			t.Errorf("%s=%v want=%v", key, after[key], value)
		}
	}
	for _, key := range []string{"status_since", "updated_at"} {
		stamp, err := time.Parse(time.RFC3339Nano, after[key].(string))
		if err != nil || stamp.Before(lower) || stamp.After(upper) {
			t.Errorf("%s=%v outside query window [%v,%v]: %v", key, after[key], lower, upper, err)
		}
		want[key] = after[key]
	}
	for key, value := range before {
		if _, reset := want[key]; !reset && !reflect.DeepEqual(after[key], value) {
			t.Errorf("preserved %s=%v want=%v", key, after[key], value)
		}
	}
}

// Removing the successful VaultUnlock trigger must fail this named endpoint test.
func TestVaultUnlockPromotesRecoveryWaitRunsLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	h, poker, events := vaultRecoveryHandler(t, e, true)
	foreign := uuid.New()
	mustExecT(e.ctx, t, e.pool, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", foreign, foreign.String()+"@vault-recovery.test")
	future := time.Now().Add(24 * time.Hour)
	eligible := []uuid.UUID{
		seedVaultRecoveryRow(t, e, e.user, "recovery_wait", "vault_locked", future, 1),
		seedVaultRecoveryRow(t, e, e.user, "recovery_wait", "vault_locked", nil, 2),
	}
	untouched := []uuid.UUID{e.run, seedVaultRecoveryRow(t, e, foreign, "recovery_wait", "vault_locked", nil, 3)}
	for i, cause := range []any{nil, "forge_unreachable", "empty_turn", "provider_outage", "codex_account_unavailable", "data_volume_full"} {
		untouched = append(untouched, seedVaultRecoveryRow(t, e, e.user, "recovery_wait", cause, future, 10+i))
	}
	for i, status := range []string{"queued", "running", "paused", "failed", "completed"} {
		untouched = append(untouched, seedVaultRecoveryRow(t, e, e.user, status, "vault_locked", future, 20+i))
	}
	before := map[uuid.UUID]map[string]any{}
	for _, id := range append(eligible, untouched...) {
		before[id] = vaultRecoveryRow(t, e, id)
	}
	var lower time.Time
	if err := e.pool.QueryRow(e.ctx, "SELECT clock_timestamp()").Scan(&lower); err != nil {
		t.Fatal(err)
	}
	rec := vaultRecoveryUnlock(h, e.ctx, e.user, recoveryUnlockPassword)
	if rec.Code != http.StatusNoContent || poker.count(e.user) != 1 {
		t.Fatalf("unlock=%d body=%s poker=%d", rec.Code, rec.Body.String(), poker.count(e.user))
	}
	var upper time.Time
	if err := e.pool.QueryRow(e.ctx, "SELECT clock_timestamp()").Scan(&upper); err != nil {
		t.Fatal(err)
	}
	for _, id := range eligible {
		assertVaultRecoveryPromoted(t, before[id], vaultRecoveryRow(t, e, id), lower, upper)
		if events.states[id] != "queued" || events.notes[id] != "queued" {
			t.Fatalf("missing synchronous publication for %s: %+v", id, events)
		}
	}
	for _, id := range untouched {
		if after := vaultRecoveryRow(t, e, id); !reflect.DeepEqual(after, before[id]) {
			t.Fatalf("ineligible row %s changed: before=%v after=%v", id, before[id], after)
		}
	}
	if len(events.states) != len(eligible) || len(events.notes) != len(eligible) {
		t.Fatalf("unexpected publication: %+v", events)
	}
	// Repeated unlock has no matching rows and cannot duplicate publication or history.
	afterFirst := map[uuid.UUID]map[string]any{}
	for _, id := range eligible {
		afterFirst[id] = vaultRecoveryRow(t, e, id)
	}
	if rec := vaultRecoveryUnlock(h, e.ctx, e.user, recoveryUnlockPassword); rec.Code != http.StatusNoContent || poker.count(e.user) != 2 {
		t.Fatalf("repeat unlock=%d poker=%d", rec.Code, poker.count(e.user))
	}
	for _, id := range eligible {
		if !reflect.DeepEqual(afterFirst[id], vaultRecoveryRow(t, e, id)) {
			t.Fatalf("repeat unlock changed %s", id)
		}
	}
}

func TestVaultUnlockFailureLeavesRecoveryWaitRunsLiveDB(t *testing.T) {
	for _, mode := range []string{"wrong-password", "no-vault"} {
		t.Run(mode+"LiveDB", func(t *testing.T) {
			e := newRecoveryEnv(t)
			h, poker, events := vaultRecoveryHandler(t, e, mode == "wrong-password")
			id := seedVaultRecoveryRow(t, e, e.user, "recovery_wait", "vault_locked", nil, 1)
			before := vaultRecoveryRow(t, e, id)
			rec := vaultRecoveryUnlock(h, e.ctx, e.user, "wrong-password")
			if rec.Code != http.StatusForbidden || !reflect.DeepEqual(before, vaultRecoveryRow(t, e, id)) ||
				poker.count(e.user) != 0 || len(events.states) != 0 || len(events.notes) != 0 || h.vault.Unlocked(e.user) {
				t.Fatalf("failed unlock=%d body=%s poker=%d events=%+v", rec.Code, rec.Body.String(), poker.count(e.user), events)
			}
		})
	}
}

func TestVaultUnlockNoMatchingRecoveryWaitRunsLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	h, poker, events := vaultRecoveryHandler(t, e, true)
	before := vaultRecoveryRow(t, e, e.run)
	rec := vaultRecoveryUnlock(h, e.ctx, e.user, recoveryUnlockPassword)
	if rec.Code != http.StatusNoContent || poker.count(e.user) != 1 ||
		len(events.states) != 0 || len(events.notes) != 0 || !reflect.DeepEqual(before, vaultRecoveryRow(t, e, e.run)) {
		t.Fatalf("empty unlock=%d poker=%d events=%+v", rec.Code, poker.count(e.user), events)
	}
}

// Use a real connection and row-lock fixture, never a production fake query seam.
// The pg_stat_activity observation proves the request reached the UPDATE and is
// blocked there before we inspect response completion.
func TestVaultUnlockPromotionSynchronousAndFailuresLiveDB(t *testing.T) {
	for _, mode := range []string{"synchronous", "query-error", "timeout"} {
		t.Run(mode+"LiveDB", func(t *testing.T) {
			e := newRecoveryEnv(t)
			h, poker, events := vaultRecoveryHandler(t, e, true)
			id := seedVaultRecoveryRow(t, e, e.user, "recovery_wait", "vault_locked", nil, 1)
			before := vaultRecoveryRow(t, e, id)
			conn, err := pgx.Connect(e.ctx, e.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close(context.Background()) })
			if mode == "query-error" {
				if _, err := conn.Exec(e.ctx, "SET statement_timeout = '1s'"); err != nil {
					t.Fatal(err)
				}
			}
			h.wsvc = workersvc.New(store.New(conn), e.box, workersvc.Params{})
			h.wsvc.SetVault(h.vault)
			h.wsvc.SetBroadcaster(events)
			h.wsvc.SetLifecycle(events)
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(e.ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", id); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			// An outer bound prevents a broken child timeout from hanging the test.
			ctx, cancel := context.WithTimeout(e.ctx, 12*time.Second)
			defer cancel()
			start := time.Now()
			go func() { done <- vaultRecoveryUnlock(h, ctx, e.user, recoveryUnlockPassword) }()
			// At most 200 observations over 4 seconds; a failure ends this subtest.
			blocked := false
			for attempts := 0; attempts < 200 && time.Since(start) < 4*time.Second; attempts++ {
				if err := e.pool.QueryRow(e.ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')",
					conn.PgConn().PID()).Scan(&blocked); err != nil {
					cancel()
					<-done
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case rec := <-done:
					t.Fatalf("response completed before blocked UPDATE: %d", rec.Code)
				case <-time.After(20 * time.Millisecond):
				}
			}
			if !blocked {
				cancel()
				<-done
				t.Fatal("did not observe promotion waiting on the seeded row lock")
			}
			select {
			case rec := <-done:
				t.Fatalf("response completed while UPDATE blocked: %d", rec.Code)
			default:
			}
			if mode == "synchronous" {
				if err := tx.Rollback(e.ctx); err != nil {
					cancel()
					<-done
					t.Fatal(err)
				}
			}
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-time.After(8 * time.Second):
				cancel()
				<-done
				t.Fatal("promotion exceeded its five-second SQL bound")
			}
			if rec.Code != http.StatusNoContent || poker.count(e.user) != 1 || !h.vault.Unlocked(e.user) {
				t.Fatalf("unlock=%d body=%s poker=%d", rec.Code, rec.Body.String(), poker.count(e.user))
			}
			after := vaultRecoveryRow(t, e, id)
			if mode == "synchronous" {
				if after["status"] != "queued" || events.states[id] != "queued" || events.notes[id] != "queued" {
					t.Fatalf("response preceded promotion/publication: row=%v events=%+v", after, events)
				}
			} else {
				if !reflect.DeepEqual(before, after) || len(events.states) != 0 || len(events.notes) != 0 {
					t.Fatalf("failed query changed row/published: row=%v events=%+v", after, events)
				}
				if mode == "timeout" && (time.Since(start) < 4*time.Second || time.Since(start) > 7*time.Second) {
					t.Fatalf("timeout duration=%v, want five-second child bound", time.Since(start))
				}
			}
		})
	}
}
