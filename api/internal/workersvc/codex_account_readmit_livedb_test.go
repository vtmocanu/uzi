package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_readmit_livedb_test.go pins PRD #1590 M4 (D5) against a real Postgres: the
// ReadmitRunCodexBinding relaxation of the write-once binding, its fences, the feed line, and the
// promote_codex_account_available pass's terminal exits (a deleted alias, a binding change on a
// linked alias). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh.

// promoteOnlyResult is promoteOnly with the pass's whole tally.
func promoteOnlyResult(t *testing.T, svc *Service, runID uuid.UUID) codexAccountPromoteResult {
	t.Helper()
	svc.codexPromote.after = uuidPredecessor(runID)
	svc.codexPromote.capOverride = 1
	res, err := svc.promoteCodexAccountAvailable(context.Background())
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	return res
}

// aliasMaterial reads the alias's current material_revision.
func aliasMaterial(t *testing.T, env codexTestEnv, aliasID uuid.UUID) int64 {
	t.Helper()
	var m int64
	if err := env.pool.QueryRow(env.ctx, `SELECT material_revision FROM codex_credential_state WHERE user_secret_id = $1`,
		aliasID).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// startRelogin is the owner's re-login PATCH on the run's own alias: material_revision+1, status
// staging, link cleared (BumpCodexMaterialRevision). It returns the new material revision.
func startRelogin(t *testing.T, env codexTestEnv, fx *codexClaimFix, status string) int64 {
	t.Helper()
	n, err := env.q.BumpCodexMaterialRevision(env.ctx, store.BumpCodexMaterialRevisionParams{
		UserSecretID: fx.aliasID, UserID: fx.userID, Status: status,
	})
	if err != nil || n != 1 {
		t.Fatalf("BumpCodexMaterialRevision = (%d, %v)", n, err)
	}
	return aliasMaterial(t, env, fx.aliasID)
}

// linkAlias links fx's alias to accountID at its current material revision
// (LinkCodexCredentialState, the reconciler's relink after a verified login).
func linkAlias(t *testing.T, env codexTestEnv, fx *codexClaimFix, accountID uuid.UUID, material int64) {
	t.Helper()
	n, err := env.q.LinkCodexCredentialState(env.ctx, store.LinkCodexCredentialStateParams{
		ProviderAccountID: pgconv.UUID(accountID), UserSecretID: fx.aliasID, UserID: fx.userID, MaterialRevision: material,
	})
	if err != nil || n != 1 {
		t.Fatalf("LinkCodexCredentialState = (%d, %v)", n, err)
	}
}

// sameIdentityRelogin is a completed, verified same-alias re-login of the quarantined account:
// the PATCH to staging, the account restore with a new login (generation+1, quarantine cleared,
// credential_revision unchanged), then the relink to the SAME account at the new material
// revision. It returns the new account generation and material revision.
func sameIdentityRelogin(t *testing.T, env codexTestEnv, fx *codexClaimFix, access string) (gen, material int64) {
	t.Helper()
	material = startRelogin(t, env, fx, "staging")
	gen = relogin(t, env, env.q, fx, access)
	linkAlias(t, env, fx, fx.accountID, material)
	return gen, material
}

// readmitLines returns the run's D5 feed status lines (kind status, event codex_binding_readmitted).
func readmitLines(t *testing.T, env codexTestEnv, runID uuid.UUID) []struct {
	seq     int32
	payload string
} {
	t.Helper()
	rows, err := env.pool.Query(env.ctx, `SELECT seq, payload::text FROM run_messages
		WHERE run_id = $1 AND kind = 'status' AND payload->>'event' = $2 ORDER BY seq`, runID, codexReadmitEvent)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		seq     int32
		payload string
	}
	for rows.Next() {
		var l struct {
			seq     int32
			payload string
		}
		if err := rows.Scan(&l.seq, &l.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// readmitRecorder is a Broadcaster that records what the promotion pass published: the feed
// lines (PublishMessage) and the swept states (PublishState), per run.
type readmitRecorder struct {
	mu       sync.Mutex
	messages map[uuid.UUID][]int32
	states   map[uuid.UUID][]string
}

func newReadmitRecorder() *readmitRecorder {
	return &readmitRecorder{messages: map[uuid.UUID][]int32{}, states: map[uuid.UUID][]string{}}
}

func (r *readmitRecorder) PublishMessage(runID uuid.UUID, seq int32, _, _, _, _ string, _ []byte, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages[runID] = append(r.messages[runID], seq)
}

func (r *readmitRecorder) PublishState(runID uuid.UUID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[runID] = append(r.states[runID], status)
}

func (*readmitRecorder) PublishHealth(uuid.UUID, string, string, bool) {}
func (*readmitRecorder) PublishInput(uuid.UUID)                        {}

// published returns the feed-line seqs and states published for runID.
func (r *readmitRecorder) published(runID uuid.UUID) ([]int32, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int32(nil), r.messages[runID]...), append([]string(nil), r.states[runID]...)
}

// assertHoldFailed checks the terminal exit from a hold: failed credential_unavailable with a
// reason naming want, the binding (material revision) not re-admitted, no feed line written.
func assertHoldFailed(t *testing.T, env codexTestEnv, runID uuid.UUID, held store.Run, want error) store.Run {
	t.Helper()
	r := mustRun(t, env, runID)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" ||
		!strings.Contains(r.FailureReason.String, want.Error()) || !r.FinishedAt.Valid {
		t.Fatalf("status=%s origin=%v reason=%q finished=%v, want failed credential_unavailable naming %q",
			r.Status, r.FailOrigin, r.FailureReason.String, r.FinishedAt.Valid, want)
	}
	if r.CodexMaterialRevision != held.CodexMaterialRevision || r.CodexAccountKey != held.CodexAccountKey ||
		r.CodexAccountRevision != held.CodexAccountRevision || r.ClaimGeneration != held.ClaimGeneration {
		t.Fatalf("binding moved: material %v->%v key %v->%v rev %v->%v gen %d->%d, want the frozen binding untouched",
			held.CodexMaterialRevision, r.CodexMaterialRevision, held.CodexAccountKey, r.CodexAccountKey,
			held.CodexAccountRevision, r.CodexAccountRevision, held.ClaimGeneration, r.ClaimGeneration)
	}
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch <= held.CodexClaimEpoch {
		t.Fatalf("hash=%x epoch=%d (held %d), want the capability revoked", r.CodexCapHash, r.CodexClaimEpoch, held.CodexClaimEpoch)
	}
	if l := readmitLines(t, env, runID); len(l) != 0 {
		t.Fatalf("a failed hold wrote %d re-admission lines", len(l))
	}
	return r
}

// TestCodexReadmitSameIdentityReloginLiveDB is D5's positive case, and the "a re-admitted run never
// receives a token captured before quarantine" pin. A held run's owner re-logs in on the SAME
// alias to the SAME identity with an unchanged credential_revision. While the new login is
// staging the run stays held; once it is linked, one promotion tick re-admits the run (only
// codex_material_revision advances, to the alias's), writes one feed status line naming the alias
// label and the revision change (no token or identity material), and promotes it with the
// promotion field set. The next claim delivers the re-login's token at its generation, and the
// payload carries neither the pre-quarantine access token nor either refresh token.
func TestCodexReadmitSameIdentityReloginLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	held := mustRun(t, env, fx.runID)
	oldMaterial := held.CodexMaterialRevision.Int64

	material := startRelogin(t, env, fx, "staging")
	if res := promoteOnlyResult(t, svc, fx.runID); res.Promoted+res.Readmitted+res.Failed != 0 {
		t.Fatalf("staging tick: %+v, want nothing decided", res)
	}
	assertStillHeld(t, env, fx.runID, held)

	access := codexToken("access-relogin")
	gen := relogin(t, env, env.q, fx, access)
	linkAlias(t, env, fx, fx.accountID, material)
	res := promoteOnlyResult(t, svc, fx.runID)
	if res.Promoted != 1 || res.Readmitted != 1 || res.Failed != 0 {
		t.Fatalf("re-admission tick: %+v, want one run re-admitted and promoted", res)
	}
	r := mustRun(t, env, fx.runID)
	assertPromoted(t, env, fx.runID, held)
	if r.CodexMaterialRevision.Int64 != material || r.CodexSecretID != held.CodexSecretID ||
		r.CodexAccountKey != held.CodexAccountKey || r.CodexAccountRevision != held.CodexAccountRevision ||
		r.CodexAuthMode != held.CodexAuthMode || r.CodexSecretLabel != held.CodexSecretLabel {
		t.Fatalf("binding after re-admission: material %v (want %d), alias/key/rev/mode/label changed=%v",
			r.CodexMaterialRevision, material, r.CodexSecretID != held.CodexSecretID || r.CodexAccountKey != held.CodexAccountKey ||
				r.CodexAccountRevision != held.CodexAccountRevision || r.CodexAuthMode != held.CodexAuthMode ||
				r.CodexSecretLabel != held.CodexSecretLabel)
	}

	lines := readmitLines(t, env, fx.runID)
	if len(lines) != 1 {
		t.Fatalf("re-admission lines = %d, want 1", len(lines))
	}
	line := lines[0]
	if r.LastSeq < line.seq {
		t.Fatalf("last_seq = %d, want >= the feed line's seq %d (a resumed worker must start past it)", r.LastSeq, line.seq)
	}
	var p codexReadmitStatusPayload
	if err := json.Unmarshal([]byte(line.payload), &p); err != nil {
		t.Fatal(err)
	}
	label := held.CodexSecretLabel.String
	if p.AliasLabel != label || p.FromMaterialRevision != oldMaterial || p.ToMaterialRevision != material ||
		!strings.Contains(p.Text, label) || !strings.Contains(p.Text, fmt.Sprintf("%d to %d", oldMaterial, material)) {
		t.Fatalf("feed line %+v, want label %q and revisions %d to %d", p, label, oldMaterial, material)
	}
	acct := env.mustAccount(t, fx.userID, fx.accountID)
	for name, secret := range map[string]string{
		"pre-quarantine access token": fx.access, "re-login access token": access,
		"provider user id": acct.ProviderUserID, "workspace account id": acct.WorkspaceAccountID,
		"account key": held.CodexAccountKey.String,
	} {
		if secret != "" && strings.Contains(line.payload, secret) {
			t.Fatalf("the feed line carries the %s", name)
		}
	}
	if strings.Contains(line.payload, "refresh") {
		t.Fatal("the feed line mentions refresh material")
	}

	// A second tick finds nothing to decide: the run is no longer held.
	if res := promoteOnlyResult(t, svc, fx.runID); res.Readmitted != 0 {
		t.Fatalf("second tick re-admitted %d, want 0", res.Readmitted)
	}

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil || payload == nil || payload.Secrets.Codex == nil {
		t.Fatalf("Claim = (%v, %v), want a Codex payload", payload != nil, err)
	}
	c := payload.Secrets.Codex
	if c.AccessToken != access || c.Generation == nil || *c.Generation != gen {
		t.Fatalf("claim token match=%v generation=%v, want the re-login's token at generation %d",
			c.AccessToken == access, c.Generation, gen)
	}
	if payload.LastSeq < line.seq {
		t.Fatalf("claim last_seq = %d, want >= the feed line's seq %d", payload.LastSeq, line.seq)
	}
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), fx.access) {
		t.Fatal("the claim payload carries the pre-quarantine access token")
	}
	if strings.Contains(string(wire), "refresh-") {
		t.Fatal("the claim payload carries a refresh token")
	}
}

