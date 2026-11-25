package workersvc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_claim_livedb_test.go is the PRD #4320 module 5 contract against a real Postgres: a
// provider-bound run's claim (binding, exclusivity, capability gating), claim-time revocation (D9)
// for issue runs and chat, the promoter arm, Resume refusal and the health reason. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

const pcOtherURL = "https://other.example.com/x"

type pcFix struct {
	*cdFix
	provider, key uuid.UUID
	plain         string
}

// newPCFix is a cdFix whose worker advertises claude_provider_v1, plus a provider at pvBase with a
// REAL sealed gateway key. The owner also holds Anthropic tokens (pinned to the worker, plus a
// default): none of them may ever reach a provider run.
func newPCFix(t *testing.T) *pcFix {
	t.Helper()
	fx := newCDFix(t)
	fx.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	fx.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, fx.workerID,
		[]string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.ClaudeProviderV1})
	pf := &pcFix{cdFix: fx, plain: "gw-key-" + uuid.NewString()}
	p, err := fx.env.q.InsertModelProvider(fx.env.ctx, store.InsertModelProviderParams{OwnerUserID: fx.userID, Name: "p-" + uuid.NewString()[:8], Engine: "claude"})
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	pf.provider = p.ID
	if err := fx.env.q.InsertModelProviderRevision(fx.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(pvDefaultMap),
	}); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	sealed, err := fx.env.box.Seal([]byte(pf.plain))
	if err != nil {
		t.Fatal(err)
	}
	k, err := fx.env.q.InsertProviderKeySecret(fx.env.ctx, store.InsertProviderKeySecretParams{
		UserID: fx.userID, Label: p.ID.String(), Ciphertext: sealed, SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("insert key: %v", err)
	}
	pf.key = k.ID
	t.Cleanup(func() {
		if _, err := fx.env.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID); err != nil {
			t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	return pf
}

func (pf *pcFix) keyRev(t *testing.T) int64 {
	t.Helper()
	var rev int64
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT enablement_rev FROM user_secrets WHERE id = $1`, pf.key).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	return rev
}

func (pf *pcFix) rotate() {
	pf.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, pf.key)
}
func (pf *pcFix) disable()   { pf.setEnabled(pf.key, false) }
func (pf *pcFix) deleteKey() { pf.env.exec(`DELETE FROM user_secrets WHERE id = $1`, pf.key) }
func (pf *pcFix) deleteProvider() {
	pf.env.exec(`DELETE FROM model_providers WHERE id = $1`, pf.provider)
}
func (pf *pcFix) allow(urls ...string) { pf.svc.p.ProviderAllowedBaseURLs = urls }

// run seeds a queued provider-bound issue run frozen at revision 1 and the key's current rev.
func (pf *pcFix) run(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	pf.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	    status, status_since, worker_id, started_at, budget_wall_seconds, session_id, harness,
	    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
	    VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', now(), $5, now() - interval '15 minutes', 3600, 'sess-pc', 'claude',
	            $6, 1, 'user_key', $7, $8)`,
		id, pf.userID, pf.repoID, int64(binary.BigEndian.Uint32(id[:4])), pf.workerID, pf.provider, pf.key, pf.keyRev(t))
	return id
}

// chat seeds a queued provider-bound chat.
func (pf *pcFix) chat(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	pf.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title, trigger_source, harness, status,
	    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
	    VALUES ($1, $2, 'chat', 'hi', 'hi', 'hi', 'chat', 'claude', 'queued', $3, 1, 'user_key', $4, $5)`,
		id, pf.userID, pf.provider, pf.key, pf.keyRev(t))
	return id
}

func (pf *pcFix) wantParked(t *testing.T, runID uuid.UUID, reason string) store.Run {
	t.Helper()
	r := mustRun(t, pf.env, runID)
	if r.Status != "paused" || r.HoldReason.String != reason {
		t.Fatalf("run %s: status=%s hold=%v, want paused/%s", runID, r.Status, r.HoldReason, reason)
	}
	pf.wantNoVendorWrites(t, r)
	return r
}

// wantNoVendorWrites: a provider run never records a vendor credential or a rejection mark.
func (pf *pcFix) wantNoVendorWrites(t *testing.T, r store.Run) {
	t.Helper()
	if r.AnthropicSecretID.Valid {
		t.Fatalf("provider run %s recorded a vendor credential: %v", r.ID, r.AnthropicSecretID)
	}
	var rejected int
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT count(*) FROM user_secrets WHERE user_id = $1 AND anthropic_rejected_at IS NOT NULL`, r.UserID).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if rejected != 0 {
		t.Fatalf("provider run %s marked %d credentials rejected", r.ID, rejected)
	}
	var epochs int
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT count(*) FROM run_credential_epochs WHERE run_id = $1`, r.ID).Scan(&epochs); err != nil {
		t.Fatal(err)
	}
	if epochs != 0 {
		t.Fatalf("provider run %s journaled %d vendor credential epochs", r.ID, epochs)
	}
}

// TestProviderClaimCarriesOnlyTheBindingLiveDB: example 2/exclusivity. The claim carries one
// `provider` object with the frozen revision, the matched allowlist entry and the decrypted key, an
// EMPTY anthropic_oauth_token, although the owner holds Anthropic tokens and the worker is pinned
// to one; nothing vendor is recorded on the run.
func TestProviderClaimCarriesOnlyTheBindingLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := pf.run(t)
	payload, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim = (%v, %v)", payload != nil, err)
	}
	p := payload.Secrets.Provider
	if p == nil || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("secrets = %+v, want a provider binding and no Anthropic token", payload.Secrets)
	}
	if p.ProviderID != pf.provider.String() || p.Revision != 1 || p.Engine != "claude" || p.Protocol != "anthropic_messages" ||
		p.BaseURL != pvBase || p.AuthStyle != "bearer" || p.Credential != pf.plain || p.ModelMap["haiku"].Model != "corp-haiku" {
		t.Fatalf("binding = %+v", p)
	}
	raw, err := json.Marshal(payload.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"anthropic_oauth_token":""`) || !strings.Contains(string(raw), `"provider":{`) {
		t.Fatalf("wire secrets = %s", raw)
	}
	r := mustRun(t, pf.env, runID)
	if r.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", r.Status)
	}
	pf.wantNoVendorWrites(t, r)
}

