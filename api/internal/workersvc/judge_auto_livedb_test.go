package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestJudgeAutoPicksHigherHeadroomLiveDB is PRD #1140 M2's judge-lane `auto` mode against
// real Postgres: a judge run for a user in `auto` mode with two POOLED, MEASURED tokens
// runs the SAME ranker the run lane uses (D7) and picks the higher-headroom token, reason
// `auto` — exactly what an issue run gets. The offline TestJudgeChoiceAutoPicksHigherHeadroom
// pins the same behaviour through hand-written candidate fixtures; this proves the REAL
// ListAutoSelectCandidates query feeds the ranker correctly end to end.
//
// It asserts judgeChoice's returned secretChoice (the id, reason and headroom), which is
// sufficient and preferred: it exercises the real query + ranker WITHOUT needing a
// box-openable token, so the seeded ciphertext is never decrypted.
//
// The reference clock is autoNow (as the offline auto tests use), so the seeded gauge
// rows mirror candRow's timestamps exactly and classify identically under autoParams'
// real staleness policy. testParams' zero Autoselect policy would make every token stale
// (MaxStaleness 0) and floor to pool_stale, so autoParams is used deliberately here.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestJudgeAutoPicksHigherHeadroomLiveDB(t *testing.T) {
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
	defer pool.Close()

	q := store.New(pool)
	svc := New(q, newBox(t), autoParams())
	svc.now = func() time.Time { return autoNow }

	// A fresh user (raw SQL); every fixture key is a fresh uuid, since the store-IT runner
	// shares ONE database across the whole LiveDB set and many columns are UNIQUE.
	userID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("judge-auto-%s@e2e", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	insertToken := func(label string, first bool) uuid.UUID {
		t.Helper()
		row, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: userID, Kind: store.KindAnthropicToken, Label: label, WantDefault: first,
			Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert token %s: %v", label, err)
		}
		return row.ID
	}

	// Two POOLED tokens. The first is born auto_eligible (#804); the second is born
	// opt-out (InsertUserSecret only pools a user's FIRST token), so pool it explicitly.
	hiID := insertToken("high-room", true) // born-eligible first token, the winner
	loID := insertToken("low-room", false) // second token, opted into the pool below
	if _, err := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{
		ID: loID, UserID: userID, Kind: store.KindAnthropicToken, AutoEligible: true,
	}); err != nil {
		t.Fatalf("pool the second token: %v", err)
	}

	// Give each token a MEASURED gauge reading, mirroring candRow: headroom is expressed
	// through the five-hour window (pct = 100 - headroom, reset in the future), the
	// seven-day window left empty, and a RECENT synced_at (relative to autoNow) so both
	// read fresh rather than stale. hiID has clearly more headroom (90) than loID (20).
	upsertRL := func(secretID uuid.UUID, headroom int16) {
		t.Helper()
		if err := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
			UserSecretID:     secretID,
			UserID:           userID,
			FiveHourPct:      pgtype.Int2{Int16: 100 - headroom, Valid: true},
			FiveHourResetsAt: pgtype.Timestamptz{Time: autoNow.Add(time.Duration(headroom) * time.Hour), Valid: true},
			SevenDayPct:      pgtype.Int2{Int16: 0, Valid: true},
			SevenDayResetsAt: pgtype.Timestamptz{}, // NULL, as candRow leaves it
			Source:           pgtype.Text{},        // NULL; the ranker does not project source
			SyncedAt:         pgtype.Timestamptz{Time: autoNow.Add(-time.Minute), Valid: true},
		}); err != nil {
			t.Fatalf("upsert rate limits for %v: %v", secretID, err)
		}
	}
	upsertRL(hiID, 90)
	upsertRL(loID, 20)

	// Put the user's judge lane in `auto` mode with no pinned pointer.
	if _, err := q.SetUserJudgeAnthropicBinding(ctx, store.SetUserJudgeAnthropicBindingParams{
		ID: userID, JudgeAnthropicBindMode: BindModeAuto, JudgeAnthropicSecretID: pgtype.UUID{},
	}); err != nil {
		t.Fatalf("set judge binding to auto: %v", err)
	}

	// A judge run owned by the user. judgeChoice → autoChoice needs run.UserID and reads
	// claimExclude(run): with LimitDeadSecretID left NULL there is no excluded dead
	// credential, so both pooled tokens rank.
	run := store.Run{ID: uuid.New(), UserID: userID, Kind: runkind.Judge}

	choice, err := svc.judgeChoice(ctx, run)
	if err != nil {
		t.Fatalf("judgeChoice: %v", err)
	}
	if choice.secretID == nil || *choice.secretID != hiID {
		t.Fatalf("secretID = %v, want the higher-headroom pooled token %v", choice.secretID, hiID)
	}
	if choice.reason != string(autoselect.ReasonAuto) {
		t.Fatalf("reason = %q, want %q (a measured winner from the real ListAutoSelectCandidates query)", choice.reason, autoselect.ReasonAuto)
	}
	if choice.headroom == nil || *choice.headroom != 90 {
		t.Fatalf("headroom = %v, want 90", choice.headroom)
	}
}
