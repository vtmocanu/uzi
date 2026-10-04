package workersvc

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The single Begin hook commits after SetState's initial read and before its
// locked reread. It runs once and a failed directive fails the test.
type scopeCapOnBegin struct {
	e       interlockLiveDB
	t       *testing.T
	svc     *Service
	runID   uuid.UUID
	ceiling int
	fired   bool
}

func (b *scopeCapOnBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	if !b.fired {
		b.fired = true
		if _, err := b.svc.SubmitInput(ctx, b.e.userID, b.runID, "scope", fmt.Sprint(b.ceiling), nil); err != nil {
			b.t.Fatalf("racing scope directive: %v", err)
		}
	}
	return b.e.pool.Begin(ctx)
}

func TestScopeCapCompletionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name           string
		initial, raise int
		race, applied  bool
	}{
		{"reached", 2, 0, false, true},
		{"zero ceiling report only", 0, 0, false, true},
		{"unreached raise after grant", 2, 3, false, false},
		{"unreached raise before lock", 2, 3, true, false},
		{"still reached raise after grant", 1, 2, false, true},
		{"still reached raise before lock", 1, 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			wid := e.seedWorker(t, []string{"completion_interlock_v1"})
			completed := []string{"m4"}
			if tc.initial == 0 {
				completed = nil
			}
			runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3", "m4"}, completed, false)
			w := store.Worker{ID: wid}
			if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "scope", fmt.Sprint(tc.initial), nil); err != nil {
				t.Fatal(err)
			}
			if tc.initial != 0 {
				attempt, err := svc.RecordCompletionAttempt(e.ctx, w, runID, CompletionAttemptRequest{Head: "h", MilestonesCompleted: []string{"m2", "m4"}})
				if err != nil || len(attempt.Unmet) != 2 || attempt.Unmet[0] != "m1" || attempt.Unmet[1] != "m3" {
					t.Fatalf("ordinary attempt=%+v err=%v", attempt, err)
				}
			}
			var persistedBefore []byte
			if err := e.pool.QueryRow(e.ctx, `SELECT milestones_completed FROM runs WHERE id=$1`, runID).Scan(&persistedBefore); err != nil {
				t.Fatal(err)
			}
			permit, err := svc.RequestCompletionPermit(e.ctx, w, runID, cappedPermitRequest(t, true))
			if err != nil || !permit.Granted {
				t.Fatalf("permit=%+v err=%v", permit, err)
			}
			ordinary, ok, err := svc.SetState(e.ctx, w, runID, StateRequest{State: "completed", Branch: strPtr("b"), Head: strPtr("h")})
			if err != nil || ok || ordinary.Status != "running" {
				t.Fatalf("capped permit used as ordinary completion: run=%+v applied=%v err=%v", ordinary, ok, err)
			}
			var hook *scopeCapOnBegin
			if tc.raise != 0 {
				if tc.race {
					hook = &scopeCapOnBegin{e: e, t: t, svc: svc, runID: runID, ceiling: tc.raise}
					svc.SetTxBeginner(hook)
				} else if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "scope", fmt.Sprint(tc.raise), nil); err != nil {
					t.Fatal(err)
				}
			}
			capped := true
			req := StateRequest{State: "completed", Branch: strPtr("b"), Head: strPtr("h"), ScopeCapped: &capped}
			const noWorkReport = "No work performed: scope ceiling is zero."
			if tc.initial == 0 {
				reportOnly := true
				req.ReportOnly = &reportOnly
				req.ReportMd = strPtr(noWorkReport)
			}
			run, applied, err := svc.SetState(e.ctx, w, runID, req)
			if err != nil || applied != tc.applied {
				t.Fatalf("applied=%v run=%+v err=%v", applied, run, err)
			}
			if tc.initial == 0 {
				if !run.ReportOnly {
					t.Fatal("zero-ceiling completion is not report-only")
				}
				var storedReport string
				if err := e.pool.QueryRow(e.ctx, `SELECT report_md FROM runs WHERE id=$1`, runID).Scan(&storedReport); err != nil {
					t.Fatal(err)
				}
				if storedReport != noWorkReport {
					t.Fatalf("stored report=%q want=%q", storedReport, noWorkReport)
				}
			}
			if hook != nil && !hook.fired {
				t.Fatal("Begin hook did not fire")
			}
			var consumed bool
			if err := e.pool.QueryRow(e.ctx, `SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE id=$1`, uuid.MustParse(permit.Permit.ID)).Scan(&consumed); err != nil {
				t.Fatal(err)
			}
			var persistedAfter []byte
			if err := e.pool.QueryRow(e.ctx, `SELECT milestones_completed FROM runs WHERE id=$1`, runID).Scan(&persistedAfter); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(persistedBefore, persistedAfter) {
				t.Fatalf("persisted milestones changed: before=%s after=%s", persistedBefore, persistedAfter)
			}
			if consumed != tc.applied {
				t.Fatalf("consumed=%v want=%v", consumed, tc.applied)
			}
			rows, err := e.pool.Query(e.ctx, `SELECT disposition FROM run_user_inputs WHERE run_id=$1 AND kind='scope' ORDER BY id`, runID)
			if err != nil {
				t.Fatal(err)
			}
			dispositions, err := pgx.CollectRows(rows, pgx.RowTo[*string])
			if err != nil {
				t.Fatal(err)
			}
			var latest *string
			if tc.applied {
				latest = strPtr("applied")
			}
			wantDispositions := []*string{latest}
			if tc.raise != 0 {
				wantDispositions = []*string{strPtr("superseded"), latest}
			}
			if len(dispositions) != len(wantDispositions) {
				t.Fatalf("scope audit rows=%d want=%d", len(dispositions), len(wantDispositions))
			}
			for i, want := range wantDispositions {
				got := dispositions[i]
				if want == nil {
					if got != nil {
						t.Fatalf("scope audit row %d disposition=%q want=NULL", i, *got)
					}
				} else if got == nil || *got != *want {
					t.Fatalf("scope audit row %d disposition=%v want=%q", i, got, *want)
				}
			}
			if tc.applied {
				if run.Status != "completed" || run.StopKind.String != "scope_capped" {
					t.Fatalf("status=%s stop=%v", run.Status, run.StopKind)
				}
				// A later edit must not invalidate a response-loss retry of an already consumed permit.
				e.exec(t, `UPDATE runs SET scope_ceiling=4 WHERE id=$1`, runID)
				retry, ok, err := svc.SetState(e.ctx, w, runID, req)
				if err != nil || !ok || retry.Status != "completed" || retry.StopKind.String != "scope_capped" {
					t.Fatalf("retry=%+v applied=%v err=%v", retry, ok, err)
				}
			} else if run.Status != "running" {
				t.Fatalf("rejected status=%s", run.Status)
			}
		})
	}
}

func TestScopeCapTerminalDeclarationCannotAuthorizeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil)
	w := store.Worker{ID: wid}
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
	// A previously issued ordinary permit cannot bypass a changed persisted unmet set.
	e.seedPermit(t, runID, wid, 1, "h")
	declared := []string{"m1", "m2"}
	_, applied, err := svc.SetState(e.ctx, w, runID, StateRequest{State: "completed", Branch: strPtr("agent/issue-1"), Head: strPtr("h"), MilestonesCompleted: &declared})
	if err != nil || applied {
		t.Fatalf("terminal declaration applied=%v err=%v", applied, err)
	}
}
