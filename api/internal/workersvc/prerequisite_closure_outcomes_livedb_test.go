package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The lookup is the existing post-commit binding seam, not a fabricated marker.
type m3StoppedLookup struct {
	PrerequisiteStore
	tx      *m3Transactions
	missing bool
}

func (q *m3StoppedLookup) GetForgeConnectionForUser(ctx context.Context, a store.GetForgeConnectionForUserParams) (store.ForgeConnection, error) {
	if q.missing || q.tx != nil && q.tx.committed.Load() {
		return store.ForgeConnection{}, pgx.ErrNoRows
	}
	return q.PrerequisiteStore.GetForgeConnectionForUser(ctx, a)
}
func m3StopAfterMarker(t *testing.T, f *prerequisiteExecutorLiveDB) *m3Transactions {
	t.Helper()
	tx := &m3Transactions{f: f}
	q := &m3StoppedLookup{PrerequisiteStore: f.env.q, tx: tx}
	f.s.SetPrerequisiteStore(q, tx.transaction)
	return tx
}

func TestPrerequisiteStoppedAfterMarkerNeverResendsLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	tx := m3StopAfterMarker(t, f)
	remote := m3RemoteFor(t, f, tx)
	m3Sweep(t, ctx, f.s)
	marked := f.readLiveDB(t)
	requests, commands := remote.snapshot()
	if requests != 1 || marked.Attempts != 1 || marked.ConnectionID != prerequisiteID(f.m.conn.ID) ||
		marked.ForgeKind.String != "github" || marked.NormalizedBaseUrl.String != f.m.conn.BaseUrl ||
		marked.ProjectID.Int64 != 1 || marked.DefaultBranch.String != "main" || marked.Checksum != f.m.row.Checksum || marked.State != "invoked" || !marked.InvokedAt.Valid || !marked.InvokedDeadline.Valid ||
		marked.PinTip.String != prerequisiteTestTip || marked.LiveRunID != prerequisiteID(marked.RunID) ||
		!tx.committed.Load() || tx.active.Load() || remote.posts.Load() != 0 || len(commands) != 0 {
		t.Fatal("post-commit stop did not leave a durable unsent invocation")
	}
	fresh := m3Fresh(f)
	f.advanceLiveDB(t, marked.InvokedDeadline.Time.Sub(f.m.now)+time.Nanosecond)
	m3Sweep(t, ctx, fresh)
	if r := f.readLiveDB(t); r.State != "create_unknown" {
		t.Fatalf("stale marker state=%s", r.State)
	}
	previousRequests, _ := remote.snapshot()
	// A bounded set of fake-clock jumps includes far more than every lease and
	// absence confirmation interval; no timeout grants permission to resend.
	listings := 0
	// A same-pass deduplication can advance the cursor past this row. Allow
	// bounded empty wrap turns; count actual listings rather than demanding
	// that every public pass process this candidate.
	for pass := 0; pass < 24 && listings < 12; pass++ {
		f.advanceLiveDB(t, 30*24*time.Hour)
		m3Sweep(t, ctx, fresh)
		r := f.readLiveDB(t)
		m3AssertFrozen(t, marked, r)
		requests, cmd := remote.snapshot()
		delta := requests - previousRequests
		if r.State != "create_unknown" || r.CleanupAttempts != 0 || remote.posts.Load() != 0 || len(cmd) != 0 || delta < 0 || delta > 1 {
			t.Fatalf("absence pass %d: state=%s cleanup=%d request delta=%d commands=%d", pass, r.State, r.CleanupAttempts, delta, len(cmd))
		}
		if delta == 1 {
			listings++
			if !r.NextAttemptAt.Time.After(f.m.now) {
				t.Fatalf("listing %d did not schedule positive backoff", listings)
			}
		}
		if r.ReconciliationAttempts != int32(min(listings, 7)) {
			t.Fatalf("absence pass %d: listings=%d reconciliation=%d", pass, listings, r.ReconciliationAttempts)
		}
		previousRequests = requests
	}
	if listings != 12 {
		t.Fatalf("listing-only recovery made %d actual listings in 24 passes, want 12", listings)
	}
}

