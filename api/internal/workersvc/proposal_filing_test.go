package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// proposalFixture wires a Service whose fake store + forge satisfy every gate of the
// happy-path scheduled issues-mode filing. Individual tests perturb one field to prove
// a gate or the clamp. It returns the Service, the fake store (to read what was
// created/stamped), the fake forge (to read the CreateIssue args), and the completed
// run to hand maybeFileProposal.
func proposalFixture(t *testing.T, slug string) (*Service, *fakeStore, *fakeForge, store.Run) {
	t.Helper()
	schedID := uuid.New()
	repoID := uuid.New()
	fs := &fakeStore{
		runSchedule: store.RunSchedule{
			ID:          schedID,
			Target:      "prompt",
			OutputMode:  pgtype.Text{String: "issues", Valid: true},
			CatalogSlug: pgtype.Text{String: slug, Valid: slug != ""},
		},
		repoRow: store.GetRepoForUserRow{
			ID:              repoID,
			ForgeProjectID:  42,
			ForgeType:       "gitlab",
			BaseUrl:         "https://gitlab.example",
			TokenCiphertext: []byte("ciphertext"),
		},
		stampProposalRows: 1,
	}
	fb := &fakeForges{f: &fakeForge{created: forge.Issue{IID: 7, WebURL: "https://gitlab.example/-/issues/7"}}}
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(fb)
	run := store.Run{
		ID:         uuid.New(),
		UserID:     uuid.New(),
		RepoID:     pgtype.UUID{Bytes: repoID, Valid: true},
		Kind:       runkind.Prompt,
		Status:     "completed",
		ScheduleID: pgtype.UUID{Bytes: schedID, Valid: true},
	}
	return svc, fs, fb.f, run
}

func aProposal() *ProposalPayload {
	return &ProposalPayload{Title: "Add a retry backoff to the poller", Body: "The poller should back off and retry."}
}

// TestMaybeFileProposalFilesExactlyMarkerLabel is the D3 never-sweepable invariant:
// a filed proposal carries EXACTLY []string{"proposal::<slug>"} — no uzi, no selector,
// no assignment — and the audit row is stamped confirmed with the issue iid.
func TestMaybeFileProposalFilesExactlyMarkerLabel(t *testing.T) {
	svc, fs, ff, run := proposalFixture(t, "feature-bingo")
	p := aProposal()

	svc.maybeFileProposal(context.Background(), run, p)

	if ff.createCalls != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", ff.createCalls)
	}
	if got, want := ff.createLabels, []string{"proposal::feature-bingo"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("filed labels = %v, want exactly %v (D3 never-sweepable)", got, want)
	}
	for _, forbidden := range []string{"uzi", "Planned", "bug", "refactor"} {
		for _, l := range ff.createLabels {
			if l == forbidden {
				t.Fatalf("filed labels %v must never contain %q", ff.createLabels, forbidden)
			}
		}
	}
	if ff.createTitle != p.Title || ff.createBody != p.Body {
		t.Errorf("CreateIssue got title=%q body=%q, want title=%q body=%q", ff.createTitle, ff.createBody, p.Title, p.Body)
	}
	if fs.createdProposal == nil {
		t.Fatal("expected a pending audit row to be inserted")
	}
	if fs.stampedProposal == nil {
		t.Fatal("expected the audit row to be stamped confirmed")
	}
	if got := fs.stampedProposal.Iid.Int64; got != 7 {
		t.Errorf("stamped iid = %d, want 7", got)
	}
}

// TestMaybeFileProposalNoSlugUsesBareMarker: a schedule with no catalog slug files the
// bare "proposal" marker (still exactly one label).
func TestMaybeFileProposalNoSlugUsesBareMarker(t *testing.T) {
	svc, _, ff, run := proposalFixture(t, "")

	svc.maybeFileProposal(context.Background(), run, aProposal())

	if got := ff.createLabels; len(got) != 1 || got[0] != "proposal" {
		t.Fatalf("filed labels = %v, want exactly [proposal]", got)
	}
}

