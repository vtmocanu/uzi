package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_job_livedb_test.go is PRD #4322 M1b against a real Postgres: a job whose owner selected a
// Claude provider source is frozen to it at create (decided under the shared secret-mutation lock,
// verified against the model map), claims against that frozen binding with no vendor credential, never
// parks, and is refused or settled terminally by every revocation. Every owner is also seeded with a
// USABLE default vendor token, so an unintended vendor fallback would succeed and fail the test.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.
// Every test name ends LiveDB so the store-IT sweep selects it.

var pjobWorkerCaps = []string{jobCap, jobFilesCap, capability.ClaudeProviderV1, capability.ClaudeProviderJobV1, capability.RecoveryArchiveV1}

// pjobFix is a provider-sourced owner (source A selected and usable, a really sealed gateway key, a
// usable default vendor token) and a job-capable provider worker.
type pjobFix struct {
	*jdFix
	plain string
	je    jobEnv
}

func newPJobFixMap(t *testing.T, modelMap string) *pjobFix {
	t.Helper()
	j := newJdFixMap(t, modelMap)
	p := &pjobFix{jdFix: j, plain: "gw-job-key-" + uuid.NewString(), je: jobEnv{codexTestEnv: j.env, svc: j.svc}}
	sealed, err := j.env.box.Seal([]byte(p.plain))
	if err != nil {
		t.Fatal(err)
	}
	j.env.exec(`UPDATE user_secrets SET ciphertext = $2 WHERE id = $1`, j.key, sealed)
	// Registered after the fixture's own cleanups, so it runs first: the owner's job rows go before
	// the provider and user they reference.
	t.Cleanup(func() {
		_, _ = j.env.pool.Exec(context.Background(), `DELETE FROM runs WHERE user_id = $1 AND kind = 'job'`, j.userID)
	})
	return p
}

func newPJobFix(t *testing.T) *pjobFix { return newPJobFixMap(t, pvDefaultMap) }

// create runs CreateJobRun for the owner with the default request.
func (p *pjobFix) create() (JobView, error) {
	return p.svc.CreateJobRun(p.env.ctx, jobReq(cliCaller(p.userID)))
}

func (p *pjobFix) mustCreate() store.Run {
	p.t.Helper()
	v, err := p.create()
	if err != nil {
		p.t.Fatalf("CreateJobRun: %v", err)
	}
	return mustRun(p.t, p.env, v.ID)
}

func (p *pjobFix) jobCount() int {
	p.t.Helper()
	return p.je.jobCount(p.t, p.userID)
}

// wantNoNewJob fails when the owner's job count moved off before.
func (p *pjobFix) wantNoNewJob(before int) {
	p.t.Helper()
	if got := p.jobCount(); got != before {
		p.t.Fatalf("job count = %d, want %d: a refused create left a run", got, before)
	}
}

// wantVendorJob asserts an unbound job: all five binding columns NULL.
func (p *pjobFix) wantVendorJob(r store.Run) {
	p.t.Helper()
	if providerBoundRun(r) || r.ModelProviderRevision.Valid || r.ModelProviderKeyMode.Valid || r.ModelProviderKeySecretID.Valid || r.ModelProviderKeyRev.Valid {
		p.t.Fatalf("vendor job carries a binding: %+v", bindingOfRun(r))
	}
	if r.Kind != "job" || r.Harness != "claude" {
		p.t.Fatalf("run = kind %s harness %s, want a Claude job", r.Kind, r.Harness)
	}
}

// wantProviderJob asserts a provider-bound job on (provider, key) with no vendor credential.
func (p *pjobFix) wantProviderJob(r store.Run, provider, key uuid.UUID) {
	p.t.Helper()
	p.wantBoundTo(r, provider, key, 1)
	if r.Kind != "job" || !r.JobProtocol.Valid || r.Status != "queued" {
		p.t.Fatalf("run = kind %s status %s job_protocol %v, want a queued stamped job", r.Kind, r.Status, r.JobProtocol)
	}
}

// jobWorker is a fresh non-Docker worker advertising caps.
func (p *pjobFix) jobWorker(caps ...string) store.Worker {
	p.t.Helper()
	return wkrRow(p.t, p.env, p.je.seedWorkerRow(p.t, p.userID, false, nil, caps...))
}

func (p *pjobFix) claim(w store.Worker) (*ClaimPayload, error) {
	return p.svc.Claim(p.env.ctx, w, nil)
}

// wantNoVendor: a provider job records no vendor credential, epoch or rejection mark.
func (p *pjobFix) wantNoVendor(id uuid.UUID) {
	p.t.Helper()
	got := mustRun(p.t, p.env, id)
	if got.AnthropicSecretID.Valid || got.AnthropicSecretLabel.Valid || got.AnthropicSelectReason.Valid || got.CredentialOverrideSecretID.Valid {
		p.t.Fatalf("provider job recorded a vendor credential: %+v", got)
	}
	if n := p.env.countRunCredentialEpochs(p.t, id); n != 0 {
		p.t.Fatalf("provider job journaled %d vendor credential epochs", n)
	}
}

func (p *pjobFix) wantFailedTerminally(id uuid.UUID, reasonPart string) store.Run {
	p.t.Helper()
	r := mustRun(p.t, p.env, id)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" || !strings.Contains(r.FailureReason.String, reasonPart) || r.HoldReason.Valid {
		p.t.Fatalf("job %s: status=%s origin=%v reason=%q hold=%v, want failed/credential_unavailable containing %q and no hold (never paused)",
			id, r.Status, r.FailOrigin, r.FailureReason.String, r.HoldReason, reasonPart)
	}
	p.wantNoVendor(id)
	return r
}

