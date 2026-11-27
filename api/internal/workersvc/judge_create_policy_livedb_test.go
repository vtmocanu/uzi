package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// judge_create_policy_livedb_test.go is PRD #4322 M1a's judge creation policy against a real
// Postgres: a Claude judge is vendor (pinned with a credential, or no Claude source) or bound to the
// owner's Claude source, decided in the create transaction under the shared secret-mutation lock,
// with every owner seeded with a USABLE default vendor token so an unintended vendor fallback would
// succeed and fail the test. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// run via ./e2e/run-store-it.sh. Every test name ends LiveDB so the store-IT sweep selects it.

// jdFix is a provider-sourced owner: source A selected and usable, a usable enabled default
// Anthropic token, a judge-enabled service and a second usable provider B (not selected).
type jdFix struct {
	*provFix
	judge *Service
	altID uuid.UUID // B
	altKy uuid.UUID
}

const pvNoOpusMap = `{"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku"}}`

func newJdFixMap(t *testing.T, modelMap string) *jdFix {
	t.Helper()
	fx := newProvFix(t)
	fx.anthropic()
	fx.addProvider(modelMap, true)
	j := &jdFix{provFix: fx}
	j.judge = judgeEnqueueSvc(fx.env, fx.userID)
	j.judge.p.ProviderAllowedBaseURLs = fx.svc.p.ProviderAllowedBaseURLs
	return j
}

func newJdFix(t *testing.T) *jdFix { return newJdFixMap(t, pvDefaultMap) }

// addAlt seeds provider B with an enabled key (usable, not selected).
func (j *jdFix) addAlt() {
	j.t.Helper()
	j.altID, j.altKy = seedProviderFor(j.t, j.env, j.userID, pvDefaultMap)
}

// selectAltDirect selects B with plain SQL: no lock, the shape of a lock-bypassing writer.
func (j *jdFix) selectAltDirect() {
	j.env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, j.userID, j.altID)
}

// newTarget seeds a completed Claude run of the owner.
func (j *jdFix) newTarget() store.Run {
	j.t.Helper()
	j.iid++
	return seedJudgeTarget(j.t, j.env, j.userID, j.repoID, "claude", 70000+j.iid)
}

// judgeOf returns the target's judge run, if one exists.
func (j *jdFix) judgeOf(target store.Run) (store.Run, bool) {
	j.t.Helper()
	id, _, ok := judgeForTarget(j.t, j.env, target.ID)
	if !ok {
		return store.Run{}, false
	}
	return mustRun(j.t, j.env, id), true
}

// mustJudge fails unless the target has a judge run.
func (j *jdFix) mustJudge(target store.Run) store.Run {
	j.t.Helper()
	r, ok := j.judgeOf(target)
	if !ok {
		j.t.Fatalf("no judge was created for %s", target.ID)
	}
	if r.Kind != "judge" {
		j.t.Fatalf("run kind = %q", r.Kind)
	}
	return r
}

func (j *jdFix) wantNoJudge(target store.Run, why string) {
	j.t.Helper()
	noJudgeAnywhere(j.t, j.env, target.ID, why)
}

func (j *jdFix) wantNoSkip(target store.Run) {
	j.t.Helper()
	if n := judgeSkipCount(j.t, j.env, target.ID); n != 0 {
		j.t.Fatalf("%d judge skip(s) recorded, want none", n)
	}
}

// wantBoundTo asserts a provider-bound Claude judge on (provider, key) with no vendor credential.
func (j *jdFix) wantBoundTo(r store.Run, provider, key uuid.UUID, revision int32) {
	j.t.Helper()
	if !r.ModelProviderID.Valid || uuid.UUID(r.ModelProviderID.Bytes) != provider || !r.ModelProviderRevision.Valid || r.ModelProviderRevision.Int32 != revision ||
		r.ModelProviderKeyMode.String != "user_key" || !r.ModelProviderKeySecretID.Valid || uuid.UUID(r.ModelProviderKeySecretID.Bytes) != key ||
		!r.ModelProviderKeyRev.Valid {
		j.t.Fatalf("judge binding = %+v, want provider %s rev %d key %s", bindingOfRun(r), provider, revision, key)
	}
	if r.Harness != "claude" || r.AnthropicSecretID.Valid || r.CodexSecretID.Valid || r.CredentialOverrideSecretID.Valid {
		j.t.Fatalf("a provider judge is claude with no vendor credential: %+v", r)
	}
}

// auto runs the automatic enqueue for target.
func (j *jdFix) auto(target store.Run) { j.judge.maybeEnqueueJudge(j.env.ctx, target) }

// manual runs the owner's rerun for target.
func (j *jdFix) manual(target store.Run) (store.Run, error) {
	return j.judge.RerunJudge(j.env.ctx, j.userID, false, target.ID)
}

func (j *jdFix) userState() (provider pgtype.UUID, mode string, pin pgtype.UUID) {
	j.t.Helper()
	if err := j.env.pool.QueryRow(j.env.ctx, `SELECT claude_provider_id, judge_anthropic_bind_mode, judge_anthropic_secret_id FROM users WHERE id = $1`, j.userID).Scan(&provider, &mode, &pin); err != nil {
		j.t.Fatalf("read user state: %v", err)
	}
	return
}

