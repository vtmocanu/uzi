package workersvc

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Companion to TestPrerequisiteRepeatedPrepareBudgetExhaustionLiveDB: the late
// row must not spend an attempt when earlier rows consume the sweep's budget.
func TestPrerequisiteEarlierItemsDeferLatePreparationLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	earlySHAs := make([]string, 6)
	for i := range earlySHAs {
		earlySHAs[i] = fmt.Sprintf("%040x", 100+i)
	}
	early := make([]store.RecoveryPrerequisiteClosure, 3)
	for i := range early {
		early[i] = prerequisiteBudgetSiblingLiveDB(t, f, earlySHAs)
	}
	listing := prerequisiteBudgetSiblingLiveDB(t, f, f.m.row.PrerequisiteShas)
	prerequisiteBudgetUnknownLiveDB(t, f, listing)
	n, err := f.env.q.DeferPrerequisitePreparation(f.env.ctx, store.DeferPrerequisitePreparationParams{
		ID: f.m.row.ID, Generation: 0,
	})
	if err != nil || n != 1 {
		t.Fatalf("schedule late row: rows=%d err=%v", n, err)
	}
	f.advanceLiveDB(t, 30*time.Second)

	read := func(row store.RecoveryPrerequisiteClosure) store.RecoveryPrerequisiteClosure {
		t.Helper()
		current, e := f.env.q.GetPrerequisiteClosure(f.env.ctx, store.GetPrerequisiteClosureParams{
			CaptureID: row.CaptureID, UserID: row.UserID,
		})
		if e != nil {
			t.Fatal(e)
		}
		return current
	}
	proofs := 0
	perSHA := map[string]int{}
	f.builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(_ context.Context, _ int64, _, sha string) (forge.Ancestry, error) {
		proofs++
		perSHA[sha]++
		if slices.Contains(earlySHAs, sha) {
			f.advanceLiveDB(t, 750*time.Millisecond)
		} else if sha != f.m.row.PrerequisiteShas[0] {
			return forge.AncestryUnknown, fmt.Errorf("unexpected prerequisite %s", sha)
		}
		return forge.AncestryAncestor, nil
	}}
	// The listing lane constructs one metadata driver, independently of the
	// four preparation drivers counted alongside the ancestry proofs below.
	for pass := 1; pass <= 4; pass++ {
		if _, e := f.s.SweepPrerequisiteClosures(f.env.ctx); e != nil {
			t.Fatal(e)
		}
		late := f.readLiveDB(t)
		if pass <= 3 {
			if late.State != "pending" || late.Attempts != 0 || late.InvokedAt.Valid || late.LiveRunID.Valid || f.h.sends != 0 {
				t.Fatalf("pass=%d late state=%s attempts=%d marker=%t live=%t sends=%d", pass, late.State, late.Attempts, late.InvokedAt.Valid, late.LiveRunID.Valid, f.h.sends)
			}
			if proofs != pass*6 || f.builder.builds != pass+1 {
				t.Fatalf("pass=%d proofs=%d builds=%d", pass, proofs, f.builder.builds)
			}
		} else {
			if late.State != "pinned" || late.Attempts != 1 || !late.InvokedAt.Valid || !late.LiveRunID.Valid || f.h.sends != 1 || proofs != 19 || f.builder.builds != 5 {
				t.Fatalf("full turn: state=%s attempts=%d marker=%t live=%t sends=%d proofs=%d builds=%d", late.State, late.Attempts, late.InvokedAt.Valid, late.LiveRunID.Valid, f.h.sends, proofs, f.builder.builds)
			}
		}
		started := 0
		for _, row := range early {
			current := read(row)
			if current.Attempts == 1 {
				started++
				if current.State != "pending" || current.Reason != "" || current.AttemptDeadline.Valid || !current.NextAttemptAt.Time.After(f.m.now) {
					t.Fatalf("early retry: state=%s reason=%s deadline=%t next=%s now=%s", current.State, current.Reason, current.AttemptDeadline.Valid, current.NextAttemptAt.Time, f.m.now)
				}
			} else if current.Attempts != 0 || current.State != "pending" {
				t.Fatalf("early state=%s attempts=%d", current.State, current.Attempts)
			}
			if current.InvokedAt.Valid || current.LiveRunID.Valid {
				t.Fatalf("early row acquired marker/live pointer: %s", current.ID)
			}
		}
		if started != min(pass, 3) {
			t.Fatalf("pass=%d started early rows=%d", pass, started)
		}
		unknown := read(listing)
		if unknown.State != "create_unknown" || unknown.Attempts != 1 || !unknown.LiveRunID.Valid || unknown.ReconciliationAttempts != 1 || !unknown.NextAttemptAt.Time.After(f.m.now) {
			t.Fatalf("listing lane: state=%s attempts=%d live=%t reconciliations=%d next=%s now=%s", unknown.State, unknown.Attempts, unknown.LiveRunID.Valid, unknown.ReconciliationAttempts, unknown.NextAttemptAt.Time, f.m.now)
		}
	}
	for _, sha := range earlySHAs {
		if perSHA[sha] != 3 {
			t.Fatalf("proof cache reused for %s: comparisons=%d want=3", sha, perSHA[sha])
		}
	}
	if perSHA[f.m.row.PrerequisiteShas[0]] != 1 {
		t.Fatalf("late comparisons=%d want=1", perSHA[f.m.row.PrerequisiteShas[0]])
	}
}

