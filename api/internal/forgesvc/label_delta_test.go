package forgesvc

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestLabelWritersUseCurrentRowDelta(t *testing.T) {
	for _, move := range []bool{false, true} {
		t.Run(map[bool]string{false: "apply", true: "move"}[move], func(t *testing.T) {
			stale := labeledIssue(4, false, "on-deck", "Planned")
			current := stale
			current.Labels = []byte(`["holder","holder","Planned"]`)
			st := &fakeStore{issue: current}
			svc := newLabelSvc(st)
			var got store.Issue
			var err error
			if move {
				got, err = svc.AutoMove(context.Background(), &fakeForge{}, 7, stale, boardColumns(), "In Progress")
			} else {
				got, err = svc.SetIssueLabel(context.Background(), &fakeForge{}, 7, stale, "extra", "", true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(st.labelUpserts) != 1 {
				t.Fatalf("writes: %+v", st.labelUpserts)
			}
			p := st.labelUpserts[0]
			if move {
				assertLabels(t, p.AddLabels, []string{"In Progress"})
				assertLabels(t, p.RemoveLabels, []string{"Planned"})
				assertLabels(t, p.Labels, []string{"on-deck", "In Progress"})
				assertLabels(t, got.Labels, []string{"holder", "holder", "In Progress"})
			} else {
				assertLabels(t, p.AddLabels, []string{"extra"})
				if string(p.RemoveLabels) != "[]" {
					t.Fatalf("empty removal = %s", p.RemoveLabels)
				}
				assertLabels(t, p.Labels, []string{"on-deck", "Planned", "extra"})
				assertLabels(t, got.Labels, []string{"holder", "holder", "Planned", "extra"})
			}
			assertLabels(t, st.issue.Labels, func() []string {
				if move {
					return []string{"holder", "holder", "In Progress"}
				}
				return []string{"holder", "holder", "Planned", "extra"}
			}())
		})
	}
}

func TestLabelWriterErrorsKeepCacheAndErrorIdentity(t *testing.T) {
	sentinel := errors.New("sentinel label failure")
	for _, writer := range []string{"move", "apply", "remove"} {
		for _, ensure := range []bool{false, true} {
			if ensure && writer == "remove" {
				continue
			}
			t.Run(writer+map[bool]string{false: " update", true: " ensure"}[ensure], func(t *testing.T) {
				issue := labeledIssue(4, false, "Planned")
				st := &fakeStore{issue: issue}
				f := &fakeForge{remoteLabels: []string{"Planned"}, removeBeforeFailure: true}
				if ensure {
					f.ensureErr = sentinel
				} else {
					f.updateErr = sentinel
				}
				svc := newLabelSvc(st)
				var err error
				if writer == "move" {
					_, err = svc.AutoMove(context.Background(), f, 7, issue, boardColumns(), "In Progress")
				} else {
					_, err = svc.SetIssueLabel(context.Background(), f, 7, issue, "Planned-extra", "", writer == "apply")
				}
				if err != sentinel {
					t.Fatalf("error = %v, want exact sentinel", err)
				}
				if len(st.upserts)+len(st.labelUpserts)+len(st.labelRemovals) != 0 {
					t.Fatalf("cache called: %+v", st)
				}
				assertLabels(t, st.issue.Labels, []string{"Planned"})
				want := 1
				if ensure {
					want = 0
				}
				if len(f.updateCalls) != want {
					t.Fatalf("update/compensation calls: %+v", f.updateCalls)
				}
				if writer == "move" && !ensure && len(f.remoteLabels) != 0 {
					t.Fatalf("remove-success/add-failure not simulated: %v", f.remoteLabels)
				}
			})
		}
	}
}
