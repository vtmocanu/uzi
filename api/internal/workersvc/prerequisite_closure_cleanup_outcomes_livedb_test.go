package workersvc

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

func TestPrerequisitePinnedCleanupForeignTipUntouchedLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	tx := &m3Transactions{f: f}
	f.s.SetPrerequisiteStore(f.env.q, tx.transaction)
	remote := m3RemoteFor(t, f, tx)
	m3Sweep(t, ctx, f.s)
	pinned := f.readLiveDB(t)
	_, created := remote.snapshot()
	if pinned.State != "pinned" || !pinned.LiveRunID.Valid || pinned.CleanupAttempts != 0 ||
		pinned.PinTip.String != prerequisiteTestTip || pinned.PinRef != pushbroker.PrereqRef(pinned.CaptureID) ||
		len(created) != 1 || created[0] != [3]string{strings.Repeat("0", 40), prerequisiteTestTip, pinned.PinRef} {
		t.Fatal("public sweep did not create the canonical live pin")
	}
	f.expireCaptureLiveDB(t)
	foreignTip := strings.Repeat("b", 40)
	remote.mu.Lock()
	remote.tip = foreignTip
	remote.mu.Unlock()
	beforeRequests, beforeCommands := remote.snapshot()
	// Expiry also makes the ordinary audit due; admit cleanup before that audit.
	f.s.prerequisite.lead = 3
	m3Sweep(t, ctx, f.s)
	after := f.readLiveDB(t)
	if after.State != "foreign" || after.Reason != "pin_foreign" || after.LiveRunID.Valid ||
		after.CleanupAttempts != 0 || after.CleanupDeadline.Valid || after.CleanupExhaustedAt.Valid {
		t.Fatal("foreign cleanup spent delete authority or retained the live pointer")
	}
	terminalFrozen := pinned
	terminalFrozen.LiveRunID = after.LiveRunID // Terminal pointer release is permitted.
	m3AssertFrozen(t, terminalFrozen, after)
	requests, commands := remote.snapshot()
	remote.mu.Lock()
	tip := remote.tip
	remote.mu.Unlock()
	if requests != beforeRequests+1 || !slices.Equal(commands, beforeCommands) || remote.posts.Load() != 1 ||
		tip != foreignTip || remote.ioInTX.Load() || remote.sendBeforeCommit.Load() || tx.active.Load() {
		t.Fatal("foreign cleanup did more than one listing or touched the foreign tip")
	}
	for i := 0; i < 2; i++ {
		f.advanceLiveDB(t, 48*time.Hour)
		fresh := m3Fresh(f)
		fresh.SetPrerequisiteStore(f.env.q, tx.transaction)
		m3Sweep(t, ctx, fresh)
		later := f.readLiveDB(t)
		m3AssertFrozen(t, after, later)
		laterRequests, laterCommands := remote.snapshot()
		remote.mu.Lock()
		tip = remote.tip
		remote.mu.Unlock()
		if later.State != "foreign" || later.Reason != "pin_foreign" || later.CleanupAttempts != 0 ||
			later.CleanupDeadline.Valid || laterRequests != requests || !slices.Equal(laterCommands, commands) ||
			remote.posts.Load() != 1 || tip != foreignTip || remote.ioInTX.Load() || tx.active.Load() {
			t.Fatal("fresh service resurrected or performed I/O for terminal foreign cleanup")
		}
	}
}

func TestPrerequisiteAmbiguousCleanupRequiresLaterAbsenceLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	tx := &m3Transactions{f: f}
	f.s.SetPrerequisiteStore(f.env.q, tx.transaction)
	remote := m3RemoteFor(t, f, tx)
	m3Sweep(t, ctx, f.s)
	pinned := f.readLiveDB(t)
	_, created := remote.snapshot()
	if pinned.State != "pinned" || !pinned.LiveRunID.Valid || pinned.CleanupAttempts != 0 ||
		pinned.PinTip.String != prerequisiteTestTip || pinned.PinRef != pushbroker.PrereqRef(pinned.CaptureID) ||
		len(created) != 1 || created[0] != [3]string{strings.Repeat("0", 40), prerequisiteTestTip, pinned.PinRef} {
		t.Fatal("public sweep did not create the canonical live pin")
	}
	f.expireCaptureLiveDB(t)
	remote.mu.Lock()
	remote.mode = "ambiguous-cleanup-readback"
	remote.mu.Unlock()
	beforeRequests, _ := remote.snapshot()
	// Cleanup must precede the audit made due by the 25-hour expiry advance.
	f.s.prerequisite.lead = 3
	m3Sweep(t, ctx, f.s)
	retry := f.readLiveDB(t)
	m3AssertFrozen(t, pinned, retry)
	if retry.State != "pinned" || !retry.LiveRunID.Valid || retry.AbsentSince.Valid || retry.AbsencePassID.Valid ||
		retry.CleanupAttempts != 1 || retry.CleanupDeadline.Valid || retry.CleanupExhaustedAt.Valid ||
		!retry.NextAttemptAt.Valid || !retry.NextAttemptAt.Time.After(f.m.now) {
		t.Fatal("failed delete confirmation inferred absence or lost durable cleanup retry")
	}
	requests, commands := remote.snapshot()
	remote.mu.Lock()
	tip := remote.tip
	remote.mu.Unlock()
	// Initial listing, broker discovery/receive/send, then both failed readbacks.
	if requests != beforeRequests+6 || len(commands) != 2 || commands[0] != created[0] ||
		commands[1] != [3]string{prerequisiteTestTip, strings.Repeat("0", 40), pinned.PinRef} ||
		remote.posts.Load() != 2 || tip != "" || remote.ioInTX.Load() || remote.sendBeforeCommit.Load() || tx.active.Load() {
		t.Fatal("ambiguous cleanup did not apply exactly one CAS delete with two failed readbacks outside transactions")
	}
	remote.mu.Lock()
	remote.mode = ""
	remote.mu.Unlock()
	f.advanceLiveDB(t, retry.NextAttemptAt.Time.Sub(f.m.now))
	fresh := m3Fresh(f)
	fresh.SetPrerequisiteStore(f.env.q, tx.transaction)
	m3Sweep(t, ctx, fresh)
	after := f.readLiveDB(t)
	terminalFrozen := pinned
	terminalFrozen.LiveRunID = after.LiveRunID
	m3AssertFrozen(t, terminalFrozen, after)
	laterRequests, laterCommands := remote.snapshot()
	remote.mu.Lock()
	tip = remote.tip
	remote.mu.Unlock()
	if after.State != "deleted" || after.LiveRunID.Valid || after.CleanupAttempts != 1 ||
		after.CleanupDeadline.Valid || after.CleanupExhaustedAt.Valid ||
		laterRequests != requests+1 || !slices.Equal(laterCommands, commands) || remote.posts.Load() != 2 ||
		tip != "" || remote.ioInTX.Load() || remote.sendBeforeCommit.Load() || tx.active.Load() {
		t.Fatal("later positive absence did not release the pointer without another delete generation")
	}
}