// TestMaybeFileProposalGates covers every silent early-return gate: each must file
// NOTHING (CreateIssue never called).
func TestMaybeFileProposalGates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fs *fakeStore, run *store.Run, p **ProposalPayload)
	}{
		{"nil proposal", func(_ *fakeStore, _ *store.Run, p **ProposalPayload) { *p = nil }},
		{"kind not prompt", func(_ *fakeStore, run *store.Run, _ **ProposalPayload) { run.Kind = runkind.Issue }},
		{"no schedule id", func(_ *fakeStore, run *store.Run, _ **ProposalPayload) { run.ScheduleID = pgtype.UUID{} }},
		{"status not completed", func(_ *fakeStore, run *store.Run, _ **ProposalPayload) { run.Status = "failed" }},
		{"output mode mr", func(fs *fakeStore, _ *store.Run, _ **ProposalPayload) {
			fs.runSchedule.OutputMode = pgtype.Text{String: "mr", Valid: true}
		}},
		{"output mode null (defaults mr)", func(fs *fakeStore, _ *store.Run, _ **ProposalPayload) {
			fs.runSchedule.OutputMode = pgtype.Text{}
		}},
		{"target not prompt", func(fs *fakeStore, _ *store.Run, _ **ProposalPayload) {
			fs.runSchedule.Target = "sweep"
		}},
		{"schedule load error", func(fs *fakeStore, _ *store.Run, _ **ProposalPayload) {
			fs.runScheduleErr = errors.New("boom")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, ff, run := proposalFixture(t, "feature-bingo")
			p := aProposal()
			tc.mutate(svc.q.(*fakeStore), &run, &p)
			svc.maybeFileProposal(context.Background(), run, p)
			if ff.createCalls != 0 {
				t.Fatalf("CreateIssue was called %d time(s); this gate must file nothing", ff.createCalls)
			}
		})
	}
}

// TestMaybeFileProposalForgesUnavailable: with no forge builder wired, filing is a
// silent no-op (and does not panic).
func TestMaybeFileProposalForgesUnavailable(t *testing.T) {
	svc, _, ff, run := proposalFixture(t, "feature-bingo")
	svc.forges = nil
	svc.maybeFileProposal(context.Background(), run, aProposal())
	if ff.createCalls != 0 {
		t.Fatalf("CreateIssue called %d times with no forges wired", ff.createCalls)
	}
}

// TestMaybeFileProposalCreateIssueErrorIsBestEffort: a forge CreateIssue error does
// not panic and does not stamp the audit row confirmed (the issue does not exist).
func TestMaybeFileProposalCreateIssueErrorIsBestEffort(t *testing.T) {
	svc, fs, ff, run := proposalFixture(t, "feature-bingo")
	ff.createErr = errors.New("forge rejected")

	svc.maybeFileProposal(context.Background(), run, aProposal())

	if ff.createCalls != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", ff.createCalls)
	}
	if fs.stampedProposal != nil {
		t.Error("must not stamp confirmed when the forge write failed")
	}
}

// TestMaybeFileProposalStampErrorIsBestEffort: a failure to stamp the audit row AFTER
// the issue was created only logs — filing still counts as done (no panic).
func TestMaybeFileProposalStampErrorIsBestEffort(t *testing.T) {
	svc, _, ff, run := proposalFixture(t, "feature-bingo")
	svc.q.(*fakeStore).stampProposalErr = errors.New("db down")

	svc.maybeFileProposal(context.Background(), run, aProposal())

	if ff.createCalls != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1 (the issue must still be filed)", ff.createCalls)
	}
}

// TestMaybeFileProposalCreateAuditRowErrorSkipsForge: a failure to insert the pending
// audit row BEFORE the issue exists aborts filing (no forge write).
func TestMaybeFileProposalCreateAuditRowErrorSkipsForge(t *testing.T) {
	svc, _, ff, run := proposalFixture(t, "feature-bingo")
	svc.q.(*fakeStore).createProposalErr = errors.New("db down")

	svc.maybeFileProposal(context.Background(), run, aProposal())

	if ff.createCalls != 0 {
		t.Fatalf("CreateIssue called %d times; must not file when the audit row insert failed", ff.createCalls)
	}
}

