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
				if fs.msg.ParkKind.String != "forge_publication_hold" {
					t.Fatalf("hold must record forge_publication_hold: %+v", fs.msg)
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

// Every automatic publication retry re-parks with a fresh status_since, so the announcement must
// be keyed on the owner episode (publication_wait_started_at), not on status_since: one wait line
// per episode, one hold line per hold, and the resume line only for an owner Resume of a hold.
func TestPublicationRetriesAnnounceOncePerEpisode(t *testing.T) {
	rc := pubParkRun(t, true)
	fs := pubStore(rc, "publication", "")
	episodeStart := rc.StatusSince.Time
	fs.pubSide.WaitStartedAt = pgtype.Timestamptz{Time: episodeStart, Valid: true}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	ctx := context.Background()
	park := func(offset time.Duration, hold bool) {
		fs.rc = rc
		fs.rc.Status = "recovery_wait"
		fs.rc.RecoveryWaitCause = txt("forge_unreachable")
		fs.rc.StatusSince = pgtype.Timestamptz{Time: episodeStart.Add(offset), Valid: true}
		fs.crossCheckRun.RecoveryWaitPhase = txt("publication")
		fs.rc.RecoveryRetryNotBefore = pgtype.Timestamptz{Time: episodeStart.Add(offset + time.Minute), Valid: true}
		fs.pubSide.HoldReason = pgtype.Text{}
		if hold {
			fs.rc.RecoveryRetryNotBefore = pgtype.Timestamptz{}
			fs.pubSide.HoldReason = txt("publication_retry_exhausted")
		}
		n.handle(ctx, stateEvent{runID: rc.ID, status: "recovery_wait"})
	}
	run := func(offset time.Duration) {
		fs.rc.Status = "running"
		fs.rc.RecoveryWaitCause = pgtype.Text{}
		fs.rc.StatusSince = pgtype.Timestamptz{Time: episodeStart.Add(offset), Valid: true}
		fs.crossCheckRun.RecoveryWaitPhase = pgtype.Text{}
		n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
	}
	park(0, false)
	run(time.Minute)
	park(2*time.Minute, false) // automatic retry re-parked: a new status_since, the same episode
	run(3 * time.Minute)
	park(5*time.Minute, false)
	run(6 * time.Minute)
	if got := countSection(fp.blocks, "Publishing paused"); got != 1 {
		t.Fatalf("wait lines = %d, want 1: %+v", got, fp.blocks)
	}
	if got := len(resumeBlocks(fp.blocks)); got != 0 {
		t.Fatalf("automatic retries posted %d resume lines, want 0: %+v", got, fp.blocks)
	}
	park(10*time.Minute, true)
	park(10*time.Minute, true) // a repeated event for the same hold
	if got := countSection(fp.blocks, "publishing held"); got != 1 {
		t.Fatalf("hold lines = %d, want 1: %+v", got, fp.blocks)
	}
	run(20 * time.Minute) // owner Resume
	run(21 * time.Minute)
	if got := len(resumeBlocks(fp.blocks)); got != 1 || !strings.Contains(resumeBlocks(fp.blocks)[0].sectionText, "publishing again") {
		t.Fatalf("resume lines = %d, want 1: %+v", got, fp.blocks)
	}
	if got := countSection(fp.blocks, "Publishing paused"); got != 1 {
		t.Fatalf("wait lines after resume = %d, want 1", got)
	}
	// The next episode (owner Resume advanced it) announces its own wait again.
	fs.pubSide.WaitStartedAt = pgtype.Timestamptz{Time: episodeStart.Add(time.Hour), Valid: true}
	park(time.Hour, false)
	if got := countSection(fp.blocks, "Publishing paused"); got != 2 {
		t.Fatalf("new episode wait lines = %d, want 2", got)
	}
}

// The root status line of a publication hold reads "Needs your attention" like the thread line
// and a provider hold, while a timed wait keeps the "Recovering" label.
func TestPublicationHoldRootLineNeedsAttention(t *testing.T) {
	for _, tc := range []struct {
		name  string
		timed bool
		want  string
	}{
		{"hold", false, "Needs your attention · publishing held"},
		{"timed wait", true, "Recovering"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := pubParkRun(t, tc.timed)
			fs := pubStore(rc, "publication", "")
			if !tc.timed {
				fs.pubSide.HoldReason = txt("publication_retry_exhausted")
			}
			fs.msgErr = pgx.ErrNoRows // first root: the root is posted, not edited
			fp := &fakePoster{}
			NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
			if len(fp.blocks) == 0 || !strings.Contains(fp.blocks[0].sectionText, tc.want) {
				t.Fatalf("root line lacks %q: %+v", tc.want, fp.blocks)
			}
		})
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
	// A failing run or side-row read is not a publication park we can key an announcement on: the
	// run stays a plain recovery_wait, with no thread line and no marker (a retry of the read
	// on a later event announces it once).
	for name, mut := range map[string]func(*fakeNotifStore){
		"side row": func(f *fakeNotifStore) { f.pubSideErr = errors.New("db down") },
		"run row":  func(f *fakeNotifStore) { f.runErr = errors.New("db down") },
	} {
		t.Run(name, func(t *testing.T) {
			fs := pubStore(rc, "publication", "")
			mut(fs)
			fp := &fakePoster{}
			NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: rc.ID, status: "recovery_wait"})
			if len(fp.blocks) != 0 || len(fs.limitPauseSet) != 0 {
				t.Fatalf("a failed read announced or stamped: posts=%+v stamps=%+v", fp.blocks, fs.limitPauseSet)
			}
		})
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