// TestProviderClaimIgnoresWorkerBindModeLiveDB: whatever the claiming worker's anthropic_bind_mode
// says (pinned, auto, default), a provider claim is the same and records no vendor credential.
func TestProviderClaimIgnoresWorkerBindModeLiveDB(t *testing.T) {
	for _, mode := range []string{BindModePinned, BindModeAuto, BindModeDefault} {
		t.Run(mode, func(t *testing.T) {
			pf := newPCFix(t)
			pf.env.exec(`UPDATE workers SET anthropic_bind_mode = $2 WHERE id = $1`, pf.workerID, mode)
			runID := pf.run(t)
			payload, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
			if err != nil || payload == nil || payload.Secrets.Provider == nil || payload.Secrets.AnthropicOAuthToken != "" {
				t.Fatalf("Claim = (%+v, %v)", payload, err)
			}
			pf.wantNoVendorWrites(t, mustRun(t, pf.env, runID))
		})
	}
}

// TestProviderClaimGatedByCapabilityLiveDB: a worker without claude_provider_v1 cannot claim a
// provider run (issue and chat) but still claims vendor runs; a capable worker claims it.
func TestProviderClaimGatedByCapabilityLiveDB(t *testing.T) {
	pf := newPCFix(t)
	pf.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, pf.workerID,
		[]string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1})
	runID, chatID := pf.run(t), pf.chat(t)
	payload, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || payload != nil {
		t.Fatalf("incapable Claim = (%v, %v), want idle", payload != nil, err)
	}
	chat, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t))
	if err != nil || chat != nil {
		t.Fatalf("incapable ClaimChat = (%v, %v), want idle", chat != nil, err)
	}
	for _, id := range []uuid.UUID{runID, chatID} {
		if r := mustRun(t, pf.env, id); r.Status != "queued" {
			t.Fatalf("run %s status = %s, want still queued", id, r.Status)
		}
	}
	// A vendor run is claimed by the same incapable worker.
	vendor := pf.queuedRun(t, "issue")
	if got, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || got == nil || got.RunID != vendor.String() {
		t.Fatalf("vendor Claim by the incapable worker = (%+v, %v)", got, err)
	}
	pf.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, pf.workerID,
		[]string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.ClaudeProviderV1})
	pf.env.exec(`UPDATE runs SET status = 'completed', worker_id = NULL WHERE id = $1`, vendor)
	got, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || got == nil || got.RunID != runID.String() || got.Secrets.Provider == nil {
		t.Fatalf("capable Claim = (%+v, %v)", got, err)
	}
	gotChat, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t))
	if err != nil || gotChat == nil || gotChat.Secrets.Provider == nil || gotChat.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("capable ClaimChat = (%+v, %v)", gotChat, err)
	}
}

