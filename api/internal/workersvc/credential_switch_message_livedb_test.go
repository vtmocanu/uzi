package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_switch_message_livedb_test.go is the M9 (task c) live-DB gate for the applied-switch
// 'credential_switch' run message emitted by recordRunCredential on the run-lane claim path: the
// epoch-delta detector, the fence-under-lock rollback, the idempotency guard, the gapless
// seq-collision recovery, the returned last_seq thread-back, the emitSwitchMessage gate, and the
// after-commit broadcast. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (run via ./e2e/run-store-it.sh); sqlc type inference is never trusted from a clean generate.

// recordingSwitchBroadcaster captures PublishMessage calls so a test can assert the after-commit
// broadcast (never before commit). The other Broadcaster methods are no-ops.
//
// When pool/ctx are set, PublishMessage also reads the two committed rows on a FRESH connection at
// publication time — the switch message at the published seq and the advanced runs.last_seq — and
// records whether BOTH are visible, which is only possible if the broadcast fires after tx.Commit.
type recordingSwitchBroadcaster struct {
	pool     *pgxpool.Pool
	ctx      context.Context
	messages []recordedSwitchMsg
	// committedVisible is set on the FIRST PublishMessage when pool is non-nil: true iff both the
	// switch row (at seq, kind credential_switch) and runs.last_seq>=seq are readable then.
	committedVisible bool
	visErr           error
}

type recordedSwitchMsg struct {
	runID   uuid.UUID
	seq     int32
	kind    string
	payload []byte
}

func (b *recordingSwitchBroadcaster) PublishMessage(runID uuid.UUID, seq int32, kind, _, _, _ string, payload []byte, _ time.Time) {
	b.messages = append(b.messages, recordedSwitchMsg{runID: runID, seq: seq, kind: kind, payload: append([]byte(nil), payload...)})
	if b.pool == nil {
		return
	}
	// Both writes committed before this call, so a fresh read must see them.
	var rowKind string
	if err := b.pool.QueryRow(b.ctx, `SELECT kind FROM run_messages WHERE run_id = $1 AND seq = $2`, runID, seq).Scan(&rowKind); err != nil {
		b.visErr = err
		return
	}
	var lastSeq int32
	if err := b.pool.QueryRow(b.ctx, `SELECT last_seq FROM runs WHERE id = $1`, runID).Scan(&lastSeq); err != nil {
		b.visErr = err
		return
	}
	b.committedVisible = rowKind == credentialSwitchMessageKind && lastSeq >= seq
}
func (b *recordingSwitchBroadcaster) PublishState(uuid.UUID, string)                {}
func (b *recordingSwitchBroadcaster) PublishHealth(uuid.UUID, string, string, bool) {}
func (b *recordingSwitchBroadcaster) PublishInput(uuid.UUID)                        {}

// seedAnthropicToken inserts a master-sealed anthropic_token user_secret and returns its id. The
// ciphertext is a synthetic fixture assembled at runtime (codexToken), never a token-shaped literal.
func (e codexTestEnv) seedAnthropicToken(t *testing.T, userID uuid.UUID, label string) uuid.UUID {
	t.Helper()
	sealed, err := e.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	id := uuid.New()
	e.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
	        VALUES ($1, $2, 'anthropic_token', $3, $4, 'master')`, id, userID, label, sealed)
	return id
}

// seedSwitchRun inserts a 'claimed' issue run at the given claim_generation + last_seq, owned by
// workerID. status/kind satisfy runs_kind_shape; claim_released_at is left NULL (a live claim).
func (e codexTestEnv) seedSwitchRun(t *testing.T, userID, workerID, repoID uuid.UUID, gen int64, lastSeq int32) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, last_seq)
	        VALUES ($1, $2, $3, 'issue', 101, 't', 'd', 'claimed', $4, $5, $6)`,
		runID, userID, repoID, workerID, gen, lastSeq)
	return runID
}

// seedEpoch inserts a run_credential_epochs row (the attribution journal) at a given generation.
func (e codexTestEnv) seedEpoch(t *testing.T, runID, secretID uuid.UUID, gen int64, label, reason string) {
	t.Helper()
	e.exec(`INSERT INTO run_credential_epochs (run_id, claim_generation, secret_id, label, select_reason)
	        VALUES ($1, $2, $3, $4, $5)`, runID, gen, secretID, label, reason)
}

