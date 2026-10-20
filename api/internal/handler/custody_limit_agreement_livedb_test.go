package handler

// #3150 cross-consumer agreement: one custody_hold_limit setting (the real settings cache, written
// with UpsertAppSetting + Invalidate exactly as the admin PUT does) feeds the workersvc claim, the
// workersvc health pill, the owner recovery-hold aggregate, the healthsvc custody.holds check and
// the Slack custody-episode reconciler. For an owner holding limit-1 counted holds none of them
// treats the owner as at the limit; with limit holds every one does. Run at 4 (below the old
// compiled-in 8) and 12 (above it) so a consumer still reading a constant fails one of the two.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type agreementNotifier struct{ owners []uuid.UUID }

func (n *agreementNotifier) Notify(_ context.Context, in notifysvc.Notification) (store.Notification, error) {
	n.owners = append(n.owners, in.UserID)
	return store.Notification{}, nil
}

// custodyOwnersInHealthCheck is the Owners evidence of healthsvc's custody.holds check (0 when the
// check reports no owner at the limit).
func custodyOwnersInHealthCheck(t *testing.T, doc healthsvc.Doc) int {
	t.Helper()
	for _, c := range doc.Checks {
		if c.ID != "custody.holds" {
			continue
		}
		for _, e := range c.Evidence {
			if e.Label == "Owners" {
				n, err := strconv.Atoi(e.Value)
				if err != nil {
					t.Fatalf("custody.holds Owners = %q", e.Value)
				}
				return n
			}
		}
		return 0
	}
	t.Fatal("custody.holds check missing from the health document")
	return 0
}

func TestCustodyLimitCrossConsumerAgreementLiveDB(t *testing.T) {
	ctx := context.Background()
	for _, limit := range []int{4, 12} {
		for _, holds := range []int{limit - 1, limit} {
			atLimit := holds >= limit
			t.Run(fmt.Sprintf("limit-%d/holds-%d", limit, holds), func(t *testing.T) {
				var cache *settings.Cache
				f := newClaimGuardFixtureWith(ctx, t, func(q *store.Queries) workersvc.CustodyHoldLimitReader {
					cache = settings.New(q, time.Hour)
					return cache
				})
				q := store.New(f.pool)
				if _, err := q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{Key: settings.KeyCustodyHoldLimit, Value: strconv.Itoa(limit)}); err != nil {
					t.Fatal(err)
				}
				cache.Invalidate()
				t.Cleanup(func() {
					mustExecT(ctx, t, f.pool, `DELETE FROM recovery_custody_holds WHERE user_id=$1`, f.owner)
					mustExecT(ctx, t, f.pool, `DELETE FROM custody_episode_notices WHERE user_id=$1`, f.owner)
					mustExecT(ctx, t, f.pool, `DELETE FROM app_settings WHERE key=$1`, settings.KeyCustodyHoldLimit)
					cache.Invalidate()
				})
				f.svc.SetHealthSettings(cache)

				fresh := f.seedEnabledRepoAndQueuedRun(ctx, t, 700, 7, protClean)
				// Past the queued-health threshold so the pill resolves the real queued reason.
				mustExecT(ctx, t, f.pool, `UPDATE runs SET status_since = now() - interval '2 hours' WHERE id=$1`, fresh)
				var repoID uuid.UUID
				if err := f.pool.QueryRow(ctx, `SELECT repo_id FROM runs WHERE id=$1`, fresh).Scan(&repoID); err != nil {
					t.Fatal(err)
				}
				hcfg := healthsvc.Config{Store: q, Pool: f.pool, Settings: cache, Now: time.Now, HeartbeatStale: 45 * time.Second}
				// Holds with no live pointers are always admission-counted.
				for range holds {
					mustExecT(ctx, t, f.pool, `INSERT INTO recovery_custody_holds
					  (user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity)
					  VALUES ($1, $2, $3, 1, 'open', $4, 'agreement-fixture')`, f.owner, repoID, uuid.New(), f.wkr.ID)
				}

				// Health pill (through the exported Sweep): the real queued reason for the fresh run.
				if _, err := f.svc.Sweep(ctx); err != nil {
					t.Fatalf("Sweep: %v", err)
				}
				var reason string
				if err := f.pool.QueryRow(ctx, `SELECT COALESCE(health_reason,'') FROM runs WHERE id=$1`, fresh).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if got := strings.Contains(reason, "too much unpublished work"); got != atLimit {
					t.Errorf("pill custody reason = %v (reason %q), want %v", got, reason, atLimit)
				}

				// healthsvc custody.holds is instance-wide and the Sweep above may touch other tests'
				// leftovers, so take its baseline now with this owner's holds closed, then reopen.
				mustExecT(ctx, t, f.pool, `UPDATE recovery_custody_holds SET state='released' WHERE user_id=$1`, f.owner)
				baseDoc, err := healthsvc.New(hcfg).Evaluate(ctx)
				if err != nil {
					t.Fatal(err)
				}
				base := custodyOwnersInHealthCheck(t, baseDoc)
				mustExecT(ctx, t, f.pool, `UPDATE recovery_custody_holds SET state='open' WHERE user_id=$1`, f.owner)

				// Owner recovery-hold aggregate.
				rsvc := recovery.New(q, f.pool, nil, recovery.Limits{CustodyHoldLimit: cache.CustodyHoldLimit, WorkerHeartbeatStale: 45 * time.Second}, nil)
				list, err := rsvc.ListHoldsForOwner(ctx, f.owner, false)
				if err != nil {
					t.Fatal(err)
				}
				wantBlocked := 0
				if atLimit {
					wantBlocked = 1
				}
				if a := list.Aggregate; a.CustodyHoldLimit != limit || a.AdmissionCountedHolds != holds || a.BlockedRuns != wantBlocked {
					t.Errorf("recovery aggregate = %+v, want limit %d counted %d blocked %d", a, limit, holds, wantBlocked)
				}

				// healthsvc custody.holds.
				doc, err := healthsvc.New(hcfg).Evaluate(ctx)
				if err != nil {
					t.Fatal(err)
				}
				wantOwners := base
				if atLimit {
					wantOwners = base + 1
				}
				if got := custodyOwnersInHealthCheck(t, doc); got != wantOwners {
					t.Errorf("healthsvc custody.holds owners = %d, want %d (baseline %d)", got, wantOwners, base)
				}

				// Slack custody-episode reconciler.
				notif := &agreementNotifier{}
				slacksvc.NewCustodyEpisodeReconciler(q, notif, cache, 45*time.Second, time.Now, nil).Reconcile(ctx)
				notified := 0
				for _, o := range notif.owners {
					if o == f.owner {
						notified++
					}
				}
				if want := map[bool]int{true: 1, false: 0}[atLimit]; notified != want {
					t.Errorf("custody episode DMs for the owner = %d, want %d", notified, want)
				}

				// Claim last: it consumes the queued run when admitted.
				payload, err := f.svc.Claim(ctx, f.wkr, nil)
				if err != nil {
					t.Fatalf("Claim: %v", err)
				}
				if claimed := payload != nil; claimed == atLimit {
					t.Errorf("claim admitted = %v with %d holds at limit %d, want %v", claimed, holds, limit, !atLimit)
				}
			})
		}
	}
}
