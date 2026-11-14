package workersvc

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #4208 M1: the dry-run retirement evaluator over a real database. Every case starts from ONE
// positive fixture (a worker hibernated for 22 days with a complete, accepted, valid inventory)
// and flips exactly one fact, so a red test names the single fact that mattered. The evaluator
// never writes, and its best verdict is conditional_retire: there is no would_retire. Skipped
// without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

const (
	retTipA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	retTipB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type retFx struct {
	*invFx
	runA, runB uuid.UUID
}

func sp(s string) *string { return &s }

// retEntry builds one inventory entry; the optional members are set through the helpers below.
func retEntry(kind, path, classification string) InventoryEntry {
	return InventoryEntry{Kind: kind, Path: path, Classification: classification}
}

func receiptEntry(kind, path string, run uuid.UUID, tip, content string) InventoryEntry {
	e := retEntry(kind, path, "needs_receipt")
	e.RunID, e.Tip, e.ContentState = sp(run.String()), sp(tip), sp(content)
	return e
}

func unprovenEntry(kind, path, classification string) InventoryEntry {
	e := retEntry(kind, path, classification)
	e.ContentState = sp("unproven_worktree_bytes")
	return e
}

// goneEntry is what the agent sends for a source whose directory is missing: disposable, a
// source_gone detail and no content state.
func goneEntry(kind, path string) InventoryEntry {
	e := retEntry(kind, path, "disposable")
	e.Detail = sp("source_gone")
	return e
}

// retOpts flips one fact of the positive fixture before the chain is driven.
type retOpts struct {
	entries  func(f *retFx) []InventoryEntry
	docker   func(d *InventoryDocker)
	complete bool
	excl     func(x *InventoryExclusion)
	noDocker bool
}

func (f *retFx) goodEntries() []InventoryEntry {
	return []InventoryEntry{
		receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-1", f.runA, retTipA, "ref"),
		receiptEntry("worktree", "/data/runner/g-r/issue-2", f.runB, retTipB, "content_proven"),
		retEntry("runtime_lock", "/data/runner/.lock", "disposable"),
	}
}

// run inserts a terminal run that owns no worker.
func (f *retFx) run() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
	            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', 1)`, id, f.user, f.repo, nextOutageIID())
	return id
}

// capture records a recovery capture of (run, tip) under a hold that names no live worker.
func (f *retFx) capture(run uuid.UUID, tip, state, expires string, bound bool) {
	f.t.Helper()
	hold := uuid.New()
	f.env.exec(`INSERT INTO recovery_custody_holds (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
	            VALUES ($1,$2,$3,$4,1,'open',$5,'fixture')`, hold, f.user, f.repo, run, uuid.New())
	f.env.exec(`INSERT INTO recovery_captures
	            (id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,expires_at)
	            VALUES ($1,$2,$3,$4,'fixture',$5,$6,$7,$8,now()+$9::interval)`,
		uuid.New(), hold, run, f.user, tip, uuid.NewString(), state, bound, expires)
}

