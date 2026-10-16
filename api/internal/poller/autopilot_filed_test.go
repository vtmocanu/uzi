package poller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The candidate is already cached: these tests do not prove filing -> forge sync
// -> cache creation. Handler writes use the repo-owner PAT, so event adder and
// issue author can be that PAT's identity, not the user who clicked in the UI.
type apFiledForge struct {
	*apForge
	labelWrites int
}

func (f *apFiledForge) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	f.labelWrites++
	return errors.New("unexpected issue label write")
}

func filedCandidate(author string) (*apStore, *apRuns, *apFiledForge) {
	return &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, author)},
		cc:         ccOwner("alice", true, true),
	}, &apRuns{}, &apFiledForge{apForge: &apForge{
		issue: forge.Issue{
			Description: "Fix the filed issue, preserving this description.",
			Labels:      []string{"agent-found", "uzi", apLabel},
		},
	}}
}

func detectFiled(st *apStore, runs *apRuns, f *apFiledForge) {
	NewAutopilot(st, runs, apLabeler{label: apLabel}).detect(context.Background(), repoRow(), f)
}

func assertFiledLabelsRetained(t *testing.T, f *apFiledForge) {
	t.Helper()
	if f.labelWrites != 0 || !slices.Equal(f.issue.Labels, []string{"agent-found", "uzi", apLabel}) {
		t.Fatalf("filed eligibility changed: label writes=%d labels=%v", f.labelWrites, f.issue.Labels)
	}
}

func assertFiledStart(t *testing.T, runs *apRuns, f *apFiledForge) {
	t.Helper()
	if len(runs.calls) != 1 {
		t.Fatalf("CreateAutopilotRun calls = %+v, want one", runs.calls)
	}
	c := runs.calls[0]
	if c.userID != apUserID || c.repoID != apRepoID || c.iid != 7 || c.desc != f.issue.Description {
		t.Fatalf("run call = %+v, want owner/repo/issue and fresh description", c)
	}
}

func TestAutopilotFiledLabelCandidates(t *testing.T) {
	cases := []struct {
		name, human, adder, author string
		enabled                    bool
		events                     []forge.LabelEvent
		wantRun, wantComment       bool
	}{
		{name: "enabled owner adder", human: "alice", adder: "alice", author: "filing-bot", enabled: true, wantRun: true},
		{name: "cached author fallback", human: "alice", adder: "repo-pat", author: "alice", enabled: true, wantRun: true},
		{name: "disabled owner", human: "alice", adder: "alice", author: "alice", wantComment: true},
		{name: "bot PAT is neither human adder nor author", human: "alice", adder: "repo-bot", author: "repo-bot", enabled: true, wantComment: true},
		{name: "other actor and author", human: "alice", adder: "bob", author: "carol", enabled: true, wantComment: true},
		{name: "empty declared human mapping", adder: "alice", author: "alice", enabled: true, wantComment: true},
		{name: "no add event", human: "alice", author: "alice", enabled: true},
		{name: "latest event removed", human: "alice", author: "alice", enabled: true, events: []forge.LabelEvent{addEvt(100, "alice"), remEvt(105, "alice")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, runs, f := filedCandidate(tc.author)
			st.cc = ccOwner(tc.human, tc.enabled, true)
			if tc.human == "" {
				// The existing no-mapping test covers NULL; exercise a declared empty value here.
				st.cc.HumanUsername.Valid = true
			}
			events := tc.events
			if tc.adder != "" {
				events = []forge.LabelEvent{addEvt(100, tc.adder)}
			}
			f.events = map[int64][]forge.LabelEvent{7: events}
			// A fresh forge author must not replace the cached attribution input.
			f.issue.Author = "fresh-unrelated-actor"
			detectFiled(st, runs, f)
			detectFiled(st, runs, f) // Same application is handled only once.

			if tc.wantRun {
				assertFiledStart(t, runs, f)
			} else if len(runs.calls) != 0 {
				t.Fatalf("unexpected run calls: %+v", runs.calls)
			}
			wantNotes := 0
			if tc.wantComment {
				wantNotes = 1
			}
			if len(f.noteAttempts) != wantNotes || len(f.notes) != wantNotes {
				t.Fatalf("comment attempts=%+v successful=%+v, want %d", f.noteAttempts, f.notes, wantNotes)
			}
			if tc.wantComment {
				note := f.noteAttempts[0]
				if note.iid != 7 || !strings.Contains(note.body, "autopilot enabled is mapped") || !strings.Contains(note.body, "remove and re-add") {
					t.Fatalf("missing consent explanation/retry instruction: %+v", note)
				}
			}
			wantRecords := 0
			if tc.wantRun || tc.wantComment {
				wantRecords = 1
			}
			if len(st.upserts) != wantRecords {
				t.Fatalf("trigger records=%+v, want %d", st.upserts, wantRecords)
			}
			if wantRecords != 0 {
				got := lastUpsert(t, st)
				if got.RepoID != apRepoID || got.IssueIid != 7 || got.LastEventID != 100 {
					t.Fatalf("recorded transition = %+v", got)
				}
			}
			assertFiledLabelsRetained(t, f)
		})
	}
}

