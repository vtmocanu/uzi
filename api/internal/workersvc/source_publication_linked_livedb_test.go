package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// PRD #4173 D11 wiring: a run on a repo linked to an issue tracker records its source
// publication as non_closing by the repo link, even when the worker reported nothing and the
// issue row says nothing; an unlinked run on the same path still records closing.
func TestSourcePublicationIsNonClosingForALinkedRepoLiveDB(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "unlinked repo records closing"
		want := "closing"
		if linked {
			name, want = "linked repo records non_closing", "non_closing"
		}
		t.Run(name, func(t *testing.T) {
			f := seedSourcePublication(t, false)
			if linked {
				conn := uuid.New()
				f.e.exec(t, `INSERT INTO issue_tracker_connections (id, user_id, kind, base_url, account_id, account_login, token_ciphertext)
				 VALUES ($1, $2, 'youtrack', 'https://issues.example.com', 'a', 'b', 'c')`, conn, f.e.userID)
				f.e.exec(t, `UPDATE repos SET issue_tracker_connection_id=$2, issue_tracker_project_id='0-1', issue_tracker_project_short='ABC',
				 eligibility_tag_id='6-1', eligibility_tag_name='uzi', issue_tracker_linked_at=now()
				 WHERE id=(SELECT repo_id FROM runs WHERE id=$1)`, f.id, conn)
			}
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			if err != nil || !result.Applied {
				t.Fatalf("completion: %+v %v", result, err)
			}
			_, b, _ := f.stored(t)
			var stamp SourcePublicationIntent
			if err := json.Unmarshal(b, &stamp); err != nil {
				t.Fatal(err)
			}
			if stamp.Intent != want {
				t.Fatalf("intent = %q, want %q (stamp %s)", stamp.Intent, want, b)
			}
			if stamp.ReportedNonClosing {
				t.Fatalf("the worker's own report must stay as reported (false): %s", b)
			}
		})
	}
}
