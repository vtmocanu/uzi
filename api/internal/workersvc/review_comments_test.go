package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestBuildReviewCommentsSnapshotByteCap — an over-cap MR review thread keeps the
// newest content, drops the oldest, and sets Truncated, charging body bytes against
// the shared 32 KiB maxIssueCommentsBytes cap (PRD #700 M2 reuses #381's caps). With
// four 10000-byte comments (r0..r3, oldest→newest), the newest three fit under 32768
// and r0 is dropped. Fails if the byte cap or the truncated flag is not honored.
func TestBuildReviewCommentsSnapshotByteCap(t *testing.T) {
	const bot = int64(7)
	body := strings.Repeat("x", 10000)
	in := []forge.MRComment{
		{ID: 1, AuthorForgeUserID: 1, AuthorUsername: "r0", Body: body, CreatedAt: commentTS(10)},
		{ID: 2, AuthorForgeUserID: 2, AuthorUsername: "r1", Body: body, CreatedAt: commentTS(20)},
		{ID: 3, AuthorForgeUserID: 3, AuthorUsername: "r2", Body: body, CreatedAt: commentTS(30)},
		{ID: 4, AuthorForgeUserID: 4, AuthorUsername: "r3", Body: body, CreatedAt: commentTS(40)},
	}
	got := BuildReviewCommentsSnapshot(in, bot)
	if got == nil {
		t.Fatal("want a snapshot, got nil")
	}
	if !got.Truncated {
		t.Error("an over-cap thread must set Truncated")
	}
	if len(got.Comments) != 3 {
		t.Fatalf("want the newest 3 comments retained under the 32 KiB cap, got %d", len(got.Comments))
	}
	if got.Comments[0].AuthorUsername != "r1" {
		t.Fatalf("want r1 as the oldest retained comment, got %q", got.Comments[0].AuthorUsername)
	}
	if got.Comments[len(got.Comments)-1].AuthorUsername != "r3" {
		t.Fatalf("want r3 as the newest retained comment, got %q", got.Comments[len(got.Comments)-1].AuthorUsername)
	}
	for _, c := range got.Comments {
		if c.AuthorUsername == "r0" {
			t.Fatal("the oldest over-cap comment r0 must be dropped")
		}
	}
}

// TestBuildReviewCommentsSnapshotBotFilterBothHalves is the discriminating filter
// test the milestone requires: the connection's OWN bot note is dropped (D1) AND a
// THIRD-PARTY review bot (CodeRabbit) note is KEPT — the feature's whole point. A
// one-sided test that only checked the drop would pass a filter that gutted all bot
// notes, so both halves are asserted. Also carries the diff anchors + monotonic ID
// through to prove the fuller field set survives the filter.
func TestBuildReviewCommentsSnapshotBotFilterBothHalves(t *testing.T) {
	const ownBot = int64(100)
	const coderabbit = int64(43)
	const human = int64(42)
	path := "api/x.go"
	line := 42
	in := []forge.MRComment{
		{ID: 10, AuthorForgeUserID: ownBot, AuthorUsername: "uzi-bot", Body: "run started", CreatedAt: commentTS(1)},
		{ID: 11, AuthorForgeUserID: human, AuthorUsername: "carol", Body: "please rename this", CreatedAt: commentTS(2)},
		{ID: 12, AuthorForgeUserID: coderabbit, AuthorUsername: "coderabbit", Body: "guard nil here",
			Path: &path, Line: &line, ReplyID: "5001", ResolveID: "PRRT_thread1", HeadSHA: "headsha999", ReviewState: forge.ReviewCommentInline, CreatedAt: commentTS(3)},
	}
	got := BuildReviewCommentsSnapshot(in, ownBot)
	if got == nil {
		t.Fatal("want a snapshot with the human + third-party bot comments, got nil")
	}
	kept := map[string]ReviewCommentSnapshot{}
	for _, c := range got.Comments {
		kept[c.AuthorUsername] = c
	}
	// Half 1: the connection's OWN bot note is dropped.
	if _, ok := kept["uzi-bot"]; ok {
		t.Error("the connection's own bot note must be dropped (D1)")
	}
	// Half 2: the third-party review bot (CodeRabbit) note is KEPT.
	cr, ok := kept["coderabbit"]
	if !ok {
		t.Fatal("a third-party review bot (CodeRabbit) note must be KEPT — this is the feature's point")
	}
	if cr.ID != 12 || cr.ReplyID != "5001" || cr.ResolveID != "PRRT_thread1" || cr.HeadSHA != "headsha999" {
		t.Errorf("CodeRabbit comment lost its anchors/ID through the filter: %+v", cr)
	}
	if cr.Path == nil || *cr.Path != "api/x.go" || cr.Line == nil || *cr.Line != 42 {
		t.Errorf("CodeRabbit comment lost its diff anchor: path=%v line=%v", cr.Path, cr.Line)
	}
	// The human note is kept too.
	if _, ok := kept["carol"]; !ok {
		t.Error("the human note must be kept")
	}
	if len(got.Comments) != 2 {
		t.Fatalf("want exactly the human + CodeRabbit notes (own bot dropped), got %d", len(got.Comments))
	}
}

// TestBuildReviewCommentsSnapshotZeroBotID — a zero bot id omits the feature entirely
// (D9), even with third-party comments present. Fails if the D9 fail-safe is removed.
func TestBuildReviewCommentsSnapshotZeroBotID(t *testing.T) {
	in := []forge.MRComment{
		{ID: 1, AuthorForgeUserID: 43, AuthorUsername: "coderabbit", Body: "nit", CreatedAt: commentTS(1)},
	}
	if got := BuildReviewCommentsSnapshot(in, 0); got != nil {
		t.Fatalf("want nil for a zero bot id (D9), got %+v", got)
	}
}

// errMRForge overrides only ListMergeRequestComments (embedding forge.Forge for the
// rest of the interface), returning a scripted error to exercise the degrade path.
type errMRForge struct {
	forge.Forge
	err error
}

func (f *errMRForge) ListMergeRequestComments(context.Context, int64, int64) ([]forge.MRComment, error) {
	return nil, f.err
}

// scriptedForges is the ForgeBuilder seam returning one scripted fake forge.
type scriptedForges struct{ f forge.Forge }

func (b scriptedForges) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

// TestFetchReviewCommentsSnapshotDegradesOnForgeError — a forge read error degrades to
// a nil snapshot WITHOUT panicking (best-effort run context, never a create failure).
// This is also the production reference to fetchReviewCommentsSnapshot until M3's
// CreateAutoMRReworkRun calls it, so the dead-code gate does not flag it.
func TestFetchReviewCommentsSnapshotDegradesOnForgeError(t *testing.T) {
	s := &Service{}
	s.SetForges(scriptedForges{f: &errMRForge{err: errors.New("forge 503")}})
	row := store.GetRepoForUserRow{
		ForgeType:       "gitlab",
		BaseUrl:         "https://forge.e2e",
		TokenCiphertext: []byte{0x1},
		ForgeProjectID:  7,
		BotForgeUserID:  100,
	}
	if got := s.fetchReviewCommentsSnapshot(context.Background(), row, 13); got != nil {
		t.Fatalf("want nil snapshot on a forge read error, got %+v", got)
	}
}