// TestCodexReadmitTerminalBindingChangesLiveDB: on a linked alias, a definite binding change is
// terminal (credential_unavailable naming the change) and is never re-admitted: the frozen
// material revision stays, and no feed line is written. Each leg sets up a re-login that would
// otherwise qualify, then breaks exactly one D5 condition.
func TestCodexReadmitTerminalBindingChangesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, env codexTestEnv, fx *codexClaimFix)
		want  error
	}{
		{"bumped credential_revision", func(t *testing.T, env codexTestEnv, fx *codexClaimFix) {
			sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
			fx.setAccount(t, "credential_revision = credential_revision + 1")
		}, ErrCodexAccountRevisionStale},
		{"different identity", func(t *testing.T, env codexTestEnv, fx *codexClaimFix) {
			material := startRelogin(t, env, fx, "staging")
			other := env.seedLinkedSubscription(t, fx.userID, "codex-other-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
			var otherAccount uuid.UUID
			if err := env.pool.QueryRow(env.ctx, `SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1`,
				other).Scan(&otherAccount); err != nil {
				t.Fatal(err)
			}
			linkAlias(t, env, fx, otherAccount, material)
		}, ErrCodexAccountTupleMismatch},
		{"alias kind changed", func(t *testing.T, env codexTestEnv, fx *codexClaimFix) {
			sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
			env.exec(`UPDATE user_secrets SET kind = 'openai_api_key' WHERE id = $1`, fx.aliasID)
		}, ErrCodexKindModeMismatch},
		{"run auth mode changed", func(t *testing.T, env codexTestEnv, fx *codexClaimFix) {
			sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
			env.exec(`UPDATE runs SET codex_auth_mode = 'api_key' WHERE id = $1`, fx.runID)
		}, ErrCodexKindModeMismatch},
		{"auth mode and kind both changed", func(t *testing.T, env codexTestEnv, fx *codexClaimFix) {
			sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
			env.exec(`UPDATE user_secrets SET kind = 'openai_api_key' WHERE id = $1`, fx.aliasID)
			env.exec(`UPDATE runs SET codex_auth_mode = 'api_key' WHERE id = $1`, fx.runID)
		}, ErrCodexKindModeMismatch},
		{"undecodable frozen identity", func(t *testing.T, env codexTestEnv, fx *codexClaimFix) {
			sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
			env.exec(`UPDATE runs SET codex_account_key = 'not-json' WHERE id = $1`, fx.runID)
		}, ErrCodexAccountTupleMismatch},
		// D1's incoherent alias: linked to no account. Only a direct write makes it (from a
		// staging row, whose link is already NULL, so the orphan trigger does not fire).
		{"linked alias with no account", func(t *testing.T, env codexTestEnv, fx *codexClaimFix) {
			startRelogin(t, env, fx, "staging")
			env.exec(`UPDATE codex_credential_state SET status = 'linked' WHERE user_secret_id = $1`, fx.aliasID)
		}, ErrCodexAliasAccountMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, svc := heldCodexFix(t, env)
			tc.setup(t, env, fx)
			held := mustRun(t, env, fx.runID)
			res := promoteOnlyResult(t, svc, fx.runID)
			if res.Failed != 1 || res.Promoted != 0 || res.Readmitted != 0 {
				t.Fatalf("tick: %+v, want one terminal failure and nothing re-admitted", res)
			}
			r := assertHoldFailed(t, env, fx.runID, held, tc.want)
			if !strings.Contains(r.FailureReason.String, held.CodexSecretLabel.String) {
				t.Fatalf("reason %q does not name the alias label %q", r.FailureReason.String, held.CodexSecretLabel.String)
			}
			if payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil); err != nil || payload != nil {
				t.Fatalf("Claim after failure = (%v, %v), want idle", payload != nil, err)
			}
		})
	}
}

