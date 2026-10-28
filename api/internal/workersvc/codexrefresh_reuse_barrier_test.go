package workersvc

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexReuseWorkerPersistenceBarrier(t *testing.T) {
	for _, leg := range []string{"within_bound", "past_bound", "owner_restore"} {
		t.Run(leg, func(t *testing.T) {
			s, st, p, admit := newReuseWorker(t)
			ctx := context.Background()
			gen := st.acct.Generation
			resume, queued, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var release sync.Once
			started := false
			s.background = func(fn func()) {
				started = true
				go func() {
					close(queued)
					<-resume
					defer close(done)
					fn()
				}()
			}
			t.Cleanup(func() {
				if started {
					release.Do(func() { close(resume) })
					<-done
				}
			})
			reconcile := func() (int, bool) {
				t.Helper()
				n, changed, err := s.reconcileCodexRefreshWithReuse(ctx, st.acct.UserID, st.acct.ID, admit)
				if err != nil {
					t.Fatal(err)
				}
				return n, changed
			}
			st.writeErr = errors.New("transient")
			n, changed := reconcile()
			if !started {
				t.Fatal("missing background handoff")
			}
			<-queued
			if n != 0 || !changed || st.admissions != 1 || p.calls != 1 ||
				st.writes != 2 || st.terminals != 0 || st.intent.State != codexIntentRotating ||
				!st.intent.ReuseAttemptedAt.Valid || st.acct.Generation != gen || len(st.acct.RecoverySealed) != 0 {
				t.Fatal("initial reconcile did not consume once and retain a pending writer after two failures")
			}
			retained := append([]byte(nil), st.lastWrite.Sealed...)
			blob, err := openReuseSealed(s, st.acct.UserID, retained, st.lastWrite.RecoverySealedWith.String)
			if err != nil || len(retained) == 0 || st.lastWrite.Gen != gen ||
				blob.AccessToken != "reuse-access" || blob.RefreshToken != "CANARY_REFRESH" {
				t.Fatal("pending writer did not retain protected merged material at the current generation")
			}
			consumed := st.intent.ReuseAttemptedAt.Time
			s.SetClock(func() time.Time { return consumed.Add(8 * time.Second) })
			n, changed = reconcile()
			if n != 0 || changed || st.intent.State != codexIntentRotating ||
				p.calls != 1 || st.admissions != 1 || st.writes != 2 || st.promotes != 0 || st.terminals != 0 {
				t.Fatal("before shared bound, sibling reconcile resolved or retried the pending writer")
			}
			var ownerSealed []byte
			switch leg {
			case "past_bound":
				s.SetClock(func() time.Time { return consumed.Add(codexRefreshReuseInFlightBound + time.Second) })
				n, changed = reconcile()
				if n != 1 || !changed || st.intent.State != codexIntentUnrecoverable ||
					st.acct.Generation != gen || p.calls != 1 || st.writes != 2 ||
					!bytes.Equal(st.lastWrite.Sealed, retained) {
					t.Fatal("past-bound crash policy lost the paused protected copy or retried OAuth")
				}
			case "owner_restore":
				ownerSealed, err = s.box.Seal([]byte(`{"access_token":"owner-access","refresh_token":"owner-refresh"}`))
				if err != nil {
					t.Fatal(err)
				}
				st.acct.Generation++
				st.acct.CoordState = codexCoordIdle
				st.acct.CoordOperationID.Valid = false
				st.acct.SealedLogin, st.acct.SealedWith = ownerSealed, store.SealedWithMaster
				n, changed = reconcile()
				if n != 1 || !changed || st.intent.State != codexIntentReconciled {
					t.Fatal("actual reconcile did not resolve the old intent after owner restore")
				}
				st.fenceLost = true // Model the real SetSlot operation fence.
			}
			st.writeErr = nil
			release.Do(func() { close(resume) })
			<-done // All fake reads and further reconciliation follow the writer's completion.
			if st.writes != 3 || p.calls != 1 || st.admissions != 1 || st.terminals != 0 {
				t.Fatal("detached writer retried a fence miss, exchanged again, or clobbered a terminal intent")
			}
			if leg == "owner_restore" {
				reconcile()
				blob, err := s.openCodexAccountLogin(context.Background(), st.acct.UserID, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch})
				if err != nil || blob.AccessToken != "owner-access" || blob.RefreshToken != "owner-refresh" ||
					!bytes.Equal(st.acct.SealedLogin, ownerSealed) || st.acct.Generation != gen+1 ||
					st.intent.State != codexIntentReconciled || st.acct.CoordState != codexCoordIdle ||
					st.acct.CoordOperationID.Valid || len(st.acct.RecoverySealed) != 0 ||
					st.promotes != 0 || p.calls != 1 || p.discoveries != 0 {
					t.Fatal("late fenced writer changed the restored owner's login, generation, or intent")
				}
				return
			}
			// Each retry reseals with a fresh nonce; plaintext equality is checked below.
			if len(st.acct.RecoverySealed) == 0 || st.acct.Generation != gen ||
				!st.acct.RecoveryGeneration.Valid || st.acct.RecoveryGeneration.Int64 != gen ||
				st.promotes != 0 || p.discoveries != 0 {
				t.Fatal("writer did not install the retained current-generation protected slot")
			}
			blob, err = openReuseSealed(s, st.acct.UserID, st.acct.RecoverySealed, st.acct.RecoverySealedWith.String)
			if err != nil || blob.AccessToken != "reuse-access" || blob.RefreshToken != "CANARY_REFRESH" {
				t.Fatal("installed protected slot lost the original refresh token")
			}
			reconcile()
			blob, err = s.openCodexAccountLogin(context.Background(), st.acct.UserID, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch})
			if err != nil || blob.AccessToken != "reuse-access" || blob.RefreshToken != "CANARY_REFRESH" ||
				st.acct.Generation != gen+1 || st.promotes != 1 || p.discoveries != 1 ||
				p.calls != 1 || st.admissions != 1 || st.writes != 3 || len(st.acct.RecoverySealed) != 0 {
				t.Fatal("identity-verified promotion did not install the retained login exactly once")
			}
			if leg == "past_bound" && st.intent.State != codexIntentUnrecoverable {
				t.Fatal("independent slot promotion unexpectedly rewrote the past-bound terminal intent")
			}
		})
	}
}