// ---------------------------------------------------------------------------------------------
// Create: the freeze and its refusals.
// ---------------------------------------------------------------------------------------------

func TestProviderJobCreateFrozenLiveDB(t *testing.T) {
	p := newPJobFix(t)
	r := p.mustCreate()
	p.wantProviderJob(r, p.provider, p.key)
	if r.ModelProviderKeyRev.Int64 != p.keyRev() {
		t.Fatalf("frozen key rev = %d, want %d", r.ModelProviderKeyRev.Int64, p.keyRev())
	}
	// A provider-only owner (no vendor credential at all) gets a provider job too.
	p.env.exec(`DELETE FROM user_secrets WHERE id = $1`, p.token)
	p.wantProviderJob(p.mustCreate(), p.provider, p.key)
}

// TestProviderJobCreateVendorUnchangedLiveDB: an owner with no Claude source keeps the vendor job, with
// every binding column NULL, and a site-list job still binds its list (no provider involved).
func TestProviderJobCreateVendorUnchangedLiveDB(t *testing.T) {
	p := newPJobFix(t)
	p.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, p.userID)
	p.wantVendorJob(p.mustCreate())
	id, name := p.je.seedProfile(t, "docs.example.com")
	v, err := p.svc.CreateJobRun(p.env.ctx, profileReq(cliCaller(p.userID), name))
	if err != nil {
		t.Fatalf("site-list vendor job: %v", err)
	}
	r := mustRun(t, p.env, v.ID)
	p.wantVendorJob(r)
	if !r.EgressProfileID.Valid || uuid.UUID(r.EgressProfileID.Bytes) != id {
		t.Fatalf("egress profile = %v, want %s", r.EgressProfileID, id)
	}
}

func TestProviderJobCreateRefusalsLiveDB(t *testing.T) {
	t.Run("unmapped default model refuses with the job source, no row", func(t *testing.T) {
		p := newPJobFixMap(t, pvJobNoSonnetMap)
		before := p.jobCount()
		_, err := p.create()
		var um *ProviderModelUnmappedError
		if !errors.Is(err, ErrProviderModelUnmapped) || !errors.As(err, &um) || um.Source != "job" || um.Model != "sonnet" {
			t.Fatalf("err = %v, want ProviderModelUnmappedError{sonnet, job}", err)
		}
		p.wantNoNewJob(before)
	})
	t.Run("an unmapped owner default refuses; the mapped control creates", func(t *testing.T) {
		p := newPJobFix(t)
		p.env.exec(`UPDATE users SET default_claude_model = 'claude-not-in-the-map' WHERE id = $1`, p.userID)
		before := p.jobCount()
		var um *ProviderModelUnmappedError
		if _, err := p.create(); !errors.As(err, &um) || um.Model != "claude-not-in-the-map" || um.Source != "job" {
			t.Fatalf("err = %v, want the unmapped owner default", err)
		}
		p.wantNoNewJob(before)
		p.env.exec(`UPDATE users SET default_claude_model = 'opus' WHERE id = $1`, p.userID)
		p.wantProviderJob(p.mustCreate(), p.provider, p.key)
	})
	unusable := []struct {
		name   string
		break_ func(p *pjobFix)
		reason string
	}{
		{"base url off the allowlist", func(p *pjobFix) { p.svc.p.ProviderAllowedBaseURLs = []string{pcOtherURL} }, SourceUnusableBaseURL},
		{"retired provider", func(p *pjobFix) {
			p.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, p.provider)
		}, SourceUnusableRetired},
		{"key disabled", func(p *pjobFix) { p.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, p.key) }, SourceUnusableKeyDisabled},
	}
	for _, tc := range unusable {
		t.Run(tc.name+": 409 provider_source_unusable, no row, no vendor fallback", func(t *testing.T) {
			p := newPJobFix(t)
			tc.break_(p)
			before := p.jobCount()
			if _, err := p.create(); !isUnusable(err, tc.reason) {
				t.Fatalf("err = %v, want provider_source_unusable (%s)", err, tc.reason)
			}
			p.wantNoNewJob(before)
		})
	}
	t.Run("a site-list job under a selected source is refused, usable or not", func(t *testing.T) {
		p := newPJobFix(t)
		_, name := p.je.seedProfile(t, "docs.example.com")
		before := p.jobCount()
		if _, err := p.svc.CreateJobRun(p.env.ctx, profileReq(cliCaller(p.userID), name)); !errors.Is(err, ErrProviderLaneUnsupported) {
			t.Fatalf("usable source: err = %v, want provider_lane_unsupported", err)
		}
		p.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, p.provider)
		if _, err := p.svc.CreateJobRun(p.env.ctx, profileReq(cliCaller(p.userID), name)); !errors.Is(err, ErrProviderLaneUnsupported) {
			t.Fatalf("unusable source: err = %v, want provider_lane_unsupported", err)
		}
		p.wantNoNewJob(before)
	})
}

// TestProviderJobCreateSharedRecipientLiveDB: an admin-shared definition freezes the RECIPIENT's own
// key; a recipient with no key of their own is refused (409, no row) and never borrows another's.
func TestProviderJobCreateSharedRecipientLiveDB(t *testing.T) {
	p := newPJobFix(t)
	shared, _ := seedSharedProvider(t, p.env, "claude", pvDefaultMap)
	other := p.env.seedUser(t)
	seedSealedKey(t, p.env, other, shared, "gw-key-OTHER-"+uuid.NewString())
	selectClaudeSource(t, p.env, p.userID, shared)
	before := p.jobCount()
	if _, err := p.create(); !isUnusable(err, SourceUnusableKeyMissing) {
		t.Fatalf("recipient without a key: err = %v, want provider_source_unusable (key_missing)", err)
	}
	p.wantNoNewJob(before)
	own := seedSealedKey(t, p.env, p.userID, shared, "gw-key-OWN-"+uuid.NewString())
	r := p.mustCreate()
	p.wantProviderJob(r, shared, own)
}

