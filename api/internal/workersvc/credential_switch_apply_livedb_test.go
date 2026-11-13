package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4201: the in-place credential switch apply, executed against a real Postgres (skipped unless
// UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh).

var applyIIDSeq int64 = 800000

const applyFixtureNonce = "nonce-apply"

type applyFx struct {
	env      codexTestEnv
	svc      *Service
	o        reevalOwner
	gen      int64
	newTok   uuid.UUID
	newPlain string
}

// newApplyFx seeds an owner whose worker advertises the in-place capability and whose runs pin the
// override to a token whose plaintext is distinct from every other token's.
func newApplyFx(t *testing.T) applyFx {
	t.Helper()
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	env.exec(`UPDATE workers SET protocol_capabilities = $2, last_heartbeat_at = now(),
	             dind_register_floor = now() - interval '1 hour', snapshot_register_nonce = $3, snapshot_epoch = 0
	          WHERE id = $1`, o.workerID, []string{capability.CredentialSwitchV1, capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1}, applyFixtureNonce)
	svc := fenceSvc(env)
	svc.SetVault(env.vlt)
	f := applyFx{env: env, svc: svc, o: o, gen: 7}
	f.newPlain = "applied-token-" + uuid.NewString()
	f.newTok = f.insertToken(t, "new-"+uuid.NewString(), f.newPlain)
	return f
}

func (f applyFx) insertToken(t *testing.T, label, plain string) uuid.UUID {
	t.Helper()
	sealed, err := f.env.box.Seal([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	f.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
	            VALUES ($1, $2, 'anthropic_token', $3, $4, 'master')`, id, f.o.userID, label, sealed)
	return id
}

func (f applyFx) worker(t *testing.T) store.Worker { return wkrRow(t, f.env, f.o.workerID) }

// run seeds a running run at f.gen pinned to f.newTok with a switch stamp pending (its op id minted
// by the trigger), and returns the run and the op id.
func (f applyFx) run(t *testing.T, status string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	id := seedHeldRun(t, f.env, f.o, atomic.AddInt64(&applyIIDSeq, 1), status, f.gen, false, false)
	f.env.exec(`UPDATE runs SET credential_override_mode = 'pinned', credential_override_secret_id = $2,
	               session_id = 'sess-1', last_seq = 5 WHERE id = $1`, id, f.newTok)
	for seq := 1; seq <= 5; seq++ {
		f.env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload, claim_generation) VALUES ($1, $2, 'text', '{"t":"x"}', $3)`, id, seq, f.gen)
	}
	return id, f.restamp(t, id)
}

// restamp (re)writes the pending stamp at f.gen with a strictly later instant, so the trigger mints a
// new op id, and returns it.
func (f applyFx) restamp(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	f.env.exec(`UPDATE runs SET credential_switch_requested_at = COALESCE(credential_switch_requested_at, now()) + interval '1 second',
	               credential_switch_generation = $2 WHERE id = $1`, id, f.gen)
	return f.opOf(t, id)
}

func (f applyFx) opOf(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	var op *uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT credential_switch_op_id FROM runs WHERE id = $1`, id).Scan(&op); err != nil || op == nil {
		t.Fatalf("read op id: %v (nil=%v)", err, op == nil)
	}
	return *op
}

func (f applyFx) req(op uuid.UUID) CredentialSwitchApplyRequest {
	return CredentialSwitchApplyRequest{ClaimGeneration: f.gen, RegisterNonce: applyFixtureNonce, OpID: op.String()}
}

func (f applyFx) apply(t *testing.T, run uuid.UUID, req CredentialSwitchApplyRequest) CredentialSwitchApplyResult {
	t.Helper()
	res, err := f.svc.ApplyCredentialSwitch(f.env.ctx, f.worker(t), run, req)
	if err != nil {
		t.Fatalf("ApplyCredentialSwitch: %v", err)
	}
	return res
}

// snap is a byte-comparable image of everything an apply may write for the run.
func (f applyFx) snap(t *testing.T, run uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT jsonb_build_object(
	    'run', (SELECT to_jsonb(r) FROM runs r WHERE r.id = $1),
	    'switch_epochs', (SELECT count(*) FROM run_credential_switch_epochs WHERE run_id = $1),
	    'base_epochs', (SELECT count(*) FROM run_credential_epochs WHERE run_id = $1),
	    'messages', (SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.seq), '[]'::jsonb) FROM run_messages m WHERE m.run_id = $1)
	)::text`, run).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// noWrite runs fn and requires the run's rows unchanged.
func (f applyFx) noWrite(t *testing.T, run uuid.UUID, fn func()) {
	t.Helper()
	before := f.snap(t, run)
	fn()
	if after := f.snap(t, run); after != before {
		t.Fatalf("the run changed:\nbefore %s\nafter  %s", before, after)
	}
}

func (f applyFx) want(t *testing.T, res CredentialSwitchApplyResult, outcome, reason string) {
	t.Helper()
	if res.Outcome != outcome || (reason != "" && res.Reason != reason) {
		t.Fatalf("outcome = %q reason %q, want %q %q", res.Outcome, res.Reason, outcome, reason)
	}
}

func TestApplyCredentialSwitchAppliedLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	res := f.apply(t, run, f.req(op))
	f.want(t, res, ApplyOutcomeApplied, "")
	if res.Token != f.newPlain || res.Ordinal != 1 || res.MessageSeq != 6 || res.Replay {
		t.Fatalf("result = %+v, want the new token at ordinal 1, message seq 6, not a replay", res)
	}
	r := mustRun(t, f.env, run)
	if r.CredentialSwitchRequestedAt.Valid || r.CredentialSwitchGeneration.Valid || r.CredentialSwitchOpID.Valid {
		t.Fatalf("the stamp must be withdrawn: %+v", r.CredentialSwitchRequestedAt)
	}
	if !r.AnthropicSecretID.Valid || uuid.UUID(r.AnthropicSecretID.Bytes) != f.newTok || r.LastSeq != 6 || r.Status != "running" || r.ClaimReleasedAt.Valid {
		t.Fatalf("run after apply: secret=%v last_seq=%d status=%s released=%v", r.AnthropicSecretID, r.LastSeq, r.Status, r.ClaimReleasedAt.Valid)
	}
	var epochs, msgs int
	var seq int32
	var payload []byte
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (SELECT count(*) FROM run_credential_switch_epochs WHERE run_id = $1 AND ordinal = 1 AND op_id = $2 AND message_seq = 6 AND secret_id = $3),
	       (SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = 'credential_switch'),
	       (SELECT seq FROM run_messages WHERE run_id = $1 AND kind = 'credential_switch'),
	       (SELECT payload FROM run_messages WHERE run_id = $1 AND kind = 'credential_switch')`, run, op, f.newTok).Scan(&epochs, &msgs, &seq, &payload); err != nil {
		t.Fatal(err)
	}
	if epochs != 1 || msgs != 1 || seq != 6 {
		t.Fatalf("epochs=%d messages=%d seq=%d, want 1 epoch, 1 message at seq 6", epochs, msgs, seq)
	}
	if strings.Contains(string(payload), f.newPlain) {
		t.Fatalf("the feed message must never carry the token: %s", payload)
	}
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil || p["ordinal"] != float64(1) || p["secret_id"] != f.newTok.String() {
		t.Fatalf("message payload = %s (err %v), want ordinal 1 and the secret id", payload, err)
	}
}

func TestApplyCredentialSwitchReplayLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	first := f.apply(t, run, f.req(op))
	f.noWrite(t, run, func() {
		again := f.apply(t, run, f.req(op))
		f.want(t, again, ApplyOutcomeApplied, "")
		if !again.Replay || again.Token != first.Token || again.Ordinal != first.Ordinal || again.MessageSeq != first.MessageSeq {
			t.Fatalf("replay = %+v, want the same bytes, ordinal and seq as %+v", again, first)
		}
	})
}

func TestApplyCredentialSwitchRefusedCommittedLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f applyFx)
	}{
		{"enablement rev bump", func(f applyFx) {
			f.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, f.newTok)
		}},
		{"disabled", func(f applyFx) { f.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, f.newTok) }},
		{"rejected", func(f applyFx) {
			f.env.exec(`UPDATE user_secrets SET anthropic_rejected_at = now() WHERE id = $1`, f.newTok)
		}},
		{"deleted", func(f applyFx) { f.env.exec(`DELETE FROM user_secrets WHERE id = $1`, f.newTok) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApplyFx(t)
			run, op := f.run(t, "running")
			f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeApplied, "")
			tc.mutate(f)
			f.noWrite(t, run, func() {
				res := f.apply(t, run, f.req(op))
				f.want(t, res, ApplyOutcomeRefusedCommitted, "")
				if res.Token != "" {
					t.Fatal("a refused replay must carry no token")
				}
			})
		})
	}
}

func TestApplyCredentialSwitchNotAppliedLiveDB(t *testing.T) {
	foreignUser := uuid.New()
	for _, tc := range []struct {
		name   string
		reason string
		setup  func(t *testing.T, f applyFx, run uuid.UUID)
	}{
		{"rejected", applyReasonCredentialRejected, func(t *testing.T, f applyFx, _ uuid.UUID) {
			f.env.exec(`UPDATE user_secrets SET anthropic_rejected_at = now() WHERE id = $1`, f.newTok)
		}},
		{"disabled", applyReasonCredentialDisabled, func(t *testing.T, f applyFx, _ uuid.UUID) {
			f.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, f.newTok)
		}},
		{"foreign owner", applyReasonCredentialUnavail, func(t *testing.T, f applyFx, run uuid.UUID) {
			f.env.exec(`INSERT INTO users (id, email, password_hash, vault_mode) VALUES ($1, $2, 'x', 'unprotected')`, foreignUser, "foreign-"+foreignUser.String()+"@e2e")
			other := f.env.seedAnthropicSecret(t, foreignUser, "theirs", false)
			f.env.exec(`UPDATE runs SET credential_override_secret_id = $2 WHERE id = $1`, run, other)
		}},
		{"wrong kind", applyReasonCredentialUnavail, func(t *testing.T, f applyFx, run uuid.UUID) {
			sealed, err := f.env.box.Seal([]byte("not-anthropic"))
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.New()
			f.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with) VALUES ($1, $2, 'openai_api_key', $3, $4, 'master')`, id, f.o.userID, "oa-"+id.String(), sealed)
			f.env.exec(`UPDATE runs SET credential_override_secret_id = $2 WHERE id = $1`, run, id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApplyFx(t)
			run, op := f.run(t, "running")
			tc.setup(t, f, run)
			f.noWrite(t, run, func() {
				res := f.apply(t, run, f.req(op))
				f.want(t, res, ApplyOutcomeNotApplied, tc.reason)
				if res.Token != "" {
					t.Fatal("a not_applied outcome must carry no token")
				}
			})
			if uuid.UUID(mustRun(t, f.env, run).CredentialSwitchOpID.Bytes) != op {
				t.Fatal("a not_applied outcome must leave the stamp pending")
			}
		})
	}
}