// Synthetic policy responses exercise the broker/executor contract. They make
// no claim about a provider's workflow or default-branch protection policy.
func TestPrerequisiteFrozenDOutcomeMatrixLiveDB(t *testing.T) {
	for _, mode := range []string{"refuse", "refuse-readback", "ambiguous-absent", "ambiguous-readback", "late-foreign"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrerequisiteExecutorLiveDB(t, 1)
			ctx := m3Context(t)
			tx := &m3Transactions{f: f}
			f.s.SetPrerequisiteStore(f.env.q, tx.transaction)
			remote := m3RemoteFor(t, f, tx)
			remote.mode = mode
			if mode == "late-foreign" {
				remote.mode = "ambiguous-absent"
			}
			// Move the advertised default only AFTER the actual marker COMMIT,
			// before the production post-commit binding check and broker send.
			tx.afterMarkerCommit = func() {
				remote.mu.Lock()
				remote.defaultTip = strings.Repeat("b", 40)
				remote.mu.Unlock()
			}
			proofs := 0
			f.builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(_ context.Context, _ int64, head, _ string) (forge.Ancestry, error) {
				if head != prerequisiteTestTip || tx.committed.Load() || tx.active.Load() {
					return forge.AncestryUnknown, errors.New("proof did not precede marker outside transaction at original D")
				}
				proofs++
				return forge.AncestryAncestor, nil
			}}
			m3Sweep(t, ctx, f.s)
			row := f.readLiveDB(t)
			_, cmd := remote.snapshot()
			if proofs != len(row.PrerequisiteShas) || len(cmd) != 1 || cmd[0] != [3]string{strings.Repeat("0", 40), prerequisiteTestTip, row.PinRef} ||
				row.PinTip.String != prerequisiteTestTip || row.DefaultBranch.String != "main" || row.Attempts != 1 ||
				!row.InvokedAt.Valid || !row.InvokedDeadline.Valid || !tx.committed.Load() || tx.active.Load() ||
				remote.ioInTX.Load() || remote.sendBeforeCommit.Load() {
				t.Fatal("command was not the prepared create-only frozen D outside committed S2")
			}
			if mode == "refuse" {
				if row.State != "refused" || row.Reason != "pin_refused" || row.LiveRunID.Valid {
					t.Fatalf("negative outcome: state=%s reason=%s live=%t", row.State, row.Reason, row.LiveRunID.Valid)
				}
				f.advanceLiveDB(t, 48*time.Hour)
				m3Sweep(t, ctx, m3Fresh(f))
				_, after := remote.snapshot()
				m3AssertFrozen(t, row, f.readLiveDB(t))
				if len(after) != 1 || remote.posts.Load() != 1 || f.readLiveDB(t).Attempts != 1 {
					t.Fatal("refusal retried with a new D")
				}
				return
			}
			if row.State != "create_unknown" || row.Reason != "pin_unknown" || !row.LiveRunID.Valid {
				t.Fatalf("ambiguous result became %s live=%t", row.State, row.LiveRunID.Valid)
			}
			// Expiry while unknown must never admit cleanup. Only a later listing may
			// first commit pinned, and a different public pass may delete.
			f.expireCaptureLiveDB(t)
			fresh := m3Fresh(f)
			for i := 0; i < 3; i++ {
				remote.mu.Lock()
				remote.mode = ""
				remote.tip = ""
				remote.mu.Unlock()
				m3Sweep(t, ctx, fresh)
				after := f.readLiveDB(t)
				m3AssertFrozen(t, row, after)
				if after.State != "create_unknown" || after.CleanupAttempts != 0 || !after.NextAttemptAt.Time.After(f.m.now) {
					t.Fatal("expired unknown cleaned up or lost pointer")
				}
				_, commands := remote.snapshot()
				if len(commands) != 1 {
					t.Fatal("unknown resent or deleted")
				}
				f.dueLiveDB(t)
			}
			remote.mu.Lock()
			remote.tip = prerequisiteTestTip
			if mode == "late-foreign" {
				remote.tip = strings.Repeat("c", 40)
			}
			remote.mu.Unlock()
			m3Sweep(t, ctx, fresh)
			after := f.readLiveDB(t)
			_, commands := remote.snapshot()
			if mode == "late-foreign" {
				if after.State != "foreign" || after.LiveRunID.Valid || after.CleanupAttempts != 0 || len(commands) != 1 {
					t.Fatal("foreign tip was touched or retained as owned")
				}
				terminalFrozen := row
				terminalFrozen.LiveRunID = after.LiveRunID // Only terminal pointer release is permitted.
				m3AssertFrozen(t, terminalFrozen, after)
				f.advanceLiveDB(t, 48*time.Hour)
				m3Sweep(t, ctx, fresh)
				_, later := remote.snapshot()
				remote.mu.Lock()
				tip := remote.tip
				remote.mu.Unlock()
				if len(later) != 1 || remote.posts.Load() != 1 || tip != strings.Repeat("c", 40) {
					t.Fatal("foreign tip changed or terminal row performed a command")
				}
				return
			}
			m3AssertFrozen(t, row, after)
			if after.State != "pinned" || after.CleanupAttempts != 0 || len(commands) != 1 {
				t.Fatal("late create did not commit pinned before cleanup")
			}
			m3Sweep(t, ctx, fresh)
			after = f.readLiveDB(t)
			_, commands = remote.snapshot()
			if after.State != "deleted" || after.LiveRunID.Valid || after.CleanupAttempts != 1 || len(commands) != 2 ||
				commands[1] != [3]string{prerequisiteTestTip, strings.Repeat("0", 40), row.PinRef} {
				t.Fatal("later pass failed guarded delete")
			}
		})
	}
}