func newRetFx(t *testing.T, opts ...func(*retOpts)) *retFx {
	t.Helper()
	o := retOpts{complete: true}
	for _, m := range opts {
		m(&o)
	}
	f := &retFx{invFx: newInvFx(t)}
	// A 300 second poll interval makes the hold chain stale after 15 minutes, so only the stale
	// case ever sees staleness.
	f.advertise(1, 300, 0)
	f.runA, f.runB = f.run(), f.run()
	f.capture(f.runA, retTipA, "available", "1 hour", true)
	f.capture(f.runB, retTipB, "available", "1 hour", true)
	entries := f.goodEntries()
	if o.entries != nil {
		entries = o.entries(f)
	}
	zero := int64(0)
	three := int64(3)
	req := InventoryRequest{
		OpID: f.op(f.id), RegisterNonce: invNonce, PodUID: f.pod.String(), DeploymentResourceVersion: invRV,
		Complete: &o.complete, Entries: entries,
		Fingerprint: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64),
		Docker:    &InventoryDocker{Containers: []InventoryDockerContainer{}, Volumes: []InventoryDockerVolume{}, ImagesCount: &three, BuildCacheBytes: &zero},
		Exclusion: &InventoryExclusion{Lane: "dind", Mechanism: "epoch_ex+latch+quiescence"},
	}
	for _, p := range f.pvcs() {
		req.PVCs = append(req.PVCs, HibernationPVCRef{Role: p.Role, Name: p.Name, UID: p.UID})
	}
	if o.docker != nil {
		o.docker(req.Docker)
	}
	if o.noDocker {
		req.Docker = nil
	}
	if o.excl != nil {
		o.excl(req.Exclusion)
	}
	rep, err := req.PrepareInventory()
	if err != nil {
		t.Fatalf("PrepareInventory: %v", err)
	}
	f.observed()
	if err := f.accept(rep); err != nil {
		t.Fatalf("accept: %v", err)
	}
	f.must(f.scalingDownT())
	f.must(f.afterExitT(TransitionScaledDown))
	f.want(f.id, "hibernated")
	f.exclusionFinal()
	f.env.exec(`UPDATE workers SET hibernated_at = now() - interval '22 days' WHERE id = $1`, f.id)
	return f
}

// sql flips one stored fact of the worker or its inventory row.
func (f *retFx) sql(q string) {
	f.t.Helper()
	f.env.exec(q, f.id)
}

func (f *retFx) verdict() RetirementVerdict {
	f.t.Helper()
	rep, err := f.svc.EvaluateHibernationRetirement(f.env.ctx)
	if err != nil {
		f.t.Fatalf("EvaluateHibernationRetirement: %v", err)
	}
	for _, v := range rep.Verdicts {
		if v.WorkerID == f.id {
			return v
		}
	}
	f.t.Fatalf("worker %s absent from %d verdicts (total %d)", f.id, len(rep.Verdicts), rep.Total)
	return RetirementVerdict{}
}

func (f *retFx) wantBlocked(code, sourceContains string) {
	f.t.Helper()
	v := f.verdict()
	b, ok := v.FirstBlocker()
	if v.Outcome != RetirementBlocked || !ok || b.Code != code || !strings.Contains(b.Source, sourceContains) {
		f.t.Fatalf("verdict = %+v, want blocked by %s (%s...)", v, code, sourceContains)
	}
}

func TestHibernationRetirementPositiveIsConditionalLiveDB(t *testing.T) {
	f := newRetFx(t)
	v := f.verdict()
	if v.Outcome != RetirementConditionalRetire || v.BlockerCount != 0 {
		t.Fatalf("verdict = %+v, want conditional_retire with no blockers", v)
	}
	if want := []string{"pv_reclaim_policy", "intervening_pod_exclusion"}; !reflect.DeepEqual(v.Unverified, want) {
		t.Fatalf("unverified = %v, want %v", v.Unverified, want)
	}
	if !v.Retain || v.Unknown || len(v.ClassPolicies) != 3 || v.ClassPolicies[2].Role != "dind_data" || v.ClassPolicies[2].Policy != "Retain" {
		t.Fatalf("class policies = %+v retain=%v unknown=%v, want the fixture's three Retain claims flagged", v.ClassPolicies, v.Retain, v.Unknown)
	}
	// The dry run is read-only: a second evaluation of the same rows gives the same verdict.
	if again := f.verdict(); !reflect.DeepEqual(v, again) {
		t.Fatalf("second evaluation differs: %+v vs %+v", v, again)
	}
}

// A source whose directory is gone holds no bytes: the agent reports it disposable with a
// source_gone detail and no content state, and every worktree-like kind is treated alike.
func TestHibernationRetirementGoneSourceIsNotABlockerLiveDB(t *testing.T) {
	for _, kind := range []string{"canonical_clone", "retained_attempt", "worktree"} {
		t.Run(kind, func(t *testing.T) {
			f := newRetFx(t, func(o *retOpts) {
				o.entries = func(f *retFx) []InventoryEntry {
					return append(f.goodEntries(), goneEntry(kind, "/data/runner/g-r/gone"))
				}
			})
			if v := f.verdict(); v.Outcome != RetirementConditionalRetire || v.BlockerCount != 0 {
				t.Fatalf("verdict = %+v, want conditional_retire with no blockers", v)
			}
		})
	}
}

