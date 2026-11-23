package workersvc

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Observe SQL names only, never arguments or credential material. This pool is
// used only by the promoter, including its store.New(tx) queries.
type codexPromotionReads struct {
	ciphertext atomic.Int64
	account    atomic.Int64
	locked     atomic.Int64
	revision   atomic.Int64
}

func (r *codexPromotionReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.HasPrefix(d.SQL, "-- name: GetUserSecretCiphertextByID "):
		r.ciphertext.Add(1)
	case strings.HasPrefix(d.SQL, "-- name: GetCodexProviderAccountSnapshot "):
		r.account.Add(1)
	case strings.HasPrefix(d.SQL, "-- name: LockCredentialDisabledRunForPromotion "):
		r.locked.Add(1)
	case strings.HasPrefix(d.SQL, "-- name: GetModelProviderRevisionForOwner "):
		r.revision.Add(1)
	}
	return ctx
}

func (*codexPromotionReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Helper/storage proof only: no Claim, capability advertisement, or provider execution.
func TestCodexProviderFrozenHoldPromotionLiveDB(t *testing.T) {
	for _, name := range []string{
		"destination_still_removed",
		"reallow_after_source_cleared",
		"reallow_after_source_changed_to_unusable_b",
		"disabled_a_rewrites_stale",
		"key_enablement_revision_changed_rewrites_stale",
	} {
		t.Run(name, func(t *testing.T) {
			f := newCodexBindingFix(t)
			ctx, cancel := context.WithTimeout(f.env.ctx, 15*time.Second)
			defer cancel()
			a, keyA := f.provider, f.key
			frozen := bindingOfRun(f.run)
			// The atomic fixture has never been claimed. Preserve its worker, release,
			// checkpoint and generation fields when installing this legal storage hold.
			if f.run.Status != "queued" || f.run.Kind != "issue" || f.run.WorkerID.Valid ||
				f.run.StartedAt.Valid || f.run.ClaimReleasedAt.Valid || len(f.run.CheckpointCoordinates) != 0 {
				t.Fatal("fixture is not an unstarted, unowned issue run")
			}
			f.env.exec(`UPDATE runs SET status='paused', hold_reason='provider_destination_not_allowed',
    status_since=now()-interval '1 minute' WHERE id=$1`, f.run.ID)
			parked := mustRun(t, f.env, f.run.ID)
			f.svc.p.ProviderAllowedBaseURLs = nil

			switch name {
			case "reallow_after_source_cleared":
				f.env.exec(`UPDATE users SET codex_provider_id=NULL WHERE id=$1`, f.userID)
			case "reallow_after_source_changed_to_unusable_b":
				seedCodexSource(t, f.provFix)
				if f.provider == a || f.key == keyA {
					t.Fatal("B must have distinct provider and key")
				}
				f.env.exec(`UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, f.key)
				// Assertions continue to name frozen A, despite current unusable B.
				f.provider, f.key = a, keyA
			case "disabled_a_rewrites_stale":
				f.env.exec(`UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, keyA)
			case "key_enablement_revision_changed_rewrites_stale":
				f.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, keyA)
			}

			// Exercise the generated locked projection, then release this test-owned
			// transaction before the promoter takes its own locks.
			tx, err := f.env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			locked, err := store.New(tx).LockCredentialDisabledRunForPromotion(ctx, store.LockCredentialDisabledRunForPromotionParams{
				ID: f.run.ID, UserID: f.userID, GlobalTimeoutSeconds: int32(f.svc.p.RunTimeout.Seconds()),
			})
			if err != nil {
				t.Fatal(err)
			}
			if locked.Harness != "codex" || bindingOfRun(promotionRunBinding(locked)) != frozen ||
				!locked.CanQueue || locked.Status != "paused" || !locked.HoldReason.Valid ||
				locked.HoldReason.String != "provider_destination_not_allowed" {
				t.Fatal("generated locked projection lost frozen Codex binding or queue eligibility")
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}

			reads := &codexPromotionReads{}
			// Reuse the fixture pool's exact DSN, including any owned database settings.
			pool, err := store.OpenPool(ctx, f.env.pool.Config().ConnString(),
				store.WithQueryTracer(reads), store.WithMaxConns(1))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			f.svc.SetTxBeginner(pool)

			wantRow := func(status, hold string, stableSince bool) {
				t.Helper()
				got := mustRun(t, f.env, f.run.ID)
				wantCodexBinding(t, f.provFix, got, 1, f.keyRevision)
				if bindingOfRun(got) != frozen || got.Status != status ||
					got.HoldReason.Valid != (hold != "") || got.HoldReason.String != hold {
					t.Fatalf("status/hold = %s/%v; want %s/%q with frozen A", got.Status, got.HoldReason, status, hold)
				}
				if stableSince && got.StatusSince != parked.StatusSince {
					t.Fatal("held rewrite changed status_since")
				}
				if got.WorkerID != parked.WorkerID || got.ClaimReleasedAt != parked.ClaimReleasedAt ||
					got.ReleasedWorkerID != parked.ReleasedWorkerID || got.ClaimGeneration != parked.ClaimGeneration ||
					!reflect.DeepEqual(got.CheckpointCoordinates, parked.CheckpointCoordinates) {
					t.Fatal("promotion changed unclaimed fixture authority or checkpoint fields")
				}
			}
			promote := func(want string) {
				t.Helper()
				got, err := f.svc.promoteCredentialDisabledRun(ctx, f.userID, f.run.ID)
				if err != nil || got != want {
					t.Fatalf("promotion = %q, %v; want %q", got, err, want)
				}
			}

			stale := name == "disabled_a_rewrites_stale" || name == "key_enablement_revision_changed_rewrites_stale"
			if stale {
				promote("paused")
				wantRow("paused", "provider_binding_stale", true)
				f.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
				promote("")
				wantRow("paused", "provider_binding_stale", true)
			} else {
				promote("")
				wantRow("paused", "provider_destination_not_allowed", true)
				if name != "destination_still_removed" {
					f.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
					promote("queued")
					wantRow("queued", "", false)
				}
			}
			if reads.locked.Load() == 0 || reads.revision.Load() == 0 {
				t.Fatal("tracer did not observe the promoter's real transaction queries")
			}
			if reads.ciphertext.Load() != 0 || reads.account.Load() != 0 {
				t.Fatal("promoter fetched credential-opening SQL inputs")
			}
		})
	}
}