func TestPrerequisiteFrozenBindingDriftNoTrafficLiveDB(t *testing.T) {
	for _, state := range []string{"create_unknown", "pinned"} {
		for _, drift := range []string{"identity", "kind", "base_url", "project", "default_branch", "deleted_lookup"} {
			t.Run(state+"/"+drift, func(t *testing.T) {
				f := newPrerequisiteExecutorLiveDB(t, 1)
				ctx := m3Context(t)
				remote := m3RemoteFor(t, f, nil)
				if state == "create_unknown" {
					remote.mode = "ambiguous-absent"
				}
				m3Sweep(t, ctx, f.s)
				if f.readLiveDB(t).State != state {
					t.Fatal("initial outcome missing")
				}
				f.expireCaptureLiveDB(t)
				frozen := f.readLiveDB(t)
				switch drift {
				case "identity":
					id := uuid.New()
					// The schema uniquely keys owner/kind/base URL. Move the
					// original table binding aside so the replacement's current
					// fields can match exactly while only its identity differs.
					f.env.exec("UPDATE forge_connections SET base_url=base_url||'/retired' WHERE id=$1", f.m.conn.ID)
					f.env.exec(`INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
      SELECT $1,user_id,forge_type,$3,bot_username,bot_forge_user_id,token_ciphertext FROM forge_connections WHERE id=$2`, id, f.m.conn.ID, f.m.conn.BaseUrl)
					f.env.exec("UPDATE repos SET connection_id=$1 WHERE id=$2", id, f.m.row.RepoID)
				case "kind":
					f.env.exec("UPDATE forge_connections SET forge_type='gitlab' WHERE id=$1", f.m.conn.ID)
				case "base_url":
					f.env.exec("UPDATE forge_connections SET base_url=base_url||'/moved' WHERE id=$1", f.m.conn.ID)
				case "project":
					f.env.exec("UPDATE repos SET forge_project_id=2 WHERE id=$1", f.m.row.RepoID)
				case "default_branch":
					f.env.exec("UPDATE repos SET default_branch='moved' WHERE id=$1", f.m.row.RepoID)
				}
				fresh := m3Fresh(f)
				if drift == "deleted_lookup" {
					fresh.SetPrerequisiteStore(&m3StoppedLookup{PrerequisiteStore: f.env.q, missing: true}, nil)
				}
				beforeRequests, beforeCommands := remote.snapshot()
				beforeBuilds := f.builder.builds
				beforePosts := remote.posts.Load()
				for i := 0; i < 3; i++ {
					m3Sweep(t, ctx, fresh)
					after := f.readLiveDB(t)
					m3AssertFrozen(t, frozen, after)
					if after.State != state || after.CleanupAttempts != 0 || !after.NextAttemptAt.Time.After(f.m.now) {
						t.Fatalf("drift outcome state=%s cleanup=%d next=%s", after.State, after.CleanupAttempts, after.NextAttemptAt.Time)
					}
					requests, commands := remote.snapshot()
					if requests != beforeRequests || remote.posts.Load() != beforePosts || len(commands) != len(beforeCommands) || f.builder.builds != beforeBuilds {
						t.Fatal("frozen binding drift performed external work")
					}
					f.dueLiveDB(t)
				}
			})
		}
	}
}