// waiters counts requests queued behind a holder of THIS user's secret-mutation advisory lock, by
// lock key alone (no backend filter).
func (j *jdFix) waiters() int {
	j.t.Helper()
	var c int
	if err := j.aux.QueryRow(j.env.ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
		AND classid::bigint = ($1::bigint & 4294967295) AND objid::bigint = ($2::bigint & 4294967295)`,
		int64(store.SecretMutationLockClass), int64(store.SecretMutationLockObjID(j.userID))).Scan(&c); err != nil {
		j.t.Fatalf("count lock waiters: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------------------------
// Coherence under the shared secret-mutation lock: real-lock writers.
// ---------------------------------------------------------------------------------------------

// TestJudgeCreatePolicyMixedReadRaceLiveDB: a judge decides under the shared secret-mutation lock
// and holds it to commit, so a writer of the pin policy or of the Claude source (both take the
// exclusive lock) queues behind the create and commits after it. After the decision (default policy,
// source A) the hook starts the real writer, waits until it is observed queued on the lock key, checks
// it has not finished, and returns. The judge commits bound to A with no vendor credential; the writer
// commits afterwards, and only the NEXT judge sees its write.
func TestJudgeCreatePolicyMixedReadRaceLiveDB(t *testing.T) {
	type subcase struct {
		name  string
		write func(ctx context.Context, j *jdFix) error
		// after asserts the writer's committed state and the next judge's lane.
		after func(t *testing.T, j *jdFix)
	}
	cases := []subcase{
		{
			name: "pin",
			// The production judge-binding writer: withSecretMutation (exclusive lock) then
			// SetUserJudgeAnthropicBinding; its post-commit promoter request runs inline.
			write: func(ctx context.Context, j *jdFix) error {
				w := New(j.env.q, j.env.box, testParams())
				w.SetTxBeginner(j.env.pool)
				w.background = func(fn func()) { fn() }
				tok := j.token
				_, err := w.SetUserJudgeBinding(ctx, j.userID, BindModePinned, &tok, nil)
				return err
			},
			after: func(t *testing.T, j *jdFix) {
				provider, mode, pin := j.userState()
				if mode != BindModePinned || !pin.Valid || uuid.UUID(pin.Bytes) != j.token || !provider.Valid || uuid.UUID(provider.Bytes) != j.provider {
					t.Fatalf("final state: mode=%q pin=%v source=%v, want pinned to %s with source A %s", mode, pin, provider, j.token, j.provider)
				}
				next := j.newTarget()
				j.auto(next)
				j.wantVendor(j.mustJudge(next))
			},
		},
		{
			name: "source",
			// The PUT's transaction shape (handler/providers_source.go inside withSecretLock): exclusive
			// lock, provider row FOR SHARE, visibility check, source write, usability read-back, commit.
			write: func(ctx context.Context, j *jdFix) error {
				tx, err := j.aux.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '20s'`); err != nil {
					return err
				}
				q := store.New(tx)
				if err := q.LockSecretMutation(ctx, j.userID); err != nil {
					return err
				}
				if _, err := q.LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: j.altID, OwnerUserID: j.userID}); err != nil {
					return err
				}
				if _, err := q.GetVisibleProviderForUse(ctx, store.GetVisibleProviderForUseParams{ID: j.altID, UserID: j.userID}); err != nil {
					return err
				}
				if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{Engine: "claude", ProviderID: pgtype.UUID{Bytes: j.altID, Valid: true}, ID: j.userID}); err != nil {
					return err
				}
				res, err := ResolveProviderSource(ctx, q, j.userID, "claude", j.judge.providerAllow)
				if err != nil {
					return err
				}
				if err := res.Err(); err != nil {
					return err
				}
				return tx.Commit(ctx)
			},
			after: func(t *testing.T, j *jdFix) {
				provider, mode, _ := j.userState()
				if !provider.Valid || uuid.UUID(provider.Bytes) != j.altID || mode == BindModePinned {
					t.Fatalf("final state: source=%v mode=%q, want source B %s and no pin", provider, mode, j.altID)
				}
				next := j.newTarget()
				j.auto(next)
				j.wantBoundTo(j.mustJudge(next), j.altID, j.altKy, 1)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := newJdFix(t)
			j.addAlt()
			target := j.newTarget()

			finished := make(chan struct{})
			var writerErr error
			var hookErr error
			var hookRan bool
			j.judge.judgeDecidedHook = func(ctx context.Context) {
				hookRan = true
				wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				go func() {
					defer cancel()
					writerErr = tc.write(wctx, j)
					close(finished)
				}()
				deadline := time.Now().Add(10 * time.Second)
				for j.waiters() < 1 {
					select {
					case <-finished:
						hookErr = fmt.Errorf("the writer finished (%v) before it was seen queued behind the judge's shared lock", writerErr)
						return
					default:
					}
					if time.Now().After(deadline) {
						hookErr = errors.New("the writer was never seen queued behind the judge's shared lock")
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				select {
				case <-finished:
					hookErr = fmt.Errorf("the writer finished (%v) while the judge still held the shared lock", writerErr)
				default:
				}
			}

			judge, err := j.manual(target)
			if !hookRan {
				t.Fatal("the decision hook never ran")
			}
			if hookErr != nil {
				select {
				case <-finished:
				case <-time.After(30 * time.Second):
				}
				t.Fatalf("hook: %v (create err = %v)", hookErr, err)
			}
			if err != nil {
				t.Fatalf("judge create: %v", err)
			}
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Fatal("the writer did not finish after the judge committed")
			}
			if writerErr != nil {
				t.Fatalf("writer: %v", writerErr)
			}
			// The judge decided before the writer's commit and stays frozen to A with no vendor id.
			j.wantBoundTo(mustRun(t, j.env, judge.ID), j.provider, j.key, 1)
			j.judge.judgeDecidedHook = nil
			tc.after(t, j)
		})
	}
}

// TestJudgeCreatePolicyLockBeforeReadLiveDB: the judge reads its policy only AFTER it holds the shared
// secret-mutation lock. The owner starts with no Claude source (an unlocked read would decide vendor
// and nothing re-checks the vendor branch). A source PUT for B holds the exclusive lock, uncommitted;
// the judge is started and observed queued on the lock key; the writer then commits. The judge must
// commit provider-bound to B, which is only possible if it read the policy after acquiring the lock.
func TestJudgeCreatePolicyLockBeforeReadLiveDB(t *testing.T) {
	for _, path := range []string{"manual", "automatic"} {
		t.Run(path, func(t *testing.T) {
			j := newJdFix(t)
			j.addAlt()
			j.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, j.userID)
			target := j.newTarget()
			ctx := j.env.ctx

			tx, err := j.aux.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatalf("begin writer: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			q := store.New(tx)
			if err := q.LockSecretMutation(ctx, j.userID); err != nil {
				t.Fatalf("writer lock: %v", err)
			}
			if _, err := q.LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: j.altID, OwnerUserID: j.userID}); err != nil {
				t.Fatalf("lock provider: %v", err)
			}
			if _, err := q.GetVisibleProviderForUse(ctx, store.GetVisibleProviderForUseParams{ID: j.altID, UserID: j.userID}); err != nil {
				t.Fatalf("visible provider: %v", err)
			}
			if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{Engine: "claude", ProviderID: pgtype.UUID{Bytes: j.altID, Valid: true}, ID: j.userID}); err != nil {
				t.Fatalf("set source: %v", err)
			}
			res, err := ResolveProviderSource(ctx, q, j.userID, "claude", j.judge.providerAllow)
			if err != nil {
				t.Fatalf("resolve source: %v", err)
			}
			if err := res.Err(); err != nil {
				t.Fatalf("source unusable: %v", err)
			}

			done := make(chan error, 1)
			go func() {
				if path == "manual" {
					_, err := j.manual(target)
					done <- err
					return
				}
				j.auto(target)
				done <- nil
			}()

			deadline := time.Now().Add(10 * time.Second)
			for j.waiters() < 1 {
				select {
				case err := <-done:
					t.Fatalf("the judge returned (%v) before it was seen queued on the shared lock", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the judge was never seen queued on the shared lock")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit writer: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("judge create: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the judge did not finish after the writer committed")
			}
			j.wantBoundTo(j.mustJudge(target), j.altID, j.altKy, 1)
		})
	}
}

