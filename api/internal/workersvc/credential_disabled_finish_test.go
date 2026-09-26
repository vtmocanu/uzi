package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_disabled_finish_test.go pins finishRunClaim's credential_disabled classification
// (PRD #1732 D14) on the transactional fake store: its own non-terminal outcome, the fenced
// park writer, and the in-transaction re-check of the credential a successful payload resolved.
// The LiveDB twins (credential_disabled_livedb_test.go) execute the same paths on Postgres.

// TestFinishRunClaimCredentialDisabledParks: errCredentialDisabled (bare or wrapped) passes the
// early return, releases the claim's own custody hold through the exact release, parks through
// ParkCredentialDisabledRun at the exact claim, and returns an idle claim with no failure.
//
// MUTATION: drop `!errors.Is(assemblyErr, errCredentialDisabled)` from finishRunClaim's early
// return; the error is then returned unclassified with nothing parked and this test fails.
func TestFinishRunClaimCredentialDisabledParks(t *testing.T) {
	for _, assemblyErr := range []error{errCredentialDisabled, fmt.Errorf("%w: pinned token", errCredentialDisabled)} {
		fs, svc, run, id := finishFixture(t)
		id.recoveryCapable = true
		fs.claimFinishHolds = []uuid.UUID{uuid.New()}
		payload, err := svc.finishRunClaim(context.Background(), run, nil, assemblyErr, id)
		if payload != nil || err != nil {
			t.Fatalf("%v: finishRunClaim = (%v, %v), want an idle claim", assemblyErr, payload, err)
		}
		want := store.ParkCredentialDisabledRunParams{ID: run.ID, WorkerID: pgconv.UUID(id.workerID), ClaimGeneration: run.ClaimGeneration}
		if fs.claimCredParked == nil || *fs.claimCredParked != want {
			t.Fatalf("%v: credential park = %+v, want %+v", assemblyErr, fs.claimCredParked, want)
		}
		if fs.claimReleased != 1 {
			t.Fatalf("%v: exact custody releases = %d, want 1", assemblyErr, fs.claimReleased)
		}
		if fs.claimFailed != nil || fs.claimRequeued != nil || fs.claimParked != nil || fs.markedFailed != nil {
			t.Fatalf("%v: classified as another outcome: failed=%v requeued=%v codexPark=%v", assemblyErr, fs.claimFailed, fs.claimRequeued, fs.claimParked)
		}
	}
}

// TestFinishRunClaimCredentialDisabledStaleClaimIdle: a lock that no longer shows this exact
// claim leaves the run untouched: no park, no release.
func TestFinishRunClaimCredentialDisabledStaleClaimIdle(t *testing.T) {
	for name, mutate := range map[string]func(*store.Run){
		"stale worker":     func(r *store.Run) { r.WorkerID = pgconv.UUID(uuid.New()) },
		"stale generation": func(r *store.Run) { r.ClaimGeneration++ },
		"running flight":   func(r *store.Run) { r.Status = "running" },
		"stale epoch":      func(r *store.Run) { r.CodexClaimEpoch++ },
	} {
		fs, svc, run, id := finishFixture(t)
		locked := run
		locked.Status, locked.WorkerID = "claimed", pgconv.UUID(id.workerID)
		mutate(&locked)
		fs.claimFinishLocked = &locked
		if payload, err := svc.finishRunClaim(context.Background(), run, nil, errCredentialDisabled, id); payload != nil || err != nil {
			t.Fatalf("%s: finishRunClaim = (%v, %v), want idle", name, payload, err)
		}
		if fs.claimCredParked != nil || fs.claimReleased != 0 || fs.claimFailed != nil {
			t.Fatalf("%s: a stale claim was written: park=%v released=%d", name, fs.claimCredParked, fs.claimReleased)
		}
	}
}

// TestFinishRunClaimRecheckParksDisabledCredential: a successful Claude payload whose recorded
// credential is disabled by the time the claim decision locks it parks instead of delivering;
// an enabled one delivers. This is the disable-after-open, before-delivery window.
//
// MUTATION: skip the claimCredentialDisabled call in finishRunClaimTx; the disabled case then
// delivers the payload and this test fails.
func TestFinishRunClaimRecheckParksDisabledCredential(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		fs, svc, run, id := finishFixture(t)
		fs.claimRun.AnthropicSecretID = pgconv.UUID(uuid.New())
		fs.claimSecretDisabled = disabled
		sent := &ClaimPayload{RunID: run.ID.String(), Secrets: ClaimSecrets{AnthropicOAuthToken: "tok"}}
		got, err := svc.finishRunClaim(context.Background(), run, sent, nil, id)
		if err != nil || fs.claimSecretChecks != 1 {
			t.Fatalf("disabled=%t: err=%v checks=%d", disabled, err, fs.claimSecretChecks)
		}
		if disabled && (got != nil || fs.claimCredParked == nil) {
			t.Fatalf("disabled credential delivered=%t parked=%v, want parked and nothing delivered", got != nil, fs.claimCredParked)
		}
		if !disabled && (got != sent || fs.claimCredParked != nil) {
			t.Fatalf("enabled credential delivered=%t parked=%v, want delivered", got == sent, fs.claimCredParked)
		}
	}
}

// TestFinishRunClaimRecheckLockBusyRetries: the re-check's NOWAIT lock refusal (55P03) rolls the
// transaction back and retries it through the bounded loop; it never blocks and never parks on
// a lock it could not read.
func TestFinishRunClaimRecheckLockBusyRetries(t *testing.T) {
	defer func(d time.Duration) { finishRunClaimRetryDelay = d }(finishRunClaimRetryDelay)
	finishRunClaimRetryDelay = time.Millisecond
	busy := &pgconn.PgError{Code: "55P03"}
	fs, svc, run, id := finishFixture(t)
	fs.claimRun.AnthropicSecretID = pgconv.UUID(uuid.New())
	fs.claimSecretLockErrs = []error{busy, busy}
	sent := &ClaimPayload{RunID: run.ID.String(), Secrets: ClaimSecrets{AnthropicOAuthToken: "tok"}}
	if got, err := svc.finishRunClaim(context.Background(), run, sent, nil, id); err != nil || got != sent {
		t.Fatalf("busy twice then free = (%v, %v), want the payload", got, err)
	}
	if fs.claimFinishBegins != finishRunClaimAttempts || fs.claimSecretChecks != finishRunClaimAttempts {
		t.Fatalf("begins=%d checks=%d, want %d each", fs.claimFinishBegins, fs.claimSecretChecks, finishRunClaimAttempts)
	}
	fs, svc, run, id = finishFixture(t)
	fs.claimRun.AnthropicSecretID = pgconv.UUID(uuid.New())
	fs.claimSecretDisabled = true
	fs.claimSecretLockErrs = []error{busy, busy, busy}
	got, err := svc.finishRunClaim(context.Background(), run, sent, nil, id)
	if got != nil || !isLockNotAvailable(err) {
		t.Fatalf("always busy = (%v, %v), want (nil, 55P03)", got, err)
	}
	if fs.claimCredParked != nil || fs.claimFailed != nil {
		t.Fatal("an unreadable re-check committed a write")
	}
	if errors.Is(err, errCredentialDisabled) {
		t.Fatal("a lock refusal was reported as a disabled credential")
	}
}