// TestCodexReadmitDeletedAliasAfterParkLiveDB: deleting the alias after the park nulls the run's
// codex_secret_id through the FK and does not end the run; the next promotion tick fails it
// credential_unavailable with a reason naming the deleted login, decided on the run row before
// any alias lock (the afterAliasLock seam never fires for it). The failure retains custody: the
// run's older-generation hold stays open, as for any failed run.
func TestCodexReadmitDeletedAliasAfterParkLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	fx.setAccount(t, "coord_state = 'quarantined'")
	svc := parkHeldCodexFix(t, env, fx)
	held := mustRun(t, env, fx.runID)

	env.exec(`DELETE FROM user_secrets WHERE id = $1`, fx.aliasID)
	r := mustRun(t, env, fx.runID)
	if r.CodexSecretID.Valid || !r.CodexMaterialRevision.Valid || r.Status != "recovery_wait" {
		t.Fatalf("after delete: secret=%v material=%v status=%s, want the FK to null only the alias id",
			r.CodexSecretID, r.CodexMaterialRevision, r.Status)
	}
	aliasLocked := 0
	svc.codexPromoteHooks = &codexPromoteTestHooks{afterAliasLock: func(_ context.Context, id uuid.UUID) {
		if id == fx.runID {
			aliasLocked++
		}
	}}
	res := promoteOnlyResult(t, svc, fx.runID)
	if res.Failed != 1 || res.Promoted != 0 {
		t.Fatalf("tick: %+v, want one terminal failure", res)
	}
	if aliasLocked != 0 {
		t.Fatalf("alias lock reached %d times for a deleted alias, want 0", aliasLocked)
	}
	r = mustRun(t, env, fx.runID)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" ||
		!strings.Contains(r.FailureReason.String, "deleted") || !strings.Contains(r.FailureReason.String, held.CodexSecretLabel.String) {
		t.Fatalf("status=%s origin=%v reason=%q, want failed credential_unavailable naming the deleted login %q",
			r.Status, r.FailOrigin, r.FailureReason.String, held.CodexSecretLabel.String)
	}
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch <= held.CodexClaimEpoch || r.CodexMaterialRevision != held.CodexMaterialRevision {
		t.Fatalf("hash=%x epoch=%d material=%v, want revoked capability and the frozen revision kept",
			r.CodexCapHash, r.CodexClaimEpoch, r.CodexMaterialRevision)
	}
	var state string
	if err := env.pool.QueryRow(env.ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, fx.holdA).Scan(&state); err != nil || state != "open" {
		t.Fatalf("gen-1 hold state = %q (%v), want open: a failed run retains custody", state, err)
	}
}

