package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #754 M3 — the caller-side exclude-relax. autoChoice/autoFloorRetry stop excluding
// the just-parked dead credential ONCE its usage window has reopened (retry_not_before
// has passed — which is what let PromoteLimitWaitRuns return the run to queued), so the
// resume re-picks or floors onto that very token instead of being pushed to a hold or
// another token. While retry_not_before is still in the future the window is closed and
// the exclusion holds. autoselect.Select's contract is untouched — the relax is a CALLER
// decision, expressed entirely through the `exclude` value claimExclude returns.

// TestClaimExcludeWindowStates is the unit test for claimExclude itself: the four
// LimitDeadSecretID/RetryNotBefore combinations, decided against the fixture clock.
func TestClaimExcludeWindowStates(t *testing.T) {
	f := newAutoFixture(t) // svc.now == autoNow via the fixture
	dead := f.fullID
	future := pgtype.Timestamptz{Time: autoNow.Add(time.Hour), Valid: true}
	past := pgtype.Timestamptz{Time: autoNow.Add(-time.Hour), Valid: true}

	cases := []struct {
		name string
		run  store.Run
		want uuid.UUID
	}{
		{
			name: "window still closed (retry_not_before in the future) → keep excluding",
			run: store.Run{
				LimitDeadSecretID: pgtype.UUID{Bytes: dead, Valid: true},
				RetryNotBefore:    future,
			},
			want: dead,
		},
		{
			name: "window reopened (retry_not_before in the past) → relax to Nil",
			run: store.Run{
				LimitDeadSecretID: pgtype.UUID{Bytes: dead, Valid: true},
				RetryNotBefore:    past,
			},
			want: uuid.Nil,
		},
		{
			name: "no reset stamp (retry_not_before NULL) → relax to Nil",
			run: store.Run{
				LimitDeadSecretID: pgtype.UUID{Bytes: dead, Valid: true},
				RetryNotBefore:    pgtype.Timestamptz{Valid: false},
			},
			want: uuid.Nil,
		},
		{
			name: "no dead credential → Nil",
			run: store.Run{
				LimitDeadSecretID: pgtype.UUID{Valid: false},
				RetryNotBefore:    future, // ignored: nothing to exclude
			},
			want: uuid.Nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.svc.claimExclude(tc.run); got != tc.want {
				t.Fatalf("claimExclude = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAutoChoiceRelaxFloorsOntoReopenedDeadToken proves the RELAX reaches the floor:
// a single pooled token that IS the just-parked dead credential, its window reopened
// (retry_not_before in the past), gauge stale. Without the relax, claimExclude would
// still return the dead id, Floor would exclude the sole pooled token, and the claim
// would HOLD (errAutoPoolEmpty). With the relax the exclude is Nil, so Floor lands on
// that same reopened token — "continue on cristi".
//
// MUTATION THIS CATCHES: claimExclude ignoring retry_not_before (always excluding the
// dead id) → the sole pooled token is excluded and the claim holds instead of flooring.
func TestAutoChoiceRelaxFloorsOntoReopenedDeadToken(t *testing.T) {
	f := newAutoFixture(t)
	// The resuming run parked on its single pooled token, whose window has since reopened.
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: f.emptyID, Valid: true}
	f.fs.claimRun.RetryNotBefore = pgtype.Timestamptz{Time: autoNow.Add(-time.Hour), Valid: true}
	// One STALE pooled token — the very dead credential. Stale ⇒ Select names no pick and
	// the ladder falls to Floor, which (relax ⇒ exclude Nil) spends this token.
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, 99*time.Hour, 0),
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) == f.fs.defaultCredID() {
		t.Fatal("floored onto the owner default — the auto lane must NEVER spend the non-pooled default (#754)")
	}
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("floored onto %v, want the reopened dead token %v — the window has reopened so it must be re-spendable",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolStale) {
		t.Fatalf("reason = %q, want %q — a floor records pool_stale", rec.AnthropicSelectReason.String, autoselect.ReasonPoolStale)
	}
}

// TestAutoChoiceStillExcludesDeadTokenWhileWindowClosed is the mirror: same single-token
// setup, but retry_not_before is still in the FUTURE, so the window is closed and the
// dead credential stays excluded. With no other pooled token the claim HOLDS
// (errAutoPoolEmpty ⇒ pool_wait, PRD #754 M4), recording nothing — re-picking now would
// immediately re-hit the limit.
//
// MUTATION THIS CATCHES: claimExclude relaxing while the window is still closed →
// the sole pooled token is spent and the run immediately re-limits.
func TestAutoChoiceStillExcludesDeadTokenWhileWindowClosed(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: f.emptyID, Valid: true}
	f.fs.claimRun.RetryNotBefore = pgtype.Timestamptz{Time: autoNow.Add(time.Hour), Valid: true}
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, 99*time.Hour, 0),
	}

	if payload := f.claim(t); payload != nil {
		t.Fatal("the only pooled token's window is still closed, yet the claim produced a payload; " +
			"it must hold rather than re-pick a token that would immediately re-limit")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential while the sole pooled token's window was still closed: %+v", f.fs.recordedCreds)
	}
	// PRD #754 M4: a window-closed hold now transitions the run to pool_wait, not requeue.
	if f.fs.poolWaitHeld == nil || f.fs.poolWaitHeld.ID != f.runID {
		t.Fatalf("run not held in pool_wait: %v — a window-closed hold holds the run", f.fs.poolWaitHeld)
	}
	if f.fs.requeuedRun != nil {
		t.Fatalf("run was requeued (%v); M4 replaced the requeue with the pool_wait hold", f.fs.requeuedRun)
	}
	if f.fs.markedFailed != nil {
		t.Fatalf("the run was failed terminally (%v); a window-closed hold must not hard-fail", f.fs.markedFailed)
	}
	// M3's exclude-relax must still be able to read the dead credential on resume, so the
	// hold must NOT clear limit_dead_secret_id — the query leaves it in place (asserted at
	// the SQL layer by the live-DB test); here we simply confirm the hold, not a requeue,
	// is what fired.
}