// countSwitchMessages returns how many 'credential_switch' run_messages a run carries.
func (e codexTestEnv) countSwitchMessages(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = 'credential_switch'`, runID).Scan(&n); err != nil {
		t.Fatalf("count switch messages: %v", err)
	}
	return n
}

func switchSvc(env codexTestEnv, bc Broadcaster) *Service {
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	if bc != nil {
		svc.SetBroadcaster(bc)
	}
	return svc
}

// switchMessagePayload is the parsed 'credential_switch' payload for assertions.
type switchMessagePayload struct {
	Label        *string `json:"label"`
	SelectReason string  `json:"select_reason"`
	SecretID     string  `json:"secret_id"`
}

// A→B switch: a prior epoch named token A, the current claim spends token B, so recordRunCredential
// emits exactly ONE 'credential_switch' message at last_seq+1 carrying B's label/reason/id, advances
// runs.last_seq to it, returns that seq (the value ClaimPayload.LastSeq must use), and broadcasts it
// AFTER commit. The epoch journal gains B's epoch. This is the headline happy path.
func TestRecordRunCredentialSwitchMessageAToBLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 2, 5)
	env.seedEpoch(t, runID, tokenA, 1, "token-a", "default")

	bc := &recordingSwitchBroadcaster{pool: env.pool, ctx: env.ctx}
	svc := switchSvc(env, bc)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: 5}
	cred := claimCred{ID: tokenB, Label: "token-b"}
	choice := secretChoice{reason: selectReasonRunPinned}

	got, err := svc.recordRunCredential(env.ctx, run, cred, choice, true)
	if err != nil {
		t.Fatalf("recordRunCredential: %v", err)
	}
	if got != 6 {
		t.Fatalf("returned last_seq = %d, want 6 (5+1)", got)
	}
	if n := env.countSwitchMessages(t, runID); n != 1 {
		t.Fatalf("credential_switch messages = %d, want exactly 1", n)
	}

	// The message: seq 6, generation 2, payload names token B.
	var seq int32
	var gen int64
	var payload []byte
	if err := env.pool.QueryRow(env.ctx,
		`SELECT seq, claim_generation, payload FROM run_messages WHERE run_id = $1 AND kind = 'credential_switch'`,
		runID).Scan(&seq, &gen, &payload); err != nil {
		t.Fatalf("read switch message: %v", err)
	}
	if seq != 6 || gen != 2 {
		t.Fatalf("switch message seq/gen = %d/%d, want 6/2", seq, gen)
	}
	var p switchMessagePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal payload %s: %v", payload, err)
	}
	if p.Label == nil || *p.Label != "token-b" {
		t.Fatalf("payload label = %v, want token-b", p.Label)
	}
	if p.SelectReason != selectReasonRunPinned {
		t.Fatalf("payload select_reason = %q, want %q", p.SelectReason, selectReasonRunPinned)
	}
	if p.SecretID != tokenB.String() {
		t.Fatalf("payload secret_id = %q, want %q", p.SecretID, tokenB)
	}

	// runs.last_seq advanced to the seq we landed.
	var lastSeq int32
	if err := env.pool.QueryRow(env.ctx, `SELECT last_seq FROM runs WHERE id = $1`, runID).Scan(&lastSeq); err != nil {
		t.Fatalf("read last_seq: %v", err)
	}
	if lastSeq != 6 {
		t.Fatalf("runs.last_seq = %d, want 6", lastSeq)
	}

	// The epoch journal recorded B at generation 2.
	epochs, err := env.q.ListRunCredentialEpochs(env.ctx, store.ListRunCredentialEpochsParams{RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("ListRunCredentialEpochs: %v", err)
	}
	if len(epochs) != 2 {
		t.Fatalf("epochs = %d, want 2 (gen 1 A, gen 2 B)", len(epochs))
	}
	if epochs[1].ClaimGeneration != 2 || !epochs[1].SecretID.Valid || uuid.UUID(epochs[1].SecretID.Bytes) != tokenB {
		t.Fatalf("gen-2 epoch = %+v, want token B", epochs[1])
	}

	// The message was broadcast AFTER commit, exactly once, with the run's seq + kind.
	if len(bc.messages) != 1 {
		t.Fatalf("broadcast messages = %d, want 1", len(bc.messages))
	}
	if bc.messages[0].seq != 6 || bc.messages[0].kind != credentialSwitchMessageKind || bc.messages[0].runID != runID {
		t.Fatalf("broadcast = %+v, want run %s seq 6 kind %s", bc.messages[0], runID, credentialSwitchMessageKind)
	}
	// Publication happened AFTER tx.Commit: at broadcast time a fresh read saw BOTH committed rows —
	// the switch message at seq 6 and the advanced runs.last_seq.
	if bc.visErr != nil {
		t.Fatalf("committed-visibility read at publish time failed: %v", bc.visErr)
	}
	if !bc.committedVisible {
		t.Fatal("switch row and advanced last_seq were NOT both visible at publish time — broadcast must fire after tx.Commit")
	}
}

// A FIRST claim (no prior epoch) emits NO message and returns the run's unchanged last_seq — there
// is no earlier token to have switched from.
func TestRecordRunCredentialFirstClaimNoMessageLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 1, 5)

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 1, LastSeq: 5}
	got, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenA, Label: "token-a"}, secretChoice{reason: selectReasonDefault}, true)
	if err != nil {
		t.Fatalf("recordRunCredential: %v", err)
	}
	if got != 5 {
		t.Fatalf("returned last_seq = %d, want 5 (unchanged; no message)", got)
	}
	if n := env.countSwitchMessages(t, runID); n != 0 {
		t.Fatalf("credential_switch messages = %d, want 0 on a first claim", n)
	}
}

// A same-token reclaim (prior epoch names the SAME token this claim spends) emits NO message: the
// token did not change, so nothing was switched.
func TestRecordRunCredentialSameTokenReclaimNoMessageLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 2, 5)
	env.seedEpoch(t, runID, tokenA, 1, "token-a", "default")

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: 5}
	got, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenA, Label: "token-a"}, secretChoice{reason: selectReasonDefault}, true)
	if err != nil {
		t.Fatalf("recordRunCredential: %v", err)
	}
	if got != 5 {
		t.Fatalf("returned last_seq = %d, want 5 (unchanged; same-token reclaim)", got)
	}
	if n := env.countSwitchMessages(t, runID); n != 0 {
		t.Fatalf("credential_switch messages = %d, want 0 on a same-token reclaim", n)
	}
}

// A same-generation retry (the whole claim re-assembles at the SAME generation, e.g. after a crash)
// re-records the epoch but must NOT duplicate the switch message: the idempotency guard keys on
// (run_id, claim_generation). Two calls → exactly one message.
func TestRecordRunCredentialSameGenerationRetryNoDuplicateLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 2, 5)
	env.seedEpoch(t, runID, tokenA, 1, "token-a", "default")

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: 5}
	cred := claimCred{ID: tokenB, Label: "token-b"}
	choice := secretChoice{reason: selectReasonRunPinned}

	first, err := svc.recordRunCredential(env.ctx, run, cred, choice, true)
	if err != nil {
		t.Fatalf("recordRunCredential #1: %v", err)
	}
	if first != 6 {
		t.Fatalf("first returned last_seq = %d, want 6", first)
	}
	// Second call at the same generation (the run row now shows last_seq 6 after the first commit,
	// but the in-memory run still snapshots 5 — exactly the crash-retry shape).
	second, err := svc.recordRunCredential(env.ctx, run, cred, choice, true)
	if err != nil {
		t.Fatalf("recordRunCredential #2: %v", err)
	}
	if n := env.countSwitchMessages(t, runID); n != 1 {
		t.Fatalf("credential_switch messages after retry = %d, want exactly 1 (idempotent)", n)
	}
	// The retry returns the run's current last_seq (6, no new message), never a bumped duplicate.
	if second != 6 {
		t.Fatalf("retry returned last_seq = %d, want 6 (locked high-water mark, no new message)", second)
	}
}

// A (run_id, seq) collision: a worker frame already occupies last_seq+1 (its InsertRunMessage is not
// serialized by the switch transaction's run-row lock). The switch message must re-read MAX(seq) and
// retry at max+1, leaving NO gap and NOT clobbering the worker frame.
func TestRecordRunCredentialSeqCollisionGaplessLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 2, 5)
	env.seedEpoch(t, runID, tokenA, 1, "token-a", "default")

	// A worker frame already at seq 6 (= last_seq+1) — the seq the switch message would first try.
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload, claim_generation)
	          VALUES ($1, 6, 'text', $2, 2)`, runID, []byte(`{"event":"text","text":"worker frame"}`))

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: 5}
	got, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenB, Label: "token-b"}, secretChoice{reason: selectReasonRunPinned}, true)
	if err != nil {
		t.Fatalf("recordRunCredential: %v", err)
	}
	if got != 7 {
		t.Fatalf("returned last_seq = %d, want 7 (retried past the seq-6 worker frame)", got)
	}
	// The worker frame at seq 6 is untouched.
	var kind6 string
	if err := env.pool.QueryRow(env.ctx, `SELECT kind FROM run_messages WHERE run_id = $1 AND seq = 6`, runID).Scan(&kind6); err != nil {
		t.Fatalf("read seq 6: %v", err)
	}
	if kind6 != "text" {
		t.Fatalf("seq 6 kind = %q, want the preserved worker frame 'text'", kind6)
	}
	// The switch message landed at seq 7 (no gap — seqs 6 and 7 both present).
	var kind7 string
	if err := env.pool.QueryRow(env.ctx, `SELECT kind FROM run_messages WHERE run_id = $1 AND seq = 7`, runID).Scan(&kind7); err != nil {
		t.Fatalf("read seq 7: %v", err)
	}
	if kind7 != credentialSwitchMessageKind {
		t.Fatalf("seq 7 kind = %q, want %q", kind7, credentialSwitchMessageKind)
	}
	if n := env.countSwitchMessages(t, runID); n != 1 {
		t.Fatalf("credential_switch messages = %d, want exactly 1 (no lost/duplicate frame)", n)
	}
}

