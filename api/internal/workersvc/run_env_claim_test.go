package workersvc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4416 M2: the claim's run-env assembly and finish check against fakes. The real transaction
// semantics (the owner lock, races, receipts in the database) are in run_env_claim_livedb_test.go.

func runEnvWorker() store.Worker {
	w := worker()
	w.ProtocolCapabilities = []string{capability.RunEnvV1}
	return w
}

// envRow seals value under the master box (no per-user vault) so a unit test opens it with no vault.
func envRow(t *testing.T, svc *Service, name, value string, secret bool, rev int32, varID uuid.UUID) store.ListRunEnvEffectiveForRunRow {
	t.Helper()
	ct, err := svc.box.Seal([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return store.ListRunEnvEffectiveForRunRow{
		VarID: varID, Rev: rev, Name: name, Secret: secret, Description: "about " + name, Scope: "repo",
		RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Ciphertext: ct, SealedWith: store.SealedWithMaster, AadVersion: 2,
	}
}

func redactRowOf(r store.ListRunEnvEffectiveForRunRow) store.ListRunEnvRedactForRunRow {
	return store.ListRunEnvRedactForRunRow{
		VarID: r.VarID, Rev: r.Rev, Scope: r.Scope, Name: r.Name, RepoID: r.RepoID, Ciphertext: r.Ciphertext,
		SealedWith: pgtype.Text{String: r.SealedWith, Valid: true}, AadVersion: pgtype.Int2{Int16: r.AadVersion, Valid: true},
	}
}

func TestAssembleRunEnvDeduplicatesByVarIDAndRev(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, newBox(t), testParams())
	id := uuid.New()
	live := envRow(t, svc, "SECRET_ONE", "sekret-2", true, 2, id)
	old := envRow(t, svc, "SECRET_ONE", "sekret-1", true, 1, id)
	other := envRow(t, svc, "PLAIN_ONE", "alpha", false, 1, uuid.New())
	// The same (var_id, rev) appears twice in each list and once in both lists; each is delivered once.
	fs.runEnvLive = []store.ListRunEnvEffectiveForRunRow{live, live, other}
	fs.runEnvRedact = []store.ListRunEnvRedactForRunRow{redactRowOf(old), redactRowOf(old), redactRowOf(live)}
	run := store.Run{ID: uuid.New(), UserID: uuid.New(), Kind: "issue"}
	payload := &ClaimPayload{}
	if err := svc.assembleRunEnv(context.Background(), runEnvWorker(), run, payload); err != nil {
		t.Fatalf("assembleRunEnv: %v", err)
	}
	if got := envNames(payload); !eq(got, []string{"PLAIN_ONE=alpha", "SECRET_ONE=sekret-2"}) {
		t.Fatalf("run_env = %v", got)
	}
	if got := redactNames(payload); !eq(got, []string{"SECRET_ONE=sekret-1"}) {
		t.Fatalf("run_env_redact = %v: the live version must not be repeated, the old one must appear once", got)
	}
	if n := len(payload.runEnv.live) + len(payload.runEnv.redact); n != 3 {
		t.Fatalf("snapshot holds %d versions, want 3", n)
	}
}

func TestAssembleRunEnvFailureModes(t *testing.T) {
	run := store.Run{ID: uuid.New(), UserID: uuid.New(), Kind: "issue"}
	cases := []struct {
		name   string
		worker store.Worker
		rows   func(svc *Service) (live []store.ListRunEnvEffectiveForRunRow, redact []store.ListRunEnvRedactForRunRow)
		want   error
	}{
		{"worker without run_env_v1", worker(), func(s *Service) ([]store.ListRunEnvEffectiveForRunRow, []store.ListRunEnvRedactForRunRow) {
			return []store.ListRunEnvEffectiveForRunRow{envRow(t, s, "A_ONE", "x", false, 1, uuid.New())}, nil
		}, errRunEnvCapabilityMissing},
		{"locked vault", runEnvWorker(), func(s *Service) ([]store.ListRunEnvEffectiveForRunRow, []store.ListRunEnvRedactForRunRow) {
			r := envRow(t, s, "A_ONE", "x", true, 1, uuid.New())
			r.SealedWith = store.SealedWithDEK
			return []store.ListRunEnvEffectiveForRunRow{r}, nil
		}, errVaultLocked},
		{"live value does not open", runEnvWorker(), func(s *Service) ([]store.ListRunEnvEffectiveForRunRow, []store.ListRunEnvRedactForRunRow) {
			r := envRow(t, s, "A_ONE", "x", true, 1, uuid.New())
			r.Ciphertext = []byte("garbage-not-sealed-by-this-box")
			return []store.ListRunEnvEffectiveForRunRow{r}, nil
		}, errRunEnvHistoryUnavailable},
		{"receipted version row missing", runEnvWorker(), func(s *Service) ([]store.ListRunEnvEffectiveForRunRow, []store.ListRunEnvRedactForRunRow) {
			r := redactRowOf(envRow(t, s, "A_ONE", "x", true, 1, uuid.New()))
			r.Ciphertext, r.SealedWith, r.AadVersion = nil, pgtype.Text{}, pgtype.Int2{}
			return nil, []store.ListRunEnvRedactForRunRow{r}
		}, errRunEnvHistoryUnavailable},
		{"unknown aad version", runEnvWorker(), func(s *Service) ([]store.ListRunEnvEffectiveForRunRow, []store.ListRunEnvRedactForRunRow) {
			r := envRow(t, s, "A_ONE", "x", true, 1, uuid.New())
			r.AadVersion = 9
			return []store.ListRunEnvEffectiveForRunRow{r}, nil
		}, errRunEnvHistoryUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			svc := New(fs, newBox(t), testParams())
			fs.runEnvLive, fs.runEnvRedact = tc.rows(svc)
			payload := &ClaimPayload{}
			err := svc.assembleRunEnv(context.Background(), tc.worker, run, payload)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(payload.Secrets.RunEnv) != 0 {
				t.Fatalf("a failed assembly left %d values in the payload", len(payload.Secrets.RunEnv))
			}
		})
	}
}

