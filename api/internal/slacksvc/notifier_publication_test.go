package slacksvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// pubParkRun is a recovery_wait / forge_unreachable run context; the phase and hold reason
// are staged on the fake store (they are not on the context row).
func pubParkRun(t *testing.T, timed bool) store.GetSlackRunContextRow {
	t.Helper()
	rc := baseRun("recovery_wait")
	rc.Kind = "issue"
	rc.RecoveryWaitCause = txt("forge_unreachable")
	rc.Branch = txt("agent/issue-42")
	rc.StatusSince = pgtype.Timestamptz{Time: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), Valid: true}
	if timed {
		rc.RecoveryRetryNotBefore = pgtype.Timestamptz{Time: rc.StatusSince.Time.Add(time.Hour), Valid: true}
	}
	return rc
}

func pubStore(rc store.GetSlackRunContextRow, phase, holdReason string) *fakeNotifStore {
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts0"}}
	if phase != "" {
		fs.crossCheckRun.RecoveryWaitPhase = txt(phase)
	}
	fs.pubSide = &store.RunPublicationResume{RunID: rc.ID, State: "active"}
	if holdReason != "" {
		fs.pubSide.HoldReason = txt(holdReason)
	}
	return fs
}

func TestPublicationWaitRecordsForgePublicationKind(t *testing.T) {
	rc := pubParkRun(t, true)
	fs := pubStore(rc, "publication", "")
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	ev := stateEvent{runID: rc.ID, status: "recovery_wait"}
	n.handle(context.Background(), ev)
	if len(fs.limitPauseSet) != 1 || fs.msg.ParkKind.String != "forge_publication" || !fs.msg.LimitPausedAt.Time.Equal(rc.StatusSince.Time) {
		t.Fatalf("publication wait must record forge_publication: %+v", fs.limitPauseSet)
	}
	if len(fp.blocks) != 1 || !strings.Contains(fp.blocks[0].sectionText, "Publishing paused") ||
		!strings.Contains(fp.blocks[0].contextText, "retrying automatically") ||
		strings.Contains(fp.blocks[0].contextText, "Resume starts") {
		t.Fatalf("timed wait wording: %+v", fp.blocks)
	}
	// A redelivered or late event for the same park posts nothing more.
	n.handle(context.Background(), ev)
	if len(fp.blocks) != 1 || len(fs.limitPauseSet) != 1 {
		t.Fatalf("duplicate event re-announced: %+v", fp.blocks)
	}
}

func TestPublicationHoldPostsItsReason(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"publication_retry_exhausted", "the forge stayed unreachable for the whole wait"},
		{"publication_custodian_unavailable", "the worker holding the unpublished source is gone; the work is local-only"},
		{"publication_model_required", "publishing needs model work a publication-only resume does not do"},
		{"publication_source_unavailable", "the unpublished work is not available"},
		{"publication_intent_mismatch", "the publication record did not match"},
		{"publication_from_the_future", "the hold reason is not recognised by this version"},
	} {
		for _, firstRoot := range []bool{false, true} {
			t.Run(tc.reason, func(t *testing.T) {
				rc := pubParkRun(t, false)
				rc.Branch = txt("agent/<@U999>|x")
				fs := pubStore(rc, "publication", tc.reason)
				if firstRoot {
					fs.msgErr = pgx.ErrNoRows
				}
				fp := &fakePoster{}
				n := NewNotifier(fs, fp, fixedBase, nil)
				n.handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
				var hold *blockCall
				for i := range fp.blocks {
					if strings.Contains(fp.blocks[i].sectionText, "publishing held") {
						hold = &fp.blocks[i]
					}
				}
				if hold == nil {
					t.Fatalf("no hold post: %+v", fp.blocks)
				}
				for _, w := range []string{tc.want, "Resume starts a new publication-only attempt (no model work)", "Cancel", "branch `agent/", "/runs/"} {
					if !strings.Contains(hold.contextText, w) {
						t.Fatalf("hold context lacks %q: %q", w, hold.contextText)
					}
				}
				if strings.Contains(hold.contextText, "<@U999>") {
					t.Fatalf("branch not escaped: %q", hold.contextText)
				}
				if fs.msg.ParkKind.String != "forge_publication" {
					t.Fatalf("hold must record forge_publication: %+v", fs.msg)
				}
			})
		}
	}
}

