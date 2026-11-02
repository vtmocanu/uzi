package slacksvc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3813: the completed parent's existing thread message discloses a failed,
// current Code cross-check. These tests drive n.handle on the shared notifier fixture.

const codeCCPublishedURL = "https://forge.example/grp/repo/pulls/7"

func codeCCParent() store.GetSlackRunContextRow {
	rc := baseRun("completed")
	rc.MrIid = text8(7)
	rc.ForgeType = "github"
	rc.MrWebUrl = txt(codeCCPublishedURL)
	return rc
}

func failedCodeCC(rc store.GetSlackRunContextRow, reason pgtype.Text, checker string) *store.CrossCheck {
	return &store.CrossCheck{
		ID: uuid.New(), LeadRunID: rc.ID, Stage: "code", Round: 1, Verdict: "failed",
		Outcome: txt("failed"), ReasonClass: reason, CheckerHarness: txt(checker),
		CheckerRunID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
	}
}

// runCompleted handles one completed event for an anchored parent and returns the
// fixture after it.
func runCompleted(t *testing.T, rc store.GetSlackRunContextRow, cc *store.CrossCheck, ccErr error, logs *bytes.Buffer) (*fakeNotifStore, *fakePoster) {
	t.Helper()
	fs := &fakeNotifStore{rc: rc, delivery: txt("U123"), codeCC: cc, codeCCErr: ccErr,
		msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	var logger *slog.Logger
	if logs != nil {
		logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	n := NewNotifier(fs, fp, fixedBase, logger)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})
	return fs, fp
}

// threadPost returns the single thread post under ts1.
func threadPost(t *testing.T, fp *fakePoster) blockCall {
	t.Helper()
	if len(fp.blocks) != 1 || fp.blocks[0].thread != "ts1" {
		t.Fatalf("want exactly one completion post threaded under ts1, got %+v", fp.blocks)
	}
	return fp.blocks[0]
}

// baselineThread is the completion message with no Code record at all.
func baselineThread(t *testing.T, rc store.GetSlackRunContextRow) blockCall {
	t.Helper()
	_, fp := runCompleted(t, rc, nil, nil, nil)
	return threadPost(t, fp)
}

func TestCodeCrossCheckAdvisoryKnownAndUnknownReasons(t *testing.T) {
	cases := []struct {
		reason pgtype.Text
		want   string
	}{
		{txt("worker_unsupported"), "worker unsupported"},
		{txt("snapshot_failed"), "snapshot failed"},
		{txt("malformed"), "checker output malformed"},
		{txt("model_error"), "model error"},
		{txt("model_timeout"), "model timed out"},
		{txt("checker_unavailable"), "checker unavailable"},
		{txt("confinement_failed"), "checker confinement failed"},
		{txt("timed_out"), "check deadline expired"},
		{txt("brand_new_reason"), "reason unavailable"},
		{txt(""), "reason unavailable"},
		{pgtype.Text{}, "reason unavailable"},
	}
	for _, tc := range cases {
		for _, checker := range []string{"codex", "claude"} { // Claude lead → Codex checker, and vice versa
			t.Run(tc.reason.String+"/"+checker, func(t *testing.T) {
				rc := codeCCParent()
				base := baselineThread(t, rc)
				fs, fp := runCompleted(t, rc, failedCodeCC(rc, tc.reason, checker), nil, nil)
				got := threadPost(t, fp)
				advisory := "Code cross-check incomplete: " + tc.want + ". Publication continued."
				if got.sectionText != "✅ *Completed*" || got.sectionText != base.sectionText {
					t.Errorf("completion label changed: %q (baseline %q)", got.sectionText, base.sectionText)
				}
				if want := base.contextText + advisory; got.contextText != want {
					t.Errorf("context = %q, want %q", got.contextText, want)
				}
				for _, link := range []string{codeCCPublishedURL, "Open in uzi", "/runs/" + rc.ID.String()} {
					if !strings.Contains(got.contextText, link) {
						t.Errorf("context lost %q: %q", link, got.contextText)
					}
				}
				if want := base.fallback + ". " + advisory; got.fallback != want {
					t.Errorf("fallback = %q, want %q", got.fallback, want)
				}
				if len(fs.codeCCAsked) != 1 || fs.codeCCAsked[0] != (store.GetCodeCrossCheckForOwnerParams{LeadRunID: rc.ID, UserID: rc.UserID}) {
					t.Errorf("want one owner-bound summary read, got %+v", fs.codeCCAsked)
				}
			})
		}
	}
}