// TestCodexReadmitStagingOrFailedStaysHeldNoTokenLiveDB: while the re-login on the run's alias is
// staging (identity unknown) or failed (the new login proved unusable), the run stays held across
// ticks, is never re-admitted or failed, and no claim delivers a token for it.
func TestCodexReadmitStagingOrFailedStaysHeldNoTokenLiveDB(t *testing.T) {
	for _, status := range []string{"staging", "failed"} {
		t.Run(status, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, svc := heldCodexFix(t, env)
			startRelogin(t, env, fx, status)
			fx.setAccount(t, "coord_state = 'idle'")
			held := mustRun(t, env, fx.runID)
			for tick := 0; tick < 2; tick++ {
				if res := promoteOnlyResult(t, svc, fx.runID); res.Promoted+res.Readmitted+res.Failed != 0 {
					t.Fatalf("tick %d: %+v, want nothing decided", tick, res)
				}
				assertStillHeld(t, env, fx.runID, held)
				if payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil); err != nil || payload != nil {
					t.Fatalf("tick %d: Claim = (%v, %v), want idle (no token for a held run)", tick, payload != nil, err)
				}
				assertStillHeld(t, env, fx.runID, held)
			}
			if n := env.countRunCredentialEpochs(t, fx.runID); n != 0 {
				t.Fatalf("credential epochs = %d, want 0", n)
			}
		})
	}
}