func TestHibernationRetirementGuardsLiveDB(t *testing.T) {
	type guard struct {
		name   string
		code   string
		source string
		opts   []func(*retOpts)
		flip   string // SQL taking the worker id as $1
		fn     func(f *retFx)
	}
	invalid := func(reason string) string {
		return `UPDATE worker_hibernation_inventories SET invalidated_reason = '` + reason + `', invalidated_at = now() WHERE worker_id = $1`
	}
	status := func(s string) string {
		return `UPDATE worker_hibernation_inventories SET status = '` + s + `' WHERE worker_id = $1`
	}
	entries := func(e ...func(f *retFx) InventoryEntry) func(*retOpts) {
		return func(o *retOpts) {
			o.entries = func(f *retFx) []InventoryEntry {
				out := f.goodEntries()
				for _, m := range e {
					out = append(out, m(f))
				}
				return out
			}
		}
	}
	one := func(e InventoryEntry) func(f *retFx) InventoryEntry { return func(*retFx) InventoryEntry { return e } }
	classified := func(class string) InventoryEntry { return retEntry("bare_repo", "/data/runner/g-r/.bare", class) }
	dockerAnon := func(anon bool) func(*retOpts) {
		return func(o *retOpts) {
			o.docker = func(d *InventoryDocker) {
				d.Volumes = []InventoryDockerVolume{{Name: "vol", Anonymous: &anon}}
			}
		}
	}
	guards := []guard{
		{name: "lane without a dind_data volume", code: BlockerLaneWithoutWriterExclusion, flip: `UPDATE worker_hibernation_inventories SET pvcs = '[{"role":"data","name":"d","uid":"u","class_reclaim_policy":"Delete"}]'::jsonb WHERE worker_id = $1`},
		{name: "no_inventory when the row is for another op", code: BlockerNoInventory, flip: `UPDATE worker_hibernation_inventories SET op_id = gen_random_uuid() WHERE worker_id = $1`},
		{name: "timed_out", code: BlockerNoInventory, source: "timed_out", flip: status("timed_out")},
		{name: "no_capability", code: BlockerNoInventory, source: "no_capability", flip: status("no_capability")},
		{name: "closed_by_scale_down", code: BlockerNoInventory, source: "closed_by_scale_down", flip: status("closed_by_scale_down")},
		{name: "unsupported", code: BlockerNoInventory, source: "unsupported", flip: status("unsupported")},
		{name: "exclusion_failed", code: BlockerNoInventory, source: "exclusion_failed", flip: status("exclusion_failed")},
		{name: "busy", code: BlockerNoInventory, source: "busy", flip: status("busy")},
		{name: "identity_changed", code: BlockerNoInventory, source: "identity_changed", flip: status("identity_changed")},
		{name: "incomplete", code: BlockerInventoryIncomplete, opts: []func(*retOpts){func(o *retOpts) { o.complete = false }}},
		{name: "uncovered writer", code: BlockerInventoryIncomplete, source: "uncovered", opts: []func(*retOpts){func(o *retOpts) {
			o.excl = func(x *InventoryExclusion) { x.Uncovered = []string{"plain_lane_writer"} }
		}}},
		{name: "invalid other_pod", code: BlockerInventoryInvalidPrefix + "other_pod", source: "other_pod", flip: invalid("other_pod")},
		{name: "invalid pod_set_unproven", code: BlockerInventoryInvalidPrefix + "pod_set_unproven", flip: invalid("pod_set_unproven")},
		{name: "invalid deployment_changed", code: BlockerInventoryInvalidPrefix + "deployment_changed", flip: invalid("deployment_changed")},
		{name: "invalid observation_gap", code: BlockerInventoryInvalidPrefix + "observation_gap", flip: invalid("observation_gap")},
		{name: "invalid process_restarted", code: BlockerInventoryInvalidPrefix + "process_restarted", flip: invalid("process_restarted")},
		{name: "invalid exclusion_lost", code: BlockerInventoryInvalidPrefix + "exclusion_lost", flip: invalid("exclusion_lost")},
		{name: "invalid written_after_inventory", code: BlockerInventoryInvalidPrefix + "written_after_inventory", flip: invalid("written_after_inventory")},
		{name: "invalid identity_changed", code: BlockerInventoryInvalidPrefix + "identity_changed", flip: invalid("identity_changed")},
		{name: "invalid scale_down_pod_unproven", code: BlockerInventoryInvalidPrefix + "scale_down_pod_unproven", flip: invalid("scale_down_pod_unproven")},
		{name: "no exclusion final", code: BlockerExclusionUntilTermination, flip: `UPDATE worker_hibernation_inventories SET exclusion_final_at = NULL WHERE worker_id = $1`},
		{name: "scale-down pod differs", code: BlockerScaleDownPodUnproven, flip: `UPDATE worker_hibernation_inventories SET scaling_down_pod_uid = gen_random_uuid() WHERE worker_id = $1`},
		{name: "scale-down pod missing", code: BlockerScaleDownPodUnproven, flip: `UPDATE worker_hibernation_inventories SET scaling_down_pod_uid = NULL WHERE worker_id = $1`},
		{name: "hold chain stale", code: BlockerHoldChainStale, flip: `UPDATE worker_hibernation_inventories SET last_held_at = now() - interval '1 hour' WHERE worker_id = $1`},
		{name: "hold chain never recorded", code: BlockerHoldChainStale, flip: `UPDATE worker_hibernation_inventories SET last_held_at = NULL WHERE worker_id = $1`},
		{name: "non-terminal run", code: BlockerNonTerminalRun, fn: func(f *retFx) { seedOutageRun(f.t, f.env, f.user, f.repo, f.id, "queued", "issue", 0, 0) }},
		{name: "manual cordon", code: BlockerManualCordon, flip: `UPDATE workers SET manual_cordoned_at = now(), manual_cordoned_by = user_id, manual_cordoned_by_label = 'a' WHERE id = $1`},
		{name: "draining", code: BlockerDraining, flip: `UPDATE workers SET draining_since = now() WHERE id = $1`},
		{name: "maintenance fenced", code: BlockerMaintenanceFenced, flip: `UPDATE workers SET maintenance_fenced = true WHERE id = $1`},
		{name: "maintenance requested", code: BlockerMaintenancePhase, source: "requested", flip: `UPDATE workers SET maintenance_phase = 'requested' WHERE id = $1`},
		{name: "maintenance ready", code: BlockerMaintenancePhase, source: "ready", flip: `UPDATE workers SET maintenance_phase = 'ready' WHERE id = $1`},
		{name: "maintenance stopping", code: BlockerMaintenancePhase, source: "stopping", flip: `UPDATE workers SET maintenance_phase = 'stopping' WHERE id = $1`},
		{name: "maintenance recycling", code: BlockerMaintenancePhase, source: "recycling", flip: `UPDATE workers SET maintenance_phase = 'recycling' WHERE id = $1`},
		{name: "protected entry", code: BlockerProtectedSource, source: "bare_repo:/data/runner/g-r/.bare", opts: []func(*retOpts){entries(one(classified("protected")))}},
		{name: "unknown entry", code: BlockerProtectedSource, opts: []func(*retOpts){entries(one(classified("unknown")))}},
		{name: "unrecoverable entry", code: BlockerProtectedSource, opts: []func(*retOpts){entries(one(classified("unrecoverable")))}},
		{name: "unrecognised classification", code: BlockerProtectedSource, opts: []func(*retOpts){entries(one(classified("mystery")))}},
		{name: "unrecognised kind", code: BlockerProtectedSource, source: "strange_kind:", opts: []func(*retOpts){entries(one(retEntry("strange_kind", "/x", "disposable")))}},
		{name: "unrecognised content state", code: BlockerProtectedSource, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			return receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-9", f.runA, retTipA, "mystery")
		})}},
		// Example 2: a failed run's tracking ref with no capture at all.
		{name: "needs_receipt without a capture", code: BlockerNoReceipt, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			return receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-3", f.run(), retTipA, "ref")
		})}},
		{name: "needs_receipt with an expired capture", code: BlockerNoReceipt, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			r := f.run()
			f.capture(r, retTipA, "available", "-1 hour", true)
			return receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-4", r, retTipA, "ref")
		})}},
		{name: "needs_receipt with a capture of another tip", code: BlockerNoReceipt, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			r := f.run()
			f.capture(r, retTipB, "available", "1 hour", true)
			return receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-5", r, retTipA, "ref")
		})}},
		{name: "terminal run and released hold but no receipt", code: BlockerNoReceipt, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			r := f.run()
			f.env.exec(`INSERT INTO recovery_custody_holds (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
			            VALUES ($1,$2,$3,$4,1,'released',$5,'fixture')`, uuid.New(), f.user, f.repo, r, uuid.New())
			return receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-6", r, retTipA, "ref")
		})}},
		{name: "unproven worktree bytes", code: BlockerUncoveredWorktreeBytes, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			return receiptEntry("worktree", "/data/runner/g-r/issue-7", f.runA, retTipA, "unproven_worktree_bytes")
		})}},
		{name: "disposable clone without proven content", code: BlockerUncoveredWorktreeBytes, opts: []func(*retOpts){entries(one(retEntry("canonical_clone", "/data/runner/g-r/issue-8", "disposable")))}},
		{name: "unproven bytes on a disposable retained attempt", code: BlockerUncoveredWorktreeBytes, opts: []func(*retOpts){entries(one(unprovenEntry("retained_attempt", "/data/runner/g-r/issue-10.attempt-1", "disposable")))}},
		{name: "unproven bytes on an unknown clone", code: BlockerUncoveredWorktreeBytes, opts: []func(*retOpts){entries(one(unprovenEntry("canonical_clone", "/data/runner/g-r/issue-11", "unknown")))}},
		{name: "a worktree receipt on a ref content state", code: BlockerUncoveredWorktreeBytes, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			return receiptEntry("worktree", "/data/runner/g-r/issue-12", f.runA, retTipA, "ref")
		})}},
		{name: "a retained attempt receipt on a ref content state", code: BlockerUncoveredWorktreeBytes, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			return receiptEntry("retained_attempt", "/data/runner/g-r/issue-13.attempt-1", f.runA, retTipA, "ref")
		})}},
		{name: "a runner ref receipt on a content_proven state", code: BlockerUncoveredWorktreeBytes, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			return receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-14", f.runA, retTipA, "content_proven")
		})}},
		{name: "a capture of another user does not satisfy the receipt", code: BlockerNoReceipt, opts: []func(*retOpts){entries(func(f *retFx) InventoryEntry {
			r, other := f.run(), uuid.New()
			f.env.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, other, "ret-"+other.String()+"@e2e")
			hold := uuid.New()
			f.env.exec(`INSERT INTO recovery_custody_holds (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
			            VALUES ($1,$2,$3,$4,1,'open',$5,'fixture')`, hold, f.user, f.repo, r, uuid.New())
			f.env.exec(`INSERT INTO recovery_captures
			            (id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,expires_at)
			            VALUES ($1,$2,$3,$4,'fixture',$5,$6,'available',true,now()+interval '1 hour')`,
				uuid.New(), hold, r, other, retTipA, uuid.NewString())
			// The fixture cleanup deletes holds only for its own user; drop the foreign capture first.
			f.t.Cleanup(func() {
				f.env.exec(`DELETE FROM recovery_captures WHERE user_id = $1`, other)
				f.env.exec(`DELETE FROM users WHERE id = $1`, other)
			})
			return receiptEntry("runner_ref", "refs/uzi-runner/agent/issue-15", r, retTipA, "ref")
		})}},
		{name: "docker container", code: BlockerDindState, source: "docker:containers", opts: []func(*retOpts){func(o *retOpts) {
			o.docker = func(d *InventoryDocker) {
				d.Containers = []InventoryDockerContainer{{ID: "c0ffee", State: "exited", Name: "build"}}
			}
		}}},
		{name: "docker anonymous volume", code: BlockerDindState, source: "docker:volumes", opts: []func(*retOpts){dockerAnon(true)}},
		{name: "docker named volume", code: BlockerDindState, source: "docker:volumes", opts: []func(*retOpts){dockerAnon(false)}},
		{name: "docker census not reported", code: BlockerDindState, source: "docker:unreported", opts: []func(*retOpts){func(o *retOpts) { o.noDocker = true }}},
	}
	for _, tc := range guards {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetFx(t, tc.opts...)
			if tc.flip != "" {
				f.sql(tc.flip)
			}
			if tc.fn != nil {
				tc.fn(f)
			}
			f.wantBlocked(tc.code, tc.source)
		})
	}
}

