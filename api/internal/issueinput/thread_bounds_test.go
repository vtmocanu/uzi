package issueinput_test

import (
	"context"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func commentTS(sec int) time.Time { return time.Unix(int64(sec), 0).UTC() }

// TestProjectThreadBotFilter — a bot-authored comment (author id ==
// bot id) is dropped and the human one is kept (D1). Fails if the D1 filter is removed.
func TestProjectThreadBotFilter(t *testing.T) {
	const bot = int64(7)
	in := []forge.IssueComment{
		{AuthorForgeUserID: 42, AuthorUsername: "human", Body: "please guard the budget", CreatedAt: commentTS(1)},
		{AuthorForgeUserID: bot, AuthorUsername: "uzi-bot", Body: "run started", CreatedAt: commentTS(2)},
	}
	got := projectEligibleThread(t, in, bot)
	if got == nil {
		t.Fatal("want a snapshot with the human comment, got nil")
	}
	if len(got.Comments) != 1 {
		t.Fatalf("want 1 kept comment, got %d", len(got.Comments))
	}
	if got.Comments[0].AuthorUsername != "human" {
		t.Fatalf("want the human comment kept, got %q", got.Comments[0].AuthorUsername)
	}
	if got.Truncated {
		t.Error("a small thread must not be marked truncated")
	}
}

// TestProjectThreadZeroBotID — a zero bot id produces an empty thread
// with Unknown set, even when human comments are present.
func TestProjectThreadZeroBotID(t *testing.T) {
	in := []forge.IssueComment{
		{AuthorForgeUserID: 42, AuthorUsername: "human", Body: "hi", CreatedAt: commentTS(1)},
	}
	if got := projectEligibleThread(t, in, 0); got.Version != issueinput.SnapshotVersion || !got.Unknown || len(got.Comments) != 0 {
		t.Fatalf("want versioned unknown empty thread, got %+v", got)
	}
}

// TestProjectThreadAllBot — an all-bot thread leaves nothing after the
// D1 filter and produces a versioned empty thread.
func TestProjectThreadAllBot(t *testing.T) {
	const bot = int64(7)
	in := []forge.IssueComment{
		{AuthorForgeUserID: bot, AuthorUsername: "uzi-bot", Body: "run started", CreatedAt: commentTS(1)},
		{AuthorForgeUserID: bot, AuthorUsername: "uzi-bot", Body: "run halted", CreatedAt: commentTS(2)},
	}
	if got := projectEligibleThread(t, in, bot); got.Version != issueinput.SnapshotVersion || got.Unknown || len(got.Comments) != 0 {
		t.Fatalf("want versioned empty thread, got %+v", got)
	}
}

// TestProjectThreadOldestFirst — the oldest-first input order is
// preserved in the output. Fails if the mapping reorders the kept comments.
func TestProjectThreadOldestFirst(t *testing.T) {
	const bot = int64(7)
	in := []forge.IssueComment{
		{AuthorForgeUserID: 1, AuthorUsername: "a", Body: "first", CreatedAt: commentTS(10)},
		{AuthorForgeUserID: 2, AuthorUsername: "b", Body: "second", CreatedAt: commentTS(20)},
		{AuthorForgeUserID: 3, AuthorUsername: "c", Body: "third", CreatedAt: commentTS(30)},
	}
	got := projectEligibleThread(t, in, bot)
	if got == nil || len(got.Comments) != 3 {
		t.Fatalf("want 3 kept comments, got %+v", got)
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if got.Comments[i].AuthorUsername != w {
			t.Fatalf("comment %d: want author %q, got %q (order not preserved)", i, w, got.Comments[i].AuthorUsername)
		}
	}
}

// TestProjectThreadOverCap — an over-cap thread keeps the newest content,
// drops the oldest, and sets Truncated. The boundary is asserted: with four 10000-byte
// comments (u0..u3, oldest→newest), the newest three fit under 32768 and u0 is dropped.
// Fails if the byte cap or the truncated flag is removed.
func TestProjectThreadOverCap(t *testing.T) {
	const bot = int64(7)
	body := strings.Repeat("x", 10000)
	in := []forge.IssueComment{
		{AuthorForgeUserID: 1, AuthorUsername: "u0", Body: body, CreatedAt: commentTS(10)},
		{AuthorForgeUserID: 2, AuthorUsername: "u1", Body: body, CreatedAt: commentTS(20)},
		{AuthorForgeUserID: 3, AuthorUsername: "u2", Body: body, CreatedAt: commentTS(30)},
		{AuthorForgeUserID: 4, AuthorUsername: "u3", Body: body, CreatedAt: commentTS(40)},
	}
	got := projectEligibleThread(t, in, bot)
	if got == nil {
		t.Fatal("want a snapshot, got nil")
	}
	if !got.Truncated {
		t.Error("an over-cap thread must set Truncated")
	}
	if len(got.Comments) != 3 {
		t.Fatalf("want the newest 3 comments retained, got %d", len(got.Comments))
	}
	// The oldest (u0) is dropped; the newest (u3) is kept, oldest-first among kept.
	if got.Comments[0].AuthorUsername != "u1" {
		t.Fatalf("want u1 as the oldest retained comment, got %q", got.Comments[0].AuthorUsername)
	}
	if got.Comments[len(got.Comments)-1].AuthorUsername != "u3" {
		t.Fatalf("want u3 as the newest retained comment, got %q", got.Comments[len(got.Comments)-1].AuthorUsername)
	}
	for _, c := range got.Comments {
		if c.AuthorUsername == "u0" {
			t.Fatal("the oldest over-cap comment u0 must be dropped")
		}
	}
}

// TestProjectThreadCountCap — a flood of tiny comments is clipped to the
// newest 200 entries (bounding metadata amplification), oldest-first
// among kept, with Truncated set. Fails if the count cap is removed (the byte cap alone
// would keep all of these, since their summed bodies fit well under 32768).
func TestProjectThreadCountCap(t *testing.T) {
	const bot = int64(7)
	n := 200 + 50
	in := make([]forge.IssueComment, 0, n)
	for i := 0; i < n; i++ {
		in = append(in, forge.IssueComment{AuthorForgeUserID: 42, AuthorUsername: "human", Body: "x", CreatedAt: commentTS(i + 1)})
	}
	got := projectEligibleThread(t, in, bot)
	if got == nil {
		t.Fatal("want a snapshot, got nil")
	}
	if !got.Truncated {
		t.Error("a thread exceeding the count cap must set Truncated")
	}
	if len(got.Comments) != 200 {
		t.Fatalf("want the newest %d comments retained, got %d", 200, len(got.Comments))
	}
	// The newest comment (highest timestamp) must survive; the oldest must be dropped.
	if got.Comments[len(got.Comments)-1].CreatedAt != commentTS(n) {
		t.Fatalf("want the newest comment (ts=%d) retained, got ts=%v", n, got.Comments[len(got.Comments)-1].CreatedAt.Unix())
	}
	if !got.Comments[0].CreatedAt.After(commentTS(1)) {
		t.Fatalf("want the oldest comments dropped, but oldest kept is ts=%v", got.Comments[0].CreatedAt.Unix())
	}
}

// TestProjectThreadSingleOverCap — when the single newest body alone
// exceeds the cap, it is kept with its body truncated byte-safe on a UTF-8 rune
// boundary and Truncated set. Uses a multi-byte body so a naive byte cut would split
// a rune. Fails if the single-comment rune-safe truncation is removed.
func TestProjectThreadSingleOverCap(t *testing.T) {
	const bot = int64(7)
	// "世" is 3 bytes; 11000 runes = 33000 bytes > 32768. A byte cut at 32768 lands
	// mid-rune (32768 % 3 == 2), so a valid result proves the rune-boundary trim.
	body := strings.Repeat("世", 11000)
	in := []forge.IssueComment{
		{AuthorForgeUserID: 42, AuthorUsername: "human", Body: body, CreatedAt: commentTS(1)},
	}
	got := projectEligibleThread(t, in, bot)
	if got == nil || len(got.Comments) != 1 {
		t.Fatalf("want 1 kept comment, got %+v", got)
	}
	if !got.Truncated {
		t.Error("a single over-cap body must set Truncated")
	}
	out := got.Comments[0].Body
	if len(out) > 32768 {
		t.Fatalf("truncated body is %d bytes, want <= %d", len(out), 32768)
	}
	if !utf8.ValidString(out) {
		t.Fatal("truncated body must stay valid UTF-8 (rune-boundary cut)")
	}
	// The largest multiple of 3 not exceeding 32768 is 32766.
	if len(out) != 32766 {
		t.Fatalf("want the cut on the rune boundary at 32766 bytes, got %d", len(out))
	}
}

// TestProjectThreadEmpty checks the versioned empty thread.
func TestProjectThreadEmpty(t *testing.T) {
	if got := projectEligibleThread(t, nil, 7); got.Version != issueinput.SnapshotVersion || got.Unknown || len(got.Comments) != 0 {
		t.Fatalf("want versioned empty thread, got %+v", got)
	}
}

type eligibleThreadForge struct{ forgetest.BaseFake }

func (eligibleThreadForge) RepositoryAuthorEligibility(context.Context, int64, int64) (forge.AuthorEligibility, error) {
	return forge.AuthorEligible, nil
}
func projectEligibleThread(t *testing.T, in []forge.IssueComment, bot int64) *issueinput.Thread {
	t.Helper()
	a := issueinput.NewAssessment(context.Background(), eligibleThreadForge{}, 1)
	defer a.Close()
	return issueinput.ProjectThread(a, in, bot)
}