// TestCodexReadmitMaterialCASFenceLiveDB pins ReadmitRunCodexBinding's material-revision CAS
// fences as the promoter drives them: the alias's material revision is moved between the
// promoter's observation and its ReadmitRunCodexBinding call. The CAS matches 0 rows, nothing is
// re-admitted, and the run is left held (not failed, not promoted): the re-read still sees a
// newer alias material than the run's, which is transient. The transaction then rolls back.
//
// The change is injected inside the promoter's OWN transaction, so this is not a concurrency
// test: a real concurrent writer (BumpCodexMaterialRevision, LinkCodexCredentialState) cannot
// move the alias row at this point, because the promoter holds it FOR SHARE and the writer
// blocks until the promoter's transaction ends. The CAS is the statement's own fence against a
// caller whose observed values are stale.
func TestCodexReadmitMaterialCASFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
	held := mustRun(t, env, fx.runID)

	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	q := &readmitRaceWrapper{Queries: store.New(tx), bump: func() {
		if _, err := tx.Exec(env.ctx, `UPDATE codex_credential_state SET material_revision = material_revision + 1
			WHERE user_secret_id = $1`, fx.aliasID); err != nil {
			t.Fatalf("inject material change: %v", err)
		}
	}}
	d, err := svc.promoteCodexAccountRunTx(env.ctx, q, fx.runID, true)
	if err != nil {
		t.Fatalf("promoteCodexAccountRunTx: %v", err)
	}
	if q.calls != 1 || q.rows != 0 {
		t.Fatalf("ReadmitRunCodexBinding calls=%d rows=%d, want one call matching 0 rows", q.calls, q.rows)
	}
	if d.outcome != codexHoldStay || d.readmit != nil {
		t.Fatalf("decision = %+v, want stay held with nothing re-admitted", d)
	}
	if err := tx.Rollback(env.ctx); err != nil {
		t.Fatal(err)
	}
	assertStillHeld(t, env, fx.runID, held)
	if l := readmitLines(t, env, fx.runID); len(l) != 0 {
		t.Fatalf("feed lines = %d, want 0", len(l))
	}
}

// readmitRaceWrapper runs bump in the promoter's own transaction just before the CAS.
type readmitRaceWrapper struct {
	*store.Queries
	bump  func()
	calls int
	rows  int64
}

func (w *readmitRaceWrapper) ReadmitRunCodexBinding(ctx context.Context, arg store.ReadmitRunCodexBindingParams) (int64, error) {
	w.calls++
	w.bump()
	n, err := w.Queries.ReadmitRunCodexBinding(ctx, arg)
	w.rows = n
	return n, err
}

