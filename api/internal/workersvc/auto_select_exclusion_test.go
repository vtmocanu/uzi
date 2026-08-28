package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #217 M2 + #754 M2 — autoChoice's dead-credential exclusion, from the claim
// path's side. autoselect's own tests own the RANKING exclusion; these own the WIRING:
// that runs.limit_dead_secret_id reaches BOTH Select (ranking exit) and Floor (the
// #754 floor exit), so the just-parked credential is never re-picked NOR re-floored.
//
// #754 M2 reshaped the non-picking exits: the old D7 fallback resolved the owner
// default (and swapped off it when it was the dead credential), which is the bug #754
// fixes. The auto lane now stays inside the pool — it floors onto another POOLED token
// or, when none remains, HOLDS (errAutoPoolEmpty ⇒ requeue). It never resolves the
// non-pooled owner default, so these tests assert the SPENT credential is a pooled one
// or that the claim held with nothing recorded.

// TestAutoChoiceRankingExitExcludesDeadCredential drives the RANKING exit. Both
// tokens are fresh and eligible and the dead one carries MORE headroom, so without
// the exclusion it wins outright — the exclusion is the only thing that hands the
// claim to the survivor, which is what proves `exclude` reaches Select.
//
// MUTATION THIS CATCHES: autoChoice passing uuid.Nil instead of the run's
// limit_dead_secret_id to Select → the dead token's 95 headroom wins.
func TestAutoChoiceRankingExitExcludesDeadCredential(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	// The window is still CLOSED (retry_not_before in the future), so #754 M3's relax does
	// NOT fire and the dead credential stays excluded — that is the state this test exercises.
	f.fs.claimRun.RetryNotBefore = pgtype.Timestamptz{Time: autoNow.Add(time.Hour), Valid: true}
	// The gap must exceed the tie tolerance (T=5), or the tie-break would hand the pick
	// to the sooner-reset token regardless of the exclusion and the test would not
	// discriminate it. 95 vs 80 is 15 points apart, so the dead token is ALONE in the
	// cluster and wins outright — unless it is excluded.
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.fullID, "console-key", true, 95, time.Minute, 0), // dead — clear headroom lead
		candRow(f.emptyID, "spare-key", true, 80, time.Minute, 0),  // the survivor
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("ranking spent %v, want the non-excluded token %v — the excluded dead token had "+
			"MORE headroom and would win but for the exclusion", uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonAuto) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonAuto)
	}
	if !rec.AnthropicHeadroomPct.Valid || rec.AnthropicHeadroomPct.Int16 != 80 {
		t.Fatalf("headroom = %+v, want the survivor's 80", rec.AnthropicHeadroomPct)
	}
}

// TestAutoChoiceFloorExcludesDeadDefaultWhenAnotherPooledTokenExists drives the FLOOR
// exit with the dead credential pooled among others. The pool is stale (so Select
// names no pick and the ladder falls to Floor) and the dead credential is the pooled
// owner default. Floor honours `exclude`, so it skips the dead default and spends the
// deterministic tie-break winner among the OTHER pooled tokens — never the dead one,
// and never the non-pooled default resolution (which #754 removed entirely).
//
// MUTATION THIS CATCHES: Floor ignoring `exclude` → the dead default (soonest-reset /
// lowest-id among equals) could be re-floored onto.
func TestAutoChoiceFloorExcludesDeadDefaultWhenAnotherPooledTokenExists(t *testing.T) {
	f := newAutoFixture(t)
	def := f.fs.defaultCredID()
	// Two known low ids so the tie-break winner is unambiguous: equal (stale) resets
	// fall through to the secret id, and 0x01 < 0x02.
	altLow := uuid.UUID{0x01}
	altHigh := uuid.UUID{0x02}

	// The resuming run parked on its owner default, window still CLOSED (retry_not_before
	// in the future) so #754 M3's relax does not fire and the exclusion applies.
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: def, Valid: true}
	f.fs.claimRun.RetryNotBefore = pgtype.Timestamptz{Time: autoNow.Add(time.Hour), Valid: true}
	// A STALE pool ⇒ Select names no pick; the rows stay auto-eligible, so Floor can
	// still choose among them.
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(altHigh, "alt-high", true, 90, 99*time.Hour, 0),
		candRow(altLow, "alt-low", true, 90, 99*time.Hour, 0),
		candRow(def, "default-pooled", true, 90, 99*time.Hour, 0),
	}
	// Only the WINNER must be openable (altHigh is never opened). Reuse the fixture's
	// sealed spare-key ciphertext, which is sealed by this fixture's box and owned by
	// the run owner.
	f.fs.byIDSecrets[altLow] = f.fs.byIDSecrets[f.emptyID]
	f.fs.byIDLabels[altLow] = "alt-low"

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != altLow {
		t.Fatalf("floor spent %v, want the tie-break winner %v, NOT the dead default %v",
			uuid.UUID(rec.AnthropicSecretID.Bytes), altLow, def)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolStale) {
		t.Fatalf("reason = %q, want %q — a floor records pool_stale", rec.AnthropicSelectReason.String, autoselect.ReasonPoolStale)
	}
	// A floor measured nothing, so no headroom is recorded.
	if rec.AnthropicHeadroomPct.Valid {
		t.Fatalf("headroom = %+v, want NULL — a floor measured no credential", rec.AnthropicHeadroomPct)
	}
}

