package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// reviewFake builds a FakeClient whose run "r1" carries a review with two
// recommendations, so the short-id resolution paths (unambiguous, ambiguous,
// no-match) can be exercised. The two ids share the 8-char prefix "aaaaaaaa" so a
// short prefix is ambiguous while a longer one is not.
func reviewFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"r1": {
			ID:      "rv1",
			Verdict: "needs_work",
			Status:  "complete",
			Recommendations: []apitypes.RecommendationDTO{
				{ID: "aaaaaaaa-1111-4111-8111-000000000001", Category: "improve_uzi", Target: "docs", Confidence: "high", RationaleMd: "add examples"},
				{ID: "aaaaaaaa-2222-4222-8222-000000000002", Category: "tests", Target: "unit", Confidence: "low"},
			},
			Dispositions: []apitypes.DispositionDTO{
				{Category: "improve_uzi", Target: "docs", Status: "done"},
			},
			Triage: apitypes.TriageDTO{Total: 2, Todo: 1, Done: 1},
		},
	}}
}

// resolve marks a recommendation done: it resolves the short id to the full rec
// id and SetDisposition receives status=done, no reason.
func TestReviewResolve(t *testing.T) {
	fc := reviewFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "r1", "aaaaaaaa-2222-4222-8222-000000000002")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDispositionStatus != "done" || fc.LastDispositionReason != "" {
		t.Errorf("resolve wire = (%q,%q), want (done, \"\")", fc.LastDispositionStatus, fc.LastDispositionReason)
	}
	if fc.LastDispositionRecID != "aaaaaaaa-2222-4222-8222-000000000002" {
		t.Errorf("resolve rec id = %q, want the full second rec id", fc.LastDispositionRecID)
	}
}

// resolve accepts the git-style short id from `show` (unambiguous prefix) and
// resolves it to the matching full rec id.
func TestReviewResolveShortID(t *testing.T) {
	fc := reviewFake()
	// "aaaaaaaa-1" uniquely prefixes the first rec (the second is "aaaaaaaa-2").
	_, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "r1", "aaaaaaaa-1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDispositionRecID != "aaaaaaaa-1111-4111-8111-000000000001" {
		t.Errorf("short id resolved to %q, want the first full rec id", fc.LastDispositionRecID)
	}
}

// An ambiguous prefix (shared by both recs) is a usage error (exit 2) and writes
// nothing.
func TestReviewResolveAmbiguousShortID(t *testing.T) {
	fc := reviewFake()
	_, errb, code := runCLI(t, fakeEnv(fc), "review", "resolve", "r1", "aaaaaaaa")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastDispositionStatus != "" {
		t.Errorf("ambiguous id should not write a disposition, got status %q", fc.LastDispositionStatus)
	}
	if !strings.Contains(errb, "ambiguous") {
		t.Errorf("want an 'ambiguous' hint, got:\n%s", errb)
	}
}

// An unknown id is a not-found (exit 4) with a refresh hint.
func TestReviewResolveUnknownID(t *testing.T) {
	fc := reviewFake()
	_, errb, code := runCLI(t, fakeEnv(fc), "review", "resolve", "r1", "deadbeef")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
	if fc.LastDispositionStatus != "" {
		t.Errorf("unknown id should not write, got status %q", fc.LastDispositionStatus)
	}
	if !strings.Contains(errb, "refresh") {
		t.Errorf("want a 'refresh' hint, got:\n%s", errb)
	}
}

// dismiss --reason wont-do maps to the wire enum wont_do.
func TestReviewDismissWontDo(t *testing.T) {
	fc := reviewFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss", "r1", "aaaaaaaa-1", "--reason", "wont-do")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDispositionStatus != "dismissed" || fc.LastDispositionReason != "wont_do" {
		t.Errorf("dismiss wire = (%q,%q), want (dismissed, wont_do)", fc.LastDispositionStatus, fc.LastDispositionReason)
	}
}

