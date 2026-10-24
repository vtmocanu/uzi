package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This composition is deliberately outside LiveDB$: the normal seven-package
// sweep must not acquire a Node dependency. Opting in makes every dependency fatal.
func TestTaskCheckpointCrossWorkerRecoveryE2E(t *testing.T) {
	if os.Getenv("UZI_TASK_CHECKPOINT_RECOVERY_E2E") != "1" {
		t.Skip("opt in with UZI_TASK_CHECKPOINT_RECOVERY_E2E=1 via e2e/run-store-it.sh")
	}
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Fatal("combined proof requires UZI_TEST_DATABASE_URL")
	}
	for _, exe := range []string{"node", "git"} {
		if _, err := exec.LookPath(exe); err != nil {
			t.Fatalf("required %s: %v", exe, err)
		}
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(root, "agent")
	deps := exec.Command("node", "--import", "tsx", "-e", "process.stdout.write('tsx-ready')")
	deps.Dir = agent
	if out, err := deps.CombinedOutput(); err != nil || string(out) != "tsx-ready" {
		t.Fatalf("required node/tsx: %s %v", out, err)
	}
	env := setupCodexLiveDB(t)
	t.Run("creation-hooks", func(t *testing.T) { taskCheckpointCreation(t, env, root) })
	for _, mode := range []string{"successful-write", "first-write-refused", "lost-ack-after-requeue"} {
		t.Run(mode, func(t *testing.T) { taskCheckpointTransfer(t, env, root, agent, mode) })
	}
}

// Each child is created by the production terminal hooks, not reconstructed SQL.
func taskCheckpointCreation(t *testing.T, env codexTestEnv, repoRoot string) {
	f := newCustodyContinuationFixture(t, env)
	root, err := f.svc.CreateTaskRun(env.ctx, f.userID, f.repoID, "checkpoint proof", "main", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DispatchTaskRun(env.ctx, f.userID, root.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.SetForgeBaseURLAllowed(func(s string) bool { return s == "https://forge.e2e" })
	scratch, err := os.MkdirTemp(filepath.Join(repoRoot, ".uzi/scratch"), "task-creation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch); err != nil {
			t.Error(err)
		}
	})
	origin := filepath.Join(scratch, "origin.git")
	taskCheckpointGit(t, scratch, "init", "--bare", "--initial-branch=main", origin)
	taskCheckpointGit(t, origin, "config", "http.receivepack", "true")
	tree := taskCheckpointGit(t, origin, "mktree")
	base := taskCheckpointGit(t, origin, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit-tree", tree, "-m", "base")
	taskCheckpointGit(t, origin, "update-ref", "refs/heads/main", base)
	tip := taskCheckpointGit(t, origin, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit-tree", tree, "-p", base, "-m", "saved work")
	packCmd := exec.Command("git", "-C", origin, "pack-objects", "--stdout", "--revs") // #nosec G204 -- fixed Git command and test-owned origin directory; no shell
	packCmd.Env = taskCheckpointEnv()
	packCmd.Stdin = strings.NewReader(tip + "\n")
	pack, err := packCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(taskCheckpointSmartHTTP(origin))
	defer remote.Close()
	var publishingID string
	f.svc.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		o.CloneURL = remote.URL + "/origin.git"
		if o.WorkBranch != "uzi/task/"+root.ID.String() || o.CheckpointKey != "uzi/task/"+publishingID {
			t.Fatalf("broker received wrong inherited coordinates: %+v", o)
		}
		result, err := pushbroker.Publish(ctx, o)
		wantRef := "refs/uzi-checkpoints/uzi/task/" + publishingID
		if err == nil && result.Ref != wantRef {
			t.Fatalf("broker actual destination=%q want literal %q", result.Ref, wantRef)
		}
		return result, err
	})
	publish := func(id uuid.UUID, claim *ClaimPayload) {
		t.Helper()
		publishingID = id.String()
		result, err := f.svc.Publish(env.ctx, f.wkr, id, tip, pack, &claim.ClaimGeneration)
		if err != nil || !result.Published {
			t.Fatalf("publish %s: %+v %v", id, result, err)
		}
		if got := taskCheckpointGit(t, origin, "rev-parse", "refs/uzi-checkpoints/uzi/task/"+id.String()); got != tip {
			t.Fatalf("literal exact ref for %s=%s want=%s", id, got, tip)
		}
	}
	a := taskCheckpointClaim(t, f, f.wkr, root.ID)
	publish(root.ID, a)
	env.exec("UPDATE runs SET status='completed',finished_at=now() WHERE id=$1", root.ID)
	f.svc.maybeEnqueueTaskReview(env.ctx, mustRun(t, env, root.ID))
	reviewID, _ := firstTaskRunForReviewTarget(t, env, root.ID)
	b := taskCheckpointClaim(t, f, f.wkr, reviewID)
	publish(reviewID, b)
	if b.ReviewTargetRunID == nil || *b.ReviewTargetRunID != root.ID.String() {
		t.Fatal("review claim lost read-only task review policy")
	}
	env.exec("UPDATE runs SET status='completed',finished_at=now() WHERE id=$1", reviewID)
	seedCompleteTaskReview(t, env, root.ID, reviewID, f.userID)
	f.svc.maybeEnqueueThenFix(env.ctx, mustRun(t, env, reviewID))
	fixID, _ := firstTaskRunForThenFixOriginal(t, env, root.ID)
	c := taskCheckpointClaim(t, f, f.wkr, fixID)
	publish(fixID, c)
	keys := map[string]bool{}
	for _, p := range []*ClaimPayload{a, b, c} {
		pair := p.CheckpointCoordinates
		if pair.WorkBranch != root.Branch.String || pair.CheckpointKey != "uzi/task/"+p.RunID || keys[pair.CheckpointKey] {
			t.Fatalf("inherited branch / distinct exact key: %+v", pair)
		}
		keys[pair.CheckpointKey] = true
	}
}