// A run outside the run-env lane (a review run, a profile-bound run, a chat or job kind) is left
// untouched and its rows are never read.
func TestAssembleRunEnvSkipsRunsOutsideTheLane(t *testing.T) {
	for _, run := range []store.Run{
		{ID: uuid.New(), UserID: uuid.New(), Kind: "issue", ReviewTargetRunID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
		{ID: uuid.New(), UserID: uuid.New(), Kind: "issue", EgressProfileID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
		{ID: uuid.New(), UserID: uuid.New(), Kind: "cross_check"},
		{ID: uuid.New(), UserID: uuid.New(), Kind: "job"},
	} {
		fs := &fakeStore{}
		svc := New(fs, newBox(t), testParams())
		fs.runEnvLive = []store.ListRunEnvEffectiveForRunRow{envRow(t, svc, "A_ONE", "x", false, 1, uuid.New())}
		payload := &ClaimPayload{}
		if err := svc.assembleRunEnv(context.Background(), worker(), run, payload); err != nil {
			t.Fatalf("%s run: %v", run.Kind, err)
		}
		if payload.runEnv != nil || len(payload.Secrets.RunEnv) != 0 {
			t.Fatalf("%s run (review=%v profile=%v) was given variables", run.Kind, run.ReviewTargetRunID.Valid, run.EgressProfileID.Valid)
		}
	}
}

// An isolated-lane claim never carries variables, whatever the assembly put there.
func TestStripForIsolationClearsRunEnv(t *testing.T) {
	p := &ClaimPayload{runEnv: &runEnvSnapshot{live: []runEnvDelivery{{rev: 1}}}}
	p.Secrets.RunEnv = []ClaimRunEnvVar{{Name: "A_ONE", Value: "x"}}
	p.Secrets.RunEnvRedact = []ClaimRunEnvRedact{{Name: "B_ONE", Value: "y"}}
	stripForIsolation(p)
	if p.runEnv != nil || p.Secrets.RunEnv != nil || p.Secrets.RunEnvRedact != nil {
		t.Fatalf("stripForIsolation left run-env state: %+v", p.Secrets)
	}
}

// finishFixture runs assembleAndFinishRunClaim over a fake issue run that has the given variables.
func runEnvFinishFixture(t *testing.T, w store.Worker, keysMatch bool) (*fakeStore, *ClaimPayload, error) {
	t.Helper()
	fs := claimSizeStore(t)
	fs.claimRun.Kind, fs.claimRun.UserID = "issue", w.UserID
	svc := newOffModeService(fs, newBox(t), testParams())
	row := envRow(t, svc, "PLAIN_ONE", "alpha", false, 1, uuid.New())
	fs.runEnvLive = []store.ListRunEnvEffectiveForRunRow{row}
	if keysMatch {
		fs.runEnvLiveKeys = []store.ListRunEnvEffectiveKeysForRunRow{{VarID: row.VarID, Rev: row.Rev}}
	} else {
		fs.runEnvLiveKeys = []store.ListRunEnvEffectiveKeysForRunRow{{VarID: row.VarID, Rev: row.Rev + 1}}
	}
	payload, err := svc.assembleAndFinishRunClaim(context.Background(), w, fs.claimRun, false)
	return fs, payload, err
}

func TestRunEnvFinishWritesReceiptsOnAMatchingRecomputation(t *testing.T) {
	fs, payload, err := runEnvFinishFixture(t, runEnvWorker(), true)
	if err != nil || payload == nil {
		t.Fatalf("claim = (%v, %v), want a payload", payload, err)
	}
	if fs.runEnvLockCalls == 0 {
		t.Fatal("a lead-lane finish did not take the owner share lock")
	}
	if len(fs.runEnvReceipts) != 1 || fs.runEnvReceipts[0].Name != "PLAIN_ONE" || fs.runEnvReceipts[0].Secret || fs.runEnvReceipts[0].Rev != 1 {
		t.Fatalf("receipts = %+v, want one plain receipt for PLAIN_ONE rev 1", fs.runEnvReceipts)
	}
}

func TestRunEnvFinishRequeuesOnAStaleSnapshot(t *testing.T) {
	fs, payload, err := runEnvFinishFixture(t, runEnvWorker(), false)
	if err != nil || payload != nil {
		t.Fatalf("claim = (%v, %v), want idle", payload, err)
	}
	if fs.claimRequeued == nil || len(fs.runEnvReceipts) != 0 || fs.claimFailed != nil || fs.claimRunEnvParked != nil {
		t.Fatalf("requeued=%v receipts=%v failed=%v parked=%v, want a requeue and nothing else", fs.claimRequeued, fs.runEnvReceipts, fs.claimFailed, fs.claimRunEnvParked)
	}
}

func TestRunEnvFinishRequeuesWhenAVariableAppearsForAWorkerWithoutTheCapability(t *testing.T) {
	// Assembly saw no variables (so needed no capability); the finish then sees one on a worker without
	// run_env_v1. The recomputation differs from the empty snapshot, so the claim is requeued (the SQL gate
	// keeps that worker off the run afterwards). The finish-time capability re-check is defence in depth
	// behind that comparison and is not separately reachable with a consistent snapshot.
	fs := claimSizeStore(t)
	w := worker()
	fs.claimRun.Kind, fs.claimRun.UserID = "issue", w.UserID
	svc := newOffModeService(fs, newBox(t), testParams())
	id := uuid.New()
	fs.runEnvLiveKeys = []store.ListRunEnvEffectiveKeysForRunRow{{VarID: id, Rev: 1}}
	payload, err := svc.assembleAndFinishRunClaim(context.Background(), w, fs.claimRun, false)
	if err != nil || payload != nil {
		t.Fatalf("claim = (%v, %v), want idle", payload, err)
	}
	if fs.claimRequeued == nil || len(fs.runEnvReceipts) != 0 {
		t.Fatalf("requeued=%v receipts=%v, want a requeue and no receipt", fs.claimRequeued, fs.runEnvReceipts)
	}
}

func TestRunEnvHistoryUnavailableParksTheClaim(t *testing.T) {
	fs := claimSizeStore(t)
	w := runEnvWorker()
	fs.claimRun.Kind, fs.claimRun.UserID = "issue", w.UserID
	svc := newOffModeService(fs, newBox(t), testParams())
	r := redactRowOf(envRow(t, svc, "A_ONE", "x", true, 1, uuid.New()))
	r.Ciphertext, r.SealedWith, r.AadVersion = nil, pgtype.Text{}, pgtype.Int2{}
	fs.runEnvRedact = []store.ListRunEnvRedactForRunRow{r}
	payload, err := svc.assembleAndFinishRunClaim(context.Background(), w, fs.claimRun, false)
	if err != nil || payload != nil {
		t.Fatalf("claim = (%v, %v), want idle", payload, err)
	}
	if fs.claimRunEnvParked == nil || fs.claimRequeued != nil || fs.claimFailed != nil || len(fs.runEnvReceipts) != 0 {
		t.Fatalf("parked=%v requeued=%v failed=%v, want only the run-env park", fs.claimRunEnvParked, fs.claimRequeued, fs.claimFailed)
	}
}

func TestRunEnvLaneMatchesTheClaimLaneForNonLeadRuns(t *testing.T) {
	fs := claimSizeStore(t)
	w := worker()
	fs.claimRun.Kind, fs.claimRun.UserID = "chat", w.UserID
	svc := newOffModeService(fs, newBox(t), testParams())
	if _, err := svc.assembleAndFinishRunClaim(context.Background(), w, fs.claimRun, false); err != nil {
		t.Fatal(err)
	}
	if fs.runEnvLockCalls != 0 {
		t.Fatal("a run outside the run-env lane took the owner share lock")
	}
}

// Worst case wire size of the run-env additions (PRD #4416 R3). Arithmetic, at the limits of
// internal/runenv:
//
//	live:   50 variables x ( value 65536 bytes, every byte escaped as \u0001 (6 bytes) = 393216
//	                         + description 1024 bytes escaped = 6144 + name 128 + a ~200 byte envelope ) = 50 x 399688
//	        = 19.98 MB
//	redact: 200 versions sharing 2097152 value bytes (the retired-byte limit), escaped = 12582912
//	        + 200 x ( name 128 + a ~150 byte envelope ) = 12.64 MB
//	total:  about 32.6 MB. The pinned ceiling adds a margin of under 8 MiB for the rest of the payload.
//
// A quarter of the claim response limit is 44040192 bytes: the additions must stay under it.
func TestWorstCaseRunEnvClaimSize(t *testing.T) {
	const (
		quarterOfLimit = claimResponseMaxBytes / 4 // 44040192
		pinnedCeiling  = 40_000_000
	)
	escaped := func(n int) string { return strings.Repeat("\x01", n) }
	name := strings.Repeat("N", 128)
	p := &ClaimPayload{Kind: "issue"}
	for i := 0; i < 50; i++ {
		p.Secrets.RunEnv = append(p.Secrets.RunEnv, ClaimRunEnvVar{
			VarID: uuid.New(), Scope: "repo", Rev: 1000000 + i, Name: name, Value: escaped(65536), Secret: true, Description: escaped(1024),
		})
	}
	perVersion := 2097152 / 200
	for i := 0; i < 200; i++ {
		p.Secrets.RunEnvRedact = append(p.Secrets.RunEnvRedact, ClaimRunEnvRedact{
			VarID: uuid.New(), Scope: "repo", Rev: 1000000 + i, Name: name, Value: escaped(perVersion),
		})
	}
	rec := httptest.NewRecorder()
	if err := httpx.WriteJSON(rec, http.StatusOK, p); err != nil {
		t.Fatal(err)
	}
	size := rec.Body.Len()
	t.Logf("worst-case run-env claim body: %d bytes (quarter of the limit %d, pinned ceiling %d)", size, quarterOfLimit, pinnedCeiling)
	if size >= quarterOfLimit {
		t.Fatalf("worst-case claim body is %d bytes, not under a quarter (%d) of the claim response limit", size, quarterOfLimit)
	}
	if size > pinnedCeiling {
		t.Fatalf("worst-case claim body is %d bytes, over the pinned ceiling %d: update the arithmetic above and review the margin", size, pinnedCeiling)
	}
	if size < 30_000_000 {
		t.Fatalf("worst-case claim body is only %d bytes: the fixture no longer reaches the limits it models", size)
	}
}

// A lane payload that carries no assembly record was never assembled by assembleRunEnv; the finish
// refuses it instead of passing it through unverified (fail closed), and a non-lane run is not asked.
func TestRunEnvFinishRefusesALanePayloadWithoutASnapshot(t *testing.T) {
	lane := store.Run{ID: uuid.New(), UserID: uuid.New(), Kind: "issue"}
	if err := settleRunEnv(context.Background(), nil, lane, &ClaimPayload{}, claimRecoveryIdentity{}); !errors.Is(err, errRunEnvSnapshotMissing) {
		t.Fatalf("lane payload without a snapshot: err = %v, want errRunEnvSnapshotMissing", err)
	}
	outside := store.Run{ID: uuid.New(), UserID: uuid.New(), Kind: "judge"}
	if err := settleRunEnv(context.Background(), nil, outside, &ClaimPayload{}, claimRecoveryIdentity{}); err != nil {
		t.Fatalf("a run outside the lane was checked: %v", err)
	}
	if err := settleRunEnv(context.Background(), nil, lane, nil, claimRecoveryIdentity{}); err != nil {
		t.Fatalf("an undelivered claim was checked: %v", err)
	}
}
