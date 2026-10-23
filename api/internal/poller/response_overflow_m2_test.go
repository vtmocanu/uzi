package poller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestM2AutopilotOverflowRecordAndRetry(t *testing.T) {
	for _, mode := range []string{"overflow", "record_failure", "comment_failure", "transient", "unknown", "bare", "error", "prerequisite"} {
		t.Run(mode, func(t *testing.T) {
			ops := []string{}
			st := &apStore{candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")}, cc: ccOwner("alice", true, true), ops: &ops}
			f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(9, "alice")}}, issueErr: fmt.Errorf("wrapped: %w", &forge.ResponseTooLargeError{Provenance: forge.ResponseIssueSuccess}), ops: &ops}
			runs := &apRuns{}
			if mode == "record_failure" {
				st.upsertErr = errors.New("db unavailable")
			}
			if mode == "comment_failure" {
				f.noteErr = errors.New("comment unavailable")
			}
			transient := mode == "transient" || mode == "unknown" || mode == "bare" || mode == "error" || mode == "prerequisite"
			switch mode {
			case "transient":
				f.issueErr = errors.New("temporary")
			case "unknown":
				f.issueErr = &forge.ResponseTooLargeError{}
			case "bare":
				f.issueErr = forge.ErrResponseTooLarge
			case "error":
				f.issueErr = &forge.ResponseTooLargeError{Provenance: forge.ResponseError, Outcome: forge.ErrorClassRateLimited}
			case "prerequisite":
				f.issueErr = &forge.ResponseTooLargeError{Provenance: forge.ResponseSuccess}
			}
			detectWith(st, runs, f)
			if len(runs.calls) != 0 || len(runs.resolutions) != 0 {
				t.Fatal("failed capture reached harness/create")
			}
			if mode == "record_failure" || transient {
				if len(f.noteAttempts) != 0 || len(st.upserts) != 0 {
					t.Fatal("failed record/transient capture consumed event")
				}
				st.upsertErr = nil
				if transient {
					f.issueErr = nil
				}
				detectWith(st, runs, f)
				if transient && len(runs.calls) != 1 {
					t.Fatal("transient capture did not retry")
				}
				if mode == "record_failure" && len(f.noteAttempts) != 1 {
					t.Fatal("failed record did not retry")
				}
			} else {
				if len(ops) != 2 || ops[0] != "record" || ops[1] != "comment" || len(f.noteAttempts) != 1 || f.noteAttempts[0].body != tooLargeComment() {
					t.Fatalf("ordering: %v notes=%v", ops, f.noteAttempts)
				}
				detectWith(st, runs, f)
				if f.fetches != 1 || len(f.noteAttempts) != 1 {
					t.Fatal("consumed event retried")
				}
			}
		})
	}
}
