package slacksvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func iterationSlackFixture() store.GetSlackRunContextRow {
	rc := baseRun("paused")
	rc.HoldReason = txt("iteration_budget_exhausted")
	rc.StartedAt = pgtype.Timestamptz{Time: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), Valid: true}
	rc.StatusSince = pgtype.Timestamptz{Time: rc.StartedAt.Time.Add(time.Hour), Valid: true}
	return rc
}

func TestIterationSlackPresentation(t *testing.T) {
	for _, kind := range []string{"issue", "mr_rework", "ci_fix", "task", "prompt", "self_improve"} {
		for _, shape := range []string{"none", "zero completed", "completed"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				rc := iterationSlackFixture()
				rc.Kind = kind
				rc.MilestonesFrozen = []byte(`[{"id":"m1","title":"first"}]`)
				rc.MilestonesCompleted = []byte(`[]`)
				if shape == "completed" {
					rc.MilestonesCompleted = []byte(`["m1"]`)
				}
				if shape == "none" {
					rc.MilestonesFrozen = []byte(`[]`)
				}
				root, rootFallback := rootBlocks(rc, "https://uzi.example")
				thread, threadFallback, ok := renderThreadBlocks(rc, "https://uzi.example")
				if !ok {
					t.Fatal("missing nonterminal thread")
				}
				for _, view := range []struct {
					blocks   []slack.Block
					fallback string
				}{{root, rootFallback}, {thread, threadFallback}} {
					encoded, err := json.Marshal(view.blocks)
					if err != nil {
						t.Fatal(err)
					}
					out := string(encoded)
					if !strings.Contains(view.fallback, "Iteration limit reached") {
						t.Fatalf("wrong fallback: %s", view.fallback)
					}
					for _, want := range []string{"Iteration limit reached", "Resume", "Cancel", "uzi run resume", "uzi run cancel"} {
						if !strings.Contains(out, want) {
							t.Fatalf("missing %s: %s", want, out)
						}
					}
					for _, forbidden := range []string{"Stop", "provider", "uzi run extend", "action_id"} {
						if strings.Contains(out, forbidden) {
							t.Fatalf("unexpected %s: %s", forbidden, out)
						}
					}
				}
			})
		}
	}
}

func TestIterationSlackFrozenWallGuidance(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		wall                        int32
		banked, extension, finalize int32
		started, statusSince        bool
		extend                      bool
	}{
		{"spent", 3600, 0, 0, 0, true, true, true},
		{"over", 3599, 0, 0, 0, true, true, true},
		{"time left", 3601, 0, 0, 0, true, true, false},
		{"banked pause", 3600, 1, 0, 0, true, true, false},
		{"extension", 3600, 0, 1, 0, true, true, false},
		{"finalize", 3600, 0, 0, 1, true, true, false},
		{"unknown wall", 0, 0, 0, 0, true, true, false},
		{"unknown start", 3600, 0, 0, 0, false, true, false},
		{"unknown park", 3600, 0, 0, 0, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := iterationSlackFixture()
			rc.BudgetWallSeconds = pgtype.Int4{Int32: tc.wall, Valid: tc.wall != 0}
			rc.StartedAt.Valid = tc.started
			rc.StatusSince.Valid = tc.statusSince
			rc.BudgetPausedSeconds = tc.banked
			rc.BudgetExtensionSeconds = tc.extension
			rc.BudgetFinalizeSeconds = tc.finalize
			for _, thread := range []bool{false, true} {
				blocks, _ := rootBlocks(rc, "https://uzi.example")
				if thread {
					blocks, _, _ = renderThreadBlocks(rc, "https://uzi.example")
				}
				encoded, err := json.Marshal(blocks)
				if err != nil {
					t.Fatal(err)
				}
				out := string(encoded)
				if strings.Contains(out, "uzi run extend") != tc.extend || strings.Contains(out, "Wall time exhausted") != tc.extend {
					t.Fatalf("wrong frozen wall guidance: %s", out)
				}
				if tc.extend && !strings.Contains(out, "Extend adds time only; then Resume") {
					t.Fatalf("missing explicit Resume: %s", out)
				}
			}
		})
	}
}

func TestIterationSlackPauseEpisodes(t *testing.T) {
	for _, firstRoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "thread", true: "root"}[firstRoot], func(t *testing.T) {
			rc := iterationSlackFixture()
			fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts0"}}
			if firstRoot {
				fs.msgErr = pgx.ErrNoRows
			}
			fp := &fakePoster{}
			n := NewNotifier(fs, fp, fixedBase, nil)
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
			if len(fp.blocks) != 1 || !strings.Contains(fp.blocks[0].sectionText, "Iteration limit reached") || len(fs.limitPauseSet) != 1 {
				t.Fatalf("iteration announcement missing: %+v", fp.blocks)
			}
			if firstRoot {
				fs.msgErr = nil
				fs.msg.ChannelID = "D1"
				fs.msg.RootTs = "ts1"
			}
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
			// Generic pauses announce repeated state events and refresh their marker.
			if len(fp.blocks) != 2 || len(fs.limitPauseSet) != 2 {
				t.Fatal("generic pause event behavior changed")
			}
			fs.rc.Status = "running"
			fs.rc.HoldReason = pgtype.Text{}
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})
			if len(resumeBlocks(fp.blocks)) != 1 || fs.msg.LimitPausedAt.Valid {
				t.Fatal("generic resume episode changed")
			}
			fs.rc = rc
			fs.rc.StatusSince.Time = rc.StatusSince.Time.Add(time.Hour)
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
			if len(fp.blocks) != 4 || len(fs.limitPauseSet) != 3 {
				t.Fatalf("second episode missing: %+v", fp.blocks)
			}
		})
	}
}

func TestIterationSlackInactiveReason(t *testing.T) {
	for _, status := range []string{"running", "queued", "completed"} {
		rc := iterationSlackFixture()
		rc.Status = status
		_, label := statusGlyph(rc)
		if label == "Iteration limit reached" || iterationStateDetail(rc) != "" {
			t.Fatalf("inactive iteration hold rendered: %s", status)
		}
	}
}