// dismiss --reason not-an-issue maps to the wire enum not_an_issue.
func TestReviewDismissNotAnIssue(t *testing.T) {
	fc := reviewFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss", "r1", "aaaaaaaa-1", "--reason", "not-an-issue")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDispositionStatus != "dismissed" || fc.LastDispositionReason != "not_an_issue" {
		t.Errorf("dismiss wire = (%q,%q), want (dismissed, not_an_issue)", fc.LastDispositionStatus, fc.LastDispositionReason)
	}
}

// A missing --reason is a usage error (exit 2) raised BEFORE any resolve/write.
func TestReviewDismissMissingReason(t *testing.T) {
	fc := reviewFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss", "r1", "aaaaaaaa-1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastDispositionStatus != "" {
		t.Errorf("missing --reason must not write, got status %q", fc.LastDispositionStatus)
	}
	// No resolve either: the rec id capture stays empty because validation short-
	// circuits before the network round-trip.
	if fc.LastDispositionRecID != "" {
		t.Errorf("missing --reason must not resolve/write, got rec id %q", fc.LastDispositionRecID)
	}
}

// An unrecognised --reason value is a usage error (exit 2) and writes nothing.
func TestReviewDismissBadReason(t *testing.T) {
	fc := reviewFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss", "r1", "aaaaaaaa-1", "--reason", "meh")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastDispositionStatus != "" {
		t.Errorf("bad --reason must not write, got status %q", fc.LastDispositionStatus)
	}
}

// undo removes a disposition: DeleteDisposition receives the resolved full rec id.
func TestReviewUndo(t *testing.T) {
	fc := reviewFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "undo", "r1", "aaaaaaaa-1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDispositionRecID != "aaaaaaaa-1111-4111-8111-000000000001" {
		t.Errorf("undo rec id = %q, want the first full rec id", fc.LastDispositionRecID)
	}
}

// undo softens ErrNoDisposition (the DELETE-404) to a friendly line + exit 0 —
// treated as already-undone, never a crash (Decision 6).
func TestReviewUndoNoDispositionExit0(t *testing.T) {
	fc := reviewFake()
	fc.DeleteDispositionErr = uzicli.ErrNoDisposition
	out, _, code := runCLI(t, fakeEnv(fc), "review", "undo", "r1", "aaaaaaaa-1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (already undone)", code)
	}
	if !strings.Contains(out, "no disposition to undo") {
		t.Errorf("want a friendly already-undone line, got:\n%s", out)
	}
}

// stats renders the human table with the triage totals.
func TestReviewStatsTable(t *testing.T) {
	fc := &uzicli.FakeClient{JudgeStatsResult: apitypes.TriageDTO{
		Total: 9, Todo: 4, Filed: 2, Done: 2, Dismissed: 1, FalsePositives: 1,
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "stats")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "TOTAL") || !strings.Contains(out, "FALSE POSITIVES") {
		t.Errorf("stats table missing labels:\n%s", out)
	}
	if !strings.Contains(out, "9") {
		t.Errorf("stats table missing the total:\n%s", out)
	}
}