// TestQueuedReasonNoProviderCapableWorkerLiveDB: the health ladder names the missing capability.
func TestQueuedReasonNoProviderCapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	row := func(runID [16]byte) store.ListActiveRunsForHealthRow {
		return store.ListActiveRunsForHealthRow{ID: runID, UserID: e.userID, Status: "queued", Health: healthOK, Harness: harnessClaude,
			ModelProviderKeyMode: pgtype.Text{String: "user_key", Valid: true}}
	}
	t.Run("no capable worker", func(t *testing.T) {
		e.seedWorker(t, nil)
		if got := svc.queuedReason(e.ctx, time.Now(), row(e.seedQueuedRun(t, nil, nil)), thWithLimit(testCustodyLimit)); got != reasonNoProviderCapableWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonNoProviderCapableWorker)
		}
	})
	t.Run("capable worker online", func(t *testing.T) {
		e.seedWorker(t, []string{capability.ClaudeProviderV1})
		if got := svc.queuedReason(e.ctx, time.Now(), row(e.seedQueuedRun(t, nil, nil)), thWithLimit(testCustodyLimit)); got != reasonWaitingWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonWaitingWorker)
		}
	})
	t.Run("a vendor run never gets the provider reason", func(t *testing.T) {
		r := row(e.seedQueuedRun(t, nil, nil))
		r.ModelProviderKeyMode = pgtype.Text{}
		if got := svc.queuedReason(e.ctx, time.Now(), r, thWithLimit(testCustodyLimit)); got == reasonNoProviderCapableWorker {
			t.Fatalf("a vendor run got the provider reason")
		}
	})
}

// TestProviderClaimRefusedWhenURLRemovedThenPromotedLiveDB is PRD example 3: the URL leaves the
// operator list while the run is queued; its next claim parks it on provider_destination_not_allowed
// (never on the owner's Anthropic token); allowlisting the URL again re-queues it via the promoter and
// the next claim succeeds with the binding.
func TestProviderClaimRefusedWhenURLRemovedThenPromotedLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := pf.run(t)
	pf.allow(pcOtherURL)
	payload, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle claim", payload != nil, err)
	}
	r := pf.wantParked(t, runID, "provider_destination_not_allowed")
	if !r.ClaimReleasedAt.Valid || r.WorkerID != (pgtype.UUID{Bytes: pf.workerID, Valid: true}) {
		t.Fatalf("park state: released=%v worker=%v", r.ClaimReleasedAt, r.WorkerID)
	}
	// Still not allowlisted: a promoter pass keeps it held.
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	pf.wantParked(t, runID, "provider_destination_not_allowed")
	// Allowlisted again: the promoter re-queues it, and the next claim carries the binding.
	pf.allow(pvBase)
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	if r := mustRun(t, pf.env, runID); r.Status != "queued" || r.HoldReason.Valid {
		t.Fatalf("after re-allowlisting: status=%s hold=%v, want queued", r.Status, r.HoldReason)
	}
	payload, err = pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || payload == nil || payload.Secrets.Provider == nil || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("Claim = (%+v, %v)", payload, err)
	}
}

