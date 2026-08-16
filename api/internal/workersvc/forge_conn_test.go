package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// forgeConnStore is a minimal workersvc.Store for the ForgeConnForRun authz tests
// (PRD #158). It embeds the interface (every unused method panics) and overrides
// only the two reads that path makes: the owned-run claim check and the
// worker-scoped connection lookup. connCalled records whether the SECOND read ran,
// which is how the 409-before-404 ordering test proves the repo_id gate short-
// circuits before the connection query.
type forgeConnStore struct {
	Store
	ownedRun store.Run
	ownedErr error

	connRow    store.GetRunForgeConnForWorkerRow
	connErr    error
	connCalled bool
}

func (f *forgeConnStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return f.ownedRun, f.ownedErr
}

func (f *forgeConnStore) GetRunForgeConnForWorker(context.Context, store.GetRunForgeConnForWorkerParams) (store.GetRunForgeConnForWorkerRow, error) {
	f.connCalled = true
	return f.connRow, f.connErr
}

// runWithRepoID builds an owned run whose repo_id is valid (a repo-bearing run),
// the identity ForgeConnForRun derives the connection from.
func runWithRepoID() store.Run {
	return store.Run{ID: uuid.New(), UserID: uuid.New(), RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
}

func TestForgeConnForRunHappyPathReturnsConnFacts(t *testing.T) {
	fs := &forgeConnStore{
		ownedRun: runWithRepoID(),
		connRow: store.GetRunForgeConnForWorkerRow{
			ForgeProjectID:  4242,
			ForgeType:       "gitlab",
			BaseUrl:         "https://gl.example.test",
			TokenCiphertext: []byte("sealed-token-bytes"),
		},
	}
	svc := New(fs, newBox(t), Params{})

	conn, err := svc.ForgeConnForRun(context.Background(), worker(), uuid.New())
	if err != nil {
		t.Fatalf("ForgeConnForRun: %v", err)
	}
	if conn.ForgeProjectID != 4242 {
		t.Errorf("ForgeProjectID = %d, want 4242 (derived from the run's connection, never a request param)", conn.ForgeProjectID)
	}
	if conn.ForgeType != "gitlab" || conn.BaseUrl != "https://gl.example.test" {
		t.Errorf("conn facts = %+v, want the connection's forge_type/base_url", conn)
	}
	if string(conn.TokenCiphertext) != "sealed-token-bytes" {
		t.Errorf("TokenCiphertext = %q, want the sealed ciphertext passed through verbatim", conn.TokenCiphertext)
	}
}

func TestForgeConnForRunNotOwnedIsErrRunNotOwned(t *testing.T) {
	// The worker does not hold the run: GetRunOwnedByWorker returns no rows, which
	// runOwnedByWorker maps to ErrRunNotOwned (→ 404 at the handler).
	fs := &forgeConnStore{ownedErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), Params{})

	_, err := svc.ForgeConnForRun(context.Background(), worker(), uuid.New())
	if !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("err = %v, want ErrRunNotOwned", err)
	}
	if fs.connCalled {
		t.Error("a run the worker does not own must not reach the connection query")
	}
}

// TestForgeConnForRunRepolessIsConflictBeforeNotFound is the load-bearing ordering
// test (PRD #158, per the fact-check): a repo-less run the worker DOES own must
// answer ErrForgeNoRepo (→ 409), never ErrRunNotOwned (→ 404). The repo_id check
// runs off the owned run FIRST, before the connection query — which INNER-JOINs
// repos and so cannot itself tell "not owned" apart from "no repo". To make the
// ordering the ONLY thing under test, the connection query is armed to return
// no-rows (which would map to ErrRunNotOwned): the result must still be
// ErrForgeNoRepo, and the connection query must never run.
func TestForgeConnForRunRepolessIsConflictBeforeNotFound(t *testing.T) {
	fs := &forgeConnStore{
		ownedRun: store.Run{ID: uuid.New(), UserID: uuid.New()}, // RepoID zero-value = invalid (repo-less)
		connErr:  pgx.ErrNoRows,                                 // would be ErrRunNotOwned if the ordering were wrong
	}
	svc := New(fs, newBox(t), Params{})

	_, err := svc.ForgeConnForRun(context.Background(), worker(), uuid.New())
	if !errors.Is(err, ErrForgeNoRepo) {
		t.Fatalf("err = %v, want ErrForgeNoRepo (409 must precede the 404 conn-query path)", err)
	}
	if errors.Is(err, ErrRunNotOwned) {
		t.Fatal("a repo-less owned run must be 409 ErrForgeNoRepo, not 404 ErrRunNotOwned")
	}
	if fs.connCalled {
		t.Error("the repo_id gate must short-circuit BEFORE the connection query on a repo-less run")
	}
}

func TestForgeConnForRunConnRaceIsNotOwned(t *testing.T) {
	// The worker owns a repo-bearing run, but the connection query returns no rows
	// (the claim dropped between the two reads). That race is treated as
	// ErrRunNotOwned rather than a 500.
	fs := &forgeConnStore{ownedRun: runWithRepoID(), connErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), Params{})

	_, err := svc.ForgeConnForRun(context.Background(), worker(), uuid.New())
	if !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("err = %v, want ErrRunNotOwned for the claim-dropped race", err)
	}
	if !fs.connCalled {
		t.Error("an owned repo-bearing run must reach the connection query")
	}
}