// A pre-start failure has no checker child; the advisory still renders.
func TestCodeCrossCheckAdvisoryNoChildPreStartFailure(t *testing.T) {
	for reason, want := range map[string]string{"worker_unsupported": "worker unsupported", "snapshot_failed": "snapshot failed"} {
		rc := codeCCParent()
		cc := failedCodeCC(rc, txt(reason), "codex")
		cc.CheckerRunID = pgtype.UUID{}
		_, fp := runCompleted(t, rc, cc, nil, nil)
		if got := threadPost(t, fp).contextText; !strings.HasSuffix(got, "Code cross-check incomplete: "+want+". Publication continued.") {
			t.Errorf("%s: missing advisory: %q", reason, got)
		}
	}
}

// Publication continued needs a persisted, https-guarded mr_web_url. mr_iid alone (the
// GitLab reconstruction) or a forged URL does not prove publication.
func TestCodeCrossCheckAdvisoryPublicationProof(t *testing.T) {
	for name, url := range map[string]pgtype.Text{
		"no url (mr_iid only)": {},
		"empty":                txt(""),
		"javascript":           txt("javascript:alert(1)"),
		"http":                 txt("http://forge.example/grp/repo/pulls/7"),
		"forged mrkdwn":        txt("<https://evil.example|Publication continued>"),
	} {
		t.Run(name, func(t *testing.T) {
			rc := codeCCParent()
			rc.ForgeType = "gitlab"
			rc.MrWebUrl = url
			_, fp := runCompleted(t, rc, failedCodeCC(rc, txt("model_error"), "codex"), nil, nil)
			got := threadPost(t, fp)
			if !strings.HasSuffix(got.contextText, "Code cross-check incomplete: model error.") {
				t.Errorf("advisory without the publication sentence expected: %q", got.contextText)
			}
			if strings.Contains(got.contextText, "Publication continued") || strings.Contains(got.fallback, "Publication continued") {
				t.Errorf("publication claimed without proof: %q / %q", got.contextText, got.fallback)
			}
		})
	}
	// No MR at all on a completed parent: the advisory stays, the sentence does not.
	rc := baseRun("completed")
	_, fp := runCompleted(t, rc, failedCodeCC(rc, txt("timed_out"), "claude"), nil, nil)
	if got := threadPost(t, fp).contextText; !strings.HasSuffix(got, "Code cross-check incomplete: check deadline expired.") {
		t.Errorf("no-MR parent: %q", got)
	}
}

func TestCodeCrossCheckAdvisoryScopeCapped(t *testing.T) {
	rc := codeCCParent()
	rc.StopKind = txt("scope_capped")
	base := baselineThread(t, rc)
	_, fp := runCompleted(t, rc, failedCodeCC(rc, txt("model_timeout"), "codex"), nil, nil)
	got := threadPost(t, fp)
	if !strings.Contains(got.sectionText, "narrowed by operator scope directive") || got.sectionText != base.sectionText {
		t.Errorf("scope-capped label changed: %q", got.sectionText)
	}
	if want := base.contextText + "Code cross-check incomplete: model timed out. Publication continued."; got.contextText != want {
		t.Errorf("context = %q, want %q", got.contextText, want)
	}
	if !strings.HasPrefix(got.fallback, "Completed (scope-capped) · ") {
		t.Errorf("scope-capped fallback changed: %q", got.fallback)
	}
}