// TestJudgeCreatePolicyChangedDetectionLiveDB: defence in depth for a writer that bypasses the lock
// (direct SQL in the hook, after the decision). The locked re-reads catch it: a changed pin policy or
// a source that froze to another provider is ErrJudgePolicyChanged (no vendor fallback); a source
// that turned unusable is ErrProviderSourceUnusable first (precedence). Nothing is created either way,
// the automatic path records no skip for a changed policy.
func TestJudgeCreatePolicyChangedDetectionLiveDB(t *testing.T) {
	type bypass struct {
		name string
		// prepare runs before the create; write is the bypassing commit in the hook.
		prepare func(j *jdFix)
		write   func(j *jdFix)
		want    error
		// skip is the reason the automatic path records, "" for none.
		skip string
	}
	for _, tc := range []bypass{
		{
			name: "pin only",
			write: func(j *jdFix) {
				j.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, j.userID, j.token)
			},
			want: ErrJudgePolicyChanged,
		},
		{
			name:  "source only, to a usable provider",
			write: func(j *jdFix) { j.selectAltDirect() },
			want:  ErrJudgePolicyChanged,
		},
		{
			name:    "source only, to an unusable provider",
			prepare: func(j *jdFix) { j.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, j.altID) },
			write:   func(j *jdFix) { j.selectAltDirect() },
			want:    ErrProviderSourceUnusable,
			skip:    JudgeSkipProviderSourceUnusable,
		},
	} {
		for _, path := range []string{"manual", "automatic"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				j := newJdFix(t)
				j.addAlt()
				if tc.prepare != nil {
					tc.prepare(j)
				}
				ran := false
				j.judge.judgeDecidedHook = func(context.Context) { ran = true; tc.write(j) }
				target := j.newTarget()
				if path == "manual" {
					_, err := j.manual(target)
					if !errors.Is(err, tc.want) {
						t.Fatalf("err = %v, want %v", err, tc.want)
					}
					if tc.want == ErrProviderSourceUnusable && errors.Is(err, ErrJudgePolicyChanged) {
						t.Fatalf("an unusable source must win over the policy comparison: %v", err)
					}
				} else {
					j.auto(target)
					if tc.skip == "" {
						j.wantNoSkip(target)
					} else {
						wantSkip(t, j.env, target.ID, "claude", tc.skip)
					}
				}
				if !ran {
					t.Fatal("the decision hook never ran")
				}
				j.wantNoJudge(target, "a changed policy must never fall back to a vendor judge")
			})
		}
	}
}

// TestJudgeCreateSourceClearedLiveDB: the source is cleared (by a writer bypassing the lock) between
// the decision and the freeze; the locked read sees no source and the create is refused as an
// unusable source, never a vendor judge although a usable default token exists.
func TestJudgeCreateSourceClearedLiveDB(t *testing.T) {
	for _, path := range []string{"manual", "automatic"} {
		t.Run(path, func(t *testing.T) {
			j := newJdFix(t)
			ran := false
			j.judge.judgeDecidedHook = func(context.Context) {
				ran = true
				j.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, j.userID)
			}
			target := j.newTarget()
			if path == "manual" {
				_, err := j.manual(target)
				if !isUnusable(err, SourceUnusableProviderMissing) {
					t.Fatalf("err = %v, want provider_source_unusable (provider_missing)", err)
				}
			} else {
				j.auto(target)
				wantSkip(t, j.env, target.ID, "claude", JudgeSkipProviderSourceUnusable)
			}
			if !ran {
				t.Fatal("the decision hook never ran")
			}
			j.wantNoJudge(target, "a cleared source")
		})
	}
}

// ---------------------------------------------------------------------------------------------
// The vendor branch and the pin.
// ---------------------------------------------------------------------------------------------

// TestJudgeCreateVendorBranchLiveDB: a vendor decision creates the vendor judge exactly as before
// (no binding), and its token outcomes are unchanged on both paths: no default token row is the
// no_usable_credential skip (automatic) and ErrNoAnthropicToken (manual), an all-disabled token is the
// credential_disabled skip and the typed disabled refusal, and the refused create leaves no judge.
func TestJudgeCreateVendorBranchLiveDB(t *testing.T) {
	clearSource := func(j *jdFix) {
		j.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, j.userID)
	}
	t.Run("no source creates the vendor judge", func(t *testing.T) {
		j := newJdFix(t)
		clearSource(j)
		a := j.newTarget()
		j.auto(a)
		j.wantVendor(j.mustJudge(a))
		b := j.newTarget()
		judge, err := j.manual(b)
		if err != nil {
			t.Fatalf("manual: %v", err)
		}
		j.wantVendor(mustRun(t, j.env, judge.ID))
		j.wantNoSkip(a)
		j.wantNoSkip(b)
	})
	t.Run("no default token", func(t *testing.T) {
		j := newJdFix(t)
		clearSource(j)
		j.env.exec(`DELETE FROM user_secrets WHERE id = $1`, j.token)
		a := j.newTarget()
		j.auto(a)
		j.wantNoJudge(a, "no token")
		wantSkip(t, j.env, a.ID, "claude", JudgeSkipNoUsableCredential)
		b := j.newTarget()
		if _, err := j.manual(b); !errors.Is(err, ErrNoAnthropicToken) {
			t.Fatalf("manual err = %v, want ErrNoAnthropicToken", err)
		}
		j.wantNoJudge(b, "no token")
		j.wantNoSkip(b) // a rerun refusal is shown synchronously and records nothing
	})
	t.Run("all tokens disabled", func(t *testing.T) {
		j := newJdFix(t)
		clearSource(j)
		disableSecret(j.env, j.token, false)
		a := j.newTarget()
		j.auto(a)
		j.wantNoJudge(a, "disabled token")
		wantSkip(t, j.env, a.ID, "claude", JudgeSkipCredentialDisabled)
		b := j.newTarget()
		_, err := j.manual(b)
		var jhe *JudgeHarnessError
		if !errors.Is(err, ErrHarnessCredentialDisabled) || !errors.As(err, &jhe) || jhe.Harness != HarnessClaude {
			t.Fatalf("manual err = %v, want the typed disabled refusal naming the claude judge", err)
		}
		j.wantNoJudge(b, "disabled token")
	})
	t.Run("a provider judge never needs a vendor token", func(t *testing.T) {
		j := newJdFix(t)
		j.env.exec(`DELETE FROM user_secrets WHERE id = $1`, j.token)
		a := j.newTarget()
		j.auto(a)
		j.wantBoundTo(j.mustJudge(a), j.provider, j.key, 1)
		b := j.newTarget()
		judge, err := j.manual(b)
		if err != nil || errors.Is(err, ErrNoAnthropicToken) {
			t.Fatalf("manual err = %v, want a provider judge without any vendor token", err)
		}
		j.wantBoundTo(mustRun(t, j.env, judge.ID), j.provider, j.key, 1)
	})
}