// TestApplyCredentialSwitchSealedWithModesLiveDB covers both stored forms of a token: a legacy
// master-sealed row, and a vault-DEK row that opens while the owner's vault is unlocked and is
// reported vault_locked (not_applied, nothing written) once it is locked.
func TestApplyCredentialSwitchSealedWithModesLiveDB(t *testing.T) {
	f := newApplyFx(t)
	credVaultEnrollment(t, f.env, f.o.userID)
	run, op := f.run(t, "running")
	// Enabling the vault converts the owner's existing rows to DEK sealing and leaves it unlocked.
	if err := f.env.vlt.Enable(f.env.ctx, f.o.userID, credVaultPassword); err != nil {
		t.Fatalf("enable vault: %v", err)
	}
	var sealedWith string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT sealed_with FROM user_secrets WHERE id = $1`, f.newTok).Scan(&sealedWith); err != nil || sealedWith != store.SealedWithDEK {
		t.Fatalf("sealed_with = %q (err %v), want dek after enabling the vault", sealedWith, err)
	}
	f.env.vlt.Lock(f.o.userID)
	f.noWrite(t, run, func() {
		f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeNotApplied, applyReasonVaultLocked)
	})
	if err := f.env.vlt.UnlockExisting(f.env.ctx, f.o.userID, credVaultPassword); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	res := f.apply(t, run, f.req(op))
	f.want(t, res, ApplyOutcomeApplied, "")
	if res.Token != f.newPlain {
		t.Fatalf("a dek-sealed token must open to its plaintext when unlocked, got %q", res.Token)
	}
}

// TestApplyCredentialSwitchRotationBlocksUntilCommitLiveDB: the apply holds the token row FOR SHARE
// from its read to its commit, so a concurrent rotation waits, the bytes returned are the
// pre-rotation bytes, and the same operation replayed afterwards is refused_committed.
func TestApplyCredentialSwitchRotationBlocksUntilCommitLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	rotated := make(chan error, 1)
	var rotationFinishedInsideApply atomic.Bool
	f.svc.afterApplySecretLock = func() {
		sealed, err := f.env.box.Seal([]byte("rotated-" + uuid.NewString()))
		if err != nil {
			rotated <- err
			return
		}
		go func() {
			_, err := f.env.q.RotateUserSecret(f.env.ctx, store.RotateUserSecretParams{ID: f.newTok, UserID: f.o.userID, Ciphertext: sealed, SealedWith: "master"})
			rotated <- err
		}()
		select {
		case rerr := <-rotated:
			_ = rerr
			rotationFinishedInsideApply.Store(true)
		case <-time.After(500 * time.Millisecond):
		}
	}
	res := f.apply(t, run, f.req(op))
	if rotationFinishedInsideApply.Load() {
		t.Fatal("the rotation completed while the apply held the token row")
	}
	f.want(t, res, ApplyOutcomeApplied, "")
	if res.Token != f.newPlain {
		t.Fatalf("the apply must return the pre-rotation bytes, got %q", res.Token)
	}
	select {
	case err := <-rotated:
		if err != nil {
			t.Fatalf("rotation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rotation never completed after the apply committed")
	}
	f.svc.afterApplySecretLock = nil
	f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeRefusedCommitted, applyReasonCredentialChanged)
}

// holdSecretRow takes the token row FOR UPDATE in its own transaction (what a rotate, disable or
// delete does first) and returns the release func.
func (f applyFx) holdSecretRow(t *testing.T) (release func()) {
	t.Helper()
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.env.ctx, `SELECT 1 FROM user_secrets WHERE id = $1 FOR UPDATE`, f.newTok); err != nil {
		_ = tx.Rollback(f.env.ctx)
		t.Fatal(err)
	}
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			_ = tx.Rollback(f.env.ctx)
		}
	}
	t.Cleanup(release)
	return release
}

// applyWithin runs one apply under a deadline, so a regression that waits on the token row fails the
// test instead of hanging it.
func (f applyFx) applyWithin(t *testing.T, run uuid.UUID, req CredentialSwitchApplyRequest) (CredentialSwitchApplyResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.env.ctx, 20*time.Second)
	defer cancel()
	return f.svc.ApplyCredentialSwitch(ctx, f.worker(t), run, req)
}

func fastApplyBackoff(t *testing.T) {
	t.Helper()
	old := applyLockBusyBackoff
	applyLockBusyBackoff = time.Millisecond
	t.Cleanup(func() { applyLockBusyBackoff = old })
}

// TestApplyCredentialSwitchBusyTokenRowLiveDB: the apply takes the token row FOR SHARE NOWAIT after
// it holds the worker and run rows. A row held by another writer (the first thing a delete, rotate
// or disable does) must never make the apply wait: it rolls back and retries, then answers
// not_applied credential_busy (fresh) or an error (replay, never refused_committed).
func TestApplyCredentialSwitchBusyTokenRowLiveDB(t *testing.T) {
	fastApplyBackoff(t)
	t.Run("a row released between attempts is applied by the retry", func(t *testing.T) {
		f := newApplyFx(t)
		run, op := f.run(t, "running")
		var calls atomic.Int32
		var release func()
		f.svc.beforeApplySecretLock = func() {
			switch calls.Add(1) {
			case 1:
				release = f.holdSecretRow(t)
			case 2:
				release()
			}
		}
		res, err := f.applyWithin(t, run, f.req(op))
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		f.want(t, res, ApplyOutcomeApplied, "")
		if calls.Load() != 2 || res.Token != f.newPlain || res.Replay {
			t.Fatalf("attempts = %d result %+v, want the second attempt to apply the new token", calls.Load(), res)
		}
	})
	t.Run("a row held through every attempt is credential_busy and commits nothing", func(t *testing.T) {
		f := newApplyFx(t)
		run, op := f.run(t, "running")
		var calls atomic.Int32
		f.svc.beforeApplySecretLock = func() {
			if calls.Add(1) == 1 {
				f.holdSecretRow(t)
			}
		}
		f.noWrite(t, run, func() {
			res, err := f.applyWithin(t, run, f.req(op))
			if err != nil {
				t.Fatalf("a busy row must be an outcome, not an error (40P01 would land here): %v", err)
			}
			f.want(t, res, ApplyOutcomeNotApplied, applyReasonCredentialBusy)
			if res.Token != "" {
				t.Fatal("a not_applied outcome must carry no token")
			}
		})
		if got := calls.Load(); got != applyLockBusyAttempts {
			t.Fatalf("attempts = %d, want %d", got, applyLockBusyAttempts)
		}
		if f.opOf(t, run) != op {
			t.Fatal("the stamp must stay pending")
		}
	})
	t.Run("a replay against a held row is an error, never refused_committed", func(t *testing.T) {
		f := newApplyFx(t)
		run, op := f.run(t, "running")
		f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeApplied, "")
		f.holdSecretRow(t)
		f.noWrite(t, run, func() {
			res, err := f.applyWithin(t, run, f.req(op))
			if err == nil || !isLockNotAvailable(err) {
				t.Fatalf("err = %v result %+v, want a 55P03 error", err, res)
			}
			if res.Outcome != "" {
				t.Fatalf("outcome = %q, want none on an error", res.Outcome)
			}
		})
	})
}

// TestApplyCredentialSwitchConcurrentDeleteDoesNotDeadlockLiveDB: a token delete locks the secret row
// first and its ON DELETE SET NULL cascade then needs the run row the apply holds. With the apply's
// token lock waiting, the two deadlock (40P01); with NOWAIT the apply yields and the delete wins.
func TestApplyCredentialSwitchConcurrentDeleteDoesNotDeadlockLiveDB(t *testing.T) {
	fastApplyBackoff(t)
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	deleted := make(chan error, 1)
	var calls atomic.Int32
	f.svc.beforeApplySecretLock = func() {
		if calls.Add(1) != 1 {
			return
		}
		go func() {
			_, err := f.env.pool.Exec(f.env.ctx, `DELETE FROM user_secrets WHERE id = $1`, f.newTok)
			deleted <- err
		}()
		// Park until the delete holds the secret row and is waiting on the run row we hold.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var waiting int
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM pg_stat_activity
			    WHERE wait_event_type = 'Lock' AND query LIKE 'DELETE FROM user_secrets%'`).Scan(&waiting); err == nil && waiting > 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("the delete never blocked on the run row")
	}
	res, err := f.applyWithin(t, run, f.req(op))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("apply deadlocked against the delete: %v", err)
	}
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	select {
	case derr := <-deleted:
		if derr != nil {
			t.Fatalf("delete: %v (a 40P01 here is the deadlock, the delete being its victim)", derr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the delete never completed")
	}
	// Once the delete lands the run's override is NULL, so a retry may legitimately apply the owner's
	// other credential; what it must never do is hand out the deleted token's bytes.
	if res.Token == f.newPlain {
		t.Fatalf("apply returned the bytes of a token a concurrent delete removed: %+v", res)
	}
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM user_secrets WHERE id = $1`, f.newTok).Scan(&n); err != nil || n != 0 {
		t.Fatalf("token rows after delete = %d (err %v), want 0", n, err)
	}
}

func TestApplyCredentialSwitchFencesCommitNothingLiveDB(t *testing.T) {
	f := newApplyFx(t)
	t.Run("stamp generation older than the claim generation", func(t *testing.T) {
		run, op := f.run(t, "running")
		// The claim is at f.gen but the stamp was written for an earlier generation.
		f.env.exec(`UPDATE runs SET credential_switch_generation = $2 WHERE id = $1`, run, f.gen-1)
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeNotApplied, applyReasonGenerationMismatch) })
	})
	t.Run("stale generation", func(t *testing.T) {
		run, op := f.run(t, "running")
		req := f.req(op)
		req.ClaimGeneration = f.gen - 1
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, req), ApplyOutcomeFenced, "") })
	})
	t.Run("old nonce after re-register at the same generation", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.env.exec(`UPDATE workers SET snapshot_register_nonce = 'nonce-after-reregister' WHERE id = $1`, f.o.workerID)
		defer f.env.exec(`UPDATE workers SET snapshot_register_nonce = $2 WHERE id = $1`, f.o.workerID, applyFixtureNonce)
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeFenced, "") })
	})
	t.Run("worker without the capability", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.o.workerID, []string{capability.CredentialSwitchV1})
		defer f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.o.workerID, []string{capability.CredentialSwitchV1, capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1})
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeFenced, "") })
	})
	t.Run("foreign worker", func(t *testing.T) {
		run, op := f.run(t, "running")
		otherWorker := uuid.New()
		f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities, snapshot_register_nonce)
		            VALUES ($1, $2, $3, $4, 'online', $5, $6)`, otherWorker, f.o.userID, "w-"+otherWorker.String(), otherWorker[:],
			[]string{capability.CredentialSwitchInPlaceV1}, applyFixtureNonce)
		f.noWrite(t, run, func() {
			_, err := f.svc.ApplyCredentialSwitch(f.env.ctx, wkrRow(t, f.env, otherWorker), run, f.req(op))
			if !errors.Is(err, ErrRunNotOwned) {
				t.Fatalf("err = %v, want ErrRunNotOwned", err)
			}
		})
	})
	t.Run("cancelled run", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.env.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, run)
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeFenced, "") })
	})
	t.Run("released claim", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeFenced, "") })
	})
	t.Run("job run", func(t *testing.T) {
		id := uuid.New()
		f.env.exec(`INSERT INTO runs(id,user_id,kind,job_type,issue_title,issue_description,status,worker_id,claim_generation,started_at)
		            VALUES($1,$2,'job','research','j','d','running',$3,$4,now())`, id, f.o.userID, f.o.workerID, f.gen)
		op := f.restamp(t, id)
		f.noWrite(t, id, func() { f.want(t, f.apply(t, id, f.req(op)), ApplyOutcomeFenced, "") })
	})
	t.Run("cross_check run", func(t *testing.T) {
		parent, _ := f.run(t, "running")
		id := uuid.New()
		f.env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status,worker_id,claim_generation)
		            VALUES($1,$2,$3,'cross_check',$4,'codex',true,60,'title','description','running',$5,$6)`, id, f.o.userID, f.o.repoID, parent, f.o.workerID, f.gen)
		op := f.restamp(t, id)
		f.noWrite(t, id, func() { f.want(t, f.apply(t, id, f.req(op)), ApplyOutcomeFenced, "") })
	})
	t.Run("unknown operation", func(t *testing.T) {
		run, _ := f.run(t, "running")
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, f.req(uuid.New())), ApplyOutcomeSuperseded, "") })
	})
	t.Run("unknown operation, no stamp pending", func(t *testing.T) {
		run, _ := f.run(t, "running")
		f.env.exec(`UPDATE runs SET credential_switch_requested_at = NULL, credential_switch_generation = NULL WHERE id = $1`, run)
		f.noWrite(t, run, func() {
			f.want(t, f.apply(t, run, f.req(uuid.New())), ApplyOutcomeNotApplied, applyReasonNoPendingSwitch)
		})
	})
	t.Run("superseded by a newer request", func(t *testing.T) {
		run, old := f.run(t, "running")
		fresh := f.restamp(t, run)
		if fresh == old {
			t.Fatal("restamp must mint a new op id")
		}
		f.noWrite(t, run, func() { f.want(t, f.apply(t, run, f.req(old)), ApplyOutcomeSuperseded, "") })
		f.want(t, f.apply(t, run, f.req(fresh)), ApplyOutcomeApplied, "")
	})
}

func TestApplyCredentialSwitchTwoStampsInOneGenerationLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op1 := f.run(t, "running")
	first := f.apply(t, run, f.req(op1))
	op2 := f.restamp(t, run)
	if op2 == op1 {
		t.Fatal("the second stamp must carry a distinct op id")
	}
	second := f.apply(t, run, f.req(op2))
	f.want(t, first, ApplyOutcomeApplied, "")
	f.want(t, second, ApplyOutcomeApplied, "")
	if first.Ordinal != 1 || second.Ordinal != 2 || first.MessageSeq == second.MessageSeq || second.MessageSeq != first.MessageSeq+1 {
		t.Fatalf("first=%+v second=%+v, want ordinals 1 and 2 at consecutive seqs", first, second)
	}
	var msgs int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = 'credential_switch'`, run).Scan(&msgs); err != nil || msgs != 2 {
		t.Fatalf("credential_switch messages = %d (err %v), want one per switch", msgs, err)
	}
}

