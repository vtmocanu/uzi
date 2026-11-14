package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Move A to a linked subscription through apply itself, then request B.
func codexApplyFromSubscription(t *testing.T) (applyFx, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	f, run, alias := codexApplyFixture(t, true)
	applied := f.apply(t, run, f.req(f.opOf(t, run)))
	if applied.Outcome != ApplyOutcomeApplied || applied.Codex == nil {
		t.Fatal("subscription setup failed", applied)
	}
	state, err := f.env.q.GetCodexCredentialState(f.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: f.o.userID})
	if err != nil {
		t.Fatal(err)
	}
	next := f.env.seedStaticAPIKey(t, f.o.userID, "race-next", "race-next-key")
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &next); err != nil {
		t.Fatal(err)
	}
	return f, run, uuid.UUID(state.ProviderAccountID.Bytes), applied.Codex.Capability
}

func TestCodexApplyBusyOldAccountLiveDB(t *testing.T) {
	for _, provenance := range []string{"other run", "poller", "unknown", "intent only"} {
		t.Run(provenance, func(t *testing.T) {
			f, run, account, _ := codexApplyFromSubscription(t)
			operation := uuid.New()
			if provenance != "intent only" {
				f.env.exec("UPDATE codex_provider_account SET coord_state='in_progress',coord_operation_id=$2,lease_deadline=now()+interval '1 minute' WHERE id=$1", account, operation)
			}
			if provenance == "other run" || provenance == "intent only" {
				f.env.exec("INSERT INTO codex_refresh_intent(operation_id,user_id,provider_account_id,from_generation,state) VALUES($1,$2,$3,0,'rotating')", operation, f.o.userID, account)
			}
			before := f.env.mustAccount(t, f.o.userID, account)
			f.noWrite(t, run, func() {
				got := f.apply(t, run, f.req(f.opOf(t, run)))
				if got.Outcome != ApplyOutcomeNotApplied || got.Reason != applyReasonCredentialBusy {
					t.Fatalf("busy A accepted: %v", got)
				}
			})
			after := f.env.mustAccount(t, f.o.userID, account)
			if after.CoordState != before.CoordState || after.CoordOperationID != before.CoordOperationID || after.LeaseDeadline != before.LeaseDeadline {
				t.Fatal("apply cleared account-wide busy state")
			}
		})
	}
}

// The initial refresh read has passed. Before locked admission, a real apply
// changes authority. The locked refresh recheck must leave no intent or lease.
func TestCodexApplyWinsLateRefreshAdmissionLiveDB(t *testing.T) {
	f, run, account, capability := codexApplyFromSubscription(t)
	fake := &fakeRefreshClient{}
	f.svc.codexRefresh = fake
	changed := false
	f.svc.q = intentReadHookStore{
		Queries: f.env.q,
		onIntentRead: func(_ store.CodexRefreshIntent, readErr error) {
			if changed || !errors.Is(readErr, pgx.ErrNoRows) {
				return
			}
			changed = true
			result := f.apply(t, run, f.req(f.opOf(t, run)))
			if result.Outcome != ApplyOutcomeApplied {
				t.Fatal("switch did not win", result)
			}
		},
	}
	op := uuid.New()
	res, err := f.svc.CoordinatedCodexRefresh(f.env.ctx, f.worker(t), run, capability, op, 0)
	if !changed || err == nil || res.AccessToken != "" || fake.calls != 0 {
		t.Fatalf("late refresh escaped admission: changed=%t calls=%d err=%v", changed, fake.calls, err)
	}
	if _, err := f.env.q.GetCodexRefreshIntent(f.env.ctx, store.GetCodexRefreshIntentParams{OperationID: op, UserID: f.o.userID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("late refresh left intent", err)
	}
	acct := f.env.mustAccount(t, f.o.userID, account)
	if acct.CoordState != "idle" || acct.CoordOperationID.Valid || acct.LeaseDeadline.Valid {
		t.Fatal("late refresh acquired lease")
	}
}
