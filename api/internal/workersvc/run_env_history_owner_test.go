package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A Codex run whose assembly found its receipted history unavailable parks even when the
// lock-time Codex authority rewrite would otherwise turn the outcome into a terminal credential
// failure (here: the run has no Codex binding). The park is decided from assembly, like the
// override-unavailable and disabled-credential parks.
func TestFinishRunClaimRunEnvHistoryParksOnCodexRunDespiteAuthorityFailure(t *testing.T) {
	fs, svc, run, id := finishFixture(t)
	run.Harness = harnessCodex
	fs.claimRun = run
	payload, err := svc.finishRunClaim(context.Background(), run, nil, errRunEnvHistoryUnavailable, id)
	if payload != nil || err != nil {
		t.Fatalf("finishRunClaim = (%v, %v), want an idle park", payload, err)
	}
	if fs.runEnv.parked == nil || fs.runEnv.parked.ID != run.ID || fs.runEnv.parked.ClaimGeneration != run.ClaimGeneration ||
		fs.runEnv.parked.WorkerID != pgconv.UUID(id.workerID) {
		t.Fatalf("parked = %+v, want the exact-claim run_env_history_unavailable park", fs.runEnv.parked)
	}
	if fs.claimFailed != nil || fs.markedFailed != nil || fs.claimRequeued != nil {
		t.Fatalf("the park was rewritten into failed=%v requeued=%v", fs.claimFailed, fs.claimRequeued)
	}
}

// sealForRunOwner seals value the way a master_aad row of owner is sealed: the AAD binds the
// owner and the run-environment kind. A row stored under another user that opens under the run
// owner's context is the case only the explicit owner check can catch.
func sealForRunOwner(t *testing.T, box *secretbox.Box, owner uuid.UUID, kind, value string) []byte {
	t.Helper()
	aad := append(append(owner[:len(owner):len(owner)], 0), kind...)
	ct, err := box.SealWithAAD([]byte(value), aad)
	if err != nil {
		t.Fatal(err)
	}
	return ct
}

// A row stored under another owner is never delivered or redacted, even when its ciphertext opens
// under the run owner's context: assembly reports the history unavailable before any value is read.
func TestAttachRunEnvRefusesRowsOfAnotherOwner(t *testing.T) {
	key := make([]byte, secretbox.KeySize)
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	wkr := store.Worker{ProtocolCapabilities: []string{capability.RunEnvV1}}
	run := store.Run{ID: uuid.New(), UserID: uuid.New(), Kind: "issue"}
	foreign := uuid.New()
	repo := uuid.New()
	repoPG := pgtype.UUID{Bytes: repo, Valid: true}
	varID := uuid.New()
	kind, err := runenv.AADFor(runenv.AADVersion2, run.UserID, runenv.ScopeRepo, &repo, true, "TOKEN", varID, 1)
	if err != nil {
		t.Fatal(err)
	}
	ct := sealForRunOwner(t, box, run.UserID, kind, envSecretValue("x"))
	newSvc := func(fs *fakeStore) *Service {
		return newOffModeService(fs, box, testParams())
	}

	t.Run("control: the run owner's own row is delivered", func(t *testing.T) {
		fs := &fakeStore{}
		fs.runEnv.effective = []store.ListRunEnvEffectiveForRunRow{{
			VarID: varID, Rev: 1, Name: "TOKEN", Secret: true, Ciphertext: ct, SealedWith: "master_aad",
			AadVersion: runenv.AADVersion2, RepoID: repoPG, UserID: run.UserID,
		}}
		payload := &ClaimPayload{}
		if err := newSvc(fs).attachRunEnv(context.Background(), wkr, run, payload); err != nil || len(payload.Secrets.RunEnv) != 1 {
			t.Fatalf("attachRunEnv = %v, delivered %d; the fixture ciphertext must open for the owner", err, len(payload.Secrets.RunEnv))
		}
	})
	t.Run("effective row", func(t *testing.T) {
		fs := &fakeStore{}
		fs.runEnv.effective = []store.ListRunEnvEffectiveForRunRow{{
			VarID: varID, Rev: 1, Name: "TOKEN", Secret: true, Ciphertext: ct, SealedWith: "master_aad",
			AadVersion: runenv.AADVersion2, RepoID: repoPG, UserID: foreign,
		}}
		payload := &ClaimPayload{}
		err := newSvc(fs).attachRunEnv(context.Background(), wkr, run, payload)
		if !errors.Is(err, errRunEnvHistoryUnavailable) || len(payload.Secrets.RunEnv) != 0 {
			t.Fatalf("attachRunEnv = %v, delivered %d; want errRunEnvHistoryUnavailable and nothing delivered", err, len(payload.Secrets.RunEnv))
		}
	})
	t.Run("redact row", func(t *testing.T) {
		fs := &fakeStore{}
		fs.runEnv.redact = []store.ListRunEnvRedactForRunRow{{
			VarID: varID, Rev: 1, Name: "TOKEN", Scope: "repo",
			RowID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Ciphertext: ct,
			SealedWith: pgtype.Text{String: "master_aad", Valid: true}, AadVersion: pgtype.Int2{Int16: runenv.AADVersion2, Valid: true},
			RowSecret: pgtype.Bool{Bool: true, Valid: true}, RowRepoID: repoPG,
			RowUserID: pgtype.UUID{Bytes: foreign, Valid: true}, RowName: pgtype.Text{String: "TOKEN", Valid: true},
		}}
		payload := &ClaimPayload{}
		err := newSvc(fs).attachRunEnv(context.Background(), wkr, run, payload)
		if !errors.Is(err, errRunEnvHistoryUnavailable) || len(payload.Secrets.RunEnvRedact) != 0 {
			t.Fatalf("attachRunEnv = %v, redacted %d; want errRunEnvHistoryUnavailable and nothing redacted", err, len(payload.Secrets.RunEnvRedact))
		}
	})
}