// TestProviderDestinationHoldGoesStaleWhileParkedLiveDB: a destination hold whose binding breaks while
// the run is parked is rewritten by the promoter to the final provider_binding_stale (its "resumes
// automatically" copy is false then), and re-allowlisting the URL never re-queues it. A still-intact
// hold keeps its reason while the URL stays de-listed.
func TestProviderDestinationHoldGoesStaleWhileParkedLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := pf.run(t)
	pf.allow(pcOtherURL)
	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v)", p != nil, err)
	}
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	pf.wantParked(t, runID, "provider_destination_not_allowed") // intact binding, URL still de-listed
	before := mustRun(t, pf.env, runID)
	pf.rotate()
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	after := pf.wantParked(t, runID, "provider_binding_stale")
	if after.StatusSince != before.StatusSince || !after.ClaimReleasedAt.Valid {
		t.Fatalf("the rewrite must keep the pause anchor and released claim: since %v -> %v, released=%v", before.StatusSince, after.StatusSince, after.ClaimReleasedAt)
	}
	pf.allow(pvBase)
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	if _, err := pf.svc.Sweep(pf.env.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	pf.wantParked(t, runID, "provider_binding_stale")
}

// TestProviderClaimRevokedBindingParksStaleLiveDB: key disable, rotation (enablement_rev bump), key
// delete and provider delete each refuse the next claim of a bound run with provider_binding_stale,
// for an issue run and a chat; no promoter releases it and an owner Resume is refused.
func TestProviderClaimRevokedBindingParksStaleLiveDB(t *testing.T) {
	for name, revoke := range map[string]func(*pcFix){
		"key disabled": (*pcFix).disable, "key rotated": (*pcFix).rotate, "key deleted": (*pcFix).deleteKey, "provider deleted": (*pcFix).deleteProvider,
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPCFix(t)
			runID, chatID := pf.run(t), pf.chat(t)
			revoke(pf)
			payload, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
			}
			pf.wantParked(t, runID, "provider_binding_stale")
			chat, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t))
			if err != nil || chat != nil {
				t.Fatalf("ClaimChat = (%v, %v), want idle", chat != nil, err)
			}
			pf.wantParked(t, chatID, "provider_binding_stale")
			// No promoter: even re-enabling a disabled key leaves the (stale) hold.
			if name == "key disabled" {
				pf.setEnabled(pf.key, true)
			}
			pf.svc.RequestCredentialDisabledPromotion(pf.userID)
			if _, err := pf.svc.Sweep(pf.env.ctx); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			pf.wantParked(t, runID, "provider_binding_stale")
			pf.wantParked(t, chatID, "provider_binding_stale")
			// Resume is refused by the writer (0 rows) for both provider holds.
			for _, id := range []uuid.UUID{runID, chatID} {
				_, err := pf.env.q.ResumePausedRun(pf.env.ctx, store.ResumePausedRunParams{
					ID: id, UserID: pf.userID, GlobalTimeoutSeconds: 3600,
					StaleCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
				})
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("ResumePausedRun(%s) = %v, want no rows (refused)", id, err)
				}
			}
		})
	}
}

// TestProviderDestinationHoldIsNotResumableLiveDB: the destination hold is refused by Resume too.
func TestProviderDestinationHoldIsNotResumableLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := pf.run(t)
	pf.allow(pcOtherURL)
	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v)", p != nil, err)
	}
	_, err := pf.env.q.ResumePausedRun(pf.env.ctx, store.ResumePausedRunParams{
		ID: runID, UserID: pf.userID, GlobalTimeoutSeconds: 3600,
		StaleCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ResumePausedRun = %v, want refused", err)
	}
}