// The bounded seq-collision loop EXHAUSTING must still advance runs.last_seq to the true high-water
// (MAX(seq)) and RETURN it — not the stale locked last_seq the old exhaustion path returned — so
// ClaimPayload.LastSeq never sits below a seq already present in run_messages and a resuming worker
// never re-uses an occupied seq (the data-integrity regression). No attribution message is inserted
// and nothing is broadcast on the skip. maxSwitchSeqAttempts is lowered to 1 so a single collision
// exhausts the loop.
func TestRecordRunCredentialSeqCollisionExhaustionAdvancesLastSeqLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())

	const lastSeq int32 = 5
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 2, lastSeq)
	env.seedEpoch(t, runID, tokenA, 1, "token-a", "default")

	// Force exhaustion: one attempt only. Restore the default so no sibling test is affected.
	defer func(orig int) { maxSwitchSeqAttempts = orig }(maxSwitchSeqAttempts)
	maxSwitchSeqAttempts = 1

	// A NON-switch worker frame at last_seq+1 collides the single attempt; a higher frame at
	// last_seq+5 makes MAX(seq) exceed the locked last_seq, so the advance is observable.
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload, claim_generation)
	          VALUES ($1, $2, 'status', $3, 2)`, runID, lastSeq+1, []byte(`{"event":"status"}`))
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload, claim_generation)
	          VALUES ($1, $2, 'status', $3, 2)`, runID, lastSeq+5, []byte(`{"event":"status"}`))

	bc := &recordingSwitchBroadcaster{}
	svc := switchSvc(env, bc)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: lastSeq}
	got, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenB, Label: "token-b"}, secretChoice{reason: selectReasonRunPinned}, true)
	if err != nil {
		t.Fatalf("recordRunCredential: %v", err)
	}
	// Returned last_seq is the true high-water (MAX(seq) = last_seq+5), not the stale locked value.
	if got != lastSeq+5 {
		t.Fatalf("returned last_seq = %d, want %d (MAX(seq), not the stale %d)", got, lastSeq+5, lastSeq)
	}
	// runs.last_seq was advanced to the same high-water inside the tx.
	var runLastSeq int32
	if err := env.pool.QueryRow(env.ctx, `SELECT last_seq FROM runs WHERE id = $1`, runID).Scan(&runLastSeq); err != nil {
		t.Fatalf("read last_seq: %v", err)
	}
	if runLastSeq != lastSeq+5 {
		t.Fatalf("runs.last_seq = %d, want %d", runLastSeq, lastSeq+5)
	}
	// No attribution message was inserted (the loop exhausted before landing one).
	if n := env.countSwitchMessages(t, runID); n != 0 {
		t.Fatalf("credential_switch messages = %d, want 0 (message skipped on exhaustion)", n)
	}
	// A skip broadcasts nothing.
	if len(bc.messages) != 0 {
		t.Fatalf("broadcast messages = %d, want 0 (no broadcast on a skipped message)", len(bc.messages))
	}
}