// --- clampWireProposal (the untrusted-input trust boundary) ---

func schedRun() store.Run {
	return store.Run{ID: uuid.New(), Kind: runkind.Prompt, ScheduleID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
}

func TestClampWireProposalNilAndKindGates(t *testing.T) {
	if got := clampWireProposal(schedRun(), nil); got != nil {
		t.Errorf("nil payload must clamp to nil, got %+v", got)
	}
	// Not a prompt run.
	nonPrompt := schedRun()
	nonPrompt.Kind = runkind.Issue
	if got := clampWireProposal(nonPrompt, aProposal()); got != nil {
		t.Errorf("non-prompt run must clamp to nil, got %+v", got)
	}
	// Prompt but no schedule.
	noSched := schedRun()
	noSched.ScheduleID = pgtype.UUID{}
	if got := clampWireProposal(noSched, aProposal()); got != nil {
		t.Errorf("run with no schedule must clamp to nil, got %+v", got)
	}
}

func TestClampWireProposalEmptyTitleDropped(t *testing.T) {
	if got := clampWireProposal(schedRun(), &ProposalPayload{Title: "   ", Body: "body"}); got != nil {
		t.Errorf("blank title must clamp to nil, got %+v", got)
	}
	// A title that is only control chars strips to empty → dropped.
	if got := clampWireProposal(schedRun(), &ProposalPayload{Title: "\x00\x07", Body: "body"}); got != nil {
		t.Errorf("control-char-only title must clamp to nil, got %+v", got)
	}
}

func TestClampWireProposalStripsControlCharsAndBounds(t *testing.T) {
	dirty := &ProposalPayload{
		Title: "hello\x00 world",
		Body:  "line1\x07line2" + strings.Repeat("a", ProposalBodyMaxBytes+4096),
	}
	got := clampWireProposal(schedRun(), dirty)
	if got == nil {
		t.Fatal("expected a clamped payload")
	}
	if strings.ContainsRune(got.Title, '\x00') || strings.ContainsRune(got.Body, '\x07') {
		t.Errorf("control chars survived the clamp: title=%q body starts %q", got.Title, got.Body[:20])
	}
	if len(got.Body) > ProposalBodyMaxBytes {
		t.Errorf("body = %d bytes, want <= %d", len(got.Body), ProposalBodyMaxBytes)
	}
}

// TestClampWireProposalScrubsSecrets pins the secret-redaction leg of the untrusted-
// input trust boundary: a scheduled prompt agent may have seen a repo secret during
// its run and echoed it (accidentally or via prompt injection) into its proposal, and
// the filed issue is a public forge artifact — so both Title and Body MUST be scrubbed
// before filing. Mirrors TestClampWireReportMdScrubsSecrets; without it, dropping the
// secretscrub.Scrub call left every other test green (a silent regression would leak a
// credential into a forge issue).
func TestClampWireProposalScrubsSecrets(t *testing.T) {
	// Fabricated secret-shaped strings, never a real credential.
	for _, secret := range []string{
		"glpat-" + "fake0000000000000000", // assembled from parts (.claude/rules/prds.md): a contiguous glpat-<20> literal in tracked source is rejected by GitHub Push Protection (GH013). Joined at runtime it is glpat- + 20 chars, which secretscrub matches; never a real credential
		"sk-ant-abcdef0123456789ABCDEF",
	} {
		got := clampWireProposal(schedRun(), &ProposalPayload{
			Title: "leak " + secret + " here",
			Body:  "the token was " + secret + " during the run",
		})
		if got == nil {
			t.Fatalf("expected a clamped payload for secret %q", secret)
		}
		if strings.Contains(got.Title, secret) {
			t.Errorf("secret survived scrub in title: %q", got.Title)
		}
		if strings.Contains(got.Body, secret) {
			t.Errorf("secret survived scrub in body: %q", got.Body)
		}
		if !strings.Contains(got.Title, "[redacted]") || !strings.Contains(got.Body, "[redacted]") {
			t.Errorf("expected redaction marker in both fields, got title=%q body=%q", got.Title, got.Body)
		}
	}
}