func TestApplyCredentialSwitchSeqLiveDB(t *testing.T) {
	f := newApplyFx(t)
	t.Run("a frame at message_seq+1 persists", func(t *testing.T) {
		run, op := f.run(t, "running")
		res := f.apply(t, run, f.req(op))
		gen := f.gen
		err := f.svc.AppendMessagesForClaim(f.env.ctx, f.worker(t), run, []IncomingMessage{{Seq: res.MessageSeq + 1, Kind: "text", Payload: json.RawMessage(`{"t":"after"}`)}}, &gen)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1 AND seq = $2`, run, res.MessageSeq+1).Scan(&n); err != nil || n != 1 {
			t.Fatalf("frame at seq %d: count %d err %v", res.MessageSeq+1, n, err)
		}
		if got := mustRun(t, f.env, run).LastSeq; got != res.MessageSeq+1 {
			t.Fatalf("last_seq = %d, want %d", got, res.MessageSeq+1)
		}
	})
	t.Run("after a server-inserted event", func(t *testing.T) {
		run, op := f.run(t, "running")
		// A server-side insert (a cross-check event) advanced the log past the worker's frames.
		f.env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 6, 'cross_check', '{"e":1}')`, run)
		f.env.exec(`UPDATE runs SET last_seq = 6 WHERE id = $1`, run)
		res := f.apply(t, run, f.req(op))
		if res.MessageSeq != 7 {
			t.Fatalf("message seq = %d, want 7 (after the server-inserted event)", res.MessageSeq)
		}
	})
	t.Run("a stray row above last_seq", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 9, 'cross_check', '{"e":1}')`, run)
		res := f.apply(t, run, f.req(op))
		if res.MessageSeq != 10 {
			t.Fatalf("message seq = %d, want 10 (GREATEST of last_seq and MAX(seq), plus one)", res.MessageSeq)
		}
		if got := mustRun(t, f.env, run).LastSeq; got != 10 {
			t.Fatalf("last_seq = %d, want 10", got)
		}
	})
}

func epochResult(seq int32, ordinal *int32, model string, in, out int64, cost float64) IncomingMessage {
	payload := fmt.Sprintf(`{"event":"result","modelUsage":{%q:{"inputTokens":%d,"outputTokens":%d,"costUSD":%g}}}`, model, in, out, cost)
	return IncomingMessage{Seq: seq, Kind: "status", Payload: json.RawMessage(payload), CredentialEpochOrdinal: ordinal}
}

func epochInit(seq int32) IncomingMessage {
	return IncomingMessage{Seq: seq, Kind: "status", Payload: json.RawMessage(`{"event":"init"}`)}
}

func i32(v int32) *int32 { return &v }

func (f applyFx) storedOrdinals(t *testing.T, run uuid.UUID) map[int32]*int32 {
	t.Helper()
	rows, err := f.env.pool.Query(f.env.ctx, `SELECT seq, credential_epoch_ordinal FROM run_messages WHERE run_id = $1 AND seq > 5 ORDER BY seq`, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int32]*int32{}
	for rows.Next() {
		var seq int32
		var ord *int32
		if err := rows.Scan(&seq, &ord); err != nil {
			t.Fatal(err)
		}
		out[seq] = ord
	}
	return out
}

func TestApplyCredentialSwitchAttributionLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	gen := f.gen
	w := f.worker(t)
	appendFrames := func(msgs ...IncomingMessage) {
		t.Helper()
		if err := f.svc.AppendMessagesForClaim(f.env.ctx, w, run, msgs, &gen); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// A frame that names ordinal 1 before the apply committed it is permanently unattributed, and an
	// explicit base ordinal is kept as sent.
	appendFrames(epochInit(6), epochResult(7, i32(1), "m", 100, 10, 0.5))
	appendFrames(epochInit(8), epochResult(9, i32(0), "m", 40, 4, 0.25))
	got := f.storedOrdinals(t, run)
	if got[7] == nil || *got[7] != -1 || got[9] == nil || *got[9] != 0 || got[6] != nil {
		t.Fatalf("stored ordinals before apply = %v, want seq7=-1 seq9=0 seq6=NULL", fmtOrd(got))
	}
	res := f.apply(t, run, f.req(op))
	f.want(t, res, ApplyOutcomeApplied, "")
	// Mixed in one append: absent stays NULL, 1 is now committed, 5 is not, 0 is the base.
	base := res.MessageSeq
	appendFrames(
		epochInit(base+1), epochResult(base+2, i32(1), "m", 70, 7, 0.1),
		epochInit(base+3), epochResult(base+4, i32(5), "m", 30, 3, 0.05),
		epochInit(base+5), epochResult(base+6, nil, "m", 20, 2, 0.02),
	)
	rows, err := f.env.pool.Query(f.env.ctx, `SELECT seq, credential_epoch_ordinal FROM run_messages WHERE run_id = $1 AND seq > $2 AND payload->>'event' = 'result' ORDER BY seq`, run, base)
	if err != nil {
		t.Fatal(err)
	}
	type pair struct {
		seq int32
		ord *int32
	}
	var after []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.seq, &p.ord); err != nil {
			t.Fatal(err)
		}
		after = append(after, p)
	}
	rows.Close()
	if len(after) != 3 || after[0].ord == nil || *after[0].ord != 1 || after[1].ord == nil || *after[1].ord != -1 || after[2].ord != nil {
		t.Fatalf("stored result ordinals after apply = %+v, want [1 -1 NULL]", after)
	}
	usage := func() map[string]*int32 {
		out := map[string]*int32{}
		r, err := f.env.pool.Query(f.env.ctx, `SELECT lineage_epoch, input_tokens, credential_epoch_ordinal FROM run_usage WHERE run_id = $1 ORDER BY lineage_epoch`, run)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var le int32
			var in int64
			var ord *int32
			if err := r.Scan(&le, &in, &ord); err != nil {
				t.Fatal(err)
			}
			out[fmt.Sprintf("%d/%d", le, in)] = ord
		}
		return out
	}
	before := usage()
	// A full refold keeps every leg where the incremental fold put it, the unattributed one included.
	run0 := mustRun(t, f.env, run)
	if err := RefoldRunUsage(f.env.ctx, f.env.pool, f.env.q, run0); err != nil {
		t.Fatalf("refold: %v", err)
	}
	afterRefold := usage()
	if fmt.Sprint(fmtOrd2(before)) != fmt.Sprint(fmtOrd2(afterRefold)) {
		t.Fatalf("run_usage attribution changed across a refold:\nbefore %v\nafter  %v", fmtOrd2(before), fmtOrd2(afterRefold))
	}
	if ord := afterRefold["1/100"]; ord == nil || *ord != -1 {
		t.Fatalf("the leg fed by the ordinal-1-before-apply frame must stay unattributed after a refold, got %v", fmtOrd2(afterRefold))
	}
}

func fmtOrd(m map[int32]*int32) string {
	var parts []string
	for k, v := range m {
		if v == nil {
			parts = append(parts, fmt.Sprintf("%d=NULL", k))
		} else {
			parts = append(parts, fmt.Sprintf("%d=%d", k, *v))
		}
	}
	return strings.Join(parts, " ")
}

func fmtOrd2(m map[string]*int32) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		if m[k] == nil {
			parts = append(parts, k+"=NULL")
		} else {
			parts = append(parts, fmt.Sprintf("%s=%d", k, *m[k]))
		}
	}
	return strings.Join(parts, " ")
}

// --- worker reports ---------------------------------------------------------------------------

func (f applyFx) report(t *testing.T, wkr store.Worker, run uuid.UUID, state string, gen int64, nonce string, op *uuid.UUID) (store.Run, bool, error) {
	t.Helper()
	g := gen
	req := StateRequest{State: state, ClaimGeneration: &g, RegisterNonce: nonce}
	if op != nil {
		s := op.String()
		req.OpID = &s
	}
	return f.svc.SetState(f.env.ctx, wkr, run, req)
}

func TestCredentialSwitchHeldWithOpLiveDB(t *testing.T) {
	f := newApplyFx(t)
	w := f.worker(t)
	t.Run("applied then replayed after a lost response", func(t *testing.T) {
		run, op := f.run(t, "running")
		got, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, &op)
		if err != nil || !applied || got.Status != "paused" || got.HoldReason.String != holdReasonCredentialSwitchSettling {
			t.Fatalf("held: status=%s hold=%v applied=%v err=%v", got.Status, got.HoldReason, applied, err)
		}
		if !got.CredentialSwitchHoldOpID.Valid || got.CredentialSwitchHoldOpID.Bytes != [16]byte(op) {
			t.Fatalf("hold identity op = %v, want %s", got.CredentialSwitchHoldOpID, op)
		}
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, &op); err != nil || !applied {
			t.Fatalf("replay of the same report: applied=%v err=%v, want it accepted", applied, err)
		}
	})
	t.Run("a different operation does not converge", func(t *testing.T) {
		run, op := f.run(t, "running")
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, &op); err != nil || !applied {
			t.Fatal(applied, err)
		}
		other := uuid.New()
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, &other); err != nil || applied {
			t.Fatalf("replay with another op: applied=%v err=%v, want refused", applied, err)
		}
	})
	t.Run("superseded nonce is refused even when the hold is in place", func(t *testing.T) {
		run, op := f.run(t, "running")
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, &op); err != nil || !applied {
			t.Fatal(applied, err)
		}
		f.env.exec(`UPDATE workers SET snapshot_register_nonce = 'nonce-newer' WHERE id = $1`, f.o.workerID)
		defer f.env.exec(`UPDATE workers SET snapshot_register_nonce = $2 WHERE id = $1`, f.o.workerID, applyFixtureNonce)
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, &op); err != nil || applied {
			t.Fatalf("old-nonce replay: applied=%v err=%v, want refused", applied, err)
		}
	})
	t.Run("stale generation is refused", func(t *testing.T) {
		run, op := f.run(t, "running")
		if _, applied, _ := f.report(t, w, run, "credential_switch_held", f.gen-1, applyFixtureNonce, &op); applied {
			t.Fatal("a stale-generation hold must not apply")
		}
		if got := mustRun(t, f.env, run).Status; got != "running" {
			t.Fatalf("status = %s, want running", got)
		}
	})
	t.Run("foreign worker is refused", func(t *testing.T) {
		run, op := f.run(t, "running")
		other := uuid.New()
		f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, snapshot_register_nonce) VALUES ($1, $2, $3, $4, 'online', 'n-o')`,
			other, f.o.userID, "w-"+other.String(), other[:])
		if _, applied, err := f.report(t, wkrRow(t, f.env, other), run, "credential_switch_held", f.gen, "n-o", &op); applied || !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("foreign worker: applied=%v err=%v, want ErrRunNotOwned", applied, err)
		}
	})
	t.Run("awaiting_input holds and keeps the open question", func(t *testing.T) {
		run, op := f.run(t, "awaiting_input")
		f.env.exec(`UPDATE runs SET open_question_id = 'q-1' WHERE id = $1`, run)
		got, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, &op)
		if err != nil || !applied || got.Status != "paused" || got.OpenQuestionID.String != "q-1" {
			t.Fatalf("awaiting_input hold: status=%s question=%v applied=%v err=%v", got.Status, got.OpenQuestionID, applied, err)
		}
	})
	t.Run("without an op id the hold stays running-only", func(t *testing.T) {
		run, _ := f.run(t, "awaiting_input")
		if _, applied, err := f.report(t, w, run, "credential_switch_held", f.gen, applyFixtureNonce, nil); err != nil || applied {
			t.Fatalf("v1 hold on awaiting_input: applied=%v err=%v, want refused", applied, err)
		}
	})
	t.Run("a bad op id is a 400", func(t *testing.T) {
		run, _ := f.run(t, "running")
		bad := "not-a-uuid"
		g := f.gen
		_, _, err := f.svc.SetState(f.env.ctx, w, run, StateRequest{State: "credential_switch_held", ClaimGeneration: &g, RegisterNonce: applyFixtureNonce, OpID: &bad})
		if !errors.Is(err, ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})
}