// TestProviderJobCreateStrictModelLiveDB: the create check resolves the same canonical model the claim
// carries; "inherit" at any level is absent and a map target literally named inherit is never matched
// by it.
func TestProviderJobCreateStrictModelLiveDB(t *testing.T) {
	inheritSonnet := `{"opus":{"model":"corp-opus"},"sonnet":{"model":"inherit"},"haiku":{"model":"corp-haiku"}}`
	inheritOpus := `{"opus":{"model":"inherit"},"sonnet":{"model":"x"},"haiku":{"model":"corp-haiku"}}`
	for _, tc := range []struct {
		name, modelMap, lane string
		wantModel            string
	}{
		{"owner default inherit falls to sonnet", pvDefaultMap, "inherit", "sonnet"},
		{"mapped owner default", pvDefaultMap, "opus", "opus"},
		{"sonnet target literally inherit, no model", inheritSonnet, "", "sonnet"},
		{"opus target inherit never matches an inherit model", inheritOpus, "inherit", "sonnet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJobFixMap(t, tc.modelMap)
			if tc.lane != "" {
				p.env.exec(`UPDATE users SET default_claude_model = $2 WHERE id = $1`, p.userID, tc.lane)
			}
			r := p.mustCreate()
			p.wantProviderJob(r, p.provider, p.key)
			w := p.jobWorker(pjobWorkerCaps...)
			payload, err := p.claim(w)
			if err != nil || payload == nil || payload.RunID != r.ID.String() {
				t.Fatalf("Claim = (%+v, %v), want the job", payload, err)
			}
			if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != tc.wantModel {
				t.Fatalf("default_model = %v, want %q", payload.Config.DefaultModel, tc.wantModel)
			}
		})
	}
	t.Run("a frozen run model of inherit falls to the mapped owner default", func(t *testing.T) {
		p := newPJobFix(t)
		p.env.exec(`UPDATE users SET default_claude_model = 'opus' WHERE id = $1`, p.userID)
		r := p.mustCreate()
		p.env.exec(`UPDATE runs SET model = 'inherit' WHERE id = $1`, r.ID)
		payload, err := p.claim(p.jobWorker(pjobWorkerCaps...))
		if err != nil || payload == nil || payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != "opus" {
			t.Fatalf("Claim = (%+v, %v), want default_model opus", payload, err)
		}
	})
}

// TestProviderJobCreateClosureBackstopLiveDB: createRunAtomic refuses a site-list job under a selected
// source before the insert closure runs, so the closure's own provider-and-profile backstop is reached
// only with an origin that disagrees with the parameters. Driving createJobRun with JobLane set and
// JobSiteList unset, a usable source and an allowed profile reaches it: ErrProviderLaneUnsupported and
// no row (read through a separate connection).
//
// MUTATION CHECK: deleting the closure's resolved.ProviderSource/profileID check makes this red.
func TestProviderJobCreateClosureBackstopLiveDB(t *testing.T) {
	p := newPJobFix(t)
	_, name := p.je.seedProfile(t, "docs.example.com")
	req := profileReq(cliCaller(p.userID), name)
	v, err := validateCreateJob(req, budgetDurationSeconds(p.svc.p.RunWallCeiling), budgetDurationSeconds(p.svc.p.RunTimeout))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.svc.createJobRun(p.env.ctx, req, v, defaultJobMaxActive, harnessCreationOrigin{JobLane: true, JobSiteList: false})
	if !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Fatalf("err = %v, want provider_lane_unsupported", err)
	}
	var n int
	if err := p.aux.QueryRow(p.env.ctx, `SELECT count(*) FROM runs WHERE user_id = $1 AND kind = 'job'`, p.userID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("separate-connection job count = %d (%v), want 0", n, err)
	}
}

// ---------------------------------------------------------------------------------------------
// Create: the lane decision under the shared secret-mutation lock.
// ---------------------------------------------------------------------------------------------

