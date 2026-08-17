package store_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The DISPLAY class path (PRD #320 M3 / D8) against a REAL Postgres: the same
// fn_run_priority_class the read queries select, proving the four label values line up
// with the rank ClaimRun orders by (fn_run_priority) over identical inputs. Two halves:
//
//   - RunPriorityClass (the pure scalar read query the bare-store DTO paths call): a
//     demoted judge run reads `background` when fresh and `restored` once past grace, an
//     issue reads `normal`, and a manual override reads `expedited` — so `restored` and
//     `normal` (which both RANK 1) stay DISTINGUISHABLE as labels, the whole reason the
//     class function exists apart from the rank.
//   - ListRunsForUser (the list read that embeds the class as a row column): a normal
//     issue row carries priority_class `normal` and an expedited one `expedited`, so the
//     Runs-list pill is the same SQL decision as the claim order. (judge/self_improve are
//     hidden from that list by kind, so the demoted labels are proven via RunPriorityClass
//     above rather than the list.)
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunPriorityClassLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	i2 := func(n int16) pgtype.Int2 { return pgtype.Int2{Int16: n, Valid: true} }

	cases := []struct {
		name    string
		kind    string
		prio    pgtype.Int2
		isStale bool
		want    string
	}{
		{"fresh judge is background", "judge", pgtype.Int2{}, false, "background"},
		{"stale judge restores", "judge", pgtype.Int2{}, true, "restored"},
		{"issue is normal", "issue", pgtype.Int2{}, false, "normal"},
		{"override is expedited", "issue", i2(2), false, "expedited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fx.q.RunPriorityClass(fx.ctx, store.RunPriorityClassParams{
				RunKind: tc.kind, Priority: tc.prio, IsStale: tc.isStale,
			})
			if err != nil {
				t.Fatalf("RunPriorityClass: %v", err)
			}
			if got != tc.want {
				t.Fatalf("class(kind=%s prio=%+v stale=%v) = %q, want %q", tc.kind, tc.prio, tc.isStale, got, tc.want)
			}
		})
	}
}

// TestListRunsForUserPriorityClassLiveDB proves the list read query embeds the class as
// a row column computed from @background_grace_cutoff, so the pill and the claim order can
// never disagree on the Runs list.
func TestListRunsForUserPriorityClassLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	now := time.Now()
	normalID := fx.insertPRun("issue", nil, nil, now.Add(-1*time.Minute))
	expeditedID := fx.insertPRun("issue", prio(2), nil, now.Add(-2*time.Minute))

	rows, err := fx.q.ListRunsForUser(fx.ctx, store.ListRunsForUserParams{
		UserID:                fx.userID,
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("ListRunsForUser: %v", err)
	}
	byID := map[string]string{}
	for _, r := range rows {
		byID[r.Run.ID.String()] = r.PriorityClass
	}
	if got := byID[normalID.String()]; got != "normal" {
		t.Fatalf("normal issue row priority_class = %q, want normal", got)
	}
	if got := byID[expeditedID.String()]; got != "expedited" {
		t.Fatalf("expedited issue row priority_class = %q, want expedited", got)
	}
}