func prerequisiteBudgetSiblingLiveDB(t *testing.T, f *prerequisiteExecutorLiveDB, shas []string) store.RecoveryPrerequisiteClosure {
	t.Helper()
	id := uuid.New()
	f.env.exec(`INSERT INTO recovery_captures
 (id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,
 source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,
 prerequisite_shas,reserved_bytes,coverage_digest,expires_at)
 SELECT $1,hold_id,run_id,user_id,original_worker_id,original_worker_identity,
 source_sha,$1::uuid::text,'available',true,byte_size,checksum,chunk_count,
 $3::text[],reserved_bytes,coverage_digest,expires_at
 FROM recovery_captures WHERE id=$2`, id, f.m.row.CaptureID, shas)
	row, err := f.env.q.InsertPrerequisiteClosure(f.env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// Install a genuine invoked binding under the production lock order, then use
// the generated outcome query to enter the listing-only lane without a send.
func prerequisiteBudgetUnknownLiveDB(t *testing.T, f *prerequisiteExecutorLiveDB, row store.RecoveryPrerequisiteClosure) {
	t.Helper()
	reserved, err := f.env.q.ReservePrerequisitePreparation(f.env.ctx, store.ReservePrerequisitePreparationParams{
		ID: row.ID, LeaseSeconds: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.prerequisiteTx(f.env.ctx, func(q PrerequisiteStore) error {
		if _, e := q.LockPrerequisiteRun(f.env.ctx, row.RunID); e != nil {
			return e
		}
		if _, e := q.LockPrerequisiteHold(f.env.ctx, row.HoldID); e != nil {
			return e
		}
		if _, e := q.LockPrerequisiteCapture(f.env.ctx, row.CaptureID); e != nil {
			return e
		}
		if _, e := q.LockPrerequisiteClosure(f.env.ctx, row.ID); e != nil {
			return e
		}
		_, e := q.MarkPrerequisiteInvoked(f.env.ctx, store.MarkPrerequisiteInvokedParams{
			ID: row.ID, Generation: reserved.Attempts, SendSeconds: 4,
			PinTip:       prerequisiteText(prerequisiteTestTip),
			ConnectionID: prerequisiteID(f.m.conn.ID), ForgeKind: prerequisiteText(f.m.conn.ForgeType),
			NormalizedBaseUrl: prerequisiteText(strings.TrimRight(f.m.conn.BaseUrl, "/")),
			ProjectID:         pgtype.Int8{Int64: f.m.repo.ForgeProjectID, Valid: true}, DefaultBranch: f.m.repo.DefaultBranch,
		})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.env.q.MarkPrerequisiteCreateUnknown(f.env.ctx, store.MarkPrerequisiteCreateUnknownParams{
		ID: row.ID, Generation: reserved.Attempts,
	})
	if err != nil || n != 1 {
		t.Fatalf("listing fixture: rows=%d err=%v", n, err)
	}
}
