package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func crossRunExtraGroups(t *testing.T) {
	for _, first := range []bool{true, false} {
		order := map[bool]string{true: "writer_first", false: "discard_first"}[first]
		t.Run("held_final/"+order, func(t *testing.T) { crossRunHeldRace(t, first) })
		for _, kind := range []string{"repo", "connection"} {
			for _, low := range []bool{true, false} {
				t.Run(fmt.Sprintf("delete_%s/%s/witness_low_%t", kind, order, low), func(t *testing.T) { crossRunDeleteRace(t, kind, first, low) })
			}
		}
	}
	for _, low := range []bool{true, false} {
		t.Run(fmt.Sprintf("shared_witness/low_%t", low), func(t *testing.T) { crossRunSharedWitnessRace(t, low) })
	}
	for _, kind := range []string{"late_mr", "late_consumer"} {
		t.Run(kind, func(t *testing.T) { crossRunLateObservation(t, kind) })
	}
}

// crossRunOrderedID returns a fresh ID strictly below (below=true) or above ref
// in byte order: it copies ref's prefix, steps the first adjustable byte and keeps
// a fresh random suffix, so it is unique per test in the shared store-IT database.
// Its order against unrelated runs is not fixed.
func crossRunOrderedID(t *testing.T, ref uuid.UUID, below bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	adjusted := false
	for i := range ref {
		if below && ref[i] > 0x00 {
			copy(id[:i], ref[:i])
			id[i] = ref[i] - 1
			adjusted = true
			break
		}
		if !below && ref[i] < 0xff {
			copy(id[:i], ref[:i])
			id[i] = ref[i] + 1
			adjusted = true
			break
		}
	}
	if !adjusted || (bytes.Compare(id[:], ref[:]) < 0) != below {
		t.Fatalf("cannot order %s against %s (below=%t)", id, ref, below)
	}
	return id
}

// The witness has no custody or other dependants. Choose its ordering before
// taking the verified snapshot, never rewrite a frozen binding. The witness sorts
// strictly below (low) or above the target.
func crossRunOrderWitness(e *inventoryEnv, witness uuid.UUID, b map[string]any, low bool) uuid.UUID {
	id := crossRunOrderedID(e.t, e.run, low)
	e.exec("UPDATE runs SET id=$2 WHERE id=$1", witness, id)
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := e.pool.Exec(ctx, "DELETE FROM runs WHERE id=$1", id); err != nil {
			e.t.Error(err)
		}
	})
	b["witness_run_id"] = id.String()
	return id
}