// TestProviderChatDestinationRefusedThenPromotedLiveDB: the chat twin of example 3.
func TestProviderChatDestinationRefusedThenPromotedLiveDB(t *testing.T) {
	pf := newPCFix(t)
	chatID := pf.chat(t)
	pf.allow(pcOtherURL)
	if c, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t)); err != nil || c != nil {
		t.Fatalf("ClaimChat = (%v, %v)", c != nil, err)
	}
	pf.wantParked(t, chatID, "provider_destination_not_allowed")
	pf.allow(pvBase)
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	if r := mustRun(t, pf.env, chatID); r.Status != "queued" {
		t.Fatalf("chat status = %s, want queued", r.Status)
	}
	c, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t))
	if err != nil || c == nil || c.Secrets.Provider == nil {
		t.Fatalf("ClaimChat = (%+v, %v)", c, err)
	}
}

// TestProviderClaimRotationBetweenAssemblyAndFinishLiveDB: a rotation that commits after assembly
// opened the key is seen by the finisher's locked re-check: nothing is delivered (no re-keyed
// payload reaches the frozen destination) and the run parks stale. Each lane gets its own fixture and
// only that lane's after-assembly hook rotates, so the issue claim cannot pre-refuse the chat at
// assembly and the CHAT finisher's re-check is what parks it.
func TestProviderClaimRotationBetweenAssemblyAndFinishLiveDB(t *testing.T) {
	t.Run("issue run", func(t *testing.T) {
		pf := newPCFix(t)
		runID := pf.run(t)
		pf.svc.claimHooks = &claimTestHooks{
			afterAssembly: func(context.Context, store.Run, *ClaimPayload, error) { pf.rotate() },
		}
		if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p != nil {
			t.Fatalf("Claim = (%v, %v), want nothing delivered", p != nil, err)
		}
		pf.wantParked(t, runID, "provider_binding_stale")
	})
	t.Run("chat", func(t *testing.T) {
		pf := newPCFix(t)
		chatID := pf.chat(t)
		pf.svc.claimHooks = &claimTestHooks{
			afterChatAssembly: func(context.Context, store.Run) { pf.rotate() },
		}
		if c, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t)); err != nil || c != nil {
			t.Fatalf("ClaimChat = (%v, %v), want nothing delivered", c != nil, err)
		}
		pf.wantParked(t, chatID, "provider_binding_stale")
	})
}

