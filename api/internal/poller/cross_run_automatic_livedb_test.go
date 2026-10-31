package poller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

func crossRunWitness(e *mergedPollEnv, id uuid.UUID, iid int64) {
	e.t.Helper()
	e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,
		status,claim_generation,mr_iid,mr_state,created_at,finished_at)
		VALUES($1,$2,$3,'issue',101,'witness','d','completed',1,$4,'opened',
		'2020-01-03','2020-01-04')`, id, e.worker.UserID, e.repo, iid)
}

func crossRunAudit(t *testing.T, e *mergedPollEnv, hold, target, witness uuid.UUID, iid int64) []byte {
	t.Helper()
	var raw []byte
	var liveWorker, liveRun *uuid.UUID
	var evidence string
	if err := e.pool.QueryRow(e.ctx, `SELECT merged_predecessor_discard_binding,
		live_worker_id,live_run_id,release_evidence FROM recovery_custody_holds WHERE id=$1`, hold).
		Scan(&raw, &liveWorker, &liveRun, &evidence); err != nil {
		t.Fatal(err)
	}
	var audit struct {
		Basis              string    `json:"basis"`
		Hold               uuid.UUID `json:"hold_id"`
		Target             uuid.UUID `json:"target_run_id"`
		Witness            uuid.UUID `json:"witness_run_id"`
		Owner              uuid.UUID `json:"owner_id"`
		Repo               uuid.UUID `json:"repo_id"`
		Connection         uuid.UUID `json:"connection_id"`
		Generation         int64     `json:"generation"`
		TerminalGeneration int64     `json:"target_terminal_generation"`
		WitnessGeneration  int64     `json:"witness_completed_generation"`
		Status             string    `json:"target_terminal_status"`
		Issue              int64     `json:"issue_iid"`
		MR                 int64     `json:"witness_mr_iid"`
		Project            int64     `json:"project_id"`
		Finished           time.Time `json:"target_finished_at"`
		Created            time.Time `json:"witness_created_at"`
		Verified           time.Time `json:"verified_at"`
	}
	if err := json.Unmarshal(raw, &audit); err != nil {
		t.Fatal(err)
	}
	if audit.Basis != "same_issue_witness" || audit.Hold != hold || audit.Target != target ||
		audit.Witness != witness || audit.Owner != e.worker.UserID || audit.Repo != e.repo ||
		audit.Connection != e.conn || audit.Generation != 1 || audit.TerminalGeneration != 1 ||
		audit.WitnessGeneration != 1 || audit.Status != "cancelled" || audit.Issue != 101 ||
		audit.MR != iid || audit.Project != 42 ||
		!audit.Finished.Equal(time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)) ||
		!audit.Created.Equal(time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC)) ||
		audit.Verified.IsZero() || audit.Verified.After(time.Now()) ||
		liveWorker != nil || liveRun != nil || evidence != "merged_predecessor_discard" {
		t.Fatalf("cross-run audit/binding mismatch for hold %s: %s", hold, raw)
	}
	return raw
}

func TestCrossRunDiscardAutomaticPollingLiveDB(t *testing.T) {
	for _, mode := range []string{"merged", "missing_pr", "wrong_pr", "unmerged", "cached_merged_unreadable",
		"network_timeout", "kind_drift", "issue_drift", "created_drift", "foreign_connection",
		"parent_cancel", "parent_deadline", "ordinary_deadline", "direct_consumer", "continued_consumer"} {
		t.Run(mode, func(t *testing.T) {
			e, witness := newCrossRunDiscardEnv(t)
			e.forge.reads = 0 // Fixture setup GET is not a polling observation.
			if mode == "ordinary_deadline" {
				e.forge.syncDeadline = true
				e.engine.interval = time.Second
			}
			parent, cancel := context.WithCancel(e.ctx)
			defer cancel()
			if mode == "missing_pr" {
				e.exec("UPDATE runs SET mr_iid=NULL WHERE id=$1", witness)
			}
			if mode == "cached_merged_unreadable" {
				e.exec("UPDATE runs SET mr_state='merged' WHERE id=$1", witness)
			}
			if mode == "foreign_connection" {
				otherOwner := uuid.New()
				e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", otherOwner, otherOwner.String()+"@cross-run.test")
				e.exec("UPDATE forge_connections SET user_id=$2 WHERE id=$1", e.conn, otherOwner)
			}
			if mode == "direct_consumer" || mode == "continued_consumer" {
				consumer := uuid.New()
				if mode == "direct_consumer" {
					e.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status) VALUES($1,$2,$3,'issue',101,'consumer','d','queued')", consumer, e.worker.UserID, e.repo)
				} else {
					e.exec("INSERT INTO runs(id,user_id,repo_id,kind,continued_issue_iid,issue_title,issue_description,status,branch) VALUES($1,$2,$3,'task',101,'consumer','d','queued','task/consumer')", consumer, e.worker.UserID, e.repo)
				}
			}
			if mode == "parent_deadline" {
				var done context.CancelFunc
				parent, done = context.WithDeadline(e.ctx, time.Unix(1, 0))
				defer done()
			}
			e.forge.read = func(ctx context.Context, project, iid int64) (forge.MergeRequest, error) {
				if project != 42 || iid != 7 {
					t.Fatalf("GET project=%d PR=%d", project, iid)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("missing observation deadline")
				}
				switch mode {
				case "wrong_pr":
					return forge.MergeRequest{IID: 8, State: forge.MRStateMerged}, nil
				case "unmerged":
					return forge.MergeRequest{IID: iid, State: forge.MRStateOpened}, nil
				case "cached_merged_unreadable":
					return forge.MergeRequest{}, errors.New("network unavailable")
				case "network_timeout":
					return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, context.DeadlineExceeded
				case "kind_drift":
					e.exec("UPDATE runs SET kind='self_improve' WHERE id=$1", witness)
				case "issue_drift":
					e.exec("UPDATE runs SET issue_iid=102 WHERE id=$1", witness)
				case "created_drift":
					e.exec("UPDATE runs SET created_at='2020-01-05' WHERE id=$1", witness)
				case "parent_cancel":
					cancel()
				}
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}
			hooks := 0
			e.recovery.SetCustodySettledHook(func(run uuid.UUID) {
				hooks++
				if run != e.run || e.state(e.hold) != "discarded" {
					t.Error("settlement hook lost target/commit boundary")
				}
			})
			e.engine.PollOnce(parent)
			want, reads := "open", 1
			if mode == "merged" || mode == "ordinary_deadline" {
				want = "discarded"
			}
			if mode == "missing_pr" || mode == "foreign_connection" || mode == "parent_deadline" {
				reads = 0
			}
			if got := e.state(e.hold); got != want || e.forge.reads != reads {
				t.Fatalf("hold=%s GETs=%d; want %s/%d", got, e.forge.reads, want, reads)
			}
			if mode == "merged" || mode == "ordinary_deadline" {
				before := crossRunAudit(t, e, e.hold, e.run, witness, 7)
				e.engine.PollOnce(e.ctx)
				var after []byte
				if err := e.pool.QueryRow(e.ctx, "SELECT merged_predecessor_discard_binding FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) || hooks != 1 || e.forge.reads != 1 {
					t.Fatal("settled target was observed or audited twice")
				}
			} else {
				var audited bool
				if err := e.pool.QueryRow(e.ctx, "SELECT merged_predecessor_discard_binding IS NOT NULL FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&audited); err != nil {
					t.Fatal(err)
				}
				if audited || hooks != 0 {
					t.Fatal("refused observation wrote audit or settled target")
				}
			}
		})
	}
	t.Run("two_witnesses_one_unmerged", func(t *testing.T) {
		e, witness := newCrossRunDiscardEnv(t)
		second := uuid.New()
		crossRunWitness(e, second, 8)
		first, merged := witness, second
		unmergedIID, mergedIID := int64(7), int64(8)
		if slices.Compare(first[:], merged[:]) > 0 {
			first, merged = merged, first
			unmergedIID, mergedIID = mergedIID, unmergedIID
		}
		e.forge.reads = 0
		var observed []int64
		e.forge.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
			observed = append(observed, iid)
			state := forge.MRStateMerged
			if iid == unmergedIID {
				state = forge.MRStateOpened
			}
			return forge.MergeRequest{IID: iid, State: state}, nil
		}
		e.engine.PollOnce(e.ctx)
		if !slices.Equal(observed, []int64{unmergedIID, mergedIID}) || e.state(e.hold) != "discarded" {
			t.Fatalf("witness sequence=%v hold=%s", observed, e.state(e.hold))
		}
		crossRunAudit(t, e, e.hold, e.run, merged, mergedIID)
	})
}

func TestCrossRunDiscardPanicIsolationLiveDB(t *testing.T) {
	for _, postCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "forge", true: "settlement"}[postCommit], func(t *testing.T) {
			a, _ := newCrossRunDiscardEnv(t)
			b, _ := newCrossRunDiscardEnv(t)
			if slices.Compare(a.repo[:], b.repo[:]) > 0 {
				a, b = b, a
			}
			a.exec("UPDATE runs SET mr_iid=8 WHERE repo_id=$1 AND status='completed'", b.repo)
			a.forge.reads = 0
			a.engine.q = mergedEnabledSet{a.q, map[uuid.UUID]bool{a.repo: true, b.repo: true}}
			a.forge.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				if iid == 7 && !postCommit {
					panic("untrusted forge data")
				}
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}
			a.recovery.SetCustodySettledHook(func(run uuid.UUID) {
				if run == a.run && postCommit {
					if a.state(a.hold) != "discarded" {
						t.Error("hook before commit")
					}
					panic("settlement hook")
				}
			})
			a.engine.PollOnce(a.ctx)
			want := "open"
			if postCommit {
				want = "discarded"
			}
			if a.state(a.hold) != want || b.state(b.hold) != "discarded" || a.forge.reads != 2 {
				t.Fatalf("panic isolation first=%s sibling=%s GETs=%d", a.state(a.hold), b.state(b.hold), a.forge.reads)
			}
		})
	}
}
