package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeSnapshotCleanupBody(t *testing.T) {
	lead, child := uuid.New(), uuid.New()
	head := strings.Repeat("b", 40)
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	base := store.GetCodeSnapshotCleanupRow{LeadRunID: lead, LeadStatus: "completed", OwnedByWorker: true}
	decided := base
	decided.RowPresent, decided.LeadClaimGeneration = true, 4
	decided.HeadCommit, decided.Outcome = text(head), text("completed")

	childRow := decided
	childRow.ChildMapped = true
	childRow.CheckerRunID = pgtype.UUID{Bytes: child, Valid: true}
	childRow.CheckerClaimGeneration, childRow.CheckerStatus = 7, text("completed")

	unclaimed := childRow
	unclaimed.CheckerClaimGeneration, unclaimed.CheckerStatus = 0, text("queued")

	noChild := decided // row present, no mapped child: a valid answer

	cases := []struct {
		name string
		row  store.GetCodeSnapshotCleanupRow
		want string // exact JSON, or "" for an error
	}{
		{"no row", base, `{"protocol":"code_snapshot_cleanup_v1","lead_run_id":"` + lead.String() + `","lead_status":"completed","owned_by_worker":true,"row_present":false,"lead_claim_generation":0,"head_commit":null,"outcome":null,"child_present":false,"checker_run_id":null,"checker_claim_generation":0,"checker_status":null}`},
		{"no child", noChild, `{"protocol":"code_snapshot_cleanup_v1","lead_run_id":"` + lead.String() + `","lead_status":"completed","owned_by_worker":true,"row_present":true,"lead_claim_generation":4,"head_commit":"` + head + `","outcome":"completed","child_present":false,"checker_run_id":null,"checker_claim_generation":0,"checker_status":null}`},
		{"claimed child", childRow, `{"protocol":"code_snapshot_cleanup_v1","lead_run_id":"` + lead.String() + `","lead_status":"completed","owned_by_worker":true,"row_present":true,"lead_claim_generation":4,"head_commit":"` + head + `","outcome":"completed","child_present":true,"checker_run_id":"` + child.String() + `","checker_claim_generation":7,"checker_status":"completed"}`},
		{"never-claimed child", unclaimed, `{"protocol":"code_snapshot_cleanup_v1","lead_run_id":"` + lead.String() + `","lead_status":"completed","owned_by_worker":true,"row_present":true,"lead_claim_generation":4,"head_commit":"` + head + `","outcome":"completed","child_present":true,"checker_run_id":"` + child.String() + `","checker_claim_generation":0,"checker_status":"queued"}`},
	}
	for _, c := range cases {
		got, err := codeSnapshotCleanupBody(c.row)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		raw, _ := json.Marshal(got)
		if string(raw) != c.want {
			t.Fatalf("%s:\n got %s\nwant %s", c.name, raw, c.want)
		}
	}

	// A mapped child that does not resolve, or a row without an outcome, is an error and
	// never a "no child" answer: the worker keeps the ref on a failed lookup.
	unresolved := decided
	unresolved.ChildMapped = true
	noOutcome := decided
	noOutcome.Outcome = pgtype.Text{}
	orphanChild := base
	orphanChild.CheckerRunID = pgtype.UUID{Bytes: child, Valid: true}
	noStatus := childRow
	noStatus.CheckerStatus = pgtype.Text{}
	for name, row := range map[string]store.GetCodeSnapshotCleanupRow{
		"unresolved child mapping": unresolved, "row without outcome": noOutcome,
		"child without row": orphanChild, "child without status": noStatus,
	} {
		if _, err := codeSnapshotCleanupBody(row); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}