// TestProviderJudgePinSemanticsLiveDB: the judge pin decides before the source. Pinned with a
// credential is a vendor judge whether the source is usable or unusable (never refused, never
// bound); pinned with a NULL pointer resolves as default and is provider-bound.
func TestProviderJudgePinSemanticsLiveDB(t *testing.T) {
	pin := func(j *jdFix) {
		tok := j.token
		if _, err := j.judge.SetUserJudgeBinding(j.env.ctx, j.userID, BindModePinned, &tok, nil); err != nil {
			t.Fatalf("pin: %v", err)
		}
	}
	for _, tc := range []struct {
		name     string
		prepare  func(j *jdFix)
		wantBnd  bool
		retireIt bool
	}{
		{name: "pinned with a pointer despite a usable source", prepare: pin},
		{name: "pinned with a pointer despite an unusable source", prepare: func(j *jdFix) {
			pin(j)
			j.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, j.provider)
		}},
		{name: "pinned with a NULL pointer is provider-bound", wantBnd: true, prepare: func(j *jdFix) {
			j.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = NULL WHERE id = $1`, j.userID)
		}},
		{name: "auto mode is provider-bound", wantBnd: true, prepare: func(j *jdFix) {
			if _, err := j.judge.SetUserJudgeBinding(j.env.ctx, j.userID, BindModeAuto, nil, nil); err != nil {
				t.Fatalf("auto: %v", err)
			}
		}},
	} {
		for _, path := range []string{"automatic", "manual"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				j := newJdFix(t)
				tc.prepare(j)
				target := j.newTarget()
				var judge store.Run
				if path == "automatic" {
					j.auto(target)
					judge = j.mustJudge(target)
				} else {
					created, err := j.manual(target)
					if err != nil {
						t.Fatalf("manual: %v", err)
					}
					judge = mustRun(t, j.env, created.ID)
				}
				if tc.wantBnd {
					j.wantBoundTo(judge, j.provider, j.key, 1)
				} else {
					j.wantVendor(judge)
				}
				j.wantNoSkip(target)
			})
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Atomicity: visibility, model rejection, gate recheck.
// ---------------------------------------------------------------------------------------------

// TestJudgeCreateAtomicVisibilityLiveDB: a reader on another connection never sees the judge row
// between its INSERT and the COMMIT, and afterwards sees all five binding values together.
func TestJudgeCreateAtomicVisibilityLiveDB(t *testing.T) {
	for _, path := range []string{"manual", "automatic"} {
		t.Run(path, func(t *testing.T) {
			j := newJdFix(t)
			target := j.newTarget()
			hookRan := false
			var seen int
			j.judge.judgeAfterInsertHook = func(ctx context.Context) {
				hookRan = true
				if err := j.aux.QueryRow(ctx, `SELECT count(*) FROM runs WHERE kind = 'judge' AND target_run_id = $1`, target.ID).Scan(&seen); err != nil {
					t.Errorf("reader: %v", err)
				}
			}
			if path == "manual" {
				if _, err := j.manual(target); err != nil {
					t.Fatalf("create: %v", err)
				}
			} else {
				j.auto(target)
			}
			if !hookRan {
				t.Fatal("the after-insert hook never ran")
			}
			if seen != 0 {
				t.Fatalf("a concurrent reader saw %d judge row(s) before COMMIT", seen)
			}
			var p, k pgtype.UUID
			var rev pgtype.Int4
			var mode pgtype.Text
			var krev pgtype.Int8
			if err := j.aux.QueryRow(j.env.ctx, `SELECT model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev
				FROM runs WHERE kind = 'judge' AND target_run_id = $1`, target.ID).Scan(&p, &rev, &mode, &k, &krev); err != nil {
				t.Fatalf("post-commit reader: %v", err)
			}
			if !p.Valid || uuid.UUID(p.Bytes) != j.provider || rev.Int32 != 1 || !rev.Valid || mode.String != "user_key" || !k.Valid || uuid.UUID(k.Bytes) != j.key ||
				!krev.Valid || krev.Int64 != j.keyRev() {
				t.Fatalf("post-commit binding = %v %v %v %v %v", p, rev, mode, k, krev)
			}
		})
	}
}

// TestJudgeCreateModelRejectRollbackLiveDB: a source whose map does not map the judge model refuses
// the create at the binding check, after the INSERT: typed unmapped error, no judge row, the skip
// row the create deleted is restored (manual) or replaced by the new reason (automatic), and the
// same owner with a mapped judge model gets the judge.
func TestJudgeCreateModelRejectRollbackLiveDB(t *testing.T) {
	t.Run("manual", func(t *testing.T) {
		j := newJdFixMap(t, pvNoOpusMap)
		target := j.newTarget()
		j.judge.recordJudgeSkip(j.env.ctx, target.ID, HarnessClaude, JudgeSkipNoUsableCredential)
		hookRan := false
		j.judge.judgeAfterInsertHook = func(context.Context) { hookRan = true }
		_, err := j.manual(target)
		var um *ProviderModelUnmappedError
		if !errors.As(err, &um) || um.Model != "opus" || um.Source != "judge" {
			t.Fatalf("err = %v, want ProviderModelUnmappedError{opus, judge}", err)
		}
		if !hookRan {
			t.Fatal("the judge was never inserted, so this did not exercise the rollback")
		}
		j.wantNoJudge(target, "an unmapped judge model")
		wantSkip(t, j.env, target.ID, "claude", JudgeSkipNoUsableCredential)
	})
	t.Run("automatic", func(t *testing.T) {
		j := newJdFixMap(t, pvNoOpusMap)
		target := j.newTarget()
		j.judge.recordJudgeSkip(j.env.ctx, target.ID, HarnessClaude, JudgeSkipNoUsableCredential)
		hookRan := false
		j.judge.judgeAfterInsertHook = func(context.Context) { hookRan = true }
		j.auto(target)
		if !hookRan {
			t.Fatal("the judge was never inserted, so this did not exercise the rollback")
		}
		j.wantNoJudge(target, "an unmapped judge model")
		wantSkip(t, j.env, target.ID, "claude", JudgeSkipProviderModelUnmapped)
	})
	t.Run("a mapped user judge model creates the judge", func(t *testing.T) {
		j := newJdFixMap(t, pvNoOpusMap)
		j.env.exec(`UPDATE users SET judge_model = 'sonnet' WHERE id = $1`, j.userID)
		target := j.newTarget()
		if _, err := j.manual(target); err != nil {
			t.Fatalf("create with a mapped model: %v", err)
		}
		j.wantBoundTo(j.mustJudge(target), j.provider, j.key, 1)
	})
}

// TestJudgeCreateGateRecheckRollbackLiveDB: the target moved to another repository after the gate was
// taken (the judgeBeforeLockHook technique of TestTerminalConsumerGateJudgeIdentityLiveDB), on a
// provider-branch create at both origins: the recheck refuses, no judge exists and the skip row the
// create deleted is restored.
func TestJudgeCreateGateRecheckRollbackLiveDB(t *testing.T) {
	for _, path := range []string{"manual", "automatic"} {
		t.Run(path, func(t *testing.T) {
			j := newJdFix(t)
			target := j.newTarget()
			other := seedOtherRepo(t, j.env, j.repoID)
			j.judge.recordJudgeSkip(j.env.ctx, target.ID, HarnessClaude, JudgeSkipNoUsableCredential)
			ran := false
			j.judge.judgeBeforeLockHook = func(context.Context) {
				ran = true
				j.env.exec(`UPDATE runs SET repo_id = $2 WHERE id = $1`, target.ID, other)
			}
			if path == "manual" {
				if _, err := j.manual(target); !errors.Is(err, ErrTerminalConsumerGateIdentity) {
					t.Fatalf("err = %v, want ErrTerminalConsumerGateIdentity", err)
				}
			} else {
				j.auto(target)
			}
			if !ran {
				t.Fatal("the provider branch never reached the target lock")
			}
			j.wantNoJudge(target, "a failed gate recheck")
			wantSkip(t, j.env, target.ID, "claude", JudgeSkipNoUsableCredential)
		})
	}
}

// TestJudgeSkipProviderReasonsCommittedLiveDB: each new provider skip reason is committed by the
// automatic refusal, read back exactly, one row per target.
func TestJudgeSkipProviderReasonsCommittedLiveDB(t *testing.T) {
	t.Run("provider_source_unusable", func(t *testing.T) {
		j := newJdFix(t)
		j.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, j.provider)
		target := j.newTarget()
		j.auto(target)
		wantSkip(t, j.env, target.ID, "claude", JudgeSkipProviderSourceUnusable)
		if n := judgeSkipCount(t, j.env, target.ID); n != 1 {
			t.Fatalf("judge_skips rows = %d", n)
		}
	})
	t.Run("provider_model_unmapped", func(t *testing.T) {
		j := newJdFixMap(t, pvNoOpusMap)
		target := j.newTarget()
		j.auto(target)
		wantSkip(t, j.env, target.ID, "claude", JudgeSkipProviderModelUnmapped)
		if n := judgeSkipCount(t, j.env, target.ID); n != 1 {
			t.Fatalf("judge_skips rows = %d", n)
		}
	})
	t.Run("a codex source no longer records provider_lane_unsupported", func(t *testing.T) {
		c := newCjFix(t)
		target := c.newTarget("codex")
		c.auto(target)
		c.wantBoundCodex(c.mustJudge(target), c.provider, c.key)
		c.wantNoSkip(target)
	})
	t.Run("codex provider_source_unusable", func(t *testing.T) {
		c := newCjFix(t)
		c.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, c.provider)
		target := c.newTarget("codex")
		c.auto(target)
		noJudgeAnywhere(t, c.env, target.ID, "a retired Codex source")
		wantSkip(t, c.env, target.ID, "codex", JudgeSkipProviderSourceUnusable)
		if n := judgeSkipCount(t, c.env, target.ID); n != 1 {
			t.Fatalf("judge_skips rows = %d", n)
		}
	})
}

// ---------------------------------------------------------------------------------------------
// Codex provider judge creation (PRD #4574 M1).
// ---------------------------------------------------------------------------------------------

// cjFix is a Codex-sourced owner: usable vendor Codex default (so an unintended vendor fallback
// would succeed and fail the test), a selected valid Codex source, judge runs on Codex, and a
// judge-enabled service that allows the provider's URL. No Anthropic token.
type cjFix struct {
	t        *testing.T
	env      codexTestEnv
	userID   uuid.UUID
	repoID   uuid.UUID
	svc      *Service
	provider uuid.UUID
	key      uuid.UUID
	iid      int64
}

func newCjFix(t *testing.T) *cjFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := judgeEnqueueSvc(env, userID)
	svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	setJudgeRunsOn(env, userID, "codex")
	c := &cjFix{t: t, env: env, userID: userID, repoID: repoID, svc: svc, iid: 90000}
	c.provider, c.key = seedCodexProviderKeyFor(t, env, userID)
	c.selectSource(c.provider)
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id = NULL, codex_provider_scope_id = NULL WHERE id = $1`, userID)
	})
	return c
}

