package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// switchLabel parses the "label" out of a credential_switch payload (jsonb reorders keys and adds
// spaces, so the raw bytes must not be string-compared).
func switchLabel(t *testing.T, payload []byte) string {
	t.Helper()
	var p struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal switch payload %s: %v", payload, err)
	}
	return p.Label
}

// GetLatestCredentialSwitchSince (PRD #1247 M9, D14) is only knowable by executing it: it filters
// on kind='credential_switch' AND created_at > @since and orders by seq DESC. That created_at gate
// is the whole point — it scopes the resume DM's token evidence to the CURRENT park cycle, so a
// switch message from an EARLIER cycle cannot re-attribute during a later park (and a
// same-generation epoch re-record never mints a new message anyway). This proves the SQL against a
// real Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway one (e2e/run-store-it.sh).
func TestGetLatestCredentialSwitchSinceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: base.Add(d), Valid: true} }

	insertSwitch := func(seq int32, at time.Duration, label string) {
		mustExec(ctx, t, f.pool,
			`INSERT INTO run_messages (run_id, seq, kind, payload, created_at)
			 VALUES ($1, $2, 'credential_switch', $3, $4)`,
			f.runID, seq, []byte(`{"label":"`+label+`","select_reason":"run_pinned","secret_id":"x"}`), base.Add(at))
	}

	// No switch message yet → no row (the resume DM stays unchanged).
	if _, err := f.q.GetLatestCredentialSwitchSince(ctx, store.GetLatestCredentialSwitchSinceParams{RunID: f.runID, Since: ts(0)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("empty run: want ErrNoRows, got %v", err)
	}

	// An EARLIER-cycle switch at t=1m, and a THIS-cycle park anchored at t=10m.
	insertSwitch(2, 1*time.Minute, "token-old")
	// A message strictly BEFORE the anchor is excluded — this is the no-re-attribute-in-later-park
	// property: querying with since = the later park's start (10m) sees nothing.
	if _, err := f.q.GetLatestCredentialSwitchSince(ctx, store.GetLatestCredentialSwitchSinceParams{RunID: f.runID, Since: ts(10 * time.Minute)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("since after the only (earlier) switch: want ErrNoRows, got %v", err)
	}

	// Now a switch IS applied during this cycle (t=12m > anchor 10m): it is returned.
	insertSwitch(6, 12*time.Minute, "token-new")
	payload, err := f.q.GetLatestCredentialSwitchSince(ctx, store.GetLatestCredentialSwitchSinceParams{RunID: f.runID, Since: ts(10 * time.Minute)})
	if err != nil {
		t.Fatalf("since before this cycle's switch: %v", err)
	}
	if got := switchLabel(t, payload); got != "token-new" {
		t.Fatalf("payload label = %q, want token-new", got)
	}

	// With TWO in-cycle switches, the NEWEST by seq wins (seq DESC).
	insertSwitch(9, 15*time.Minute, "token-newest")
	payload, err = f.q.GetLatestCredentialSwitchSince(ctx, store.GetLatestCredentialSwitchSinceParams{RunID: f.runID, Since: ts(10 * time.Minute)})
	if err != nil {
		t.Fatalf("two in-cycle switches: %v", err)
	}
	if got := switchLabel(t, payload); got != "token-newest" {
		t.Fatalf("payload label = %q, want token-newest", got)
	}
}