func TestCredentialSwitchFailedWithOpLiveDB(t *testing.T) {
	f := newApplyFx(t)
	w := f.worker(t)
	run, op := f.run(t, "running")
	other := uuid.New()
	if _, applied, err := f.report(t, w, run, "credential_switch_failed", f.gen, "", &other); err != nil || applied {
		t.Fatalf("failed with another op: applied=%v err=%v, want it to clear nothing", applied, err)
	}
	if !mustRun(t, f.env, run).CredentialSwitchRequestedAt.Valid {
		t.Fatal("the stamp must survive a give-up naming another operation")
	}
	if _, applied, err := f.report(t, w, run, "credential_switch_failed", f.gen, "", &op); err != nil || !applied {
		t.Fatalf("failed with the pending op: applied=%v err=%v", applied, err)
	}
	if mustRun(t, f.env, run).CredentialSwitchRequestedAt.Valid {
		t.Fatal("the stamp must be cleared")
	}
}

func TestCredentialSwitchUncertainLiveDB(t *testing.T) {
	f := newApplyFx(t)
	w := f.worker(t)
	for _, status := range []string{"running", "awaiting_approval"} {
		t.Run(status+" parks paused with no hold reason", func(t *testing.T) {
			run, op := f.run(t, status)
			f.env.exec(`UPDATE runs SET open_question_id = 'q-9' WHERE id = $1`, run)
			got, applied, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, applyFixtureNonce, &op)
			if err != nil || !applied || got.Status != "paused" || got.HoldReason.Valid || !got.WorkerID.Valid || got.OpenQuestionID.String != "q-9" || got.ClaimReleasedAt.Valid {
				t.Fatalf("uncertain: status=%s hold=%v worker=%v question=%v applied=%v err=%v", got.Status, got.HoldReason, got.WorkerID, got.OpenQuestionID, applied, err)
			}
			// The worker emits the feed message through its own batcher at a seq it owns; the server
			// writes none, so it cannot collide with the worker's live frames.
			var n int
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = 'credential_switch_uncertain'`, run).Scan(&n); err != nil || n != 0 {
				t.Fatalf("server-written credential_switch_uncertain messages = %d (err %v), want 0", n, err)
			}
			if _, applied, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, applyFixtureNonce, &op); err != nil || !applied {
				t.Fatalf("replay: applied=%v err=%v, want it to converge", applied, err)
			}
		})
	}
	t.Run("a committed apply is also uncertain-reportable", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeApplied, "")
		if _, applied, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, applyFixtureNonce, &op); err != nil || !applied {
			t.Fatalf("uncertain after a committed apply: applied=%v err=%v", applied, err)
		}
	})
	t.Run("a pending wall pause takes the worker's own park", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = 'wall' WHERE id = $1`, run)
		_, _, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, applyFixtureNonce, &op)
		if !errors.Is(err, ErrCredentialSwitchUncertainParkPending) {
			t.Fatalf("err = %v, want ErrCredentialSwitchUncertainParkPending", err)
		}
		if got := mustRun(t, f.env, run).Status; got != "running" {
			t.Fatalf("status = %s, want running", got)
		}
	})
	t.Run("an owner pause request is left in place", func(t *testing.T) {
		run, op := f.run(t, "running")
		f.env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = 'now' WHERE id = $1`, run)
		got, applied, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, applyFixtureNonce, &op)
		if err != nil || !applied || !got.PauseRequestedAt.Valid || got.PauseMode.String != "now" {
			t.Fatalf("uncertain with an owner pause pending: applied=%v err=%v pause=%v/%v", applied, err, got.PauseRequestedAt, got.PauseMode)
		}
	})
	t.Run("an unknown operation is refused", func(t *testing.T) {
		run, _ := f.run(t, "running")
		unknown := uuid.New()
		if _, applied, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, applyFixtureNonce, &unknown); err != nil || applied {
			t.Fatalf("applied=%v err=%v, want refused", applied, err)
		}
	})
	t.Run("a foreign worker is refused", func(t *testing.T) {
		run, op := f.run(t, "running")
		other := uuid.New()
		f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, snapshot_register_nonce) VALUES ($1, $2, $3, $4, 'online', 'n-u')`,
			other, f.o.userID, "w-"+other.String(), other[:])
		if _, applied, err := f.report(t, wkrRow(t, f.env, other), run, "credential_switch_uncertain", f.gen, "n-u", &op); applied || !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("applied=%v err=%v, want ErrRunNotOwned", applied, err)
		}
	})
	t.Run("a superseded nonce is refused", func(t *testing.T) {
		run, op := f.run(t, "running")
		if _, applied, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, "nonce-stale", &op); err != nil || applied {
			t.Fatalf("applied=%v err=%v, want refused", applied, err)
		}
	})
	t.Run("op id is required", func(t *testing.T) {
		run, _ := f.run(t, "running")
		_, _, err := f.report(t, w, run, "credential_switch_uncertain", f.gen, applyFixtureNonce, nil)
		if !errors.Is(err, ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState", err)
		}
	})
}