func TestPublicationResumePosts(t *testing.T) {
	rc := pubParkRun(t, false)
	fs := pubStore(rc, "publication", "publication_retry_exhausted")
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	ev := stateEvent{runID: rc.ID, status: "recovery_wait"}
	n.handle(context.Background(), ev)
	// Owner Resume: the run is queued, then running again.
	fs.rc.Status = "running"
	fs.rc.RecoveryWaitCause = pgtype.Text{}
	fs.crossCheckRun.RecoveryWaitPhase = pgtype.Text{}
	fs.rc.RecoveryRetryNotBefore = pgtype.Timestamptz{}
	n.handle(context.Background(), ev)
	rb := resumeBlocks(fp.blocks)
	if len(rb) != 1 || !strings.Contains(rb[0].sectionText, "publishing again") || strings.Contains(rb[0].sectionText, "usage limit") {
		t.Fatalf("publication resume wording: %+v", rb)
	}
	if fs.msg.LimitPausedAt.Valid {
		t.Fatal("resume must consume the marker")
	}
	n.handle(context.Background(), ev)
	if len(resumeBlocks(fp.blocks)) != 1 {
		t.Fatal("repeated running re-posted the resume")
	}
}

func TestPublicationOptedOutOwnerStaysSilent(t *testing.T) {
	for name, mut := range map[string]func(*fakeNotifStore){
		"opted out or unlinked": func(f *fakeNotifStore) { f.deliveryErr = pgx.ErrNoRows },
		"empty target":          func(f *fakeNotifStore) { f.delivery = pgtype.Text{} },
	} {
		t.Run(name, func(t *testing.T) {
			rc := pubParkRun(t, false)
			fs := pubStore(rc, "publication", "publication_model_required")
			mut(fs)
			fp := &fakePoster{}
			NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
			if len(fp.blocks)+len(fp.posts)+len(fp.updateBlocks) != 0 || len(fs.limitPauseSet) != 0 {
				t.Fatalf("silent owner got output: %+v %+v", fp, fs.limitPauseSet)
			}
		})
	}
}

func TestPublicationNotificationFailureIsSwallowed(t *testing.T) {
	rc := pubParkRun(t, false)
	// A failed post leaves the marker unset (retryable) and does not panic or change the run.
	fs := pubStore(rc, "publication", "publication_retry_exhausted")
	fp := &fakePoster{postErr: errors.New("slack down")}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
	if len(fs.limitPauseSet) != 0 {
		t.Fatalf("failed post stamped the marker: %+v", fs.limitPauseSet)
	}
	// It retries on the next event, then dedupes.
	fp.postErr = nil
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
	delivered := len(fp.blocks) // the fake records the refused attempt too
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
	if len(fs.limitPauseSet) != 1 || len(fp.blocks) != delivered || delivered != 2 {
		t.Fatalf("retry then dedupe: stamps=%+v posts=%+v", fs.limitPauseSet, fp.blocks)
	}
	// A failing side-row or run read leaves the run a plain (silent) recovery_wait.
	for _, mut := range []func(*fakeNotifStore){
		func(f *fakeNotifStore) { f.pubSideErr = errors.New("db down") },
	} {
		fs := pubStore(rc, "publication", "")
		mut(fs)
		fp := &fakePoster{}
		NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
		if fs.msg.ParkKind.Valid && fs.msg.ParkKind.String != "forge_publication" {
			t.Fatalf("unexpected kind %q", fs.msg.ParkKind.String)
		}
	}
}

func TestPreCloneForgeParkIsNotRecorded(t *testing.T) {
	rc := pubParkRun(t, true)
	fs := pubStore(rc, "", "") // no phase: the pre-clone forge park
	fp := &fakePoster{}
	NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
	if len(fs.limitPauseSet) != 0 || countSection(fp.blocks, "ublishing") != 0 {
		t.Fatalf("pre-clone park must stay unrecorded: stamps=%+v posts=%+v", fs.limitPauseSet, fp.blocks)
	}
}
