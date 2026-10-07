package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
)

func TestRunNowRemovalMapping(t *testing.T) {
	iid := int64(7)
	for _, flags := range []struct{ removed, failed bool }{{true, false}, {false, true}, {false, false}} {
		id := uuid.New()
		got := runNowResponse(schedsvc.FireOutcome{Matched: 1, Started: []schedsvc.Started{{
			IssueIID: &iid, RunID: id, Title: "candidate", WebURL: "https://forge.e2e/7",
			LabelRemoved: flags.removed, LabelRemoveFailed: flags.failed, SelectorLabel: "on-deck",
		}}})
		if got.Created != 1 || len(got.Started) != 1 || len(got.RunIDs) != 1 || got.RunIDs[0] != id.String() {
			t.Fatalf("response=%+v", got)
		}
		s := got.Started[0]
		if s.LabelRemoved != flags.removed || s.LabelRemoveFailed != flags.failed || s.SelectorLabel != "on-deck" || s.WebURL != "https://forge.e2e/7" || s.IssueIID == nil || *s.IssueIID != 7 || s.RunID != id.String() {
			t.Fatalf("started=%+v", s)
		}
	}
}