// The fence rejects a RELEASED claim: the run's claim was released (claim_released_at set) under this
// old flight. recordRunCredential must abort WITHOUT writing — no credential recorded, no epoch, no
// message — and return errRunVanished (the caller drops it like a vanished run).
func TestRecordRunCredentialFenceRejectsReleasedClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 2, 5)
	// Arm the fence: the claim was released.
	env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, runID)

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: 5}
	_, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenB, Label: "token-b"}, secretChoice{reason: selectReasonRunPinned}, true)
	if !errors.Is(err, errRunVanished) {
		t.Fatalf("recordRunCredential on a released claim: want errRunVanished, got %v", err)
	}
	// Nothing written: no credential recorded, no epoch, no message.
	var secretIsNull bool
	if err := env.pool.QueryRow(env.ctx, `SELECT anthropic_secret_id IS NULL FROM runs WHERE id = $1`, runID).Scan(&secretIsNull); err != nil {
		t.Fatalf("read anthropic_secret_id: %v", err)
	}
	if !secretIsNull {
		t.Fatal("anthropic_secret_id was written despite the fence rejection")
	}
	var epochCount int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_credential_epochs WHERE run_id = $1`, runID).Scan(&epochCount); err != nil {
		t.Fatalf("count epochs: %v", err)
	}
	if epochCount != 0 {
		t.Fatalf("epochs = %d, want 0 (fence rejected before any write)", epochCount)
	}
	if n := env.countSwitchMessages(t, runID); n != 0 {
		t.Fatalf("credential_switch messages = %d, want 0 (fence rejected)", n)
	}
}

// The fence also rejects a SUPERSEDED claim: a reclaim bumped the run's generation past the one this
// flight holds. Nothing is written and the claim is dropped like a vanished run.
func TestRecordRunCredentialFenceRejectsSupersededClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())
	// The DB row is at generation 3 (a reclaim already advanced it); this flight still holds 2.
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 3, 5)

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: 5}
	if _, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenB, Label: "token-b"}, secretChoice{reason: selectReasonRunPinned}, true); !errors.Is(err, errRunVanished) {
		t.Fatalf("recordRunCredential on a superseded claim: want errRunVanished, got %v", err)
	}
	var epochCount int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_credential_epochs WHERE run_id = $1`, runID).Scan(&epochCount); err != nil {
		t.Fatalf("count epochs: %v", err)
	}
	if epochCount != 0 {
		t.Fatalf("epochs = %d, want 0 (superseded generation fenced out)", epochCount)
	}
	if n := env.countSwitchMessages(t, runID); n != 0 {
		t.Fatalf("credential_switch messages = %d, want 0", n)
	}
}

