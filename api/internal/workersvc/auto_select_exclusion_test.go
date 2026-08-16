package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #217 M2 — autoChoice's dead-credential exclusion, from the claim path's side.
// autoselect's own tests own the RANKING exclusion; these own the WIRING: that
// runs.limit_dead_secret_id reaches Select on the ranking exit, and that the
// CONDITIONAL fallback exclusion resolves the owner default before comparing it.

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

// TestAutoChoiceFallbackExcludesDeadDefaultWhenAlternativeExists drives the FALLBACK
// exit with an alternative available. The pool is stale (so Select returns
// pool_stale and never names a pick) but the rows are still auto-eligible, and the
// dead credential is the owner default. The fallback resolves that default, sees it
// IS the excluded credential, and picks the deterministic lowest-id alternative
// instead of re-spending the dead token.
//
// MUTATION THIS CATCHES: the fallback ignoring `exclude` (returning the owner default
// unconditionally) → the dead default is re-spent.
func TestAutoChoiceFallbackExcludesDeadDefaultWhenAlternativeExists(t *testing.T) {
	f := newAutoFixture(t)
	def := f.fs.defaultCredID()
	// Two known low ids so "the lowest-id alternative" is unambiguous, and both below
	// the default's 0xd0… first byte.
	altLow := uuid.UUID{0x01}
	altHigh := uuid.UUID{0x02}

	// The resuming run parked on its owner default.
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: def, Valid: true}
	// A STALE pool ⇒ Select falls back (pool_stale); the rows stay auto-eligible, so
	// lowestAltCandidate can still choose among them.
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
		t.Fatalf("fallback spent %v, want the lowest-id alternative %v, NOT the dead default %v",
			uuid.UUID(rec.AnthropicSecretID.Bytes), altLow, def)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolStale) {
		t.Fatalf("reason = %q, want %q — the fallback reason is preserved even when the resolved "+
			"credential changes", rec.AnthropicSelectReason.String, autoselect.ReasonPoolStale)
	}
	// A fallback measured nothing, so no headroom is recorded.
	if rec.AnthropicHeadroomPct.Valid {
		t.Fatalf("headroom = %+v, want NULL — a fallback measured no credential", rec.AnthropicHeadroomPct)
	}
}

// TestAutoChoiceFallbackSpendsDeadDefaultWhenNoAlternative is SC4: a single-token
// user's resume still runs. The dead default is the only pooled token, so there is
// no alternative to resolve to and the dead credential is spent anyway — auto never
// fails a run.
//
// The reason is pool_EMPTY, not pool_stale, and that is the exclusion's own doing:
// Select skips the dead credential BEFORE `pooled` is set, so a pool whose only
// member is the excluded token reads as empty. The point of the test is which
// credential is SPENT, and it is the dead default because there is nothing else.
//
// MUTATION THIS CATCHES: making the fallback exclusion UNCONDITIONAL → a single-token
// user's resume has nothing to spend and the claim breaks.
func TestAutoChoiceFallbackSpendsDeadDefaultWhenNoAlternative(t *testing.T) {
	f := newAutoFixture(t)
	def := f.fs.defaultCredID()
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: def, Valid: true}
	// The only pooled row IS the dead default. Excluded before `pooled` is set, so
	// Select sees an empty pool and never names a pick ⇒ the fallback exit.
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(def, "default-pooled", true, 90, 99*time.Hour, 0),
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != def {
		t.Fatalf("a single-token user's resume spent %v, want the dead default %v — auto never "+
			"fails a run (SC4)", uuid.UUID(rec.AnthropicSecretID.Bytes), def)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolEmpty) {
		t.Fatalf("reason = %q, want %q — the sole pooled token is the excluded dead one, which "+
			"Select drops before `pooled` is set", rec.AnthropicSelectReason.String, autoselect.ReasonPoolEmpty)
	}
}

// TestAutoChoiceFallbackKeepsDefaultWhenItIsNotTheDeadCredential pins the FALSE arm
// of the conditional exclusion. The dead credential is a non-default pooled token, so
// the fallback's resolved owner default differs from the exclusion — and the rule is
// to spend that default, NOT to swap to an alternative. The exclusion fires only when
// the fallback WOULD otherwise spend the dead credential.
//
// MUTATION THIS CATCHES: making the fallback swap unconditional (or inverting the
// `resolved == exclude` guard) → an alternative is spent when the default was fine.
func TestAutoChoiceFallbackKeepsDefaultWhenItIsNotTheDeadCredential(t *testing.T) {
	f := newAutoFixture(t)
	// The run parked on a non-default pooled token (fullID), not the owner default.
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	// A stale pool ⇒ fallback. An alternative (emptyID) exists and must NOT be chosen,
	// because the owner default the fallback resolves is not the dead credential.
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, 99*time.Hour, 0),
		candRow(f.fullID, "console-key", true, 90, 99*time.Hour, 0),
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fs.defaultCredID() {
		t.Fatalf("fallback spent %v, want the owner default %v — the resolved default is NOT the "+
			"dead credential, so the exclusion must not fire", uuid.UUID(rec.AnthropicSecretID.Bytes), f.fs.defaultCredID())
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolStale) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonPoolStale)
	}
}

// TestAutoChoiceNilDeadSecretIsUnchanged: a claim that is NOT resuming from a park
// (limit_dead_secret_id NULL) takes the exclude==uuid.Nil early return — today's
// behaviour exactly, a stale pool falling back to the owner default with no
// GetDefaultUserSecretMeta comparison in the exclusion path.
func TestAutoChoiceNilDeadSecretIsUnchanged(t *testing.T) {
	f := newAutoFixture(t) // claimRun.LimitDeadSecretID left invalid
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, 99*time.Hour, 0), // stale ⇒ fallback
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fs.defaultCredID() {
		t.Fatalf("with no dead-secret id, fell back to %v, want the owner default %v",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.fs.defaultCredID())
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolStale) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonPoolStale)
	}
}