func (c *cjFix) selectSource(provider uuid.UUID) {
	c.t.Helper()
	c.env.exec(`UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = (SELECT scope_owner_id FROM model_providers WHERE id = $2) WHERE id = $1`, c.userID, provider)
}

// seedCodexProviderWithMap seeds a Codex provider (revision 1) with the given model map and an
// enabled key, without selecting it.
func (c *cjFix) seedCodexProviderWithMap(modelMap string) (provider, key uuid.UUID) {
	c.t.Helper()
	p, err := c.env.q.InsertModelProvider(c.env.ctx, store.InsertModelProviderParams{OwnerUserID: c.userID, Name: "cj-" + uuid.NewString()[:8], Engine: harnessCodex})
	if err != nil {
		c.t.Fatalf("insert provider: %v", err)
	}
	if err := c.env.q.InsertModelProviderRevision(c.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(modelMap),
	}); err != nil {
		c.t.Fatalf("insert revision: %v", err)
	}
	k, err := c.env.q.InsertProviderKeySecret(c.env.ctx, store.InsertProviderKeySecretParams{
		UserID: c.userID, Label: p.ID.String(), Ciphertext: []byte("ct"), SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true},
	})
	if err != nil {
		c.t.Fatalf("insert key: %v", err)
	}
	return p.ID, k.ID
}