// The extra refusals that need their own rows rather than a column flip.
func TestHibernationRetirementExtraRefusalsLiveDB(t *testing.T) {
	t.Run("open custody hold", func(t *testing.T) {
		f := newRetFx(t)
		f.env.exec(`INSERT INTO recovery_custody_holds (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id)
		            VALUES ($1,$2,$3,$4,1,'open',$5,'fixture',$5)`, uuid.New(), f.user, f.repo, f.run(), f.id)
		f.wantBlocked(BlockerOpenCustodyHold, "")
	})
	t.Run("local replica capture", func(t *testing.T) {
		f := newRetFx(t)
		hold, run := uuid.New(), f.run()
		f.env.exec(`INSERT INTO recovery_custody_holds (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
		            VALUES ($1,$2,$3,$4,1,'open',$5,'fixture')`, hold, f.user, f.repo, run, uuid.New())
		f.env.exec(`INSERT INTO recovery_captures (id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,local_replica_worker_id,expires_at)
		            VALUES ($1,$2,$3,$4,'fixture',$5,$6,'available',true,$7,now()+interval '1 hour')`,
			uuid.New(), hold, run, f.user, retTipA, uuid.NewString(), f.id)
		f.wantBlocked(BlockerLocalReplicaCapture, "")
	})
	t.Run("active publication resume custody", func(t *testing.T) {
		f := newRetFx(t)
		run := f.run()
		tx, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(f.env.ctx, `SELECT set_config('uzi.publication_resume_api','v1',true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.env.ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, worker_id, intent, state)
			VALUES ($1, 1, 1, $2, json_build_object('run_id', $3::text), 'active')`, run, f.id, run.String()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		f.wantBlocked(BlockerPublicationResumeCustody, "")
	})
}

// Every guard is counted, the first in the stable order is shown.
func TestHibernationRetirementCountsEveryBlockerLiveDB(t *testing.T) {
	f := newRetFx(t)
	f.sql(`UPDATE worker_hibernation_inventories SET exclusion_final_at = NULL, last_held_at = NULL WHERE worker_id = $1`)
	f.sql(`UPDATE workers SET draining_since = now() WHERE id = $1`)
	v := f.verdict()
	var codes []string
	for _, b := range v.Blockers {
		codes = append(codes, b.Code)
	}
	want := []string{BlockerExclusionUntilTermination, BlockerHoldChainStale, BlockerDraining}
	if v.Outcome != RetirementBlocked || v.BlockerCount != 3 || !reflect.DeepEqual(codes, want) {
		t.Fatalf("verdict = %+v codes=%v, want blocked by %v in that order", v, codes, want)
	}
}

// Example 3: a wake after the inventory was accepted rotates the op, so the accepted row no longer
// belongs to the current op and the retirement is cancelled.
func TestHibernationRetirementCancelledByWakeLiveDB(t *testing.T) {
	f := newRetFx(t)
	before := f.op(f.id)
	if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, f.id, "run queued"); err != nil {
		t.Fatalf("WakeWorker: %v", err)
	}
	f.want(f.id, "waking")
	if f.op(f.id) == before {
		t.Fatal("the wake did not rotate the op")
	}
	if v := f.verdict(); v.Outcome != RetirementCancelled {
		t.Fatalf("verdict = %+v, want cancelled", v)
	}
}