func TestAutopilotFiledRefusalEnableReplayThenReAdd(t *testing.T) {
	for _, failedPost := range []bool{false, true} {
		name := "comment succeeds"
		if failedPost {
			name = "comment POST fails"
		}
		t.Run(name, func(t *testing.T) {
			st, runs, f := filedCandidate("repo-bot")
			st.cc.AutopilotEnabled = false
			f.events = map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}
			var ops []string
			st.ops, runs.ops, f.ops = &ops, &ops, &ops
			if failedPost {
				f.noteErr = errors.New("comment POST unavailable")
			}

			detectFiled(st, runs, f)
			if len(runs.calls) != 0 || len(st.upserts) != 1 || lastUpsert(t, st).LastEventID != 100 ||
				len(f.noteAttempts) != 1 || strings.Join(ops, ",") != "record,comment" {
				t.Fatalf("refusal must record then attempt one comment: runs=%+v records=%+v attempts=%+v ops=%v", runs.calls, st.upserts, f.noteAttempts, ops)
			}
			if f.noteAttempts[0].iid != 7 || !strings.Contains(f.noteAttempts[0].body, "autopilot enabled is mapped") {
				t.Fatalf("unexpected refusal: %+v", f.noteAttempts)
			}
			wantNotes := 1
			if failedPost {
				wantNotes = 0
			}
			if len(f.notes) != wantNotes {
				t.Fatalf("successful comments=%+v, want %d", f.notes, wantNotes)
			}
			assertFiledLabelsRetained(t, f)

			st.cc.AutopilotEnabled = true
			f.noteErr = nil // A recovered POST must still never be retried.
			detectFiled(st, runs, f)
			if len(runs.calls) != 0 || len(st.upserts) != 1 || len(f.noteAttempts) != 1 || len(f.notes) != wantNotes {
				t.Fatalf("enabling must not replay the refused application: runs=%+v records=%+v attempts=%+v notes=%+v", runs.calls, st.upserts, f.noteAttempts, f.notes)
			}

			f.events[7] = append(f.events[7], remEvt(105, "alice"))
			detectFiled(st, runs, f)
			if len(runs.calls) != 0 || len(st.upserts) != 1 || len(f.noteAttempts) != 1 {
				t.Fatal("removal must not start, record or comment")
			}
			f.events[7] = append(f.events[7], addEvt(110, "alice"))
			detectFiled(st, runs, f)
			detectFiled(st, runs, f)
			assertFiledStart(t, runs, f)
			if len(st.upserts) != 2 || lastUpsert(t, st).LastEventID != 110 || len(f.noteAttempts) != 1 || len(f.notes) != wantNotes ||
				strings.Join(ops, ",") != "record,comment,create,record" {
				t.Fatalf("re-add must start exactly once: records=%+v attempts=%+v notes=%+v ops=%v", st.upserts, f.noteAttempts, f.notes, ops)
			}
			assertFiledLabelsRetained(t, f)
		})
	}
}