func crossRunHeldRace(t *testing.T, releaseFirst bool) {
	h := newHeldEnv(t)
	h.w.ProtocolCapabilities = append(h.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
	h.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", h.w.ID, h.w.ProtocolCapabilities)
	e := &inventoryEnv{t: t, ctx: h.ctx, pool: h.pool, q: h.q, svc: h.svc, w: h.w, run: h.run, hold: h.hold, gen: 1}
	finished := time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC)
	created := finished.Add(time.Second)
	h.exec("UPDATE runs SET issue_iid=101,status='cancelled',claim_generation=3,mr_iid=NULL,fail_origin=NULL,finished_at=$2 WHERE id=$1", h.run, finished)
	witness := uuid.New()
	h.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,mr_iid,created_at) VALUES($1,$2,$3,'issue',101,'witness','body','completed',1,7,$4)", witness, h.user, h.repo, created)
	var conn uuid.UUID
	if err := h.pool.QueryRow(h.ctx, "SELECT connection_id FROM repos WHERE id=$1", h.repo).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	b := map[string]any{"repo_id": h.repo.String(), "connection_id": conn.String(), "project_id": int64(1), "forge_type": "github", "base_url": "https://example.com",
		"target_terminal_generation": int64(3), "witness_completed_generation": int64(1), "target_finished_at": finished.Format(time.RFC3339Nano), "witness_created_at": created.Format(time.RFC3339Nano), "verified_at": time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)}
	a, s := crossRunInputs(t, e, witness, b)
	sibling := uuid.New()
	insertMergedHold(e, sibling, h.user, h.repo, h.run, h.w.ID, 3, true)
	r := newCrossRunRace(t)
	g := r.barrier()
	var policyGate *crossRunBarrier
	if !releaseFirst {
		policyGate = g
	} else {
		policyGate = r.barrier()
	}
	policy, _, policyName := r.service(e, policyGate, nil)
	_, releaseDB, releaseName := r.service(e, nil, nil)
	// Keep the forge fake and held-publication configuration of the real service.
	h.svc.pool = releaseDB
	if releaseFirst {
		h.svc.heldPreAckHook = func(tx pgx.Tx) {
			if err := g.pause(r.ctx, tx); err != nil {
				t.Error(err)
			}
		}
	}
	var policyHooks atomic.Int32
	policy.SetCustodySettledHook(func(run uuid.UUID) {
		if run != h.run {
			t.Error("foreign policy hook")
		}
		policyHooks.Add(1)
	})
	pubBefore := crossRunRow(t, e, "run_held_publications", h.pub)
	release := func(ctx context.Context) (apitypes.RecoveryReleaseResponse, error) {
		return h.svc.Release(ctx, h.w, h.run, h.request())
	}
	discard := func(ctx context.Context) (int64, error) { return policy.DiscardCrossRunHolds(ctx, a, s) }
	var rc <-chan reconcileOperation[apitypes.RecoveryReleaseResponse]
	var dc <-chan reconcileOperation[int64]
	if releaseFirst {
		rc = crossRunStart(r, release)
		pid := g.await(t)
		dc = crossRunStart(r, discard)
		waitCrossRunBlock(t, e, policyName, pid)
		g.release()
		got := awaitReconcileOperation(t, rc)
		if got.err != nil {
			t.Fatal(got.err)
		}
		h.wantAck(got.value)
		// Policy cannot change the already released hold or acknowledged publication.
		policyGate.await(t)
		holdAfter := crossRunRow(t, e, "recovery_custody_holds", h.hold)
		pubAfter := crossRunRow(t, e, "run_held_publications", h.pub)
		policyGate.release()
		crossRunWantDiscard(t, dc, 1)
		if holdAfter != crossRunRow(t, e, "recovery_custody_holds", h.hold) || pubAfter != crossRunRow(t, e, "run_held_publications", h.pub) {
			t.Fatal("policy changed released receipt or ACK/TTL")
		}
		var state string
		var ttl float64
		if err := e.pool.QueryRow(r.ctx, "SELECT state,extract(epoch FROM expires_at-acknowledged_at) FROM run_held_publications WHERE id=$1", h.pub).Scan(&state, &ttl); err != nil || state != "acknowledged" || ttl != h.retention.Seconds() {
			t.Fatalf("ACK TTL state=%s ttl=%v err=%v", state, ttl, err)
		}
		h.svc.heldPreAckHook = nil
		replay, err := h.svc.Release(r.ctx, h.w, h.run, h.request())
		if err != nil || !reflect.DeepEqual(got.value, replay) {
			t.Fatalf("lost ACK replay err=%v response changed=%t", err, !reflect.DeepEqual(got.value, replay))
		}
		if holdAfter != crossRunRow(t, e, "recovery_custody_holds", h.hold) || pubAfter != crossRunRow(t, e, "run_held_publications", h.pub) {
			t.Fatal("replay changed receipt or ACK/TTL")
		}
		assertMergedHoldState(t, e, h.hold, "released")
	} else {
		dc = crossRunStart(r, discard)
		pid := g.await(t)
		rc = crossRunStart(r, release)
		waitCrossRunBlock(t, e, releaseName, pid)
		g.release()
		crossRunWantDiscard(t, dc, 2)
		got := awaitReconcileOperation(t, rc)
		if got.err != nil || got.value.Released || !got.value.Retained || got.value.HoldsReleased != 0 || got.value.Reason != HeldReleaseNotEligible {
			t.Fatalf("late FINAL err=%v response=%+v", got.err, got.value)
		}
		if h.lists() == 0 {
			t.Fatal("real FINAL did not perform forge preflight")
		}
		if pubBefore != crossRunRow(t, e, "run_held_publications", h.pub) {
			t.Fatal("late FINAL fabricated ACK or TTL")
		}
		var final *string
		if err := e.pool.QueryRow(r.ctx, "SELECT final_disposition FROM recovery_custody_holds WHERE id=$1", h.hold).Scan(&final); err != nil || final != nil {
			t.Fatalf("late FINAL fabricated receipt: %v", err)
		}
	}
	assertMergedHoldState(t, e, sibling, "discarded")
	if policyHooks.Load() != 1 {
		t.Fatal("policy hook count")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	want := 0
	if releaseFirst {
		want = 1
	}
	if len(h.settled) != want {
		t.Fatalf("release hooks=%d want=%d", len(h.settled), want)
	}
	for _, run := range h.settled {
		if run != h.run {
			t.Error("foreign release hook")
		}
	}
}