// TestProviderClaimHeldRevisionRowRetriesNowaitLiveDB: the finisher's revision read is NOWAIT. A
// provider delete cascades the revision rows before the key row, while the finisher holds the key row
// FOR SHARE: a BLOCKING revision lock there is a lock cycle (40P01, which the finishers do not
// retry). With NOWAIT a revision row held by another session is 55P03, the bounded whole-transaction
// retry the finishers already have, never 40P01 and never a wait. The run is not mutated by the
// refused finish, and once the holder lets go the same claim delivers. Issue run and chat.
func TestProviderClaimHeldRevisionRowRetriesNowaitLiveDB(t *testing.T) {
	defer func(d time.Duration) { finishRunClaimRetryDelay = d }(finishRunClaimRetryDelay)
	finishRunClaimRetryDelay = time.Millisecond
	for _, lane := range []string{"issue run", "chat"} {
		t.Run(lane, func(t *testing.T) {
			pf := newPCFix(t)
			var id uuid.UUID
			if lane == "chat" {
				id = pf.chat(t)
			} else {
				id = pf.run(t)
			}
			holder, err := pf.env.pool.Begin(pf.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(pf.env.ctx) }()
			hold := func() {
				// What a provider delete's cascade holds on the revision row mid-statement.
				if _, err := holder.Exec(pf.env.ctx, `SELECT 1 FROM model_provider_revisions WHERE provider_id = $1 AND revision = 1 FOR UPDATE`, pf.provider); err != nil {
					t.Errorf("hold revision row: %v", err)
				}
			}
			pf.svc.claimHooks = &claimTestHooks{
				afterAssembly:     func(context.Context, store.Run, *ClaimPayload, error) { hold() },
				afterChatAssembly: func(context.Context, store.Run) { hold() },
			}
			type result struct {
				delivered bool
				err       error
			}
			claim := func() result {
				done := make(chan result, 1)
				go func() {
					if lane == "chat" {
						c, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t))
						done <- result{c != nil, err}
						return
					}
					p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
					done <- result{p != nil, err}
				}()
				select {
				case r := <-done:
					return r
				case <-time.After(20 * time.Second):
					_ = holder.Rollback(pf.env.ctx) // free the goroutine before failing
					t.Fatalf("claim finishing blocked on the held revision row (a blocking revision lock)")
					return result{}
				}
			}
			r := claim()
			var pgErr *pgconn.PgError
			if errors.As(r.err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("finisher hit a deadlock (40P01) on the revision row: %v", r.err)
			}
			if r.delivered || !isLockNotAvailable(r.err) {
				t.Fatalf("claim = (delivered=%v, %v), want (nothing, 55P03)", r.delivered, r.err)
			}
			if got := mustRun(t, pf.env, id); got.Status != "claimed" || got.HoldReason.Valid {
				t.Fatalf("a refused finish mutated the run: %s %v", got.Status, got.HoldReason)
			}
			// The holder lets go; the sweeper would requeue the held claim, and the next claim delivers.
			if err := holder.Rollback(pf.env.ctx); err != nil {
				t.Fatal(err)
			}
			pf.svc.claimHooks = nil
			pf.env.exec(`UPDATE runs SET status = 'queued', claim_released_at = NULL WHERE id = $1`, id)
			if r := claim(); r.err != nil || !r.delivered {
				t.Fatalf("claim after release = (delivered=%v, %v), want delivered", r.delivered, r.err)
			}
		})
	}
}

// TestProviderBindingStaleBeatsDestinationLiveDB: precedence. A binding that is BOTH broken and
// de-listed is provider_binding_stale (final for the run), never the destination hold the promoter
// would re-queue once the URL is allowed again. Issue run and chat.
func TestProviderBindingStaleBeatsDestinationLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID, chatID := pf.run(t), pf.chat(t)
	pf.rotate()
	pf.allow(pcOtherURL)
	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want idle", p != nil, err)
	}
	pf.wantParked(t, runID, "provider_binding_stale")
	if c, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t)); err != nil || c != nil {
		t.Fatalf("ClaimChat = (%v, %v), want idle", c != nil, err)
	}
	pf.wantParked(t, chatID, "provider_binding_stale")
}

// TestProviderClaimHeldKeyRowRetriesNowaitLiveDB: a key row locked by a concurrent rotation during
// claim finishing is a NOWAIT refusal (55P03), returned after the bounded retries without waiting on
// the writer and without mutating the run. Issue run and chat.
func TestProviderClaimHeldKeyRowRetriesNowaitLiveDB(t *testing.T) {
	defer func(d time.Duration) { finishRunClaimRetryDelay = d }(finishRunClaimRetryDelay)
	finishRunClaimRetryDelay = time.Millisecond
	pf := newPCFix(t)
	runID, chatID := pf.run(t), pf.chat(t)
	holder, err := pf.env.pool.Begin(pf.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(pf.env.ctx) }()
	hold := func() {
		if _, err := holder.Exec(pf.env.ctx, `SELECT 1 FROM user_secrets WHERE id = $1 FOR UPDATE`, pf.key); err != nil {
			t.Fatalf("hold key row: %v", err)
		}
	}
	pf.svc.claimHooks = &claimTestHooks{
		afterAssembly:     func(context.Context, store.Run, *ClaimPayload, error) { hold() },
		afterChatAssembly: func(context.Context, store.Run) { hold() },
	}
	start := time.Now()
	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); p != nil || !isLockNotAvailable(err) {
		t.Fatalf("Claim = (%v, %v), want (nil, 55P03)", p != nil, err)
	}
	if r := mustRun(t, pf.env, runID); r.Status != "claimed" || r.HoldReason.Valid {
		t.Fatalf("a refused finish mutated the run: %s %v", r.Status, r.HoldReason)
	}
	// The held run stays claimed (the sweeper requeues it); queue the chat's claim fresh.
	if c, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t)); c != nil || !isLockNotAvailable(err) {
		t.Fatalf("ClaimChat = (%v, %v), want (nil, 55P03)", c != nil, err)
	}
	if r := mustRun(t, pf.env, chatID); r.Status != "claimed" || r.HoldReason.Valid {
		t.Fatalf("a refused chat finish mutated the run: %s %v", r.Status, r.HoldReason)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("claim finishing blocked on the held key row")
	}
}

