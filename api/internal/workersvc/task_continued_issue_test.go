package workersvc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type linkedTaskForge struct {
	forgetest.BaseFake
	issue        forge.Issue
	err          error
	project, iid int64
	calls        int
}

func (f *linkedTaskForge) GetIssue(_ context.Context, project, iid int64) (forge.Issue, error) {
	f.calls++
	f.project, f.iid = project, iid
	return f.issue, f.err
}

func TestLinkedTaskValidation(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		iid                     int64
		repoErr, fetchErr, want error
		pr, active              bool
	}{
		{name: "open unlabeled issue", iid: 17},
		{name: "closed issue", iid: 18},
		{name: "ownership first", iid: -1, repoErr: pgx.ErrNoRows, want: ErrRepoNotFound},
		{name: "zero", iid: 0, want: ErrContinuedIssueInvalid},
		{name: "negative", iid: -1, want: ErrContinuedIssueInvalid},
		{name: "PR", iid: 17, pr: true, want: ErrContinuedIssueInvalid},
		{name: "missing", iid: 17, fetchErr: &forge.IssueNotFoundError{Err: errors.New("404 not found")}, want: ErrContinuedIssueNotFound},
		{name: "mismatched issue", iid: 17, want: ErrContinuedIssueNotFound},
		{name: "empty response", iid: 17, want: ErrContinuedIssueNotFound},
		{name: "outage", iid: 17, fetchErr: errors.New("unavailable private credential"), want: ErrForgeIssueRead},
		{name: "active including pool_wait", iid: 17, active: true, want: ErrActiveRunExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := aValidRepoRow()
			row.ForgeProjectID = 621
			fs := &fakeStore{repoRow: row, repoErr: tc.repoErr, hasActiveRunForIssue: tc.active}
			svc := New(fs, newBox(t), testParams())
			f := &linkedTaskForge{issue: forge.Issue{IID: tc.iid, State: "opened", IsPullRequest: tc.pr}, err: tc.fetchErr}
			if tc.name == "mismatched issue" {
				f.issue.IID = 18
			}
			if tc.name == "empty response" {
				f.issue = forge.Issue{}
			}
			if tc.name == "closed issue" {
				f.issue.State = "closed"
			}
			svc.SetForges(m2Builder{f})
			_, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "task", "", false, false, false, false, &tc.iid)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want %v", err, tc.want)
			}
			if tc.want != nil {
				if fs.taskRunParams != nil {
					t.Fatal("insert after refusal")
				}
			} else if fs.taskRunParams == nil || fs.taskRunParams.ContinuedIssueIid != (pgtype.Int8{Int64: tc.iid, Valid: true}) {
				t.Fatalf("link not persisted: %+v", fs.taskRunParams)
			}
			wantCalls := 1
			if tc.repoErr != nil || tc.iid <= 0 {
				wantCalls = 0
			}
			if f.calls != wantCalls {
				t.Fatalf("forge calls=%d want %d", f.calls, wantCalls)
			}
			if wantCalls > 0 && (f.project != 621 || f.iid != tc.iid) {
				t.Fatalf("wrong forge scope %d/%d", f.project, f.iid)
			}
		})
	}
}

// The real SDK's oversized 404 must be a failed validation, never an absent
// issue or a successful insertion. Flushing headers makes the body unknown-length.
func TestLinkedTaskBoundedValidation(t *testing.T) {
	for _, driver := range []forge.Type{forge.TypeGitHub, forge.TypeGitLab, forge.TypeForgejo} {
		t.Run(string(driver), func(t *testing.T) {
			issueReceived := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/issues/17") {
					select {
					case issueReceived <- struct{}{}:
					default:
					}
					w.WriteHeader(http.StatusNotFound)
					w.(http.Flusher).Flush()
					_, _ = w.Write([]byte("{\"message\":\"" + strings.Repeat("x", 5<<10) + "\"}"))
					return
				}
				_, _ = w.Write([]byte(`{"id":621,"name":"widgets","owner":{"login":"acme"}}`))
			}))
			defer srv.Close()
			f, err := forge.New(driver, srv.URL, "fixture", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			fs := &fakeStore{repoRow: aValidRepoRow()}
			svc := New(fs, newBox(t), testParams())
			svc.SetForges(m2Builder{f})
			iid := int64(17)
			_, err = svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "task", "", false, false, false, false, &iid)
			if !errors.Is(err, ErrForgeIssueRead) || fs.taskRunParams != nil {
				t.Fatalf("oversized validation err=%v inserted=%v", err, fs.taskRunParams != nil)
			}
			select {
			case <-issueReceived:
			default:
				t.Fatal("validation failed before the issue endpoint received /issues/17")
			}
		})
	}
}

func TestLinkedTaskConstraintMapping(t *testing.T) {
	for _, kind := range []string{"initial", "review", "fix"} {
		for _, constraint := range []string{"uq_runs_one_active_per_issue", "uq_one_active_task_review_per_target", "uq_one_active_then_fix_per_target", "runs_pkey"} {
			t.Run(kind+"/"+constraint, func(t *testing.T) {
				raw := &pgconn.PgError{Code: "23505", ConstraintName: constraint}
				fs := &fakeStore{repoRow: aValidRepoRow(), taskRunErr: raw, taskReviewRunErr: raw, thenFixRunErr: raw, hasAnthropicToken: true}
				svc := New(fs, newBox(t), testParams())
				var err error
				switch kind {
				case "initial":
					_, err = svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "task", "", false, false, false, false, nil)
				case "review":
					_, err = svc.CreateTaskReviewRun(context.Background(), uuid.New(), uuid.New(), uuid.New(), "uzi/task/a", "main", HarnessClaude)
				case "fix":
					_, err = svc.CreateThenFixRun(context.Background(), uuid.New(), uuid.New(), uuid.New(), "uzi/task/a", "main", "fix", pgtype.Int4{}, pgtype.Int4{}, HarnessClaude)
				}
				want := error(raw)
				switch constraint {
				case "uq_runs_one_active_per_issue":
					want = ErrActiveRunExists
				case "uq_one_active_task_review_per_target":
					want = ErrTaskReviewAlreadyActive
				case "uq_one_active_then_fix_per_target":
					want = ErrThenFixAlreadyActive
				}
				if !errors.Is(err, want) {
					t.Fatalf("error=%v want %v", err, want)
				}
			})
		}
	}
}

func TestLinkedTaskSweepPoolConflictNoNotification(t *testing.T) {
	owner := uuid.New()
	held := heldRun(owner)
	fs := &fakeStore{
		poolWaitRuns:         []store.ListPoolWaitRunsRow{held},
		autoCandidatesByUser: map[uuid.UUID][]store.ListAutoSelectCandidatesRow{owner: eligibleCand()},
		promotePoolWaitErr:   &pgconn.PgError{Code: "23505", ConstraintName: "uq_runs_one_active_per_issue"},
	}
	svc := New(fs, newBox(t), testParams())
	bc := &parkBroadcaster{}
	svc.SetBroadcaster(bc)
	n, err := svc.resumePoolWaitRuns(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("resumed=%d err=%v", n, err)
	}
	if bc.sawState(held.ID, "queued") {
		t.Fatal("conflict broadcast queued")
	}
	if len(fs.poolWaitRuns) != 1 {
		t.Fatal("held row removed")
	}
}