// stats --json emits the raw (unenveloped) TriageDTO.
func TestReviewStatsJSON(t *testing.T) {
	fc := &uzicli.FakeClient{JudgeStatsResult: apitypes.TriageDTO{Total: 3, FalsePositives: 1}}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "stats", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"total": 3`) || !strings.Contains(out, `"false_positives": 1`) {
		t.Errorf("stats --json unexpected:\n%s", out)
	}
}

// show renders the short rec id, the per-row disposition chip, and the triage line.
func TestReviewShowRendersIDsAndTriage(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(reviewFake()), "review", "show", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "aaaaaaaa") {
		t.Errorf("show missing a short rec id:\n%s", out)
	}
	if !strings.Contains(out, "(done)") {
		t.Errorf("show missing the disposition chip:\n%s", out)
	}
	if !strings.Contains(out, "triage:") || !strings.Contains(out, "false positive") {
		t.Errorf("show missing the triage line:\n%s", out)
	}
}

// show sanitizes untrusted judge free text for the human TTY (Risk 13): ANSI/CSI
// control bytes are stripped while visible text survives.
func TestReviewShowSanitizesHumanRender(t *testing.T) {
	// The ESC bytes sit between whole words so the visible text is contiguous after
	// the control byte (only 0x1b) is stripped — sanitizeTTY drops C0/C1, not the
	// printable CSI letters that follow.
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"r1": {Verdict: "needs_work", Status: "complete", SummaryMd: "\x1bclean summary",
			Recommendations: []apitypes.RecommendationDTO{
				{ID: "abcdef01-0000-4000-8000-000000000000", Category: "improve_uzi", Target: "\x1bdocs", Confidence: "high"},
			}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "show", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("human render leaked ESC (0x1b):\n%q", out)
	}
	for _, want := range []string{"clean summary", "docs", "abcdef01"} {
		if !strings.Contains(out, want) {
			t.Errorf("human render dropped visible text %q:\n%q", want, out)
		}
	}
}

// A read-only uza_ token is refused (404) on an owner-only mutation (Decision 5).
// This models uza_ FAITHFULLY: it READS another user's review fine (owner-or-admin,
// GetReviewForTarget) and is refused ONLY on the strict-owner-only WRITE. So the
// review fetch succeeds (resolveRecID resolves the short id) and SetDisposition
// returns the 404 — exit 4. A prior version set a GLOBAL Err, which failed the
// resolve READ first and never reached the write, so it proved nothing about the
// write-path refusal; SetDispositionErr expresses "read OK, write 404".
func TestReviewMutationRefusedForReadOnlyToken(t *testing.T) {
	fc := reviewFake() // a valid, readable review — the uza_ read succeeds
	fc.SetDispositionErr = uzicli.Exitf(uzicli.ExitNotFound, "run not found")
	_, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "r1", "aaaaaaaa-1")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
	// The write was actually REACHED (read + resolve succeeded) then refused — this
	// is the owner-only mutation gate, not a read failure. Prove the resolve mapped
	// the short id to the full rec id and the write carried the intended verdict.
	if fc.LastDispositionStatus != dispStatusDone {
		t.Errorf("SetDisposition not reached (status=%q) — the test would pass on any read error, proving nothing", fc.LastDispositionStatus)
	}
	if fc.LastDispositionRecID != "aaaaaaaa-1111-4111-8111-000000000001" {
		t.Errorf("resolve did not map the short id before the refusal, got rec id %q", fc.LastDispositionRecID)
	}
}

// The undo (DELETE) path is likewise owner-only: a uza_ token reads the review but
// its delete is refused (404). Distinct from ErrNoDisposition, which is the benign
// already-undone soft path (exit 0); a 404 from the write is a hard refusal (exit 4).
func TestReviewUndoRefusedForReadOnlyToken(t *testing.T) {
	fc := reviewFake()
	fc.DeleteDispositionErr = uzicli.Exitf(uzicli.ExitNotFound, "run not found")
	_, _, code := runCLI(t, fakeEnv(fc), "review", "undo", "r1", "aaaaaaaa-1")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
	if fc.LastDispositionRecID != "aaaaaaaa-1111-4111-8111-000000000001" {
		t.Errorf("undo did not reach the delete with the resolved rec id, got %q", fc.LastDispositionRecID)
	}
}

// The deprecated `uzi run review` alias still works (shares runReviewShow) and its
// cobra deprecation notice goes to STDERR, keeping --json stdout pure.
func TestRunReviewDeprecatedAliasNoticeOnStderr(t *testing.T) {
	fc := reviewFake()
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "review", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out, "deprecated") {
		t.Errorf("deprecation notice leaked to stdout (would corrupt --json):\n%s", out)
	}
	if !strings.Contains(errb, "deprecated") {
		t.Errorf("want the deprecation notice on stderr, got:\n%s", errb)
	}
	if !strings.Contains(out, `"review"`) {
		t.Errorf("alias --json did not emit the review envelope:\n%s", out)
	}
}