func TestHibernationRetirementNotYetEligibleLiveDB(t *testing.T) {
	f := newRetFx(t)
	f.sql(`UPDATE workers SET hibernated_at = now() - (interval '21 days' - interval '1 hour') WHERE id = $1`)
	v := f.verdict()
	if v.Outcome != RetirementNotYetEligible || v.PastPeriod || v.BlockerCount != 0 || v.Remaining <= 59*time.Minute || v.Remaining > time.Hour+time.Minute {
		t.Fatalf("verdict = %+v, want not_yet_eligible with about 1h left", v)
	}
	// A blocker is still computed and shown, but it does not make the worker blocked.
	f.sql(`UPDATE workers SET draining_since = now() WHERE id = $1`)
	v = f.verdict()
	if b, ok := v.FirstBlocker(); v.Outcome != RetirementNotYetEligible || !ok || b.Code != BlockerDraining {
		t.Fatalf("verdict = %+v, want not_yet_eligible carrying the draining blocker", v)
	}
}

// V23: the evaluator sees only what the api stored. A PVC-mounting pod that existed only BETWEEN two
// held observations leaves no trace in that evidence, so both variants produce the same output and
// intervening_pod_exclusion stays unverified. This test records the hidden pod as metadata only and
// never feeds it to the server; it does not claim the evaluator detects it.
func TestHibernationRetirementBetweenTickPodIsUnobservedLiveDB(t *testing.T) {
	type variant struct {
		name string
		// hiddenPodBetweenObservations is test metadata: it is never given to the service.
		hiddenPodBetweenObservations bool
	}
	var got []RetirementVerdict
	for _, vr := range []variant{{"A: no hidden pod", false}, {"B: pod exists only between two held observations", true}} {
		f := newRetFx(t)
		// Drive the same observable evidence: two held observations, each with no pod.
		f.must(f.afterExitT(TransitionHeld))
		f.must(f.afterExitT(TransitionHeld))
		f.wantValid()
		v := f.verdict()
		v.WorkerID = uuid.Nil
		if v.Outcome != RetirementConditionalRetire || !reflect.DeepEqual(v.Unverified, []string{"pv_reclaim_policy", "intervening_pod_exclusion"}) {
			t.Fatalf("%s: verdict = %+v, want conditional_retire with both facts unverified", vr.name, v)
		}
		_ = vr.hiddenPodBetweenObservations
		got = append(got, v)
	}
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Fatalf("variants differ: %+v vs %+v", got[0], got[1])
	}
}

