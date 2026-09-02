package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #111 M1 — every claim records WHICH Anthropic credential it spent.
//
// The single fact under test throughout: the recorded id is the id whose ciphertext
// was OPENED (D8), not whatever the user's default happened to be a moment later and
// not something derived independently. That is why each case asserts the recorded id
// against the by-id lookup the open actually performed, rather than against the
// fixture's expectation — the two agreeing is the property; the fixture agreeing
// with itself is not.

// claimFixture builds a repo-ful run-lane fixture with a default credential and one
// named credential, plus the bot PAT the run lane opens before the Anthropic one.
type claimFixture struct {
	fs        *fakeStore
	svc       *Service
	owner     uuid.UUID
	consoleID uuid.UUID
	runID     uuid.UUID
}

func newClaimFixture(t *testing.T) claimFixture {
	t.Helper()
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-RUNCRED-abcdef1234567890"))
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-runcred-abcdef123"))
	sealedConsole, _ := box.Seal([]byte("anthropic-CONSOLE-runcred-abcdef123"))

	owner, consoleID, runID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: runID, UserID: owner, IssueIid: pgtype.Int8{Int64: 4, Valid: true},
			IssueTitle: "t", IssueDescription: "d", Status: "claimed",
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgconv.TextOrNull("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:          sealedDefault,
		defaultSecretLabel: "default",
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			consoleID: {
				UserID: owner, Kind: store.KindAnthropicToken,
				Ciphertext: sealedConsole, SealedWith: store.SealedWithMaster,
			},
		},
		byIDLabels: map[uuid.UUID]string{consoleID: "console-key"},
	}
	return claimFixture{fs: fs, svc: New(fs, box, testParams()), owner: owner, consoleID: consoleID, runID: runID}
}

// onlyRecord returns the single credential write the fixture made, failing if the
// claim recorded none or more than one. "Exactly one" is part of the contract: a
// second write would mean a lane recorded before the open as well as after.
func onlyRecord(t *testing.T, fs *fakeStore) store.SetRunAnthropicSecretParams {
	t.Helper()
	if len(fs.recordedCreds) != 1 {
		t.Fatalf("credential writes = %d, want exactly 1: %+v", len(fs.recordedCreds), fs.recordedCreds)
	}
	return fs.recordedCreds[0]
}

// TestClaimRecordsOwnerDefaultCredential: an UNBOUND worker's claim records the
// owner's default — which is only expressible at all because D8 resolves that
// default to a concrete id before opening it. Before PRD #111 the default was
// resolved inside secretopen's by-kind ciphertext query and no id ever escaped, so
// this run could not have said what it spent.
func TestClaimRecordsOwnerDefaultCredential(t *testing.T) {
	f := newClaimFixture(t)

	payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a payload, got idle")
	}

	rec := onlyRecord(t, f.fs)
	if rec.ID != f.runID || rec.UserID != f.owner {
		t.Fatalf("recorded against run/user (%v,%v), want (%v,%v)", rec.ID, rec.UserID, f.runID, f.owner)
	}
	// The recorded id is the OPENED id — asserted against the lookup the open made,
	// not against the fixture's own idea of what the default is.
	if len(f.fs.byIDLookups) != 1 {
		t.Fatalf("by-id opens = %d, want 1", len(f.fs.byIDLookups))
	}
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fs.byIDLookups[0].ID {
		t.Fatalf("recorded %v but opened %v — the record must name the credential that was actually decrypted (D8)",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.fs.byIDLookups[0].ID)
	}
	if rec.AnthropicSecretLabel.String != "default" {
		t.Fatalf("recorded label = %q, want default", rec.AnthropicSecretLabel.String)
	}
	if rec.AnthropicSelectReason.String != selectReasonDefault {
		t.Fatalf("recorded reason = %q, want %q", rec.AnthropicSelectReason.String, selectReasonDefault)
	}
	// M1 measures no headroom and must not invent one; M4 is its first writer.
	if rec.AnthropicHeadroomPct.Valid {
		t.Fatalf("recorded headroom %+v, want NULL", rec.AnthropicHeadroomPct)
	}
	// And the label came from a lookup scoped to the RUN OWNER, which is the whole
	// reason it is read alongside the id rather than by a second unscoped query.
	if len(f.fs.defaultMetaLookups) != 1 || f.fs.defaultMetaLookups[0].UserID != f.owner {
		t.Fatalf("default meta lookups = %+v, want exactly one scoped to %v", f.fs.defaultMetaLookups, f.owner)
	}
	if f.fs.defaultMetaLookups[0].Kind != store.KindAnthropicToken {
		t.Fatalf("default meta lookup kind = %q, want %q", f.fs.defaultMetaLookups[0].Kind, store.KindAnthropicToken)
	}
}