func (c *cjFix) newTarget(harness string) store.Run {
	c.t.Helper()
	c.iid++
	return seedJudgeTarget(c.t, c.env, c.userID, c.repoID, harness, c.iid)
}

func (c *cjFix) auto(target store.Run) { c.svc.maybeEnqueueJudge(c.env.ctx, target) }

func (c *cjFix) manual(target store.Run) (store.Run, error) {
	return c.svc.RerunJudge(c.env.ctx, c.userID, false, target.ID)
}

func (c *cjFix) mustJudge(target store.Run) store.Run {
	c.t.Helper()
	id, _, ok := judgeForTarget(c.t, c.env, target.ID)
	if !ok {
		c.t.Fatalf("no judge was created for %s", target.ID)
	}
	return mustRun(c.t, c.env, id)
}

func (c *cjFix) wantNoSkip(target store.Run) {
	c.t.Helper()
	if n := judgeSkipCount(c.t, c.env, target.ID); n != 0 {
		c.t.Fatalf("%d judge skip(s) recorded, want none", n)
	}
}

// wantBoundCodex asserts a provider-bound Codex judge on (provider, key): all five binding columns,
// harness codex, and no vendor credential or Codex authority.
func (c *cjFix) wantBoundCodex(r store.Run, provider, key uuid.UUID) {
	c.t.Helper()
	if r.Kind != "judge" || r.Harness != "codex" {
		c.t.Fatalf("judge kind=%q harness=%q, want a codex judge", r.Kind, r.Harness)
	}
	if !r.ModelProviderID.Valid || uuid.UUID(r.ModelProviderID.Bytes) != provider || !r.ModelProviderRevision.Valid || r.ModelProviderRevision.Int32 != 1 ||
		r.ModelProviderKeyMode.String != "user_key" || !r.ModelProviderKeySecretID.Valid || uuid.UUID(r.ModelProviderKeySecretID.Bytes) != key ||
		!r.ModelProviderKeyRev.Valid {
		c.t.Fatalf("judge binding = %+v, want provider %s rev 1 key %s", bindingOfRun(r), provider, key)
	}
	if r.CodexSecretID.Valid || r.CodexAuthMode.Valid || r.AnthropicSecretID.Valid || r.CredentialOverrideSecretID.Valid {
		c.t.Fatalf("a Codex provider judge carries vendor authority: codex=%v auth=%v anthropic=%v override=%v",
			r.CodexSecretID, r.CodexAuthMode, r.AnthropicSecretID, r.CredentialOverrideSecretID)
	}
}

// TestCodexProviderJudgeCreateLiveDB: a Codex judge for an owner with a selected, usable Codex
// source is bound to it at creation, on the automatic and the manual path, with no vendor Codex
// credential and no skip. The owner also holds a usable vendor Codex default, so a vendor fallback
// would create a different row and fail the test.
func TestCodexProviderJudgeCreateLiveDB(t *testing.T) {
	t.Run("automatic", func(t *testing.T) {
		c := newCjFix(t)
		target := c.newTarget("codex")
		c.auto(target)
		c.wantBoundCodex(c.mustJudge(target), c.provider, c.key)
		c.wantNoSkip(target)
	})
	t.Run("manual", func(t *testing.T) {
		c := newCjFix(t)
		target := c.newTarget("codex")
		created, err := c.manual(target)
		if err != nil {
			t.Fatalf("manual: %v", err)
		}
		c.wantBoundCodex(mustRun(t, c.env, created.ID), c.provider, c.key)
		c.wantNoSkip(target)
	})
	t.Run("claude target judged on codex", func(t *testing.T) {
		c := newCjFix(t)
		target := c.newTarget("claude")
		c.auto(target)
		c.wantBoundCodex(c.mustJudge(target), c.provider, c.key)
	})
	t.Run("unset source is the vendor codex judge", func(t *testing.T) {
		c := newCjFix(t)
		c.env.exec(`UPDATE users SET codex_provider_id = NULL, codex_provider_scope_id = NULL WHERE id = $1`, c.userID)
		target := c.newTarget("codex")
		c.auto(target)
		r := c.mustJudge(target)
		if b := bindingOfRun(r); b != (bindingParams{}) {
			t.Fatalf("a vendor Codex judge carries a provider binding: %+v", b)
		}
		if r.Harness != "codex" || !r.CodexSecretID.Valid {
			t.Fatalf("vendor judge harness=%q codex secret=%v, want codex with its own credential", r.Harness, r.CodexSecretID)
		}
	})
	t.Run("an anthropic judge pin change does not affect the codex judge", func(t *testing.T) {
		c := newCjFix(t)
		pin := c.env.seedAnthropicSecret(t, c.userID, "pin-"+uuid.NewString(), false)
		c.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, c.userID, pin)
		target := c.newTarget("codex")
		c.auto(target)
		c.wantBoundCodex(c.mustJudge(target), c.provider, c.key)
		manualTarget := c.newTarget("codex")
		created, err := c.manual(manualTarget)
		if err != nil {
			t.Fatalf("manual with an Anthropic pin: %v", err)
		}
		c.wantBoundCodex(mustRun(t, c.env, created.ID), c.provider, c.key)
	})
	// The pin DRIFTS inside creation: it is written between the lane decision and the locked re-read,
	// so the decision and the re-read disagree on the Anthropic pin. The Codex lane ignores it and
	// still freezes the Codex source.
	for _, path := range []string{"manual", "automatic"} {
		t.Run("an anthropic judge pin drifting during creation does not affect the codex judge/"+path, func(t *testing.T) {
			c := newCjFix(t)
			pin := c.env.seedAnthropicSecret(t, c.userID, "pin-"+uuid.NewString(), false)
			ran := false
			c.svc.judgeDecidedHook = func(context.Context) {
				ran = true
				c.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, c.userID, pin)
			}
			target := c.newTarget("codex")
			var judge store.Run
			if path == "manual" {
				created, err := c.manual(target)
				if err != nil {
					t.Fatalf("manual with a drifting Anthropic pin: %v", err)
				}
				judge = mustRun(t, c.env, created.ID)
			} else {
				c.auto(target)
				c.wantNoSkip(target)
				judge = c.mustJudge(target)
			}
			if !ran {
				t.Fatal("the decision hook never ran")
			}
			c.wantBoundCodex(judge, c.provider, c.key)
		})
	}
}

