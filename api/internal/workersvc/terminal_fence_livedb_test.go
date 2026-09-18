package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1391 Run B M3c: it EXECUTES the terminal fence (SetState's
// pre-mutation CountRunMessagesThrough contiguity check) and the hardened message-gaps read
// (atomic worker/generation auth + the RunMessageGaps LAG/keyset query) against a REAL Postgres —
// sqlc's type deduction is not Postgres's, and the LAG window + BETWEEN range scan can pass
// `sqlc generate` yet behave differently at execute. Skipped unless UZI_TEST_DATABASE_URL is set
// (setupCodexLiveDB skips).

// seedRunningRun inserts one issue run in 'running' status owned by o's worker, at the given
// last_seq high-water mark. anthropic_secret_id is set (like the other seeds) so the completion
// fan-out has a well-formed row to re-read.
func seedRunningRun(t *testing.T, env codexTestEnv, o reevalOwner, issueIID int64, lastSeq int32) uuid.UUID {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, status_since, worker_id, anthropic_secret_id, last_seq, started_at, budget_paused_seconds)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', now(), $5, $6, $7, now(), 0)`,
		id, o.userID, o.repoID, issueIID, o.workerID, o.altTok, lastSeq)
	return id
}

// insertRunMsg inserts one message at an explicit seq (deliberately allowing gaps — the fence's
// whole job is to notice them).
func insertRunMsg(t *testing.T, env codexTestEnv, runID uuid.UUID, seq int32) {
	t.Helper()
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, $2, 'text', '{}'::jsonb)`, runID, seq)
}

func throughPtr(n int64) *int64 { return &n }

// TestSetStateTerminalFenceLiveDB executes the terminal fence end to end (PRD #1391 Run B M3c, D3):
// a terminal (completed/failed) report carrying messages_through_seq is refused with
// ErrMessagesPending until run_messages hold a contiguous [1..through], applied once they do, and
// entirely bypassed when the field is absent (legacy). Dropping the last_seq check OR the
// CountRunMessagesThrough contiguity check reddens a sub-test.
func TestSetStateTerminalFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams()) // WorkerGapFillMax = 10000
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}

	t.Run("refuses when last_seq below through", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6001, 2)
		insertRunMsg(t, env, id, 1)
		insertRunMsg(t, env, id, 2)
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed", MessagesThroughSeq: throughPtr(5)})
		if !errors.Is(err, ErrMessagesPending) {
			t.Fatalf("err = %v, want ErrMessagesPending (last_seq 2 < through 5)", err)
		}
		if applied {
			t.Fatal("a fenced completion must not be applied")
		}
		if got := statusOf(t, env, id); got != "running" {
			t.Fatalf("status = %q, want running (the refused completion must not transition)", got)
		}
	})

	t.Run("applies when contiguous through the reported seq", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6002, 2)
		insertRunMsg(t, env, id, 1)
		insertRunMsg(t, env, id, 2)
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed", MessagesThroughSeq: throughPtr(2)})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !applied || run.Status != "completed" {
			t.Fatalf("applied=%v status=%q, want applied completed", applied, run.Status)
		}
	})

	t.Run("ignores the fence when messages_through_seq is absent", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6003, 0)
		// No messages stored at all, but no through reported → unfenced → completes.
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed"})
		if err != nil {
			t.Fatalf("err = %v, want nil (a legacy worker's completion is unfenced)", err)
		}
		if !applied || run.Status != "completed" {
			t.Fatalf("applied=%v status=%q, want applied completed", applied, run.Status)
		}
	})

	t.Run("refuses a hole below last_seq (count < through)", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6004, 2)
		insertRunMsg(t, env, id, 2) // seq 1 is a hole; last_seq(2) already >= through(2)
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed", MessagesThroughSeq: throughPtr(2)})
		if !errors.Is(err, ErrMessagesPending) {
			t.Fatalf("err = %v, want ErrMessagesPending (count 1 < through 2, the hole is invisible to last_seq)", err)
		}
		if applied {
			t.Fatal("a fenced completion must not be applied")
		}
	})

	t.Run("refuses then applies once the interior hole is filled", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6005, 3)
		insertRunMsg(t, env, id, 1)
		insertRunMsg(t, env, id, 3) // hole at 2; {1,3} count 2 < through 3
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed", MessagesThroughSeq: throughPtr(3)})
		if !errors.Is(err, ErrMessagesPending) {
			t.Fatalf("err = %v, want ErrMessagesPending ({1,3} count 2 < through 3)", err)
		}
		if applied {
			t.Fatal("must not apply while a hole remains")
		}
		insertRunMsg(t, env, id, 2) // fill the hole → {1,2,3} count 3 == through
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed", MessagesThroughSeq: throughPtr(3)})
		if err != nil {
			t.Fatalf("after fill: err = %v, want nil", err)
		}
		if !applied || run.Status != "completed" {
			t.Fatalf("after fill: applied=%v status=%q, want applied completed", applied, run.Status)
		}
	})

	t.Run("rejects a negative through", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6006, 0)
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed", MessagesThroughSeq: throughPtr(-1)})
		if !errors.Is(err, ErrInvalidState) {
			t.Fatalf("err = %v, want ErrInvalidState (a negative through is invalid)", err)
		}
		if applied {
			t.Fatal("a negative through must not be applied")
		}
	})

	t.Run("fences a failed transition too", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6007, 2)
		insertRunMsg(t, env, id, 1)
		insertRunMsg(t, env, id, 2)
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "failed", MessagesThroughSeq: throughPtr(5)})
		if !errors.Is(err, ErrMessagesPending) {
			t.Fatalf("failed report err = %v, want ErrMessagesPending", err)
		}
		if applied {
			t.Fatal("a fenced failure must not be applied")
		}
		if got := statusOf(t, env, id); got != "running" {
			t.Fatalf("status = %q, want running (the refused failure must not transition)", got)
		}
	})
}

// TestSetStateTerminalFenceGapUnrecoverableLiveDB pins the distinct unrecoverable-gap classification
// (PRD #1391 Run B M3c): a `through` more than WorkerGapFillMax above the stored count is
// ErrGapUnrecoverable (→ typed 409 gap_unrecoverable), NOT ErrMessagesPending and NOT the 400
// ErrInvalidState — the hole is too large to ever fill, so the worker must stop re-parking.
func TestSetStateTerminalFenceGapUnrecoverableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams()) // WorkerGapFillMax = 10000
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}

	id := seedRunningRun(t, env, o, 6100, 3)
	insertRunMsg(t, env, id, 1)
	insertRunMsg(t, env, id, 2)
	insertRunMsg(t, env, id, 3)
	// through far above the stored count of 3: shortfall 19997 > WorkerGapFillMax (10000).
	_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "completed", MessagesThroughSeq: throughPtr(20000)})
	if !errors.Is(err, ErrGapUnrecoverable) {
		t.Fatalf("err = %v, want ErrGapUnrecoverable", err)
	}
	if errors.Is(err, ErrMessagesPending) {
		t.Fatal("an unrecoverable gap must NOT be classified as messages_pending")
	}
	if errors.Is(err, ErrInvalidState) {
		t.Fatal("an unrecoverable gap is a 409, NOT a 400 ErrInvalidState")
	}
	if applied {
		t.Fatal("an unrecoverable-gap completion must not be applied")
	}
	if got := statusOf(t, env, id); got != "running" {
		t.Fatalf("status = %q, want running (the refused completion must not transition)", got)
	}
}

// TestRunMessageGapsLiveDB executes the hardened message-gaps read (PRD #1391 Run B M3c): the
// holes for a {1,3} run, the atomic worker/generation fence (a foreign worker, released claim and
// stale same-worker generation all 404 via ErrRunNotOwned), and keyset pagination over a HUGE
// `through` with a small limit, proving the query never materialises `through` rows and pages bounded
// {first,last} ranges with a cursor.
func TestRunMessageGapsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}

	t.Run("returns the interior hole for {1,3}", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6200, 3)
		insertRunMsg(t, env, id, 1)
		insertRunMsg(t, env, id, 3)
		page, err := svc.RunMessageGaps(env.ctx, wkr, id, 0, 3, 0, 256)
		if err != nil {
			t.Fatalf("RunMessageGaps: %v", err)
		}
		if len(page.Gaps) != 1 || page.Gaps[0].First != 2 || page.Gaps[0].Last != 2 {
			t.Fatalf("gaps = %+v, want exactly [{2,2}]", page.Gaps)
		}
		if page.NextCursor != nil {
			t.Fatalf("next_cursor = %v, want nil (the whole gap set fit in one page)", *page.NextCursor)
		}
	})

	t.Run("a foreign worker gets ErrRunNotOwned", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6201, 2)
		insertRunMsg(t, env, id, 2)
		foreign := store.Worker{ID: uuid.New(), UserID: o.userID}
		_, err := svc.RunMessageGaps(env.ctx, foreign, id, 0, 2, 0, 256)
		if !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("err = %v, want ErrRunNotOwned (a foreign worker must not read the gaps)", err)
		}
	})

	t.Run("a released claim gets ErrRunNotOwned", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6202, 2)
		insertRunMsg(t, env, id, 2)
		// A held-state switch RELEASES the claim; the superseded flight must not inspect the gaps.
		env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
		_, err := svc.RunMessageGaps(env.ctx, wkr, id, 0, 2, 0, 256)
		if !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("err = %v, want ErrRunNotOwned (a released/superseded claim must not read the gaps)", err)
		}
	})

	t.Run("a stale generation cannot read a same-worker reclaim", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6204, 2)
		insertRunMsg(t, env, id, 2)
		setRunGeneration(t, env, id, 8) // same worker, newer flight

		if _, err := svc.RunMessageGaps(env.ctx, wkr, id, 7, 2, 0, 256); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("stale generation err = %v, want ErrRunNotOwned", err)
		}
		page, err := svc.RunMessageGaps(env.ctx, wkr, id, 8, 2, 0, 256)
		if err != nil {
			t.Fatalf("current generation: %v", err)
		}
		if len(page.Gaps) != 1 || page.Gaps[0] != (MessageGap{First: 1, Last: 1}) {
			t.Fatalf("current generation gaps = %+v, want [{1 1}]", page.Gaps)
		}
	})

	t.Run("keyset pages a huge through with a small limit", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 6203, 9)
		// present = {2,5,9}; every other seq in [1..through] is a hole. With through = 1_000_000 a
		// naive materialisation would enumerate a million rows — this query derives the gaps from
		// the three present rows + one sentinel, so it returns promptly and bounded.
		insertRunMsg(t, env, id, 2)
		insertRunMsg(t, env, id, 5)
		insertRunMsg(t, env, id, 9)
		const through = int64(1_000_000)
		const limit = int64(2)
		var got [][2]int64
		cursor := int64(0)
		pages := 0
		for {
			page, err := svc.RunMessageGaps(env.ctx, wkr, id, 0, through, cursor, limit)
			if err != nil {
				t.Fatalf("page at cursor %d: %v", cursor, err)
			}
			if int64(len(page.Gaps)) > limit {
				t.Fatalf("page returned %d gaps, want <= limit %d", len(page.Gaps), limit)
			}
			for _, g := range page.Gaps {
				got = append(got, [2]int64{g.First, g.Last})
			}
			pages++
			if pages > 100 {
				t.Fatal("keyset walk did not terminate within 100 pages")
			}
			if page.NextCursor == nil {
				break
			}
			if *page.NextCursor <= cursor {
				t.Fatalf("next_cursor %d did not advance past cursor %d", *page.NextCursor, cursor)
			}
			cursor = *page.NextCursor
		}
		want := [][2]int64{{1, 1}, {3, 4}, {6, 8}, {10, through}}
		if len(got) != len(want) {
			t.Fatalf("gaps = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("gap %d = %v, want %v (full set %v)", i, got[i], want[i], got)
			}
		}
	})
}