// TestReadmitRunCodexBindingFencesLiveDB pins every fence of ReadmitRunCodexBinding directly: a
// control call re-admits (1 row), and each leg breaks exactly one D5 condition, via the row state
// or the call's observed values, and must match 0 rows. Each leg runs in a rolled-back
// transaction on a fresh fixture, so a leg that wrongly matches cannot leak into another.
func TestReadmitRunCodexBindingFencesLiveDB(t *testing.T) {
	type call struct{ old, new int64 }
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, env codexTestEnv, fx *codexClaimFix, sibling uuid.UUID) // mutates rows; may be nil
		call   func(old, new int64, sibling int64) call                                   // observed values; nil = the true ones
		secret func(fx *codexClaimFix, sibling uuid.UUID) uuid.UUID                       // nil = the run's alias
		key    func(canonical string) string                                              // nil = the account's Go encoding
		want   int64
	}{
		{name: "control", want: 1},
		{name: "run not in recovery_wait", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			// The promoter leaves recovery_wait_cause in place on a promoted run, so a queued run
			// can still carry the cause: only the status fence rejects it.
			env.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, fx.runID)
		}},
		{name: "another recovery_wait cause", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE runs SET recovery_wait_cause = 'provider_outage' WHERE id = $1`, fx.runID)
		}},
		{name: "observed alias id differs", secret: func(_ *codexClaimFix, sibling uuid.UUID) uuid.UUID { return sibling }},
		{name: "alias state row of a sibling alias", call: func(old, _, sib int64) call { return call{old, sib} }},
		{name: "run auth mode api_key", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE runs SET codex_auth_mode = 'api_key' WHERE id = $1`, fx.runID)
		}},
		{name: "alias kind openai_api_key", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE user_secrets SET kind = 'openai_api_key' WHERE id = $1`, fx.aliasID)
		}},
		{name: "alias status failed", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE codex_credential_state SET status = 'failed' WHERE user_secret_id = $1`, fx.aliasID)
		}},
		{name: "alias status staging", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE codex_credential_state SET status = 'staging' WHERE user_secret_id = $1`, fx.aliasID)
		}},
		{name: "alias unlinked", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE codex_credential_state SET provider_account_id = NULL WHERE user_secret_id = $1`, fx.aliasID)
		}},
		{name: "identity differs", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			fx.setAccount(t, "provider_user_id = provider_user_id || '-other'")
		}},
		{name: "frozen key undecodable", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE runs SET codex_account_key = 'not-json' WHERE id = $1`, fx.runID)
		}},
		{name: "frozen key NULL", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE runs SET codex_account_key = NULL WHERE id = $1`, fx.runID)
		}},
		// jsonb-equal to the account's tuple, but not the Go encoding codexCheckAccountTuple
		// compares (jsonb's text form puts a space after the comma).
		{name: "frozen key non-canonical", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE runs SET codex_account_key = (codex_account_key::jsonb)::text WHERE id = $1`, fx.runID)
		}},
		{name: "observed account key differs", key: func(canonical string) string {
			return strings.Replace(canonical, `","`, `", "`, 1)
		}},
		{name: "credential_revision bumped", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			fx.setAccount(t, "credential_revision = credential_revision + 1")
		}},
		{name: "account quarantined", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			fx.setAccount(t, "coord_state = 'quarantined'")
		}},
		{name: "account in_progress", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			fx.setAccount(t, "coord_state = 'in_progress', coord_operation_id = gen_random_uuid(), lease_deadline = now() + interval '1 hour'")
		}},
		{name: "stale observed run material", call: func(old, new, _ int64) call { return call{old - 1, new} }},
		{name: "stale observed alias material", call: func(old, new, _ int64) call { return call{old, new + 1} }},
		{name: "alias material behind the run", setup: func(t *testing.T, env codexTestEnv, fx *codexClaimFix, _ uuid.UUID) {
			env.exec(`UPDATE codex_credential_state SET material_revision = 0 WHERE user_secret_id = $1`, fx.aliasID)
			env.exec(`UPDATE runs SET codex_material_revision = 5 WHERE id = $1`, fx.runID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, _ := heldCodexFix(t, env)
			sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
			// A sibling codex_auth alias of the same owner, linked to the SAME account, with its
			// material far ahead: a statement that did not pin the state row to the run's own alias
			// could re-admit the run off this row.
			sibling := env.seedLinkedSubscription(t, fx.userID, "codex-sibling-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
			env.exec(`UPDATE codex_credential_state SET provider_account_id = $2, material_revision = 1000
				WHERE user_secret_id = $1`, sibling, fx.accountID)
			if tc.setup != nil {
				tc.setup(t, env, fx, sibling)
			}
			r := mustRun(t, env, fx.runID)
			c := call{r.CodexMaterialRevision.Int64, aliasMaterial(t, env, fx.aliasID)}
			if tc.call != nil {
				c = tc.call(c.old, c.new, 1000)
			}
			secret := fx.aliasID
			if tc.secret != nil {
				secret = tc.secret(fx, sibling)
			}
			acct := env.mustAccount(t, fx.userID, fx.accountID)
			key, err := codexAccountKey(acct.ProviderUserID, acct.WorkspaceAccountID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.key != nil {
				key = tc.key(key)
			}
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			n, err := store.New(tx).ReadmitRunCodexBinding(env.ctx, store.ReadmitRunCodexBindingParams{
				ID: fx.runID, SecretID: secret, AccountKey: key, OldMaterialRevision: c.old, NewMaterialRevision: c.new,
			})
			if err != nil {
				t.Fatalf("ReadmitRunCodexBinding: %v", err)
			}
			if n != tc.want {
				t.Fatalf("rows = %d, want %d", n, tc.want)
			}
			if tc.want == 1 {
				var got int64
				if err := tx.QueryRow(env.ctx, `SELECT codex_material_revision FROM runs WHERE id = $1`, fx.runID).Scan(&got); err != nil || got != c.new {
					t.Fatalf("material after re-admission = %d (%v), want %d", got, err, c.new)
				}
			}
		})
	}
}

// TestCodexReadmitNonCanonicalFrozenKeyLiveDB is the regression for a re-admission committed
// together with a terminal failure. The run's frozen codex_account_key is rewritten by direct SQL
// to a form that is jsonb-equal to the account's tuple but is not the Go encoding
// (codexAccountKey), with an otherwise qualifying same-identity re-login. The Go authority check
// compares the key text, so the run can never be released and the tick fails it
// credential_unavailable (tuple mismatch). ReadmitRunCodexBinding's key fence must agree with that
// check: nothing is re-admitted (the frozen material revision stays), no feed line is written or
// published, and Readmitted is not counted.
func TestCodexReadmitNonCanonicalFrozenKeyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
	canonical := mustRun(t, env, fx.runID).CodexAccountKey.String
	env.exec(`UPDATE runs SET codex_account_key = (codex_account_key::jsonb)::text WHERE id = $1`, fx.runID)
	held := mustRun(t, env, fx.runID)
	var jsonbEqual bool
	if err := env.pool.QueryRow(env.ctx, `SELECT $1::jsonb = $2::jsonb`, held.CodexAccountKey.String, canonical).
		Scan(&jsonbEqual); err != nil {
		t.Fatal(err)
	}
	if held.CodexAccountKey.String == canonical || !jsonbEqual {
		t.Fatalf("seeded key %q vs canonical %q (jsonb-equal %v), want a different text with equal jsonb",
			held.CodexAccountKey.String, canonical, jsonbEqual)
	}
	rec := newReadmitRecorder()
	svc.SetBroadcaster(rec)

	res := promoteOnlyResult(t, svc, fx.runID)
	if res.Readmitted != 0 || res.Promoted != 0 || res.Failed != 1 {
		t.Fatalf("tick: %+v, want one terminal failure and nothing re-admitted", res)
	}
	assertHoldFailed(t, env, fx.runID, held, ErrCodexAccountTupleMismatch)
	msgs, states := rec.published(fx.runID)
	if len(msgs) != 0 || len(states) != 1 || states[0] != "failed" {
		t.Fatalf("published messages=%v states=%v, want no feed line and one failed state", msgs, states)
	}
}

// readmitThenDivergeWrapper makes a re-admitted transaction classify as a terminal failure: once
// its ReadmitRunCodexBinding has matched a row, the following GetRunCodexAuthContext re-read
// returns the frozen key in a non-canonical form, the divergence the SQL key fence now prevents.
type readmitThenDivergeWrapper struct {
	codexPromoteQueries
	readmitted bool
	rows       *[]int64
}

func (w *readmitThenDivergeWrapper) ReadmitRunCodexBinding(ctx context.Context, arg store.ReadmitRunCodexBindingParams) (int64, error) {
	n, err := w.codexPromoteQueries.ReadmitRunCodexBinding(ctx, arg)
	*w.rows = append(*w.rows, n)
	w.readmitted = w.readmitted || n == 1
	return n, err
}

func (w *readmitThenDivergeWrapper) GetRunCodexAuthContext(ctx context.Context, id uuid.UUID) (store.GetRunCodexAuthContextRow, error) {
	row, err := w.codexPromoteQueries.GetRunCodexAuthContext(ctx, id)
	if err == nil && w.readmitted {
		row.CodexAccountKey.String = strings.Replace(row.CodexAccountKey.String, `","`, `", "`, 1)
	}
	return row, err
}

// TestCodexReadmitWithoutPromoteRollsBackLiveDB pins the promoter's guarantee that a re-admission
// commits only together with its promotion, independently of the SQL fences: a transaction that
// re-admits the run and then classifies it as terminal is rolled back whole, and the run is
// decided again with re-admission disabled. Here the second decision sees the unrelaxed binding
// (material still behind the alias), so the run stays held: nothing is re-admitted, failed,
// promoted, counted, written to the feed or published.
func TestCodexReadmitWithoutPromoteRollsBackLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	sameIdentityRelogin(t, env, fx, codexToken("access-relogin"))
	held := mustRun(t, env, fx.runID)
	var rows []int64
	txs := 0
	svc.codexPromoteHooks = &codexPromoteTestHooks{wrapQueries: func(q codexPromoteQueries) codexPromoteQueries {
		txs++
		return &readmitThenDivergeWrapper{codexPromoteQueries: q, rows: &rows}
	}}
	rec := newReadmitRecorder()
	svc.SetBroadcaster(rec)

	res := promoteOnlyResult(t, svc, fx.runID)
	if res.Readmitted != 0 || res.Promoted != 0 || res.Failed != 0 {
		t.Fatalf("tick: %+v, want nothing decided", res)
	}
	if txs != 2 || len(rows) != 1 || rows[0] != 1 {
		t.Fatalf("transactions=%d readmit rows=%v, want a re-admitting transaction then one without re-admission", txs, rows)
	}
	assertStillHeld(t, env, fx.runID, held)
	if l := readmitLines(t, env, fx.runID); len(l) != 0 {
		t.Fatalf("feed lines = %d, want 0", len(l))
	}
	if msgs, states := rec.published(fx.runID); len(msgs) != 0 || len(states) != 0 {
		t.Fatalf("published messages=%v states=%v, want nothing", msgs, states)
	}
}

// TestCodexA1RelinkOneSweepResumesLiveDB is amendment A1 end to end through the production path:
// a queued run whose alias completed a verified same-identity relink (a re-login PATCH, then the
// relink to the SAME account at the new material revision) is held by ClaimRun's gate, and ONE
// full Sweep parks it (park_codex_account_unavailable), re-admits it and promotes it
// (promote_codex_account_available, last in Sweep). It ends queued with the alias's material
// revision, exactly one committed and published feed line, no failure, and CodexAccountReadmitted
// counted; the next claim delivers it. Both page cursors start at the fixture run with a one-row
// page, so no other run in the shared database is examined by either pass.
func TestCodexA1RelinkOneSweepResumesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	material := startRelogin(t, env, fx, "staging")
	linkAlias(t, env, fx, fx.accountID, material)
	if payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil); err != nil || payload != nil {
		t.Fatalf("Claim before the sweep = (%v, %v), want idle (held by the gate)", payload != nil, err)
	}
	before := mustRun(t, env, fx.runID)
	if before.Status != "queued" || before.CodexMaterialRevision.Int64 >= material {
		t.Fatalf("status=%s material=%v, want queued behind the alias's %d", before.Status, before.CodexMaterialRevision, material)
	}

	svc := gateSweepService(env, fx.svc)
	rec := newReadmitRecorder()
	svc.SetBroadcaster(rec)
	svc.codexPark.after, svc.codexPark.capOverride = uuidPredecessor(fx.runID), 1
	svc.codexPromote.after, svc.codexPromote.capOverride = uuidPredecessor(fx.runID), 1
	res, err := svc.Sweep(env.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.CodexAccountParked != 1 || res.CodexAccountPromoted != 1 || res.CodexAccountReadmitted != 1 || res.CodexAccountFailed != 0 {
		t.Fatalf("Sweep: parked=%d promoted=%d readmitted=%d failed=%d, want 1/1/1/0",
			res.CodexAccountParked, res.CodexAccountPromoted, res.CodexAccountReadmitted, res.CodexAccountFailed)
	}
	r := mustRun(t, env, fx.runID)
	if r.Status != "queued" || r.FailOrigin.Valid || r.FailureReason.Valid || r.CodexMaterialRevision.Int64 != material ||
		r.CodexAccountKey != before.CodexAccountKey || r.CodexAccountRevision != before.CodexAccountRevision {
		t.Fatalf("status=%s origin=%v material=%v (want %d) key/rev changed=%v, want queued, re-admitted, not failed",
			r.Status, r.FailOrigin, r.CodexMaterialRevision, material,
			r.CodexAccountKey != before.CodexAccountKey || r.CodexAccountRevision != before.CodexAccountRevision)
	}
	lines := readmitLines(t, env, fx.runID)
	if len(lines) != 1 {
		t.Fatalf("re-admission lines = %d, want 1", len(lines))
	}
	msgs, states := rec.published(fx.runID)
	if len(msgs) != 1 || msgs[0] != lines[0].seq {
		t.Fatalf("published feed seqs %v, want exactly the committed line's %d", msgs, lines[0].seq)
	}
	for _, st := range states {
		if st == "failed" {
			t.Fatalf("published states %v include failed", states)
		}
	}
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil || payload == nil || payload.Secrets.Codex == nil {
		t.Fatalf("Claim after the sweep = (%v, %v), want a Codex payload", payload != nil, err)
	}
}