// TestProviderRetiredProviderStillClaimsLiveDB: retiring stops new selections; a bound run keeps
// its frozen revision and still claims.
func TestProviderRetiredProviderStillClaimsLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := pf.run(t)
	pf.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, pf.provider)
	p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || p == nil || p.RunID != runID.String() || p.Secrets.Provider == nil {
		t.Fatalf("Claim = (%+v, %v)", p, err)
	}
}

// TestProviderSameRunContinuationUsesFrozenBindingLiveDB: after the owner switches or clears their
// source (and edits the provider), a re-claim of the SAME run uses the frozen revision and key.
func TestProviderSameRunContinuationUsesFrozenBindingLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := pf.run(t)
	first, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || first == nil {
		t.Fatalf("first Claim = (%v, %v)", first != nil, err)
	}
	// The owner selects the provider, edits it (revision 2 at another URL) and then clears the source.
	pf.env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, pf.userID, pf.provider)
	if _, err := pf.env.pool.Exec(pf.env.ctx, `UPDATE model_providers SET current_revision = 2 WHERE id = $1`, pf.provider); err != nil {
		t.Fatal(err)
	}
	pf.env.exec(`INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map)
	    VALUES ($1, 2, 'anthropic_messages', $2, 'x_api_key', $3::jsonb)`, pf.provider, pvBase, `{"haiku":{"model":"new-haiku"}}`)
	pf.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, pf.userID)
	// Requeue the same run (as a worker restart would) and claim again.
	pf.env.exec(`UPDATE runs SET status = 'queued', claim_released_at = NULL WHERE id = $1`, runID)
	again, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || again == nil || again.Secrets.Provider == nil {
		t.Fatalf("re-Claim = (%+v, %v)", again, err)
	}
	b := again.Secrets.Provider
	if b.Revision != 1 || b.AuthStyle != "bearer" || b.ModelMap["haiku"].Model != "corp-haiku" || b.Credential != pf.plain {
		t.Fatalf("re-claim binding = %+v, want the frozen revision 1", b)
	}
}

// TestCredentialPromoterSkipsProviderRowsLiveDB: a provider-bound row is never promoted on the
// owner's Anthropic default, and a credentials_reset release does not churn: an intact binding is
// claimed once, a stale one parks on provider_binding_stale (no promoter) and stays there.
func TestCredentialPromoterSkipsProviderRowsLiveDB(t *testing.T) {
	pf := newPCFix(t)
	// credential_disabled on a provider row: the owner HAS an enabled default, yet no promotion.
	held := pf.run(t)
	pf.env.exec(`UPDATE runs SET status = 'paused', hold_reason = 'credential_disabled', claim_released_at = now() WHERE id = $1`, held)
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	if r := mustRun(t, pf.env, held); r.Status != "paused" || r.HoldReason.String != "credential_disabled" {
		t.Fatalf("a provider row was promoted on the owner's default: %s %v", r.Status, r.HoldReason)
	}
	pf.env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, held)

	// credentials_reset on an intact provider row is released; its claim succeeds once.
	ok := pf.run(t)
	pf.env.exec(`UPDATE runs SET status = 'paused', hold_reason = 'credentials_reset', claim_released_at = now() WHERE id = $1`, ok)
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	if r := mustRun(t, pf.env, ok); r.Status != "queued" {
		t.Fatalf("intact provider row: status=%s, want released", r.Status)
	}
	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p == nil || p.RunID != ok.String() {
		t.Fatalf("Claim = (%+v, %v)", p, err)
	}
	pf.env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, ok)

	// credentials_reset on a stale row: released once, refused once, then final.
	stale := pf.run(t)
	pf.env.exec(`UPDATE runs SET status = 'paused', hold_reason = 'credentials_reset', claim_released_at = now() WHERE id = $1`, stale)
	pf.rotate()
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p != nil {
		t.Fatalf("stale Claim = (%v, %v)", p != nil, err)
	}
	pf.wantParked(t, stale, "provider_binding_stale")
	for i := 0; i < 3; i++ {
		pf.svc.RequestCredentialDisabledPromotion(pf.userID)
		pf.wantParked(t, stale, "provider_binding_stale")
	}
}