// TestAutoChoiceHoldsWhenTheOnlyPooledTokenIsTheDeadCredential is #754 M2's change to
// the old SC4 behaviour. When the resuming run's ONLY pooled token is the excluded
// dead credential, there is nothing else pooled to spend — and the auto lane must NOT
// fall to the non-pooled owner default (the #754 bug). Floor.ok is false (the sole
// pooled candidate is excluded), so autoChoice signals errAutoPoolEmpty and the claim
// HOLDS: it requeues, records nothing, and never spends the default.
//
// MUTATION THIS CATCHES: reinstating the owner-default fallback here → the dead
// default (or the plain default) is re-spent instead of the run holding.
func TestAutoChoiceHoldsWhenTheOnlyPooledTokenIsTheDeadCredential(t *testing.T) {
	f := newAutoFixture(t)
	def := f.fs.defaultCredID()
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: def, Valid: true}
	// Window still CLOSED (retry_not_before in the future), so #754 M3's relax does not
	// fire — re-picking would immediately re-hit the limit — and the exclusion holds.
	f.fs.claimRun.RetryNotBefore = pgtype.Timestamptz{Time: autoNow.Add(time.Hour), Valid: true}
	// The only pooled row IS the dead default. Floor excludes it ⇒ ok false ⇒ hold.
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(def, "default-pooled", true, 90, 99*time.Hour, 0),
	}

	if payload := f.claim(t); payload != nil {
		t.Fatal("the only pooled token was the dead credential, yet the claim produced a payload; " +
			"M2 holds rather than spending the non-pooled default")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential when the only pooled token was dead: %+v — above all it must not be the default",
			f.fs.recordedCreds)
	}
	if f.fs.requeuedRun == nil || *f.fs.requeuedRun != f.runID {
		t.Fatalf("run not requeued: %v — an empty-pool hold is transient", f.fs.requeuedRun)
	}
	if f.fs.markedFailed != nil {
		t.Fatalf("the run was failed terminally (%v); the empty-pool hold must not hard-fail (M2 interim)", f.fs.markedFailed)
	}
}

// TestAutoChoiceFloorSkipsTheDeadTokenAndSpendsTheSurvivingPooledToken drives the
// floor exit when the dead credential is a NON-default pooled token. The dead token is
// excluded from the floor, so the OTHER pooled token (the survivor) is spent — a
// pooled credential, never the non-pooled owner default (#754).
//
// MUTATION THIS CATCHES: reinstating the owner-default resolution on the non-picking
// exit → the survivor is skipped and the default is spent.
func TestAutoChoiceFloorSkipsTheDeadTokenAndSpendsTheSurvivingPooledToken(t *testing.T) {
	f := newAutoFixture(t)
	// The run parked on a non-default pooled token (fullID), not the owner default, with
	// its window still CLOSED (retry_not_before in the future) so the exclusion applies.
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	f.fs.claimRun.RetryNotBefore = pgtype.Timestamptz{Time: autoNow.Add(time.Hour), Valid: true}
	// A stale pool ⇒ the floor exit. The survivor (emptyID) is the only non-excluded
	// pooled token and must be spent; the owner default must NOT appear.
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, 99*time.Hour, 0),
		candRow(f.fullID, "console-key", true, 90, 99*time.Hour, 0),
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) == f.fs.defaultCredID() {
		t.Fatal("floored onto the owner default — the auto lane must NEVER spend the non-pooled default (#754)")
	}
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("floored onto %v, want the surviving pooled token %v",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolStale) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonPoolStale)
	}
}

// TestAutoChoiceNilDeadSecretFloorsOntoThePooledToken: a claim that is NOT resuming
// from a park (limit_dead_secret_id NULL) takes the exclude==uuid.Nil path — a stale
// pool floors onto its pooled token (pool_stale), NOT the owner default (#754).
func TestAutoChoiceNilDeadSecretFloorsOntoThePooledToken(t *testing.T) {
	f := newAutoFixture(t) // claimRun.LimitDeadSecretID left invalid
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, 99*time.Hour, 0), // stale ⇒ floor
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) == f.fs.defaultCredID() {
		t.Fatal("floored onto the owner default with no dead-secret id — #754 forbids spending the non-pooled default")
	}
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("floored onto %v, want the pooled token %v", uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolStale) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonPoolStale)
	}
}