// jobSourceWriter is the PUT /providers/source transaction shape (handler/providers_source.go inside
// withSecretLock): the exclusive secret-mutation lock, the provider row FOR SHARE, the source write.
// to == uuid.Nil clears the source.
func (p *pjobFix) jobSourceWriter(ctx context.Context, to uuid.UUID) error {
	tx, err := p.aux.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := p.writeSource(ctx, tx, to); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *pjobFix) writeSource(ctx context.Context, tx pgx.Tx, to uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '20s'`); err != nil {
		return err
	}
	q := store.New(tx)
	if err := q.LockSecretMutation(ctx, p.userID); err != nil {
		return err
	}
	arg := store.SetUserProviderSourceParams{Engine: "claude", ID: p.userID}
	if to != uuid.Nil {
		if _, err := q.LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: to, OwnerUserID: p.userID}); err != nil {
			return err
		}
		arg.ProviderID = pgtype.UUID{Bytes: to, Valid: true}
	}
	_, err := q.SetUserProviderSource(ctx, arg)
	return err
}

// TestProviderJobCreateRaceCreateFirstLiveDB: a job decides its lane under the shared secret-mutation
// lock and holds it to commit, so the real source writer (exclusive lock) queues behind the create. The
// hook runs after the lock and the read: it starts the writer, waits until it is observed queued on the
// lock key (pg_locks), checks it has not finished, and returns. The job commits bound to the ORIGINAL
// source with no vendor credential; the writer commits afterwards.
func TestProviderJobCreateRaceCreateFirstLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   func(p *pjobFix) uuid.UUID
		want func(p *pjobFix) pgtype.UUID
	}{
		{"writer clears the source", func(*pjobFix) uuid.UUID { return uuid.Nil }, func(*pjobFix) pgtype.UUID { return pgtype.UUID{} }},
		{"writer switches to source B", func(p *pjobFix) uuid.UUID { return p.altID }, func(p *pjobFix) pgtype.UUID { return pgtype.UUID{Bytes: p.altID, Valid: true} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJobFix(t)
			p.addAlt()
			finished := make(chan struct{})
			var writerErr, hookErr error
			var hookRan bool
			p.svc.jobDecidedHook = func(context.Context) {
				hookRan = true
				wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				go func() {
					defer cancel()
					writerErr = p.jobSourceWriter(wctx, tc.to(p))
					close(finished)
				}()
				deadline := time.Now().Add(10 * time.Second)
				for p.waiters() < 1 {
					select {
					case <-finished:
						hookErr = fmt.Errorf("the writer finished (%v) before it was seen queued behind the job's shared lock", writerErr)
						return
					default:
					}
					if time.Now().After(deadline) {
						hookErr = errors.New("the writer was never seen queued behind the job's shared lock")
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				select {
				case <-finished:
					hookErr = fmt.Errorf("the writer finished (%v) while the job still held the shared lock", writerErr)
				default:
				}
			}
			t.Cleanup(func() { p.svc.jobDecidedHook = nil })
			v, err := p.create()
			if !hookRan {
				t.Fatal("the lane hook never ran")
			}
			if hookErr != nil {
				select {
				case <-finished:
				case <-time.After(30 * time.Second):
				}
				t.Fatalf("hook: %v (create err = %v)", hookErr, err)
			}
			if err != nil {
				t.Fatalf("job create: %v", err)
			}
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Fatal("the writer did not finish after the job committed")
			}
			if writerErr != nil {
				t.Fatalf("writer: %v", writerErr)
			}
			// The job read the source before the writer's commit and stays frozen to A, with no vendor id.
			r := mustRun(t, p.env, v.ID)
			p.wantProviderJob(r, p.provider, p.key)
			if r.AnthropicSecretID.Valid {
				t.Fatalf("anthropic_secret_id = %v, want NULL on a provider job", r.AnthropicSecretID)
			}
			provider, _, _ := p.userState()
			if want := tc.want(p); provider != want {
				t.Fatalf("final source = %v, want %v after the writer committed", provider, want)
			}
		})
	}
}

// TestProviderJobCreateRaceWriterFirstLiveDB: the job reads the lane only AFTER it holds the shared
// lock. The writer holds the exclusive lock (uncommitted), the create is started and observed queued on
// the lock key, then the writer commits: the job reflects the newly committed decision. Mirrors
// TestJudgeCreatePolicyLockBeforeReadLiveDB.
func TestProviderJobCreateRaceWriterFirstLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name     string
		start    func(p *pjobFix) // the committed state before the race
		to       func(p *pjobFix) uuid.UUID
		siteList bool
		check    func(t *testing.T, p *pjobFix, v JobView, err error)
	}{
		{
			name: "writer clears the source: the job is a vendor job",
			to:   func(*pjobFix) uuid.UUID { return uuid.Nil },
			check: func(t *testing.T, p *pjobFix, v JobView, err error) {
				if err != nil {
					t.Fatalf("job create: %v", err)
				}
				p.wantVendorJob(mustRun(t, p.env, v.ID))
			},
		},
		{
			name: "writer selects B: the job is bound to B",
			start: func(p *pjobFix) {
				p.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, p.userID)
			},
			to: func(p *pjobFix) uuid.UUID { return p.altID },
			check: func(t *testing.T, p *pjobFix, v JobView, err error) {
				if err != nil {
					t.Fatalf("job create: %v", err)
				}
				p.wantProviderJob(mustRun(t, p.env, v.ID), p.altID, p.altKy)
			},
		},
		{
			name: "a site-list job is refused once the writer selects a source",
			start: func(p *pjobFix) {
				p.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, p.userID)
			},
			to:       func(p *pjobFix) uuid.UUID { return p.altID },
			siteList: true,
			check: func(t *testing.T, p *pjobFix, v JobView, err error) {
				if !errors.Is(err, ErrProviderLaneUnsupported) {
					t.Fatalf("err = %v, want provider_lane_unsupported", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJobFix(t)
			p.addAlt()
			if tc.start != nil {
				tc.start(p)
			}
			req := jobReq(cliCaller(p.userID))
			if tc.siteList {
				_, req.EgressProfile = p.je.seedProfile(t, "docs.example.com")
			}
			ctx := p.env.ctx
			tx, err := p.aux.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatalf("begin writer: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := p.writeSource(ctx, tx, tc.to(p)); err != nil {
				t.Fatalf("writer: %v", err)
			}
			type result struct {
				v   JobView
				err error
			}
			done := make(chan result, 1)
			// A failure below must not leave the create racing the fixture cleanup: cancel it, release the
			// writer and join the goroutine first (this cleanup was registered after the fixture's, so it runs before it).
			gctx, cancelCreate := context.WithCancel(ctx)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				v, err := p.svc.CreateJobRun(gctx, req)
				done <- result{v, err}
			}()
			t.Cleanup(func() {
				cancelCreate()
				_ = tx.Rollback(ctx)
				<-joined
			})
			deadline := time.Now().Add(10 * time.Second)
			for p.waiters() < 1 {
				select {
				case r := <-done:
					t.Fatalf("the job returned (%v) before it was seen queued on the shared lock", r.err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the job was never seen queued on the shared lock")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit writer: %v", err)
			}
			select {
			case r := <-done:
				tc.check(t, p, r.v, r.err)
			case <-time.After(30 * time.Second):
				t.Fatal("the job did not finish after the writer committed")
			}
		})
	}
}

// putUnattachedInput stores a small input file the job create can attach.
func (p *pjobFix) putUnattachedInput(jf *JobFiles, body string) store.JobFile {
	p.t.Helper()
	row, err := jf.Reserve(p.env.ctx, ReserveParams{UserID: p.userID, DisplayName: "in.txt", Direction: JobFileInput, DeclaredSize: int64(len(body))})
	if err != nil {
		p.t.Fatalf("Reserve: %v", err)
	}
	out, err := jf.Write(p.env.ctx, row.ID, p.userID, bytes.NewReader([]byte(body)), WriteOptions{ContentType: "text/plain"})
	if err != nil {
		p.t.Fatalf("Write: %v", err)
	}
	return out
}

// TestProviderJobCreateVisibilityLiveDB (A6): the create is one transaction. (i) With the real input,
// attachment and origin written and the run bound, a separate connection sees nothing until the commit,
// then everything. (ii) A failure at the commit seam leaves no row, input, origin or attachment. (iii)
// A post-insert model refusal (the one error only the post-insert verification produces) leaves nothing,
// while a mapped control reaches the commit seam.
func TestProviderJobCreateVisibilityLiveDB(t *testing.T) {
	counts := func(p *pjobFix, runID uuid.UUID, file uuid.UUID) (runs, inputs, origins, attached int) {
		p.t.Helper()
		row := p.aux.QueryRow(p.env.ctx, `SELECT
			(SELECT count(*) FROM runs WHERE id = $1),
			(SELECT count(*) FROM job_inputs WHERE run_id = $1),
			(SELECT count(*) FROM job_origins WHERE run_id = $1),
			(SELECT count(*) FROM job_files WHERE id = $2 AND run_id = $1)`, runID, file)
		if err := row.Scan(&runs, &inputs, &origins, &attached); err != nil {
			p.t.Fatalf("count: %v", err)
		}
		return
	}
	newReq := func(p *pjobFix) (CreateJobParams, store.JobFile) {
		jf := NewJobFiles(p.env.pool, p.env.box, wide(), nil)
		p.svc.SetJobFiles(jf)
		f := p.putUnattachedInput(jf, "attached input")
		req := jobReq(cliCaller(p.userID))
		req.Inputs = []JobInput{{Name: "notes.md", Content: "alpha"}}
		req.InputFileIDs = []uuid.UUID{f.ID}
		return req, f
	}
	t.Run("(i) nothing visible before the commit, everything after", func(t *testing.T) {
		p := newPJobFix(t)
		req, f := newReq(p)
		var seen bool
		p.svc.createBeforeCommitHook = func(run store.Run) {
			seen = true
			if r, in, o, a := counts(p, run.ID, f.ID); r+in+o+a != 0 {
				t.Errorf("before commit a separate connection sees runs=%d inputs=%d origins=%d attached=%d, want all 0", r, in, o, a)
			}
			if !providerBoundRun(run) {
				t.Errorf("the run at the commit seam is not provider-bound: %+v", bindingOfRun(run))
			}
		}
		t.Cleanup(func() { p.svc.createBeforeCommitHook = nil })
		v, err := p.svc.CreateJobRun(p.env.ctx, req)
		if err != nil || !seen {
			t.Fatalf("CreateJobRun = (%v, %v), seam reached = %v", v.ID, err, seen)
		}
		if r, in, o, a := counts(p, v.ID, f.ID); r != 1 || in != 1 || o != 1 || a != 1 {
			t.Fatalf("after commit runs=%d inputs=%d origins=%d attached=%d, want all 1", r, in, o, a)
		}
		var inName, inContent string
		if err := p.aux.QueryRow(p.env.ctx, `SELECT name, content_md FROM job_inputs WHERE run_id = $1`, v.ID).Scan(&inName, &inContent); err != nil {
			t.Fatalf("read the persisted input: %v", err)
		}
		if inName != "notes.md" || inContent != "alpha" {
			t.Fatalf("persisted input = (%q, %q), want (notes.md, alpha)", inName, inContent)
		}
	})
	t.Run("(ii) a commit-seam failure leaves nothing", func(t *testing.T) {
		p := newPJobFix(t)
		req, f := newReq(p)
		boom := errors.New("injected commit-seam failure")
		var runID uuid.UUID
		p.svc.createBeforeCommitErrHook = func(run store.Run) error { runID = run.ID; return boom }
		t.Cleanup(func() { p.svc.createBeforeCommitErrHook = nil })
		before := p.jobCount()
		if _, err := p.svc.CreateJobRun(p.env.ctx, req); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the injected failure", err)
		}
		if runID == uuid.Nil {
			t.Fatal("the commit seam was never reached")
		}
		if r, in, o, a := counts(p, runID, f.ID); r+in+o+a != 0 {
			t.Fatalf("after the failed create runs=%d inputs=%d origins=%d attached=%d, want all 0", r, in, o, a)
		}
		p.wantNoNewJob(before)
	})
	t.Run("(iii) the post-insert model refusal leaves nothing; the mapped control commits", func(t *testing.T) {
		unmapped := newPJobFixMap(t, pvJobNoSonnetMap)
		inserted := unmapped.countRunInserts(t)
		var reached atomic.Int32
		unmapped.svc.createBeforeCommitHook = func(store.Run) { reached.Add(1) }
		t.Cleanup(func() { unmapped.svc.createBeforeCommitHook = nil })
		before := unmapped.jobCount()
		_, err := unmapped.create()
		var um *ProviderModelUnmappedError
		if !errors.As(err, &um) || um.Source != "job" || um.Model != "sonnet" {
			t.Fatalf("err = %v, want the post-insert ProviderModelUnmappedError{sonnet, job}", err)
		}
		if reached.Load() != 0 {
			t.Fatalf("the commit seam ran %d times for a refused create", reached.Load())
		}
		unmapped.wantNoNewJob(before)
		// The refusal is post-insert: the run INSERT executed (a non-transactional sequence bump from a
		// trigger survives the rollback) and was then rolled back. A typed error raised before the
		// insert would leave the count at 0 and fail here.
		if n := inserted(); n != 1 {
			t.Fatalf("run INSERT statements for the refused create = %d, want exactly 1 (insert ran, then rolled back)", n)
		}

		mapped := newPJobFix(t)
		mappedInserted := mapped.countRunInserts(t)
		var reachedMapped atomic.Int32
		mapped.svc.createBeforeCommitHook = func(store.Run) { reachedMapped.Add(1) }
		t.Cleanup(func() { mapped.svc.createBeforeCommitHook = nil })
		mapped.wantProviderJob(mapped.mustCreate(), mapped.provider, mapped.key)
		if reachedMapped.Load() != 1 {
			t.Fatalf("the mapped control reached the commit seam %d times, want 1", reachedMapped.Load())
		}
		if n := mappedInserted(); n != 1 {
			t.Fatalf("control: run INSERT statements = %d, want 1 (the counter itself works)", n)
		}
	})
}

// ---------------------------------------------------------------------------------------------
// Claim.
// ---------------------------------------------------------------------------------------------

func TestProviderJobCreateClaimFrozenLiveDB(t *testing.T) {
	p := newPJobFix(t)
	r := p.mustCreate()
	w := p.jobWorker(pjobWorkerCaps...)
	payload, err := p.claim(w)
	if err != nil || payload == nil || payload.RunID != r.ID.String() || payload.Job == nil {
		t.Fatalf("Claim = (%+v, %v), want the provider job", payload, err)
	}
	b := payload.Secrets.Provider
	if b == nil || payload.Secrets.AnthropicOAuthToken != "" || payload.Secrets.Codex != nil || payload.Secrets.ForgePAT != "" {
		t.Fatalf("secrets = %+v, want only a provider binding", payload.Secrets)
	}
	if b.ProviderID != p.provider.String() || b.Revision != 1 || b.Engine != "claude" || b.BaseURL != pvBase || b.Credential != p.plain || b.ModelMap["sonnet"].Model != "corp-sonnet" {
		t.Fatalf("binding = %+v", b)
	}
	if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != "sonnet" || !modelMapped(b.ModelMap, *payload.Config.DefaultModel) {
		t.Fatalf("default_model = %v, want the mapped canonical alias", payload.Config.DefaultModel)
	}
	if got := mustRun(t, p.env, r.ID); got.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", got.Status)
	}
	p.wantNoVendor(r.ID)
}

// TestProviderJobClaimSharedRecipientKeyLiveDB: two recipients of one shared definition each hold their
// own key; each job's claim opens exactly its owner's key, and the frozen key id is that owner's.
func TestProviderJobClaimSharedRecipientKeyLiveDB(t *testing.T) {
	a := newPJobFix(t)
	shared, _ := seedSharedProvider(t, a.env, "claude", pvDefaultMap)
	plainA, plainB := "gw-key-A-"+uuid.NewString(), "gw-key-B-"+uuid.NewString()
	keyA := seedSealedKey(t, a.env, a.userID, shared, plainA)
	selectClaudeSource(t, a.env, a.userID, shared)

	b := newPJobFix(t)
	keyB := seedSealedKey(t, b.env, b.userID, shared, plainB)
	selectClaudeSource(t, b.env, b.userID, shared)

	for _, tc := range []struct {
		p     *pjobFix
		key   uuid.UUID
		plain string
	}{{a, keyA, plainA}, {b, keyB, plainB}} {
		r := tc.p.mustCreate()
		tc.p.wantProviderJob(r, shared, tc.key)
		payload, err := tc.p.claim(tc.p.jobWorker(pjobWorkerCaps...))
		if err != nil || payload == nil || payload.RunID != r.ID.String() || payload.Secrets.Provider == nil {
			t.Fatalf("Claim = (%+v, %v)", payload, err)
		}
		if payload.Secrets.Provider.Credential != tc.plain {
			t.Fatalf("the claim opened a key that is not its owner's (credential %q)", payload.Secrets.Provider.Credential)
		}
		if got := mustRun(t, tc.p.env, r.ID); uuid.UUID(got.ModelProviderKeySecretID.Bytes) != tc.key {
			t.Fatalf("frozen key %v, want %s", got.ModelProviderKeySecretID, tc.key)
		}
		tc.p.wantNoVendor(r.ID)
	}
}

// TestProviderJobClaimNoRebindLiveDB: while the job is queued the owner clears the source and the
// shared edit changes nothing: the claim delivers the FROZEN binding and no vendor token.
func TestProviderJobClaimNoRebindLiveDB(t *testing.T) {
	p := newPJobFix(t)
	r := p.mustCreate()
	p.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, p.userID)
	payload, err := p.claim(p.jobWorker(pjobWorkerCaps...))
	if err != nil || payload == nil || payload.RunID != r.ID.String() {
		t.Fatalf("Claim = (%+v, %v)", payload, err)
	}
	if b := payload.Secrets.Provider; b == nil || b.ProviderID != p.provider.String() || b.Credential != p.plain || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("secrets = %+v, want the FROZEN binding and no vendor token", payload.Secrets)
	}
	p.wantNoVendor(r.ID)
}

// TestProviderJobClaimRevocationNeverParksLiveDB: every revocation before assembly settles the job as
// failed/credential_unavailable with the typed reason, delivers nothing and never parks it.
func TestProviderJobClaimRevocationNeverParksLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(p *pjobFix)
		reason string
	}{
		{"key disabled", func(p *pjobFix) {
			p.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
		}, holdProviderBindingStale},
		{"key rotated", func(p *pjobFix) {
			p.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
		}, holdProviderBindingStale},
		{"key deleted", func(p *pjobFix) { p.env.exec(`DELETE FROM user_secrets WHERE id = $1`, p.key) }, holdProviderBindingStale},
		{"url removed from the allowlist", func(p *pjobFix) { p.svc.p.ProviderAllowedBaseURLs = []string{pcOtherURL} }, holdProviderDestinationNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJobFix(t)
			r := p.mustCreate()
			tc.revoke(p)
			w := p.jobWorker(pjobWorkerCaps...)
			for i := 0; i < 2; i++ {
				payload, err := p.claim(w)
				if err != nil || payload != nil {
					t.Fatalf("Claim %d = (%v, %v), want nothing delivered", i, payload != nil, err)
				}
			}
			p.wantFailedTerminally(r.ID, tc.reason)
		})
	}
}

// TestProviderJobClaimFinalRecheckLiveDB: the binding is intact at assembly (the hook proves assembly
// delivered a payload) and revoked only afterwards, so the finisher's locked re-check is what refuses:
// nothing is delivered, the job fails terminally (credential_unavailable), never parks, records no
// vendor credential, and a stale finisher (a newer claim generation) settles nothing.
func TestProviderJobClaimFinalRecheckLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(p *pjobFix)
		reason string
	}{
		{"key rotated", func(p *pjobFix) {
			p.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
		}, holdProviderBindingStale},
		{"key disabled", func(p *pjobFix) {
			p.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
		}, holdProviderBindingStale},
		{"url removed from the allowlist", func(p *pjobFix) { p.svc.p.ProviderAllowedBaseURLs = []string{pcOtherURL} }, holdProviderDestinationNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJobFix(t)
			r := p.mustCreate()
			assembled := false
			p.svc.claimHooks = &claimTestHooks{
				afterAssembly: func(_ context.Context, run store.Run, payload *ClaimPayload, err error) {
					if run.ID == r.ID && err == nil && payload != nil && payload.Secrets.Provider != nil {
						assembled = true
					}
					tc.revoke(p)
				},
			}
			t.Cleanup(func() { p.svc.claimHooks = nil })
			payload, err := p.claim(p.jobWorker(pjobWorkerCaps...))
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want nothing delivered", payload != nil, err)
			}
			if !assembled {
				t.Fatal("assembly did not succeed before the revocation, so the locked re-check was not what refused")
			}
			p.wantFailedTerminally(r.ID, tc.reason)
		})
	}
	t.Run("a stale finisher settles nothing", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		var genAtHook int64
		assembled := false
		p.svc.claimHooks = &claimTestHooks{
			afterAssembly: func(_ context.Context, run store.Run, payload *ClaimPayload, err error) {
				if run.ID == r.ID && err == nil && payload != nil && payload.Secrets.Provider != nil {
					assembled = true
				}
				genAtHook = mustRun(t, p.env, r.ID).ClaimGeneration
				p.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
				p.env.exec(`UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`, r.ID)
			},
		}
		t.Cleanup(func() { p.svc.claimHooks = nil })
		payload, err := p.claim(p.jobWorker(pjobWorkerCaps...))
		if err != nil || payload != nil {
			t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
		}
		if !assembled {
			t.Fatal("assembly did not succeed before the newer claim generation, so the stale finisher race was not reached")
		}
		got := mustRun(t, p.env, r.ID)
		if got.Status != "claimed" || got.FailOrigin.Valid || got.HoldReason.Valid {
			t.Fatalf("stale finisher settled the run: status=%s origin=%v hold=%v", got.Status, got.FailOrigin, got.HoldReason)
		}
		if got.ClaimGeneration != genAtHook+1 {
			t.Fatalf("claim_generation = %d, want %d: the stale finisher changed the advanced generation", got.ClaimGeneration, genAtHook+1)
		}
	})
}

// TestProviderJobClaimQueuedModelUnmappedLiveDB: a model the frozen map no longer carries (the owner's
// lane default changed while the job queued) settles terminally with the typed reason, once.
func TestProviderJobClaimQueuedModelUnmappedLiveDB(t *testing.T) {
	p := newPJobFix(t)
	r := p.mustCreate()
	p.env.exec(`UPDATE users SET default_claude_model = 'claude-not-in-the-map' WHERE id = $1`, p.userID)
	payload, err := p.claim(p.jobWorker(pjobWorkerCaps...))
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want nothing delivered", payload != nil, err)
	}
	got := p.wantFailedTerminally(r.ID, holdProviderModelUnmapped)
	if n := strings.Count(got.FailureReason.String, holdProviderModelUnmapped); n != 1 {
		t.Fatalf("failure_reason %q carries %s %d times, want once", got.FailureReason.String, holdProviderModelUnmapped, n)
	}
}

// TestProviderJobGatewayLimitFailsTerminallyLiveDB: a provider job's limit report (whatever type or reset
// the worker sent) fails it terminally with the job sentence, no limit columns and no park; a vendor
// job's report keeps the vendor wording; limit_wait itself is refused for any job.
func TestProviderJobGatewayLimitFailsTerminallyLiveDB(t *testing.T) {
	running := func(p *pjobFix, w store.Worker, r store.Run) int64 {
		t.Helper()
		payload, err := p.claim(w)
		if err != nil || payload == nil || payload.RunID != r.ID.String() {
			t.Fatalf("Claim = (%+v, %v)", payload, err)
		}
		gen := payload.ClaimGeneration
		if _, applied, err := p.svc.SetState(p.env.ctx, w, r.ID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
			t.Fatalf("start: applied=%v err=%v", applied, err)
		}
		return gen
	}
	t.Run("provider job with a limit type", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		w := p.jobWorker(pjobWorkerCaps...)
		gen := running(p, w, r)
		// The owner's vendor Anthropic credential (the fixture's usable token) and its limit state: a
		// gateway limit is the provider's, so none of it may move.
		vendorState := func() string {
			var st string
			if err := p.env.pool.QueryRow(p.env.ctx, `SELECT to_jsonb(s)::text || '|' || COALESCE((SELECT jsonb_agg(to_jsonb(l))::text FROM anthropic_rate_limits l WHERE l.user_secret_id = s.id), 'none')
				FROM user_secrets s WHERE s.id = $1 AND s.kind = 'anthropic_token'`, p.token).Scan(&st); err != nil {
				t.Fatalf("read the vendor credential state: %v", err)
			}
			return st
		}
		beforeVendor := vendorState()
		reset := time.Now().Add(5 * time.Hour).UnixMilli()
		got, applied, err := p.svc.SetState(p.env.ctx, w, r.ID, StateRequest{State: "failed", ClaimGeneration: &gen,
			FailureReason: strPtr("worker text"), RateLimitType: strPtr("five_hour"), LimitResetsAt: &reset})
		if err != nil || !applied || got.Status != "failed" {
			t.Fatalf("failed report: status=%s applied=%v err=%v", got.Status, applied, err)
		}
		row := mustRun(t, p.env, r.ID)
		if row.FailureReason.String != providerJobGatewayLimitReason || strings.Contains(row.FailureReason.String, "Anthropic") ||
			row.LimitResetsAt.Valid || row.RetryNotBefore.Valid || row.RateLimitType.Valid || row.LimitWaitCount != 0 {
			t.Fatalf("reason=%q resets=%v retry=%v type=%v waits=%d, want the job wording and NULL limit columns",
				row.FailureReason.String, row.LimitResetsAt, row.RetryNotBefore, row.RateLimitType, row.LimitWaitCount)
		}
		if after := vendorState(); after != beforeVendor {
			t.Fatalf("the vendor credential's limit state moved on a gateway limit:\nbefore %s\nafter  %s", beforeVendor, after)
		}
	})
	t.Run("limit_wait is refused and the job is untouched", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		w := p.jobWorker(pjobWorkerCaps...)
		gen := running(p, w, r)
		_, applied, err := p.svc.SetState(p.env.ctx, w, r.ID, StateRequest{State: "limit_wait", ClaimGeneration: &gen, RateLimitType: strPtr(RateLimitTypeProviderGatewayLimit)})
		if !errors.Is(err, ErrJobNeverParks) || applied {
			t.Fatalf("limit_wait: applied=%v err=%v, want ErrJobNeverParks", applied, err)
		}
		if row := mustRun(t, p.env, r.ID); row.Status != "running" || row.RateLimitType.Valid {
			t.Fatalf("status=%s type=%v, want the running job untouched", row.Status, row.RateLimitType)
		}
	})
	t.Run("vendor job keeps the vendor wording", func(t *testing.T) {
		p := newPJobFix(t)
		p.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, p.userID)
		p.env.exec(`UPDATE user_secrets SET is_default = true WHERE id = $1`, p.token)
		r := p.mustCreate()
		p.env.exec(`UPDATE runs SET wait_on_limit = false WHERE id = $1`, r.ID)
		w := p.jobWorker(jobCap, jobFilesCap)
		gen := running(p, w, r)
		if _, applied, err := p.svc.SetState(p.env.ctx, w, r.ID, StateRequest{State: "failed", ClaimGeneration: &gen, RateLimitType: strPtr("five_hour")}); err != nil || !applied {
			t.Fatalf("failed report: applied=%v err=%v", applied, err)
		}
		if got := mustRun(t, p.env, r.ID).FailureReason.String; got != "Anthropic usage limit (five_hour) reached" {
			t.Fatalf("vendor job reason = %q, want the unchanged vendor wording", got)
		}
	})
}

// countRunInserts counts INSERT statements into runs for this fixture's owner, including ones a later
// rollback undoes: an AFTER INSERT trigger bumps a sequence, and nextval is not transactional. The
// returned func reads the count. The trigger is filtered to this owner, so concurrent tests do not
// add to it, and everything is dropped in cleanup.
func (p *pjobFix) countRunInserts(t *testing.T) func() int64 {
	t.Helper()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	seq, fn, trg := "pjob_ins_seq_"+id, "pjob_ins_fn_"+id, "pjob_ins_trg_"+id
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE SEQUENCE %s`, seq),
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN PERFORM nextval('%s'); RETURN NEW; END $f$`, fn, seq),
		fmt.Sprintf(`CREATE TRIGGER %s AFTER INSERT ON runs FOR EACH ROW WHEN (NEW.user_id = '%s') EXECUTE FUNCTION %s()`, trg, p.userID, fn),
	} {
		if _, err := p.env.pool.Exec(p.env.ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON runs`, trg),
			fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn),
			fmt.Sprintf(`DROP SEQUENCE IF EXISTS %s`, seq),
		} {
			_, _ = p.env.pool.Exec(context.Background(), stmt)
		}
	})
	return func() int64 {
		var n int64
		q := fmt.Sprintf(`SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM %s`, seq)
		if err := p.env.pool.QueryRow(p.env.ctx, q).Scan(&n); err != nil {
			t.Fatalf("read run insert counter: %v", err)
		}
		return n
	}
}