// TestClaimRecordsWorkerBoundCredential: a BOUND worker's claim records that
// credential and calls the mode "pinned", so the run view can tell a deliberate pin
// from a default that happens to name the same token (D20).
func TestClaimRecordsWorkerBoundCredential(t *testing.T) {
	f := newClaimFixture(t)
	wkr := store.Worker{
		ID: uuid.New(), UserID: f.owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: f.consoleID, Valid: true},
	}

	if _, err := f.svc.Claim(context.Background(), wkr); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.consoleID {
		t.Fatalf("recorded %v, want the bound credential %v", uuid.UUID(rec.AnthropicSecretID.Bytes), f.consoleID)
	}
	if rec.AnthropicSecretLabel.String != "console-key" {
		t.Fatalf("recorded label = %q, want console-key", rec.AnthropicSecretLabel.String)
	}
	if rec.AnthropicSelectReason.String != selectReasonPinned {
		t.Fatalf("recorded reason = %q, want %q", rec.AnthropicSelectReason.String, selectReasonPinned)
	}
	// The label lookup is owner-scoped in its own right. This is the assertion that
	// would fail if the label were ever fetched by `SELECT label WHERE id = $1`.
	if len(f.fs.metaByIDLookups) != 1 {
		t.Fatalf("by-id meta lookups = %d, want 1", len(f.fs.metaByIDLookups))
	}
	if f.fs.metaByIDLookups[0].UserID != f.owner || f.fs.metaByIDLookups[0].ID != f.consoleID {
		t.Fatalf("label lookup was (%v,%v), want (%v,%v) — an unscoped label read hands the claim another user's label",
			f.fs.metaByIDLookups[0].ID, f.fs.metaByIDLookups[0].UserID, f.consoleID, f.owner)
	}
}

// TestClaimRecordsNothingWhenOpenFails: a credential that could not be opened was
// never spent, so recording it would be a lie — and specifically the kind of lie
// that makes a run's attribution worse than having none. The claim still fails
// terminally, exactly as it did before.
func TestClaimRecordsNothingWhenOpenFails(t *testing.T) {
	f := newClaimFixture(t)
	wkr := store.Worker{
		ID: uuid.New(), UserID: f.owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
	}

	payload, err := f.svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("a claim whose credential does not resolve must not produce a payload")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded %+v for a credential that never opened", f.fs.recordedCreds)
	}
	if f.fs.markedFailed == nil {
		t.Fatal("the run should have been failed with credential-unavailable")
	}
}

// TestTokenlessUserKeepsItsFailureText: the auditor's item on D8. A token-less user
// now raises pgx.ErrNoRows from the metadata RESOLVE — a query that did not exist
// before — and if that propagated raw, a case that has always been a clean terminal
// credential failure would become an opaque 500 with different text in
// runs.failure_reason. The string is asserted verbatim because e2e and handler
// assertions read it.
func TestTokenlessUserKeepsItsFailureText(t *testing.T) {
	f := newClaimFixture(t)
	f.fs.anthropicErr = pgx.ErrNoRows // no default row at all

	payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err != nil {
		t.Fatalf("Claim returned an error instead of failing the run: %v", err)
	}
	if payload != nil {
		t.Fatal("a token-less user's claim must not produce a payload")
	}
	if f.fs.markedFailed == nil {
		t.Fatal("the run should have been failed")
	}
	const want = "credential unavailable: no Anthropic token configured for this user"
	if got := f.fs.markedFailed.FailureReason.String; got != want {
		t.Fatalf("failure reason = %q, want %q — D8 moved where this error is raised, "+
			"and it must not move what the user is told", got, want)
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded %+v for a user with no credential", f.fs.recordedCreds)
	}
}

// TestDefaultDeletedBetweenResolveAndOpen documents the ONE behaviour change D8
// buys its guarantee with, so it is a decision on the record rather than a
// discovery. If the default token is deleted in the window between resolving its id
// and opening it, the claim now fails terminally; the pre-D8 single statement would
// have opened whatever the new default was. Narrow and accepted — the same delete a
// moment earlier produces the same outcome — and PRD #111 D14 adds a retry for the
// auto lane, where an optimizer-caused failure would be a regression rather than a
// race.
func TestDefaultDeletedBetweenResolveAndOpen(t *testing.T) {
	f := newClaimFixture(t)
	f.fs.defaultCiphertextErr = pgx.ErrNoRows // resolve succeeded, the open finds nothing

	payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected no payload")
	}
	if f.fs.markedFailed == nil {
		t.Fatal("the run should have been failed with credential-unavailable")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded %+v although nothing was opened", f.fs.recordedCreds)
	}
}

// TestRecordFailureFailsTheClaim: recording is not best-effort. A claim that
// delivered a token but could not say which one is precisely the silent
// mis-attribution this milestone exists to remove, so the write failing fails the
// claim — cheaply, because the run stays 'claimed' with no payload and
// SweepClaimedNeverStarted requeues it at ClaimGrace.
func TestRecordFailureFailsTheClaim(t *testing.T) {
	f := newClaimFixture(t)
	f.fs.recordCredErr = errors.New("boom")

	_, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err == nil {
		t.Fatal("a failed credential record must fail the claim, not deliver an unattributable payload")
	}
	if !strings.Contains(err.Error(), "record run anthropic credential") {
		t.Fatalf("error should name the record write, got: %v", err)
	}
}