// keyOpenSpy counts the store reads that precede a provider key open.
type keyOpenSpy struct {
	Store
	rd                            providerClaimReader
	bindingReads, ciphertextReads atomic.Int32
}

func (s *keyOpenSpy) GetModelProviderRevisionForOwner(ctx context.Context, arg store.GetModelProviderRevisionForOwnerParams) (store.GetModelProviderRevisionForOwnerRow, error) {
	return s.rd.GetModelProviderRevisionForOwner(ctx, arg)
}

func (s *keyOpenSpy) GetProviderBindingKeyByID(ctx context.Context, arg store.GetProviderBindingKeyByIDParams) (store.GetProviderBindingKeyByIDRow, error) {
	s.bindingReads.Add(1)
	return s.rd.GetProviderBindingKeyByID(ctx, arg)
}

func (s *keyOpenSpy) GetUserSecretCiphertextByID(ctx context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	s.ciphertextReads.Add(1)
	return s.Store.GetUserSecretCiphertextByID(ctx, arg)
}

// TestProviderBindingNeverOpensTheKeyForAWorkerWithoutTheCapabilityLiveDB: the Go-side backstop to the
// SQL claim clauses. A provider-bound run (real frozen binding, real sealed key) assembled for a
// worker lacking claude_provider_v1 is refused terminally on the run and chat lanes before the key
// row or its ciphertext is read; the same run assembled for a capable worker does open the key
// (so the spy is live).
func TestProviderBindingNeverOpensTheKeyForAWorkerWithoutTheCapabilityLiveDB(t *testing.T) {
	pf := newPCFix(t)
	spy := &keyOpenSpy{Store: pf.svc.q, rd: pf.svc.q.(providerClaimReader)}
	pf.svc.q = spy
	runRow, chatRow := mustRun(t, pf.env, pf.run(t)), mustRun(t, pf.env, pf.chat(t))
	capable := pf.worker(t)
	incapable := capable
	incapable.ProtocolCapabilities = []string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1}

	if _, err := pf.svc.assembleClaim(pf.env.ctx, incapable, runRow); !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("assembleClaim for an incapable worker: err = %v, want terminal provider_lane_unsupported", err)
	}
	if _, err := pf.svc.assembleChatClaim(pf.env.ctx, incapable, chatRow); !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("assembleChatClaim for an incapable worker: err = %v, want terminal provider_lane_unsupported", err)
	}
	if b, c := spy.bindingReads.Load(), spy.ciphertextReads.Load(); b != 0 || c != 0 {
		t.Fatalf("the key was reached for an incapable worker: %d binding read(s), %d ciphertext read(s)", b, c)
	}

	pb, err := pf.svc.assembleProviderBinding(pf.env.ctx, capable, runRow)
	if err != nil || pb == nil || pb.Credential != pf.plain {
		t.Fatalf("assembleProviderBinding for a capable worker = (%+v, %v)", pb, err)
	}
	if spy.bindingReads.Load() == 0 || spy.ciphertextReads.Load() == 0 {
		t.Fatalf("the spy saw no key open for the capable worker: %d / %d", spy.bindingReads.Load(), spy.ciphertextReads.Load())
	}
}