func crossRunDeleteRace(t *testing.T, kind string, deleteFirst, low bool) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	w = crossRunOrderWitness(e, w, b, low)
	a, s := crossRunInputs(t, e, w, b)
	e.exec("UPDATE repos SET enabled=false WHERE id=$1", a.RepoID)
	ready := e.reserve("survive-pointer-teardown")
	body := e.upload(ready)
	archive, err := mergedCapture(t, e, ready)
	if err != nil {
		t.Fatal(err)
	}
	r := newCrossRunRace(t)
	g := r.barrier()
	var policyGate *crossRunBarrier
	if !deleteFirst {
		policyGate = g
	}
	policy, _, policyName := r.service(e, policyGate, nil)
	var policyHooks atomic.Int32
	policy.SetCustodySettledHook(func(run uuid.UUID) {
		if run != a.RunID {
			t.Error("foreign deletion policy hook")
		}
		policyHooks.Add(1)
	})
	_, deleteDB, deleteName := r.service(e, nil, nil)
	conn := uuid.MustParse(b["connection_id"].(string))
	deleting := func(ctx context.Context) (int64, error) {
		tx, err := deleteDB.Begin(ctx)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q := store.New(tx)
		var n int64
		if kind == "repo" {
			n, err = q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: a.RepoID, UserID: a.OwnerID})
		} else {
			n, err = q.DeleteForgeConnectionForUser(ctx, store.DeleteForgeConnectionForUserParams{ID: conn, UserID: a.OwnerID})
		}
		if err != nil {
			return n, err
		}
		return n, tx.Commit(ctx)
	}
	discard := func(ctx context.Context) (int64, error) { return policy.DiscardCrossRunHolds(ctx, a, s) }
	var dc, pc <-chan reconcileOperation[int64]
	if deleteFirst {
		// This parent barrier is only a staging point: prove the policy waits on
		// the deleting backend's run locks, rather than on this external locker.
		parent, err := e.pool.Begin(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		r.releases = append(r.releases, func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = parent.Rollback(ctx)
		})
		table, id := "repos", a.RepoID
		if kind == "connection" {
			table, id = "forge_connections", conn
		}
		var parentPID int32
		if err := parent.QueryRow(r.ctx, "SELECT pg_backend_pid()::int FROM "+table+" WHERE id=$1 FOR UPDATE", id).Scan(&parentPID); err != nil {
			t.Fatal(err)
		}
		dc = crossRunStart(r, deleting)
		deletingPID := waitCrossRunBlock(t, e, deleteName, parentPID)
		pc = crossRunStart(r, discard)
		waitCrossRunBlock(t, e, policyName, deletingPID)
		if err := parent.Rollback(r.ctx); err != nil {
			t.Fatal(err)
		}
		got := awaitReconcileOperation(t, dc)
		var pgerr *pgconn.PgError
		if got.value != 0 || !errors.As(got.err, &pgerr) || pgerr.Code != "23503" {
			t.Fatalf("OPEN custody deletion must FK-refuse: n=%d err=%v", got.value, got.err)
		}
		crossRunWantDiscard(t, pc, 1)
	} else {
		pc = crossRunStart(r, discard)
		pid := g.await(t)
		dc = crossRunStart(r, deleting)
		waitCrossRunBlock(t, e, deleteName, pid)
		g.release()
		crossRunWantDiscard(t, pc, 1)
		got := awaitReconcileOperation(t, dc)
		if got.err != nil || got.value != 1 {
			t.Fatalf("settled deletion n=%d err=%v", got.value, got.err)
		}
	}
	assertMergedHoldState(t, e, e.hold, "discarded")
	crossRunAssertAudit(t, e, e.hold, a, s, 1)
	after, err := mergedCapture(t, e, ready)
	// Capture provenance and retention survive parent pointer teardown.
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(archive, after) {
		t.Fatal("deletion changed immutable capture metadata or TTL")
	}
	e.bytes(ready, body)
	if policyHooks.Load() != 1 {
		t.Fatal("deletion policy hook count")
	}
}