func taskCheckpointClaim(t *testing.T, f *custodyContinuationFixture, worker store.Worker, id uuid.UUID) *ClaimPayload {
	t.Helper()
	caps := []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1}
	f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker.ID, caps)
	p, err := f.svc.Claim(f.env.ctx, worker, nil)
	if err != nil || p == nil || p.RunID != id.String() || p.CheckpointCoordinates == nil || p.CheckpointWriteUnresolved == nil {
		t.Fatalf("production Claim missing exact run/descriptor/evidence: %+v err=%v", p, err)
	}
	return p
}

func taskCheckpointTransfer(t *testing.T, env codexTestEnv, root, agent, mode string) {
	f := newCustodyContinuationFixture(t, env)
	f.svc.SetForgeBaseURLAllowed(func(s string) bool { return s == "https://forge.e2e" })
	run, err := f.svc.CreateTaskRun(env.ctx, f.userID, f.repoID, "recover committed and dirty bytes", "main", false, mode == "successful-write", mode == "successful-write", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DispatchTaskRun(env.ctx, f.userID, run.ID); err != nil {
		t.Fatal(err)
	}
	first := taskCheckpointClaim(t, f, f.wkr, run.ID)
	if mode == "successful-write" {
		rootRun := run
		env.exec("UPDATE runs SET status='completed',finished_at=now() WHERE id=$1", rootRun.ID)
		f.svc.maybeEnqueueTaskReview(env.ctx, mustRun(t, env, rootRun.ID))
		reviewID, _ := firstTaskRunForReviewTarget(t, env, rootRun.ID)
		taskCheckpointClaim(t, f, f.wkr, reviewID)
		env.exec("UPDATE runs SET status='completed',finished_at=now() WHERE id=$1", reviewID)
		seedCompleteTaskReview(t, env, rootRun.ID, reviewID, f.userID)
		f.svc.maybeEnqueueThenFix(env.ctx, mustRun(t, env, reviewID))
		fixID, _ := firstTaskRunForThenFixOriginal(t, env, rootRun.ID)
		run = mustRun(t, env, fixID)
		first = taskCheckpointClaim(t, f, f.wkr, run.ID)
		if first.CheckpointCoordinates.WorkBranch != "uzi/task/"+rootRun.ID.String() ||
			first.CheckpointCoordinates.CheckpointKey != "uzi/task/"+run.ID.String() ||
			first.CheckpointCoordinates.WorkBranch == first.CheckpointCoordinates.CheckpointKey {
			t.Fatal("successful transfer must use inherited root branch and current child key")
		}
	}
	scratch, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "task-checkpoint-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch); err != nil {
			t.Error(err)
		}
	})
	// Normalize only our private parent; never chmod/chgrp the shared scratch root.
	if err := os.Chown(scratch, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scratch, 0700); err != nil { // #nosec G302 -- private MkdirTemp directory used as TMPDIR needs owner traversal
		t.Fatal(err)
	}
	origin := filepath.Join(scratch, "origin.git")
	cfg := map[string]any{"scratch": scratch, "origin": origin, "claim": first, "mode": mode}
	node := func(t *testing.T, action string) string {
		t.Helper()
		preserve := action == "recover" && (cfg["mode"] == "unknown" || cfg["mode"] == "absent" || cfg["mode"] == "mismatch" || cfg["mode"] == "unrelated")
		var authority, attempts, refs string
		attemptSnapshot := func() string {
			t.Helper()
			var value string
			if err := env.pool.QueryRow(env.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY id),'[]'::jsonb)::text
 FROM checkpoint_publish_attempts a WHERE run_id=$1`, run.ID).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		if preserve {
			var open int
			if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open'", run.ID).Scan(&open); err != nil || open == 0 {
				t.Fatalf("refusal fixture must protect open custody: %d %v", open, err)
			}
			authority = coordinateAuthoritySnapshot(t, env, run.ID)
			attempts = attemptSnapshot()
			refs = taskCheckpointGit(t, origin, "for-each-ref", "--format=%(refname) %(objectname)")
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(scratch, "config.json")
		if err := os.WriteFile(config, data, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(env.ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "test/fixtures/task-checkpoint-recovery-e2e.ts", action, config) // #nosec G204 -- fixed Node fixture, test-defined action and private config path; no shell
		cmd.Dir = agent
		cmd.Env = append(taskCheckpointEnv(), "TMPDIR="+scratch)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("node %s mode=%v: %v\n%s", action, cfg["mode"], err, out)
		}
		if preserve && (coordinateAuthoritySnapshot(t, env, run.ID) != authority || attemptSnapshot() != attempts ||
			taskCheckpointGit(t, origin, "for-each-ref", "--format=%(refname) %(objectname)") != refs) {
			t.Fatal("recovery refusal changed custody, frozen authority, pending evidence or remote refs")
		}
		t.Logf("node %s: %s", action, out)
		return string(out)
	}
	node(t, "init")
	var receive atomic.Int32
	var requeued atomic.Bool
	backend := taskCheckpointSmartHTTP(origin)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			receive.Add(1)
			if mode == "lost-ack-after-requeue" && requeued.CompareAndSwap(false, true) {
				// The command is already admitted and on the wire. Requeue BEFORE the
				// backend applies it, then lose its ACK at the transport seam.
				if _, err := env.pool.Exec(r.Context(), "UPDATE runs SET status='queued' WHERE id=$1", run.ID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				recorder := httptest.NewRecorder()
				backend.ServeHTTP(recorder, r)
				http.Error(w, "lost receive-pack acknowledgement", http.StatusBadGateway)
				return
			}
		}
		backend.ServeHTTP(w, r)
	}))
	defer remote.Close()
	cfg["url"] = remote.URL + "/origin.git"
	node(t, "prepare")
	var state struct{ Tip, Base, Committed, Marker string }
	data, err := os.ReadFile(filepath.Join(scratch, "state.json")) // #nosec G304 -- fixed fixture filename in the test-owned private MkdirTemp directory
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	pack, err := os.ReadFile(filepath.Join(scratch, "checkpoint.pack")) // #nosec G304 -- fixed fixture filename in the test-owned private MkdirTemp directory
	if err != nil {
		t.Fatal(err)
	}
	if mode == "first-write-refused" {
		// A known ancestry refusal happens before receive-pack. Generic hook
		// rejection text is deliberately NOT proof that an attempted write is absent.
		tree := taskCheckpointGit(t, origin, "rev-parse", state.Base+"^{tree}")
		continuation := taskCheckpointGit(t, origin, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
			"commit-tree", tree, "-p", state.Base, "-m", "competing branch before first checkpoint")
		taskCheckpointGit(t, origin, "update-ref", "refs/heads/"+first.CheckpointCoordinates.WorkBranch, continuation)
		cfg["continuation_tip"] = continuation
	}
	f.svc.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		o.CloneURL = remote.URL + "/origin.git" // Only the destination changes; auth and policy stay production.
		return pushbroker.Publish(ctx, o)
	})
	result, publishErr := f.svc.Publish(env.ctx, f.wkr, run.ID, state.Tip, pack, &first.ClaimGeneration)
	wantReceive := int32(1)
	if mode == "first-write-refused" {
		wantReceive = 0
		var attempts int
		if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", run.ID).Scan(&attempts); err != nil || attempts != 0 ||
			publishErr != nil || result.Published || result.Skipped != "not_descendant" {
			t.Fatalf("definite ancestry refusal: result=%+v attempts=%d query=%v publish=%v", result, attempts, err, publishErr)
		}
	}
	if receive.Load() != wantReceive {
		t.Fatalf("owned HTTP receive-pack requests=%d want=%d; publish=%+v err=%v", receive.Load(), wantReceive, result, publishErr)
	}
	persisted := mustRun(t, env, run.ID)
	if len(persisted.CheckpointCoordinates) == 0 {
		t.Fatal("publication lost descriptor")
	}
	ref := "refs/uzi-checkpoints/" + first.CheckpointCoordinates.CheckpointKey
	if mode == "successful-write" {
		if publishErr != nil || !result.Published || persisted.CheckpointTip.String != state.Tip {
			t.Fatalf("successful publication=%+v tip=%+v err=%v", result, persisted.CheckpointTip, publishErr)
		}
	} else if persisted.CheckpointTip.Valid {
		t.Fatal("unconfirmed/refused first write persisted a tip")
	}
	if mode != "first-write-refused" {
		if got := taskCheckpointGit(t, origin, "rev-parse", ref); got != state.Tip {
			t.Fatalf("remote landed=%s want=%s", got, state.Tip)
		}
	} else if got := taskCheckpointGit(t, origin, "for-each-ref", "--format=%(refname)", ref); got != "" {
		t.Fatalf("definitely refused write landed: %s", got)
	}
	if mode == "lost-ack-after-requeue" {
		if !requeued.Load() || publishErr == nil {
			t.Fatalf("lost ACK not observed: %v", publishErr)
		}
		var attempts int
		if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", run.ID).Scan(&attempts); err != nil || attempts != 1 {
			t.Fatalf("unresolved attempt=%d err=%v", attempts, err)
		}
	}
	node(t, "delete-A")
	env.exec("UPDATE runs SET status='queued' WHERE id=$1", run.ID)
	// Run all three UNCHANGED archived SQL entry points against this same task,
	// including the landed remote / NULL database-tip window.
	taskCheckpointHistoricalRefusal(t, f, run.ID)
	secondWorker := f.wkr
	secondWorker.ID = uuid.New()
	secondWorker.Name = "worker-B-" + secondWorker.ID.String()
	env.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')",
		secondWorker.ID, f.userID, secondWorker.Name, secondWorker.ID[:])
	registered, _, err := f.svc.Register(env.ctx, secondWorker, "e2e", "", nil, nil,
		[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := taskCheckpointClaim(t, f, registered, run.ID)
	if second.ClaimGeneration != first.ClaimGeneration+1 {
		t.Fatal("B did not claim same UUID at next generation")
	}
	owned := mustRun(t, env, run.ID)
	if uuid.UUID(owned.WorkerID.Bytes) != registered.ID {
		t.Fatal("B not persisted owner")
	}
	if second.CheckpointCoordinates.WorkBranch != first.CheckpointCoordinates.WorkBranch ||
		second.CheckpointCoordinates.CheckpointKey != first.CheckpointCoordinates.CheckpointKey {
		t.Fatal("B changed inherited branch/current-child key")
	}
	cfg["claim"] = second
	switch mode {
	case "first-write-refused":
		if *second.CheckpointWriteUnresolved || second.CheckpointTip != nil {
			t.Fatal("definite refusal not projected as explicit false / NULL anchor")
		}
		cfg["mode"] = "refused"
		node(t, "recover")
	case "lost-ack-after-requeue":
		if !*second.CheckpointWriteUnresolved || second.CheckpointTip != nil {
			t.Fatal("lost ACK handoff hid unresolved write / fabricated anchor")
		}
		cfg["mode"] = "unknown"
		node(t, "recover")
	case "successful-write":
		if second.CheckpointTip == nil || *second.CheckpointTip != state.Tip {
			t.Fatal("B missing own anchor")
		}
		// Move only the default branch, keeping the authorized historical bytes.
		node(t, "advance-default")
		cfg["mode"] = "success"
		node(t, "recover")
		t.Run("runner-feed", func(t *testing.T) { cfg["mode"] = "runner-feed"; node(t, "recover") })
		t.Run("absent-exact-ref", func(t *testing.T) {
			taskCheckpointGit(t, origin, "update-ref", "-d", ref)
			cfg["mode"] = "absent"
			node(t, "recover")
		})
		t.Run("mismatched-exact-ref", func(t *testing.T) {
			taskCheckpointGit(t, origin, "update-ref", ref, state.Base)
			cfg["mode"] = "mismatch"
			node(t, "recover")
		})
		t.Run("unrelated-workbranch", func(t *testing.T) {
			taskCheckpointGit(t, origin, "update-ref", ref, state.Tip)
			// A sibling commit is neither an ancestor nor a descendant of A's work.
			tree := taskCheckpointGit(t, origin, "rev-parse", state.Base+"^{tree}")
			sibling := taskCheckpointGit(t, origin, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
				"commit-tree", tree, "-p", state.Base, "-m", "unrelated published work")
			taskCheckpointGit(t, origin, "update-ref", "refs/heads/"+second.CheckpointCoordinates.WorkBranch, sibling)
			// Probe the real broker independently of GitCache's refusal. The moved
			// default plus competing work branch must not silently rebase the pack.
			taskCheckpointGit(t, origin, "update-ref", ref, state.Base)
			beforeReceive := receive.Load()
			brokerResult, brokerErr := pushbroker.Publish(env.ctx, pushbroker.Options{
				CloneURL: remote.URL + "/origin.git", WorkBranch: second.CheckpointCoordinates.WorkBranch,
				CheckpointKey: second.CheckpointCoordinates.CheckpointKey, DefaultBranch: "main",
				DeclaredTip: state.Tip, Pack: pack,
			})
			if !errors.Is(brokerErr, pushbroker.ErrNotDescendant) || brokerResult.AlreadyCurrent || receive.Load() != beforeReceive {
				t.Fatalf("broker ancestry rejection=%+v err=%v receive=%d", brokerResult, brokerErr, receive.Load()-beforeReceive)
			}
			if taskCheckpointGit(t, origin, "rev-parse", ref) != state.Base ||
				taskCheckpointGit(t, origin, "rev-parse", "refs/heads/"+second.CheckpointCoordinates.WorkBranch) != sibling {
				t.Fatal("broker ancestry refusal moved checkpoint or competing branch")
			}
			taskCheckpointGit(t, origin, "update-ref", ref, state.Tip)
			cfg["mode"] = "unrelated"
			node(t, "recover")
		})
	}
}

func taskCheckpointHistoricalRefusal(t *testing.T, f *custodyContinuationFixture, id uuid.UUID) {
	t.Helper()
	env := f.env
	for _, name := range []string{"ClaimRun", "ReadoptRunsFromSnapshot", "FrozenReadoptRunsFromSnapshot"} {
		t.Run("historical-"+name, func(t *testing.T) {
			run := mustRun(t, env, id)
			if name != "ClaimRun" {
				env.exec("INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at) VALUES($1,$2,$3,'running',false,1,now())",
					f.workerID, id, run.ClaimGeneration)
				defer env.exec("DELETE FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2", f.workerID, id)
			}
			before := coordinateAuthoritySnapshot(t, env, id)
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			q := store.New(tx)
			if _, err := q.GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
				t.Fatal(err)
			}
			var params any = f.workerID
			if name == "ClaimRun" {
				params = coordinateClaimParams(f)
			}
			if name == "FrozenReadoptRunsFromSnapshot" {
				locks, err := captureWorkerRecoveryLocks(env.ctx, q, f.workerID, []validatedEntry{{runID: id}}, validatedFinalizeResume{})
				if err != nil {
					t.Fatal(err)
				}
				ledger, parents, err := locks.parameters()
				if err != nil {
					t.Fatal(err)
				}
				params = store.FrozenReadoptRunsFromSnapshotParams{WorkerID: f.workerID, FrozenTargets: ledger, LockedParentIds: parents}
			}
			sql, args := historicalCoordinateQuery(t, name, params)
			sp, err := tx.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = sp.Exec(env.ctx, sql, args...)
			requireCoordinateSQLState(t, err, "23514")
			if err := sp.Rollback(env.ctx); err != nil {
				t.Fatal(err)
			}
			// Commit the surrounding transaction so its earlier work cannot be
			// hidden by a blanket rollback of the entire admission boundary.
			if err := tx.Commit(env.ctx); err != nil {
				t.Fatal(err)
			}
			if after := coordinateAuthoritySnapshot(t, env, id); after != before {
				t.Fatal("historical refusal left authority residue")
			}
		})
	}
}

func taskCheckpointEnv() []string {
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "TMPDIR=") {
			env = append(env, value)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
}
func taskCheckpointGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- fixed Git executable with test-defined arguments and fixture paths; no shell
	cmd.Env = taskCheckpointEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// taskCheckpointSmartHTTP supplies only the HTTP framing; the installed Git
// implements advertisement, pack negotiation, hooks and ref updates. Some worker
// images omit the optional CGI git-http-backend executable.
func taskCheckpointSmartHTTP(origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var operation, service string
		advertise := r.Method == http.MethodGet && r.URL.Path == "/origin.git/info/refs"
		if advertise {
			service = r.URL.Query().Get("service")
		} else if r.Method == http.MethodPost {
			service = strings.TrimPrefix(r.URL.Path, "/origin.git/")
		}
		switch service {
		case "git-upload-pack":
			operation = "upload-pack"
		case "git-receive-pack":
			operation = "receive-pack"
		default:
			http.NotFound(w, r)
			return
		}
		args := []string{operation, "--stateless-rpc"}
		if advertise {
			args = append(args, "--advertise-refs")
		}
		args = append(args, origin)
		var input []byte
		if !advertise {
			var err error
			input, err = io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			if err != nil || len(input) > 1<<20 {
				http.Error(w, "fixture request exceeds its byte budget", http.StatusRequestEntityTooLarge)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // Fixed Git executable, literal allowlisted operation, and owned fixture path; no shell.
		cmd.Env = taskCheckpointEnv()
		cmd.Stdin = bytes.NewReader(input)
		stdout := taskCheckpointHTTPBuffer{limit: 1 << 20}
		stderr := taskCheckpointHTTPBuffer{limit: 64 << 10}
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		output := stdout.Bytes()
		if err != nil {
			http.Error(w, "fixture Git: "+stderr.String(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		if advertise {
			w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
			prefix := "# service=" + service + "\n"
			_, _ = fmt.Fprintf(w, "%04x%s0000", len(prefix)+4, prefix) // #nosec G705 -- switch-allowlisted Git service in a smart-HTTP advertisement, not HTML
		} else {
			w.Header().Set("Content-Type", "application/x-"+service+"-result")
		}
		_, _ = w.Write(output)
	})
}

type taskCheckpointHTTPBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *taskCheckpointHTTPBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *taskCheckpointHTTPBuffer) String() string { return b.buffer.String() }
func (b *taskCheckpointHTTPBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, fmt.Errorf("fixture Git output exceeds its byte budget")
	}
	return b.buffer.Write(p)
}