// emitSwitchMessage=false (the chat/judge lanes) NEVER emits a message, even when the epoch delta
// would otherwise fire — but it STILL records the credential + epoch (attribution is unchanged).
func TestRecordRunCredentialEmitFalseNoMessageLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())
	runID := env.seedSwitchRun(t, userID, workerID, repoID, 2, 5)
	env.seedEpoch(t, runID, tokenA, 1, "token-a", "default")

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.Issue, ClaimGeneration: 2, LastSeq: 5}
	got, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenB, Label: "token-b"}, secretChoice{reason: selectReasonRunPinned}, false)
	if err != nil {
		t.Fatalf("recordRunCredential(emit=false): %v", err)
	}
	if got != 5 {
		t.Fatalf("returned last_seq = %d, want 5 (unchanged; no message on emit=false)", got)
	}
	if n := env.countSwitchMessages(t, runID); n != 0 {
		t.Fatalf("credential_switch messages = %d, want 0 when emitSwitchMessage=false", n)
	}
	// Credential recording still happened: the gen-2 epoch is written.
	var epochCount int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_credential_epochs WHERE run_id = $1 AND claim_generation = 2`, runID).Scan(&epochCount); err != nil {
		t.Fatalf("count gen-2 epochs: %v", err)
	}
	if epochCount != 1 {
		t.Fatalf("gen-2 epochs = %d, want 1 (credential still recorded)", epochCount)
	}
}

// A self_improve run never emits the message (D10 belt-and-braces), even on the emit=true run lane
// with a differing prior epoch: its lane follows the judge binding and is not switchable.
func TestRecordRunCredentialSelfImproveNoMessageLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	tokenA := env.seedAnthropicToken(t, userID, "token-a-"+uuid.NewString())
	tokenB := env.seedAnthropicToken(t, userID, "token-b-"+uuid.NewString())
	// A proper self_improve run (issue-shaped per runs_kind_shape).
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, last_seq)
	          VALUES ($1, $2, $3, 'self_improve', 101, 't', 'd', 'claimed', $4, 2, 5)`, runID, userID, repoID, workerID)
	env.seedEpoch(t, runID, tokenA, 1, "token-a", "default")

	svc := switchSvc(env, nil)
	run := store.Run{ID: runID, UserID: userID, Kind: runkind.SelfImprove, ClaimGeneration: 2, LastSeq: 5}
	if _, err := svc.recordRunCredential(env.ctx, run, claimCred{ID: tokenB, Label: "token-b"}, secretChoice{reason: selectReasonJudge}, true); err != nil {
		t.Fatalf("recordRunCredential(self_improve): %v", err)
	}
	if n := env.countSwitchMessages(t, runID); n != 0 {
		t.Fatalf("credential_switch messages = %d, want 0 for self_improve (D10)", n)
	}
}