// --- inputs ----------------------------------------------------------------------------------

func TestInputsDrainForInPlaceWorkerDuringPendingSwitchLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	f.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'steer me')`, run)
	got, err := f.svc.ConsumeInputs(f.env.ctx, f.worker(t), run)
	if err != nil {
		t.Fatal(err)
	}
	if got.CredentialSwitch == nil || got.CredentialSwitch.OpID != op.String() || got.CredentialSwitch.Generation != f.gen {
		t.Fatalf("signal = %+v, want generation %d op %s", got.CredentialSwitch, f.gen, op)
	}
	if len(got.Inputs) != 1 || !got.Receipts {
		t.Fatalf("an in-place worker drains normally during a pending switch: inputs=%d receipts=%v", len(got.Inputs), got.Receipts)
	}
	// A v1-only worker keeps the signal-and-no-drain contract.
	f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.o.workerID, []string{capability.CredentialSwitchV1, capability.InputReceiptsV1})
	got, err = f.svc.ConsumeInputs(f.env.ctx, f.worker(t), run)
	if err != nil {
		t.Fatal(err)
	}
	if got.CredentialSwitch == nil || len(got.Inputs) != 0 {
		t.Fatalf("a v1 worker must get the signal and no inputs: %+v", got)
	}
}

// --- codex epoch journal -----------------------------------------------------------------------

func TestRecordCodexEpochIdempotentLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	codexSecret := f.env.seedAnthropicSecret(t, f.o.userID, "codex-alias", false)
	r := mustRun(t, f.env, run)
	r.CodexSecretID = pgconv.UUID(codexSecret)
	f.svc.recordCodexEpoch(context.Background(), r)
	f.svc.recordCodexEpoch(context.Background(), r)
	var n int
	var reason, label string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*), max(select_reason), max(label) FROM run_credential_epochs WHERE run_id = $1 AND claim_generation = $2 AND secret_id = $3`, run, f.gen, codexSecret).Scan(&n, &reason, &label); err != nil || n != 1 || reason != "codex_binding" || label != "codex-alias" {
		t.Fatalf("codex epoch rows = %d reason %q label %q (err %v), want exactly one codex_binding row", n, reason, label, err)
	}
	if got := f.opOf(t, run); got != op {
		t.Fatal("recording a Codex epoch must not clear a switch stamp")
	}
	if mustRun(t, f.env, run).AnthropicSecretID.Valid && uuid.UUID(mustRun(t, f.env, run).AnthropicSecretID.Bytes) == codexSecret {
		t.Fatal("a Codex epoch must never be written through SetRunAnthropicSecret")
	}
}