// TestCodexProviderJudgeFrozenAcrossSourceChangeLiveDB: the owner re-selects another Codex source
// while the judge is queued; the judge keeps its frozen binding, and only the next judge binds the
// new source.
func TestCodexProviderJudgeFrozenAcrossSourceChangeLiveDB(t *testing.T) {
	c := newCjFix(t)
	target := c.newTarget("codex")
	c.auto(target)
	judge := c.mustJudge(target)
	c.wantBoundCodex(judge, c.provider, c.key)

	other, otherKey := c.seedCodexProviderWithMap(`{"default":{"model":"other"}}`)
	c.selectSource(other)
	c.wantBoundCodex(mustRun(t, c.env, judge.ID), c.provider, c.key)

	next := c.newTarget("codex")
	c.auto(next)
	c.wantBoundCodex(c.mustJudge(next), other, otherKey)
}

// TestCodexProviderJudgeUnusableSourceLiveDB: a retired, key-less or invalid-map Codex source
// creates no judge on either path and never falls back to the vendor Codex credential the owner
// holds. The automatic path records provider_source_unusable (never provider_model_unmapped, a
// Claude-only reason); the manual path is the typed ErrProviderSourceUnusable.
func TestCodexProviderJudgeUnusableSourceLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *cjFix)
		reason string
	}{
		{"retired", func(c *cjFix) {
			c.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, c.provider)
		}, SourceUnusableRetired},
		{"key disabled", func(c *cjFix) {
			c.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, c.key)
		}, SourceUnusableKeyDisabled},
		{"model map without a default", func(c *cjFix) {
			p, _ := c.seedCodexProviderWithMap(`{"gpt-6-sol":{"model":"y"}}`)
			c.selectSource(p)
		}, SourceUnusableInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCjFix(t)
			tc.mutate(c)
			auto := c.newTarget("codex")
			c.auto(auto)
			noJudgeAnywhere(t, c.env, auto.ID, "an unusable Codex source")
			wantSkip(t, c.env, auto.ID, "codex", JudgeSkipProviderSourceUnusable)
			manual := c.newTarget("codex")
			if _, err := c.manual(manual); !isUnusable(err, tc.reason) {
				t.Fatalf("manual err = %v, want provider_source_unusable (%s)", err, tc.reason)
			}
			noJudgeAnywhere(t, c.env, manual.ID, "an unusable Codex source")
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Shared definitions (creation side).
// ---------------------------------------------------------------------------------------------

// TestProviderJudgeSharedDefinitionLiveDB: a judge on an admin-shared per-user-key definition binds
// the shared revision and the RECIPIENT's own key (automatic and manual), a recipient without a key
// is refused with no borrow of another user's key and no vendor fallback, an edit gives a fresh judge
// the new revision while the existing judge keeps its frozen one, and a retired definition refuses a
// fresh judge. (The claim-side half is the next milestone's.)
func TestProviderJudgeSharedDefinitionLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	plainB := "gw-key-B-" + uuid.NewString()
	fx.useShared(plainB) // B: the shared definition selected with B's own key
	j := &jdFix{provFix: fx}
	j.judge = judgeEnqueueSvc(fx.env, fx.userID)
	j.judge.p.ProviderAllowedBaseURLs = fx.svc.p.ProviderAllowedBaseURLs

	// C selected the same definition and has NO key, though C holds a usable vendor token.
	c := fx.env.seedUser(t)
	fx.env.seedAnthropicSecret(t, c, "c-default-"+uuid.NewString(), true)
	selectClaudeSource(t, fx.env, c, fx.provider)
	cSvc := judgeEnqueueSvc(fx.env, c)
	cSvc.p.ProviderAllowedBaseURLs = fx.svc.p.ProviderAllowedBaseURLs
	cTarget := func() store.Run {
		fx.iid++
		return seedJudgeTarget(t, fx.env, c, fx.repoID, "claude", 71000+fx.iid)
	}

	first := j.newTarget()
	j.auto(first)
	firstJudge := j.mustJudge(first)
	j.wantBoundTo(firstJudge, fx.provider, fx.key, 1)
	second := j.newTarget()
	created, err := j.manual(second)
	if err != nil {
		t.Fatalf("manual on a shared definition: %v", err)
	}
	j.wantBoundTo(mustRun(t, fx.env, created.ID), fx.provider, fx.key, 1)

	// C: refused key_missing at both origins; no judge, no vendor fallback, B's key never borrowed.
	ct := cTarget()
	cSvc.maybeEnqueueJudge(fx.env.ctx, ct)
	noJudgeAnywhere(t, fx.env, ct.ID, "a recipient without a key")
	wantSkip(t, fx.env, ct.ID, "claude", JudgeSkipProviderSourceUnusable)
	ct2 := cTarget()
	if _, err := cSvc.RerunJudge(fx.env.ctx, c, false, ct2.ID); !isUnusable(err, SourceUnusableKeyMissing) {
		t.Fatalf("C manual err = %v, want provider_source_unusable (key_missing)", err)
	}
	noJudgeAnywhere(t, fx.env, ct2.ID, "a recipient without a key")

	// C attaches their own key: the judge binds C's key, never B's.
	keyC := seedSealedKey(t, fx.env, c, fx.provider, "gw-key-C-"+uuid.NewString())
	ct3 := cTarget()
	cSvc.maybeEnqueueJudge(fx.env.ctx, ct3)
	id, _, ok := judgeForTarget(t, fx.env, ct3.ID)
	if !ok {
		t.Fatal("C with a key got no judge")
	}
	if got := mustRun(t, fx.env, id); uuid.UUID(got.ModelProviderKeySecretID.Bytes) != keyC || !got.ModelProviderKeySecretID.Valid {
		t.Fatalf("C's judge froze key %v, want C's %s", got.ModelProviderKeySecretID, keyC)
	}

	// An admin edit (revision 2): the existing judge keeps revision 1, a fresh judge freezes revision 2.
	rev, err := fx.env.q.BumpSharedModelProviderRevision(fx.env.ctx, fx.provider)
	if err != nil {
		t.Fatalf("bump shared: %v", err)
	}
	if err := fx.env.q.InsertModelProviderRevision(fx.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: fx.provider, Revision: rev, Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer",
		ModelMap: []byte(`{"opus":{"model":"edited-opus"},"sonnet":{"model":"edited-sonnet"},"haiku":{"model":"edited-haiku"}}`),
	}); err != nil {
		t.Fatalf("insert edited revision: %v", err)
	}
	j.wantBoundTo(mustRun(t, fx.env, firstJudge.ID), fx.provider, fx.key, 1)
	third := j.newTarget()
	j.auto(third)
	j.wantBoundTo(j.mustJudge(third), fx.provider, fx.key, rev)
	fourth := j.newTarget()
	created, err = j.manual(fourth)
	if err != nil {
		t.Fatalf("manual after the edit: %v", err)
	}
	j.wantBoundTo(mustRun(t, fx.env, created.ID), fx.provider, fx.key, rev)

	// Retired: a fresh judge is refused (automatic and manual), the frozen ones are untouched.
	if n, err := fx.env.q.RetireSharedModelProvider(fx.env.ctx, fx.provider); err != nil || n != 1 {
		t.Fatalf("retire shared = (%d, %v)", n, err)
	}
	fifth := j.newTarget()
	j.auto(fifth)
	j.wantNoJudge(fifth, "a retired definition")
	wantSkip(t, fx.env, fifth.ID, "claude", JudgeSkipProviderSourceUnusable)
	sixth := j.newTarget()
	if _, err := j.manual(sixth); !isUnusable(err, SourceUnusableRetired) {
		t.Fatalf("manual after retire err = %v, want retired", err)
	}
	j.wantNoJudge(sixth, "a retired definition")
	j.wantBoundTo(mustRun(t, fx.env, firstJudge.ID), fx.provider, fx.key, 1)
}