// Controls: each sends the original completion message byte-identical to the no-record
// baseline. "pending" and the unreachable shapes are fixture-level controls.
func TestCodeCrossCheckAdvisoryAbsentControls(t *testing.T) {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	cases := map[string]func(cc *store.CrossCheck){
		"completed outcome (legacy verdict failed)": func(cc *store.CrossCheck) { cc.Outcome = txt("completed"); cc.ReasonClass = pgtype.Text{} },
		"pending":                func(cc *store.CrossCheck) { cc.Outcome = txt("pending"); cc.ReasonClass = pgtype.Text{} },
		"null outcome":           func(cc *store.CrossCheck) { cc.Outcome = pgtype.Text{} },
		"interrupted timed_out":  func(cc *store.CrossCheck) { cc.ReasonClass = txt("timed_out"); cc.InterruptedAt = now },
		"interrupted superseded": func(cc *store.CrossCheck) { cc.ReasonClass = txt("superseded"); cc.InterruptedAt = now },
		"superseded":             func(cc *store.CrossCheck) { cc.ReasonClass = txt("superseded") },
		"interrupted pre-start":  func(cc *store.CrossCheck) { cc.ReasonClass = txt("snapshot_failed"); cc.InterruptedAt = now },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rc := codeCCParent()
			base := baselineThread(t, rc)
			cc := failedCodeCC(rc, txt("model_error"), "codex")
			mutate(cc)
			_, fp := runCompleted(t, rc, cc, nil, nil)
			if got := threadPost(t, fp); !reflect.DeepEqual(got, base) {
				t.Errorf("want the baseline completion message, got %+v (baseline %+v)", got, base)
			}
		})
	}
	// The baseline itself is the normal completion: label, MR link, parent link.
	base := baselineThread(t, codeCCParent())
	if base.sectionText != "✅ *Completed*" || !strings.Contains(base.contextText, codeCCPublishedURL) || !strings.Contains(base.contextText, "Open in uzi") {
		t.Errorf("baseline is not the normal completion message: %+v", base)
	}
}

func TestCodeCrossCheckAdvisorySummaryReadFailure(t *testing.T) {
	rc := codeCCParent()
	base := baselineThread(t, rc)
	var logs bytes.Buffer
	_, fp := runCompleted(t, rc, nil, errors.New("db down"), &logs)
	if got := threadPost(t, fp); !reflect.DeepEqual(got, base) {
		t.Errorf("read failure must leave the completion message unchanged: %+v (baseline %+v)", got, base)
	}
	if !strings.Contains(logs.String(), "load code cross-check summary failed") {
		t.Errorf("read failure not logged: %q", logs.String())
	}
	// No row is silent.
	logs.Reset()
	runCompleted(t, rc, nil, nil, &logs)
	if strings.Contains(logs.String(), "code cross-check") {
		t.Errorf("no row must not log: %q", logs.String())
	}
}

// Hostile stored content (reason text, findings, a forged URL) never reaches the line.
func TestCodeCrossCheckAdvisoryHostileContent(t *testing.T) {
	rc := codeCCParent()
	rc.ForgeType = "gitlab"
	rc.MrWebUrl = txt("javascript:<!channel>")
	cc := failedCodeCC(rc, txt("model_error <!channel> <https://evil.example|click>\x1b[31m\n*bold*"), "codex")
	cc.Findings = []byte(`[{"title":"<!here> leak","body":"https://evil.example diff --git"}]`)
	_, fp := runCompleted(t, rc, cc, nil, nil)
	got := threadPost(t, fp)
	if !strings.HasSuffix(got.contextText, "Code cross-check incomplete: reason unavailable.") {
		t.Fatalf("want the fixed unavailable advisory, got %q", got.contextText)
	}
	for _, bad := range []string{"<!channel>", "<!here>", "evil.example", "\x1b", "*bold*", "diff --git", "Publication continued"} {
		if strings.Contains(got.contextText, bad) || strings.Contains(got.fallback, bad) {
			t.Errorf("hostile content %q reached the message: %q / %q", bad, got.contextText, got.fallback)
		}
	}
}

// Reprocessing the same completion event posts exactly what the baseline posts for that
// event, each post carrying the advisory once; it adds no message of its own.
func TestCodeCrossCheckAdvisoryRepeatedEvent(t *testing.T) {
	count := func(cc *store.CrossCheck) []blockCall {
		rc := codeCCParent()
		if cc != nil {
			cc.LeadRunID = rc.ID
		}
		fs := &fakeNotifStore{rc: rc, delivery: txt("U123"), codeCC: cc,
			msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
		fp := &fakePoster{dmChannel: "D1"}
		n := NewNotifier(fs, fp, fixedBase, nil)
		for range 2 {
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})
		}
		return fp.blocks
	}
	base := count(nil)
	with := count(failedCodeCC(codeCCParent(), txt("model_error"), "codex"))
	if len(base) < 2 {
		t.Fatalf("baseline must post the completion message once per event, got %d posts", len(base))
	}
	if len(with) != len(base) {
		t.Fatalf("advisory changed the post count: %d vs baseline %d", len(with), len(base))
	}
	for _, p := range with {
		if strings.Count(p.contextText, "Code cross-check incomplete") != 1 {
			t.Errorf("each completion post carries the advisory exactly once: %q", p.contextText)
		}
	}
}