// Candidates beyond the evaluation bound are counted but not evaluated; the oldest come first.
func TestHibernationRetirementEvaluationBoundLiveDB(t *testing.T) {
	f := newRetFx(t)
	var existing int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM workers WHERE hibernation_state IN ('hibernated','waking') AND kind='hosted' AND NOT ephemeral AND id <> $1`, f.id).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Skipf("%d unrelated hibernated workers share this database", existing)
	}
	// 50 further hibernated workers, newer than the fixture's (22 days) so the fixture is evaluated first.
	f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, hibernation_state, hibernation_op_id, hibernated_at)
	            SELECT gen_random_uuid(), $1, 'bulk-' || n, decode(md5(n::text || clock_timestamp()::text), 'hex'), 'online', 'hosted', 'm', 'base',
	                   'hibernated', gen_random_uuid(), now() - interval '1 day' - n * interval '1 minute'
	            FROM generate_series(1, 50) n`, f.user)
	rep, err := f.svc.EvaluateHibernationRetirement(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 51 || rep.Evaluated != 50 || len(rep.Verdicts) != 50 {
		t.Fatalf("total/evaluated/verdicts = %d/%d/%d, want 51/50/50", rep.Total, rep.Evaluated, len(rep.Verdicts))
	}
	if rep.Verdicts[0].WorkerID != f.id {
		t.Fatalf("the oldest worker is not evaluated first")
	}
	// The newest bulk worker (1 day minus 50 minutes... the shortest hibernation) is the one left out.
	var newest uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT id FROM workers WHERE hibernation_state='hibernated' ORDER BY hibernated_at DESC LIMIT 1`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	for _, v := range rep.Verdicts {
		if v.WorkerID == newest {
			t.Fatal("the newest worker was evaluated")
		}
	}
}