// ---------------------------------------------------------------------------------------------
// Codex judge for a Claude-source owner.
// ---------------------------------------------------------------------------------------------

// TestCodexJudgeClaudeSourceOwnerLiveDB: an owner whose only source is a Claude provider still gets
// a vendor Codex judge (automatic and manual) with their own Codex credential; setting a Codex source
// then binds the Codex judge to it on both paths (PRD #4574 M1).
func TestCodexJudgeClaudeSourceOwnerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env) // usable vendor Codex default, no Anthropic token
	svc := judgeEnqueueSvc(env, userID)
	svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	setJudgeRunsOn(env, userID, "codex")
	provider, _ := seedProviderFor(t, env, userID, pvDefaultMap)
	env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, userID, provider)
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL, codex_provider_id = NULL, codex_provider_scope_id = NULL WHERE id = $1`, userID)
	})

	auto := seedJudgeTarget(t, env, userID, repoID, "claude", 9821)
	svc.maybeEnqueueJudge(env.ctx, auto)
	id, h, ok := judgeForTarget(t, env, auto.ID)
	if !ok || h != string(HarnessCodex) {
		t.Fatalf("automatic: want a Codex judge for a Claude-source owner, got ok=%v harness=%q", ok, h)
	}
	if b := bindingOfRun(mustRun(t, env, id)); b != (bindingParams{}) {
		t.Fatalf("a vendor Codex judge carries a provider binding: %+v", b)
	}
	manual := seedJudgeTarget(t, env, userID, repoID, "claude", 9822)
	created, err := svc.RerunJudge(env.ctx, userID, false, manual.ID)
	if err != nil || created.Harness != string(HarnessCodex) {
		t.Fatalf("manual: judge=%+v err=%v, want a Codex judge", created, err)
	}
	if b := bindingOfRun(mustRun(t, env, created.ID)); b != (bindingParams{}) {
		t.Fatalf("a vendor Codex judge carries a provider binding: %+v", b)
	}

	// A Codex source binds the Codex judge on both paths, with no vendor Codex credential.
	codexProvider, codexKey := seedCodexProviderKeyFor(t, env, userID)
	env.exec(`UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = id WHERE id = $1`, userID, codexProvider)
	wantBound := func(r store.Run) {
		t.Helper()
		b := bindingOfRun(r)
		if r.Harness != "codex" || !b.ProviderID.Valid || uuid.UUID(b.ProviderID.Bytes) != codexProvider || !b.KeySecret.Valid ||
			uuid.UUID(b.KeySecret.Bytes) != codexKey || r.CodexSecretID.Valid || r.CodexAuthMode.Valid {
			t.Fatalf("judge = harness %q binding %+v codex secret %v, want a Codex judge bound to %s", r.Harness, b, r.CodexSecretID, codexProvider)
		}
	}
	boundAuto := seedJudgeTarget(t, env, userID, repoID, "claude", 9823)
	svc.maybeEnqueueJudge(env.ctx, boundAuto)
	autoID, _, ok := judgeForTarget(t, env, boundAuto.ID)
	if !ok {
		t.Fatal("automatic: want a Codex judge bound to the Codex source")
	}
	wantBound(mustRun(t, env, autoID))
	if n := judgeSkipCount(t, env, boundAuto.ID); n != 0 {
		t.Fatalf("automatic: %d judge skip(s) recorded, want none", n)
	}
	boundManual := seedJudgeTarget(t, env, userID, repoID, "claude", 9824)
	created, err = svc.RerunJudge(env.ctx, userID, false, boundManual.ID)
	if err != nil {
		t.Fatalf("manual err = %v, want a Codex judge bound to the Codex source", err)
	}
	wantBound(mustRun(t, env, created.ID))
}
