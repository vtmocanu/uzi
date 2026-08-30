package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #831 regression, per-change coverage for a race the nightly compose E2E
// exercised but no CI job did. Two concurrent admin settings PUTs — one setting
// uzi_label, one setting autopilot_label — to the SAME value would together violate
// the cross-key invariant (uzi_label != autopilot_label, settings.ValidateMerged).
// Exactly one must be accepted and the other rejected; the two labels must never both
// commit equal.
//
// The transaction's own guard, ListAppSettingsForUpdate (SELECT ... FOR UPDATE), is
// NOT sufficient when the contended key has no stored row: FOR UPDATE locks only rows
// that exist, so the loser's READ COMMITTED snapshot cannot see the winner's freshly
// INSERTED row and both pass the check. store.SettingsMutationLockKey (a
// pg_advisory_xact_lock taken first in the tx) is the fix — a mutex that holds
// regardless of which rows exist.
//
// This test DELETES the uzi_label row before each race, reproducing the exact #831
// condition (a fresh DB has no uzi_label row: 00036 seeded the retired prd_label, and
// PRD #764 renamed the key). That makes the advisory lock, not the 00178 seed row, the
// thing under test.
//
// It runs the race raceIterations times because WITHOUT the lock the defect is only
// ~50% per race: FOR UPDATE serializes the two transactions via the existing
// autopilot_label row lock, so the collision is missed only when the uzi_label writer
// (the one INSERTing a NEW row invisible to the loser's snapshot) wins that ordering.
// The winner-INSERTs case is a coin flip, so a single race is a weak discriminator and
// would be flaky. Over raceIterations flips, an unlocked handler fails with
// overwhelming probability while the fixed one is deterministic (the loser always
// blocks at the advisory lock and re-reads after commit), so this test is both
// non-flaky in the fixed state and a reliable catcher of a regression.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.
const raceIterations = 16

func TestConcurrentCrossKeyLabelPutLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	admin := mkSecretUser(t, pool)
	h := &Handler{
		pool:     pool,
		q:        store.New(pool),
		box:      newHandlerTestBox(t),
		settings: settings.New(store.New(pool), time.Minute),
	}

	put := func(key string) int {
		body, _ := json.Marshal(map[string]any{"settings": map[string]string{key: "SHARED"}})
		rec := httptest.NewRecorder()
		h.UpdateSettings(rec, userReq(http.MethodPut, "/api/admin/settings", string(body), admin, nil))
		return rec.Code
	}

	for iter := 0; iter < raceIterations; iter++ {
		// Reproduce the #831 starting state each iteration: no uzi_label row (so a write
		// to it is an INSERT of a new row), autopilot_label back at its default. This is
		// a shared DB, so reset explicitly rather than assuming a clean slate. finding_label
		// is cleared too: ValidateMerged also rejects uzi_label == finding_label, so a
		// leftover finding_label == "SHARED" from prior state would 400 the uzi writer for
		// an unrelated reason and make the assertion vacuous.
		if _, err := pool.Exec(ctx, `DELETE FROM app_settings WHERE key IN ('uzi_label','prd_label','finding_label')`); err != nil {
			t.Fatalf("iter %d: reset label rows: %v", iter, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO app_settings (key, value) VALUES ('autopilot_label','autopilot')
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
			t.Fatalf("iter %d: reset autopilot_label: %v", iter, err)
		}
		h.settings.Invalidate()

		var wg sync.WaitGroup
		codes := make([]int, 2)
		start := make(chan struct{})
		keys := [2]string{settings.KeyUziLabel, settings.KeyAutopilotLabel}
		for i, key := range keys {
			wg.Add(1)
			go func(i int, key string) {
				defer wg.Done()
				<-start
				codes[i] = put(key)
			}(i, key)
		}
		close(start)
		wg.Wait()

		ok, bad := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusBadRequest:
				bad++
			default:
				t.Fatalf("iter %d: unexpected settings PUT status: %d (codes=%v)", iter, c, codes)
			}
		}
		if ok != 1 || bad != 1 {
			t.Fatalf("iter %d: concurrent cross-key PUT: want exactly one 200 + one 400, got codes=%v", iter, codes)
		}

		// The committed state must never hold the two labels equal — the invariant the
		// whole transaction exists to protect. Read effective values straight from the DB.
		h.settings.Invalidate()
		rows, err := store.New(pool).ListAppSettings(ctx)
		if err != nil {
			t.Fatalf("iter %d: read back: %v", iter, err)
		}
		eff := settings.Effective(rows)
		if eff[settings.KeyUziLabel] == eff[settings.KeyAutopilotLabel] {
			t.Fatalf("iter %d: invariant broken: uzi_label == autopilot_label == %q after the race", iter, eff[settings.KeyUziLabel])
		}
	}
}