// TestRecordVanishedRunDropsTheClaim: 0 rows affected is not a write failure, it is
// the run having disappeared under the claim (its forge connection cascade-deleted
// the repo → run). Every other claim-path reader treats that as errRunVanished and
// drops the claim silently, and so does this one — no run to fail, nothing to report.
func TestRecordVanishedRunDropsTheClaim(t *testing.T) {
	f := newClaimFixture(t)
	var zero int64
	f.fs.recordCredRows = &zero

	payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner})
	if err != nil {
		t.Fatalf("a vanished run must be dropped, not surfaced as an error: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle for a vanished run")
	}
	if f.fs.markedFailed != nil {
		t.Fatal("a vanished run must not be marked failed — there is no row to fail")
	}
}

// TestJudgeClaimRecordsItsBinding: the judge lane records too, and it is the lane
// where this matters most — the judge binding exists so retrospectives can be billed
// to a separate account, and until now nothing in the data said whether they were.
func TestJudgeClaimRecordsItsBinding(t *testing.T) {
	box := newBox(t)
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-judgerec-abcdef12"))
	sealedJudge, _ := box.Seal([]byte("anthropic-JUDGEKEY-judgerec-abcdef1"))

	owner, judgeID, runID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: runID, Kind: runkind.Judge, Status: "claimed",
			IssueTitle: "judge", IssueDescription: "d", UserID: owner,
		},
		anthropic:   sealedDefault,
		judgeSecret: pgtype.UUID{Bytes: judgeID, Valid: true},
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			judgeID: {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedJudge, SealedWith: store.SealedWithMaster},
		},
		byIDLabels: map[uuid.UUID]string{judgeID: "review-key"},
	}
	svc := New(fs, box, testParams())

	if _, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: owner}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	rec := onlyRecord(t, fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != judgeID || rec.AnthropicSecretLabel.String != "review-key" {
		t.Fatalf("judge run recorded (%v,%q), want (%v,\"review-key\")",
			uuid.UUID(rec.AnthropicSecretID.Bytes), rec.AnthropicSecretLabel.String, judgeID)
	}
	// `judge`, not `pinned`. This assertion said `pinned` through M1, when the
	// vocabulary held two values and the judge binding had to borrow one; M4 closed the
	// set and gave the lane its own. It matters because D20 makes the run view name the
	// MODE: "pinned" tells a user their WORKER is bound to review-key, so they go
	// looking at Settings → Workers, where no such binding exists — the choice was made
	// by their judge setting, on a different page.
	if rec.AnthropicSelectReason.String != selectReasonJudge {
		t.Fatalf("judge reason = %q, want %q (the JUDGE binding named it, not a worker's)",
			rec.AnthropicSelectReason.String, selectReasonJudge)
	}
	if rec.ID != runID {
		t.Fatalf("judge recorded against run %v, want %v", rec.ID, runID)
	}
}

// TestChatClaimRecordsTheDefault: chat is deliberately not bindable (D5), so its
// reason is always "default". The record is still not redundant — it is what lets
// M4's in-flight bias see chat spend against a credential at all (D18), which R3
// previously listed as a limit precisely because no per-run record existed.
func TestChatClaimRecordsTheDefault(t *testing.T) {
	box := newBox(t)
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-chatrec-abcdef123"))

	owner, runID := uuid.New(), uuid.New()
	fs := &fakeStore{
		chatClaimRun: store.Run{
			ID: runID, UserID: owner, Kind: runkind.Chat, Status: "claimed",
			Title: pgconv.TextOrNull("a chat"),
		},
		anthropic:          sealedDefault,
		defaultSecretLabel: "default",
	}
	svc := New(fs, box, testParams())

	payload, err := svc.ClaimChat(context.Background(), store.Worker{ID: uuid.New(), UserID: owner})
	if err != nil {
		t.Fatalf("ClaimChat: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a chat payload, got idle")
	}
	rec := onlyRecord(t, fs)
	if rec.ID != runID || rec.UserID != owner {
		t.Fatalf("chat recorded against (%v,%v), want (%v,%v)", rec.ID, rec.UserID, runID, owner)
	}
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != fs.defaultCredID() {
		t.Fatalf("chat recorded %v, want the owner's default %v", uuid.UUID(rec.AnthropicSecretID.Bytes), fs.defaultCredID())
	}
	if rec.AnthropicSelectReason.String != selectReasonDefault {
		t.Fatalf("chat reason = %q, want %q — chat has no binding to pin", rec.AnthropicSelectReason.String, selectReasonDefault)
	}
}