// With no anchor yet, only the root posts: no thread message, so no advisory and no read.
func TestCodeCrossCheckAdvisoryNoAnchor(t *testing.T) {
	rc := codeCCParent()
	fs := &fakeNotifStore{rc: rc, delivery: txt("U123"), msgErr: pgx.ErrNoRows, codeCC: failedCodeCC(rc, txt("model_error"), "codex")}
	fp := &fakePoster{}
	NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})
	if len(fp.blocks) != 1 || fp.blocks[0].thread != "" {
		t.Fatalf("want only the root post, got %+v", fp.blocks)
	}
	if strings.Contains(fp.blocks[0].contextText+fp.blocks[0].fallback, "Code cross-check") || len(fs.codeCCAsked) != 0 {
		t.Errorf("no-anchor root must not carry or read the advisory: %+v asked=%+v", fp.blocks[0], fs.codeCCAsked)
	}
}

// A cross_check child stays DM-free and never reads the summary, anchored or not.
func TestCodeCrossCheckAdvisoryChildStaysSuppressed(t *testing.T) {
	for _, anchored := range []bool{false, true} {
		fs, fp := crossCheckSuppressionCase("cross_check", "completed", "", anchored)
		fs.codeCC = failedCodeCC(fs.rc, txt("model_error"), "codex")
		NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: fs.rc.ID, status: "completed"})
		assertNoCrossCheckDM(t, fs, fp)
		if len(fs.codeCCAsked) != 0 {
			t.Errorf("anchored=%v: child read the code summary: %+v", anchored, fs.codeCCAsked)
		}
	}
}

// A non-completed terminal (failed) never reads the summary or renders the advisory.
func TestCodeCrossCheckAdvisoryOnlyOnCompleted(t *testing.T) {
	rc := codeCCParent()
	rc.Status = "failed"
	fs := &fakeNotifStore{rc: rc, delivery: txt("U123"), codeCC: failedCodeCC(rc, txt("model_error"), "codex"),
		msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	NewNotifier(fs, fp, fixedBase, nil).handle(context.Background(), stateEvent{runID: rc.ID, status: "failed"})
	if len(fs.codeCCAsked) != 0 {
		t.Errorf("failed run read the code summary: %+v", fs.codeCCAsked)
	}
	for _, p := range fp.blocks {
		if strings.Contains(p.contextText, "Code cross-check") {
			t.Errorf("failed run carries the advisory: %+v", p)
		}
	}
}

// The advisory is its own context block, with a block_id distinct from the completed one.
func TestWithCodeCrossCheckAdvisoryBlockShape(t *testing.T) {
	blocks, fallback, _ := renderThreadBlocks(codeCCParent(), "https://uzi.example")
	same, sameFallback := withCodeCrossCheckAdvisory(blocks, fallback, "")
	if len(same) != len(blocks) || sameFallback != fallback {
		t.Fatalf("empty advisory must not change the message")
	}
	got, _ := withCodeCrossCheckAdvisory(blocks, fallback, "Code cross-check incomplete: model error.")
	if len(got) != len(blocks)+1 {
		t.Fatalf("want one appended block, got %d", len(got)-len(blocks))
	}
	cb, ok := got[len(got)-1].(*slack.ContextBlock)
	if !ok || cb.BlockID != codeCrossCheckAdvisoryBlockID {
		t.Fatalf("appended block = %#v", got[len(got)-1])
	}
	for _, b := range blocks {
		if c, ok := b.(*slack.ContextBlock); ok && c.BlockID == codeCrossCheckAdvisoryBlockID {
			t.Errorf("block_id collides with an existing block")
		}
	}
}
