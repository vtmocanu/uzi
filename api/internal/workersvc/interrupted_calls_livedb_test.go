package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestInterruptedCallsEligibilityLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, nil)
	parent := e.seedActiveRunOwnedBy(t, wid)
	e.exec(t, "UPDATE workers SET snapshot_register_nonce='nonce' WHERE id=$1", wid)
	w, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(e.q, nil, Params{})
	for i, kind := range []string{"issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework", "chat", "judge", "job", "cross_check"} {
		t.Run(kind, func(t *testing.T) {
			id := uuid.New()
			var repo any = e.repoID
			var issue, pipeline, ref, mr, branch, target, job any
			harness, report := "claude", false
			switch kind {
			case "issue", "self_improve":
				issue = 700 + i
			case "ci_fix":
				pipeline, ref = 700+i, "candidate"
			case "task":
				branch = "task/" + id.String()
			case "mr_rework":
				ref, mr, target = "candidate", 700+i, parent
			case "chat":
				repo = nil
			case "judge":
				repo, target = nil, parent
			case "job":
				repo, job = nil, "research"
			case "cross_check":
				target, harness, report = parent, "codex", true
			}
			e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,pipeline_id,pipeline_ref,mr_iid,branch,target_run_id,job_type,harness,report_only,budget_wall_seconds,issue_title,issue_description,status,worker_id,claim_generation)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,60,'title','description','running',$14,2)`,
				id, e.userID, repo, kind, issue, pipeline, ref, mr, branch, target, job, harness, report, wid)
			raw, err := svc.InterruptedCalls(e.ctx, w, id, "nonce", 2)
			eligible := i < 6
			if !eligible {
				if !errors.Is(err, ErrRunNotOwned) {
					t.Fatalf("ineligible %s returned %q, %v", kind, raw, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var out apitypes.InterruptedCallsResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if out.RunID != id.String() || out.ClaimGeneration != 2 || len(out.Entries) != 0 {
				t.Fatalf("wrong empty identity: %s", raw)
			}
			e.exec(t, "UPDATE runs SET status='claimed' WHERE id=$1", id)
			if _, err := svc.InterruptedCalls(e.ctx, w, id, "nonce", 2); err != nil {
				t.Fatalf("claimed %s: %v", kind, err)
			}
		})
	}
	if _, err := svc.InterruptedCalls(e.ctx, w, uuid.New(), "nonce", 2); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("wrong run: %v", err)
	}
}

func TestInterruptedCallsWindowLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, nil)
	run := e.seedActiveRunOwnedBy(t, wid)
	e.exec(t, "UPDATE workers SET snapshot_register_nonce='nonce' WHERE id=$1", wid)
	e.exec(t, "UPDATE runs SET claim_generation=4 WHERE id=$1", run)
	w, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(e.q, nil, Params{})
	clear := func() { e.exec(t, "DELETE FROM run_messages WHERE run_id=$1", run) }
	add := func(seq int, kind string, p any, agent, instance any, gen any) {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		e.exec(t, "INSERT INTO run_messages(run_id,seq,kind,payload,agent,agent_instance,claim_generation) VALUES($1,$2,$3,$4,$5,$6,$7)", run, seq, kind, raw, agent, instance, gen)
	}
	use := func(id, command any) any {
		return map[string]any{"id": id, "name": "Bash", "input": map[string]any{"command": command}}
	}
	result := func(id any) any {
		return map[string]any{"tool_use_id": id, "content": "synthetic interruption", "is_error": true}
	}
	read := func(n int, p int64) apitypes.InterruptedCallsResponse {
		t.Helper()
		raw, err := svc.InterruptedCalls(e.ctx, w, run, "nonce", 4)
		if err != nil {
			t.Fatal(err)
		}
		var out apitypes.InterruptedCallsResponse
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Entries) != n {
			t.Fatalf("entries=%d want=%d: %s", len(out.Entries), n, raw)
		}
		if p == 0 && out.SourceGeneration != nil || p > 0 && (out.SourceGeneration == nil || *out.SourceGeneration != p) {
			t.Fatalf("source=%v want=%d", out.SourceGeneration, p)
		}
		return out
	}
	t.Run("empty authorized", func(t *testing.T) { clear(); read(0, 0) })
	t.Run("selected unmatched completed late synthetic", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("a", "echo a"), "lead", nil, 1)
		add(2, "tool_use", use("b", "echo b"), "lead", nil, 2)
		read(1, 2)
		add(3, "tool_result", result("b"), "lead", nil, 2)
		read(0, 2)
	})
	t.Run("tool-less skip NULL current future excluded", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("a", "echo a"), "lead", nil, 1)
		add(2, "text", map[string]any{"text": "no tools"}, "lead", nil, 3)
		add(3, "tool_use", use("b", "echo b"), "lead", nil, nil)
		add(4, "tool_use", use("c", "echo c"), "lead", nil, 4)
		add(5, "tool_use", use("d", "echo d"), "lead", nil, 5)
		read(1, 1)
	})
	t.Run("other-generation results do not settle", func(t *testing.T) {
		clear()
		add(1, "tool_result", result("same"), "lead", nil, 2)
		add(2, "tool_use", use("same", "echo candidate"), "lead", nil, 3)
		add(3, "tool_result", result("same"), "lead", nil, 4)
		read(1, 3)
		add(4, "tool_result", result("same"), "lead", nil, 3)
		read(0, 3)
	})
	t.Run("newer results-only generation does not fall back", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("old", "echo old"), "lead", nil, 1)
		add(2, "tool_result", result("unrelated"), "lead", nil, 3)
		read(0, 3)
	})
	t.Run("newer tool bearing malformed no fallback", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("a", "echo a"), "lead", nil, 1)
		add(2, "tool_use", use(123, "echo b"), "lead", nil, 3)
		if out := read(0, 3); !out.Unavailable {
			t.Fatal(out)
		}
	})
	t.Run("JSON types and SQL command omission", func(t *testing.T) {
		for _, cmd := range []any{123, map[string]any{"command": "echo"}, strings.Repeat("x", 2049), strings.Repeat("界", 2048)} {
			clear()
			add(1, "tool_use", use("a", cmd), "lead", nil, 3)
			read(0, 3)
		}
		clear()
		add(1, "tool_use", use("a", strings.Repeat("x", 2048)), "lead", nil, 3)
		read(1, 3)
	})
	t.Run("full key byte boundaries", func(t *testing.T) {
		for _, n := range []int{129, 1024, 1025} {
			clear()
			id := strings.Repeat("x", n)
			add(1, "tool_use", use(id, "echo"), "lead", nil, 3)
			want := 1
			if n > 1024 {
				want = 0
			}
			read(want, 3)
			add(2, "tool_result", result(id), "lead", nil, 3)
			read(0, 3)
		}
	})
	t.Run("duplicates and result before use", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("a", "echo"), "lead", nil, 3)
		add(2, "tool_use", use("a", 12), "lead", nil, 3)
		read(0, 3)
		clear()
		add(1, "tool_result", result("a"), "lead", nil, 3)
		add(2, "tool_use", use("a", "echo"), "lead", nil, 3)
		read(0, 3)
	})
	t.Run("persisted instances and malformed null lanes", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("a", "echo"), "reviewer", "dispatch-a", 3)
		add(2, "tool_result", result("a"), "reviewer", "dispatch-b", 3)
		read(1, 3)
		add(3, "tool_result", result("a"), "lead", "dispatch-a", 3)
		read(0, 3)
		clear()
		add(1, "tool_use", use("a", "echo"), "lead", nil, 3)
		add(2, "tool_result", result("a"), "reviewer", nil, 3)
		read(1, 3)
		clear()
		add(1, "tool_use", use("a", "echo"), "lead", "", 3)
		read(0, 3)
		clear()
		add(1, "tool_use", use("a", "echo"), "lead", strings.Repeat("x", 128), 3)
		read(0, 3)
	})
	t.Run("ingestion ceiling loses pairing authority", func(t *testing.T) {
		clear()
		e.exec(t, "UPDATE runs SET claim_generation=3 WHERE id=$1", run)
		generation := int64(3)
		raw, _ := json.Marshal(use("a", "echo"))
		svc.SetTxBeginner(e.pool)
		if err := svc.AppendMessagesForClaim(e.ctx, w, run, []IncomingMessage{{Seq: 1, Kind: "tool_use", Agent: "reviewer", AgentInstance: strings.Repeat("x", 129), Payload: raw}}, &generation); err != nil {
			t.Fatal(err)
		}
		var instance string
		if err := e.pool.QueryRow(e.ctx, "SELECT agent_instance FROM run_messages WHERE run_id=$1 AND seq=1", run).Scan(&instance); err != nil {
			t.Fatal(err)
		}
		if instance != strings.Repeat("x", 128) {
			t.Fatalf("unexpected persisted instance %q", instance)
		}
		e.exec(t, "UPDATE runs SET claim_generation=4 WHERE id=$1", run)
		read(0, 3)
	})
	t.Run("anykind newest256 displaces old tools", func(t *testing.T) {
		for _, kind := range []string{"text", "thinking", "status"} {
			clear()
			add(1, "tool_use", use("old", "echo"), "lead", nil, 1)
			e.exec(t, "INSERT INTO run_messages(run_id,seq,kind,payload,agent,claim_generation) SELECT $1,s,$2,'{}'::jsonb,'reviewer',4 FROM generate_series(2,257) s", run, kind)
			if out := read(0, 0); out.InspectedCount != 256 {
				t.Fatal(out)
			}
		}
	})
	t.Run("fences current row rather than cached worker", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("a", "echo"), "lead", nil, 3)
		for _, tc := range []struct{ name, change, restore string }{
			{"release", "claim_released_at=now()", "claim_released_at=NULL"},
			{"reclaim", "claim_generation=5", "claim_generation=4"},
			{"status", "status='queued'", "status='running'"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e.exec(t, "UPDATE runs SET "+tc.change+" WHERE id=$1", run)
				_, err := svc.InterruptedCalls(e.ctx, w, run, "nonce", 4)
				if !errors.Is(err, ErrRunNotOwned) {
					t.Fatalf("%v", err)
				}
				e.exec(t, "UPDATE runs SET "+tc.restore+" WHERE id=$1", run)
			})
		}
		for _, nonce := range []string{"stale"} {
			if _, err := svc.InterruptedCalls(e.ctx, w, run, nonce, 4); !errors.Is(err, ErrRunNotOwned) {
				t.Fatal(err)
			}
		}
		e.exec(t, "UPDATE workers SET snapshot_register_nonce='rotated' WHERE id=$1", wid)
		if _, err := svc.InterruptedCalls(e.ctx, w, run, "nonce", 4); !errors.Is(err, ErrRunNotOwned) {
			t.Fatal(err)
		}
		e.exec(t, "UPDATE workers SET snapshot_register_nonce='nonce' WHERE id=$1", wid)
		otherOwner := uuid.New()
		e.exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", otherOwner, otherOwner.String()+"@interrupted.test")
		e.exec(t, "UPDATE runs SET user_id=$2 WHERE id=$1", run, otherOwner)
		if _, err := svc.InterruptedCalls(e.ctx, w, run, "nonce", 4); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("run owner fence: %v", err)
		}
		e.exec(t, "UPDATE runs SET user_id=$2 WHERE id=$1", run, e.userID)
		e.exec(t, "UPDATE workers SET user_id=$2 WHERE id=$1", wid, otherOwner)
		if _, err := svc.InterruptedCalls(e.ctx, w, run, "nonce", 4); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("current worker owner fence: %v", err)
		}
		e.exec(t, "UPDATE workers SET user_id=$2 WHERE id=$1", wid, e.userID)
		wrong := w
		wrong.UserID = uuid.New()
		if _, err := svc.InterruptedCalls(e.ctx, wrong, run, "nonce", 4); !errors.Is(err, ErrRunNotOwned) {
			t.Fatal(err)
		}
		wrong = w
		wrong.ID = uuid.New()
		if _, err := svc.InterruptedCalls(e.ctx, wrong, run, "nonce", 4); !errors.Is(err, ErrRunNotOwned) {
			t.Fatal(err)
		}
	})
	t.Run("deterministic read versus reclaim", func(t *testing.T) {
		clear()
		add(1, "tool_use", use("a", "echo"), "lead", nil, 3)
		reached, proceed := make(chan struct{}), make(chan struct{})
		wrapped := interruptedFake{Store: e.q, read: func(ctx context.Context, p store.InterruptedCallsWindowParams) ([]store.InterruptedCallsWindowRow, error) {
			close(reached)
			select {
			case <-proceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return e.q.InterruptedCallsWindow(ctx, p)
		}}
		fenced := New(wrapped, nil, Params{})
		done := make(chan error, 1)
		go func() { _, err := fenced.InterruptedCalls(e.ctx, w, run, "nonce", 4); done <- err }()
		<-reached
		e.exec(t, "UPDATE runs SET claim_generation=5 WHERE id=$1", run)
		close(proceed)
		if err := <-done; !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("reclaim returned %v", err)
		}
		e.exec(t, "UPDATE runs SET claim_generation=4 WHERE id=$1", run)
	})
}