func crossRunAssertAudit(t *testing.T, e *inventoryEnv, hold uuid.UUID, a CrossRunTargetBinding, s VerifiedCrossRunWitnessSnapshot, generation int64) {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(e.ctx, "SELECT merged_predecessor_discard_binding FROM recovery_custody_holds WHERE id=$1", hold).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got crossRunDiscardBinding
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := crossRunDiscardBinding{Basis: "same_issue_witness", HoldID: hold, Generation: generation, TargetRunID: a.RunID, TargetTerminalGeneration: a.Generation, TargetTerminalStatus: a.Status,
		WitnessRunID: s.binding.RunID, WitnessCompletedGeneration: s.binding.CompletedGeneration, OwnerID: a.OwnerID, RepoID: a.RepoID, IssueIID: a.IssueIID, TargetFinishedAt: a.FinishedAt,
		WitnessCreatedAt: s.binding.CreatedAt, ConnectionID: s.binding.ConnectionID, ForgeType: s.binding.ForgeType, BaseURL: s.binding.BaseURL, ProjectID: s.binding.ProjectID, WitnessMRIID: s.binding.MRIID, VerifiedAt: s.verifiedAt}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hold %s typed discard audit differs", hold)
	}
}

func crossRunSharedWitnessRace(t *testing.T, low bool) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	w = crossRunOrderWitness(e, w, b, low)
	a, s := crossRunInputs(t, e, w, b)
	// The second target sits on the same side of B as the first, so B sorts
	// strictly below (low) or above both targets.
	second, hold := crossRunOrderedID(t, w, !low), uuid.New()
	if (bytes.Compare(w[:], e.run[:]) < 0) != low || (bytes.Compare(w[:], second[:]) < 0) != low {
		t.Fatalf("witness %s not ordered against targets %s and %s (low=%t)", w, e.run, second, low)
	}
	e.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,finished_at) VALUES($1,$2,$3,'issue',101,'second target','body','cancelled',4,$4)", second, a.OwnerID, a.RepoID, a.FinishedAt)
	insertMergedHold(e, hold, a.OwnerID, a.RepoID, second, e.w.ID, 2, true)
	a2 := a
	a2.RunID, a2.Generation = second, 4
	e2 := &inventoryEnv{t: t, ctx: e.ctx, pool: e.pool, q: e.q, svc: e.svc, w: e.w, run: second, hold: hold, gen: 2}
	partial1, partial2 := e.reserve("shared-B-first"), e2.reserve("shared-B-second")
	for _, id := range []uuid.UUID{partial1, partial2} {
		e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", id, []byte("partial"))
	}
	witnessBefore := crossRunRow(t, e, "runs", w)
	r := newCrossRunRace(t)
	g1, g2 := r.barrier(), r.barrier()
	s1, _, _ := r.service(e, g1, nil)
	s2, _, _ := r.service(e, g2, nil)
	var hooks1, hooks2 atomic.Int32
	s1.SetCustodySettledHook(func(run uuid.UUID) {
		if run != a.RunID {
			t.Error("first hook target")
		}
		hooks1.Add(1)
	})
	s2.SetCustodySettledHook(func(run uuid.UUID) {
		if run != a2.RunID {
			t.Error("second hook target")
		}
		hooks2.Add(1)
	})
	c1 := crossRunStart(r, func(ctx context.Context) (int64, error) { return s1.DiscardCrossRunHolds(ctx, a, s) })
	g1.await(t)
	c2 := crossRunStart(r, func(ctx context.Context) (int64, error) { return s2.DiscardCrossRunHolds(ctx, a2, s) })
	// Both commits must be reached while neither has released its witness SHARE.
	// UPDATE on B would make the second barrier time out.
	g2.await(t)
	g1.release()
	g2.release()
	crossRunWantDiscard(t, c1, 1)
	crossRunWantDiscard(t, c2, 1)
	crossRunAssertAudit(t, e, e.hold, a, s, 1)
	crossRunAssertAudit(t, e, hold, a2, s, 2)
	for _, item := range []struct {
		env *inventoryEnv
		id  uuid.UUID
	}{{e, partial1}, {e2, partial2}} {
		c, err := mergedCapture(t, item.env, item.id)
		chunks, chunkErr := item.env.q.ListCaptureChunks(r.ctx, item.id)
		if err != nil || chunkErr != nil || c.State != "discarded" || len(chunks) != 0 {
			t.Fatalf("shared witness capture %s not fully settled: err=%v chunksErr=%v", item.id, err, chunkErr)
		}
	}
	if witnessBefore != crossRunRow(t, e, "runs", w) {
		t.Fatal("shared witness changed")
	}
	replay, _, _ := r.service(e, nil, nil)
	replay.SetCustodySettledHook(func(uuid.UUID) { t.Error("replay emitted hook") })
	for _, target := range []CrossRunTargetBinding{a, a2} {
		n, err := replay.DiscardCrossRunHolds(r.ctx, target, s)
		if n != 0 || err != nil {
			t.Fatalf("replay n=%d err=%v", n, err)
		}
	}
	if hooks1.Load() != 1 || hooks2.Load() != 1 {
		t.Fatal("target hook counts")
	}
}