func TestPrerequisiteDrainDuringAdmittedSendAndReadbackLiveDB(t *testing.T) {
	for _, phase := range []string{"send", "readback"} {
		t.Run(phase, func(t *testing.T) {
			f := newPrerequisiteExecutorLiveDB(t, 1)
			ctx := m3Context(t)
			remote := m3RemoteFor(t, f, nil)
			remote.block = phase
			remote.entered = make(chan struct{})
			remote.exited = make(chan struct{})
			remote.root = f.s.prerequisite.root
			// Kicks read an immutable clock. SQL and this snapshot agree until
			// Drain joins; fake time advances only after that join below.
			startedAt := f.m.now
			f.s.SetClock(func() time.Time { return startedAt })
			f.s.StartPrerequisiteClosures(ctx)
			// Cleanup is registered after the HTTP server, so drain precedes Close.
			t.Cleanup(func() {
				c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if e := f.s.DrainPrerequisiteClosures(c); e != nil {
					t.Error(e)
				}
			})
			if !f.s.KickPrerequisiteClosure(f.m.row.UserID, f.m.row.CaptureID) {
				t.Fatal("kick refused")
			}
			m3Wait(t, ctx, remote.entered)
			row := f.readLiveDB(t)
			if row.State != "invoked" || !row.InvokedAt.Valid || !row.LiveRunID.Valid {
				t.Fatal("HTTP barrier was not after durable invocation")
			}
			if remote.root.Err() != nil {
				t.Fatal("server root canceled before shutdown")
			}
			select {
			case <-remote.canceled:
				t.Fatal("broker child timeout canceled HTTP before Drain")
			default:
			}
			drain, cancel := context.WithTimeout(ctx, 2*time.Second)
			e := f.s.DrainPrerequisiteClosures(drain)
			cancel()
			if e != nil {
				t.Fatal(e)
			}
			m3Wait(t, ctx, remote.exited)
			m3Wait(t, ctx, remote.canceled)
			if !errors.Is(remote.root.Err(), context.Canceled) || remote.canceledBeforeRoot.Load() || ctx.Err() != nil {
				t.Fatal("HTTP cancellation did not follow Drain's root cancellation")
			}
			if f.s.KickPrerequisiteClosure(row.UserID, row.CaptureID) {
				t.Fatal("post-drain kick admitted")
			}
			after := f.readLiveDB(t)
			m3AssertFrozen(t, row, after)
			if after.State != "invoked" && after.State != "create_unknown" {
				t.Fatalf("drain state=%s", after.State)
			}
			_, commands := remote.snapshot()
			sends := len(commands)
			if sends != 1 || commands[0] != [3]string{strings.Repeat("0", 40), prerequisiteTestTip, row.PinRef} {
				t.Fatal("shutdown did not intercept the actual admitted create command")
			}
			remote.mu.Lock()
			remote.block = ""
			remote.mode = ""
			remote.tip = ""
			remote.mu.Unlock()
			f.s.SetClock(func() time.Time { return f.m.now }) // The kick has joined.
			fresh := m3Fresh(f)
			f.advanceLiveDB(t, 24*time.Hour)
			// A stale invocation and its reconciliation are deliberately separate passes.
			for i := 0; i < 4; i++ {
				before := f.readLiveDB(t)
				beforeRequests, _ := remote.snapshot()
				m3Sweep(t, ctx, fresh)
				requests, _ := remote.snapshot()
				wantRequests := beforeRequests + 1
				if before.State == "invoked" {
					wantRequests = beforeRequests
				}
				if requests != wantRequests || remote.posts.Load() != 1 {
					t.Fatal("fresh service did not perform listing-only recovery")
				}
				after = f.readLiveDB(t)
				m3AssertFrozen(t, row, after)
				if after.State != "create_unknown" {
					t.Fatalf("resumed state=%s", after.State)
				}
				f.dueLiveDB(t)
			}
			_, later := remote.snapshot()
			if len(later) != sends {
				t.Fatal("fresh service resent an admitted invocation")
			}
		})
	}
}
