package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_disabled_pin_race_livedb_test.go pins the D5 refusal of a NEW per-run pin against
// a disable that commits while the write is in flight (PRD #1732): the validator reads outside
// any lock, so each writer re-checks the pin in its own transaction (lockPinnedOverrideEnabled).
// Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

// raceDisable holds a disable of tok uncommitted, the way the Settings disable does it (the
// owner's secret mutation lock, then the row update), starts call, and commits the disable once
// call has either returned or is blocked behind the disable's transaction. It returns call's
// error. Code that validates outside any lock and never waits returns first and writes against
// the still-enabled read; the fixed code blocks, and after the commit sees the disable.
func (fx *cdFix) raceDisable(t *testing.T, tok uuid.UUID, call func() error) error {
	t.Helper()
	ctx := fx.env.ctx
	tx, err := fx.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := store.LockSecretMutation(ctx, tx, fx.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, tok); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- call() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			if cerr := tx.Commit(ctx); cerr != nil {
				t.Fatal(cerr)
			}
			return err
		default:
		}
		var blocked bool
		if err := fx.env.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
		    WHERE $1::int = ANY (pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the write neither returned nor blocked behind the disable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// assertOverrideUnchanged fails when the run's status or override moved from before.
func assertOverrideUnchanged(t *testing.T, env codexTestEnv, before store.Run) {
	t.Helper()
	after := mustRun(t, env, before.ID)
	if after.Status != before.Status || after.CredentialOverrideMode != before.CredentialOverrideMode ||
		after.CredentialOverrideSecretID != before.CredentialOverrideSecretID ||
		after.CredentialSwitchRequestedAt.Valid != before.CredentialSwitchRequestedAt.Valid {
		t.Fatalf("run %s written despite the refusal: status %s->%s override %v/%v -> %v/%v switch %v -> %v", before.ID,
			before.Status, after.Status, before.CredentialOverrideMode, before.CredentialOverrideSecretID,
			after.CredentialOverrideMode, after.CredentialOverrideSecretID,
			before.CredentialSwitchRequestedAt.Valid, after.CredentialSwitchRequestedAt.Valid)
	}
}

// TestSetRunCredentialConcurrentDisableRefusedLiveDB: `uzi run set-token` onto a token whose
// disable commits after the validation read but before the write is refused with
// ErrCredentialDisabled and writes nothing, on every arm that writes an override: the
// reassignment of a run held on credential_disabled (which would otherwise requeue it on the
// disabled token), a queued run, and a held (running) run's switch stamp.
//
// MUTATION: drop the lockPinnedOverrideEnabled call from any arm (reassignCredentialDisabledRun,
// the queued/parked arm or stampHeldStateSwitch in run_credential.go); that subtest then gets a
// nil error and the pin written onto the disabled token.
func TestSetRunCredentialConcurrentDisableRefusedLiveDB(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, fx *cdFix) uuid.UUID
	}{
		{name: "held on credential_disabled", seed: func(t *testing.T, fx *cdFix) uuid.UUID {
			fx.setEnabled(fx.pinTok, false)
			return fx.parkedRun(t, "issue")
		}},
		{name: "queued", seed: func(t *testing.T, fx *cdFix) uuid.UUID {
			id := fx.queuedRun(t, "issue")
			fx.env.exec(`UPDATE runs SET credential_override_mode = 'default' WHERE id = $1`, id)
			return id
		}},
		{name: "running on a switch-capable worker", seed: func(t *testing.T, fx *cdFix) uuid.UUID {
			id := fx.queuedRun(t, "issue")
			fx.env.exec(`UPDATE runs SET status = 'running', claim_generation = 3, anthropic_secret_id = $2 WHERE id = $1`, id, fx.pinTok)
			return id
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			id := tc.seed(t, fx)
			before := mustRun(t, fx.env, id)
			err := fx.raceDisable(t, fx.otherTok, func() error {
				_, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, id, CredentialOverrideModePinned, &fx.otherTok)
				return err
			})
			if !errors.Is(err, ErrCredentialDisabled) {
				t.Fatalf("SetRunCredential racing a disable: err = %v, want ErrCredentialDisabled", err)
			}
			assertOverrideUnchanged(t, fx.env, before)
		})
	}
}

// TestCreateRunPinConcurrentDisableRefusedLiveDB: `uzi run create --token` onto a token whose
// disable commits after the create's validation read is refused with ErrCredentialDisabled and
// creates no run.
//
// MUTATION: drop the lockPinnedOverrideEnabled call in createRun (service.go); the create then
// returns nil and a run pinned to the disabled token exists.
func TestCreateRunPinConcurrentDisableRefusedLiveDB(t *testing.T) {
	fx := newCDFix(t)
	const iid = 4732
	fx.env.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	          VALUES ($1, $2, $3, 'pin race', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`,
		uuid.New(), fx.repoID, int64(iid))
	waitFalse := false
	pinned := &RawCredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &fx.otherTok}
	err := fx.raceDisable(t, fx.otherTok, func() error {
		_, err := fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, iid, "desc", &waitFalse, nil, false, nil, nil, pinned)
		return err
	})
	if !errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("CreateRun racing a disable: err = %v, want ErrCredentialDisabled", err)
	}
	var n int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM runs WHERE repo_id = $1 AND issue_iid = $2`, fx.repoID, int64(iid)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d run(s) created on the disabled pin, want none", n)
	}
}