func crossRunLateObservation(t *testing.T, kind string) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	a, s := crossRunInputs(t, e, w, b)
	r := newCrossRunRace(t)
	g := r.barrier()
	policy, _, _ := r.service(e, g, nil)
	_, writerDB, writerName := r.service(e, nil, nil)
	var hooks atomic.Int32
	policy.SetCustodySettledHook(func(run uuid.UUID) {
		if run != e.run {
			t.Error("foreign hook")
		}
		hooks.Add(1)
	})
	pc := crossRunStart(r, func(ctx context.Context) (int64, error) { return policy.DiscardCrossRunHolds(ctx, a, s) })
	pid := g.await(t)
	writer := crossRunStart(r, func(ctx context.Context) (int64, error) {
		tx, err := writerDB.Begin(ctx)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var n int64
		if kind == "late_mr" {
			n, err = store.New(tx).ReconcileRunMR(ctx, store.ReconcileRunMRParams{ID: e.run, WorkerID: pgconv.UUID(e.w.ID), MrIid: pgtype.Int8{Int64: 8, Valid: true}})
		} else {
			tag, execErr := tx.Exec(ctx, "INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status) VALUES($1,$2,$3,'issue',101,'late consumer','body','queued')", uuid.New(), a.OwnerID, a.RepoID)
			n, err = tag.RowsAffected(), execErr
		}
		if err != nil {
			return 0, err
		}
		return n, tx.Commit(ctx)
	})
	if kind == "late_mr" {
		waitCrossRunBlock(t, e, writerName, pid)
		g.release()
		crossRunWantDiscard(t, pc, 1)
		got := awaitReconcileOperation(t, writer)
		if got.err != nil || got.value != 1 {
			t.Fatalf("late MR n=%d err=%v", got.value, got.err)
		}
		var mr int64
		if err := e.pool.QueryRow(r.ctx, "SELECT mr_iid FROM runs WHERE id=$1", e.run).Scan(&mr); err != nil || mr != 8 {
			t.Fatalf("late MR=%d err=%v", mr, err)
		}
	} else {
		// Admission commits after the policy's final check while its transaction is
		// still paused; no global consumer fence is part of this policy.
		got := awaitReconcileOperation(t, writer)
		if got.err != nil || got.value != 1 {
			t.Fatalf("late consumer n=%d err=%v", got.value, got.err)
		}
		g.release()
		crossRunWantDiscard(t, pc, 1)
	}
	assertMergedHoldState(t, e, e.hold, "discarded")
	crossRunAssertAudit(t, e, e.hold, a, s, 1)
	if hooks.Load() != 1 {
		t.Fatal("late observation policy hook count")
	}
}
