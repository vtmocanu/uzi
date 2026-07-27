package workersvc

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PRD #111 M4 — the `auto` bind mode, from the claim path's side.
//
// autoselect's own tests own the RANKING; these own the WIRING, and the difference
// is deliberate. What can only break here: a non-auto worker running the selector, a
// lane recording a headroom it did not measure, the D14 retry firing on the wrong
// error or on the wrong lane, and the query being asked for the wrong user.

var autoNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// autoParams is a REAL policy, not the zero value. testParams() leaves Autoselect
// zero — MaxStaleness 0, i.e. nothing is ever fresh — which is the correct degraded
// behaviour for a factory with no poller (R2) and would make every case below fall
// back, passing for the wrong reason.
func autoParams() Params {
	p := testParams()
	p.Autoselect = autoselect.Policy{
		MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute, InflightPenalty: 3,
	}
	return p
}

// candRow builds one row of ListAutoSelectCandidates. headroom is expressed through
// the FIVE-hour window with the seven-day one empty, so a case that does not care
// about D22's binding-window rule reads its headroom straight off the argument.
func candRow(id uuid.UUID, label string, pooled bool, headroom int16, syncedAgo time.Duration, inFlight int64) store.ListAutoSelectCandidatesRow {
	return store.ListAutoSelectCandidatesRow{
		UserSecretID: id,
		Label:        label,
		AutoEligible: pooled,
		FiveHourPct:  pgtype.Int2{Int16: 100 - headroom, Valid: true},
		SevenDayPct:  pgtype.Int2{Int16: 0, Valid: true},
		FiveHourResetsAt: pgtype.Timestamptz{
			Time: autoNow.Add(time.Duration(headroom) * time.Hour), Valid: true,
		},
		SyncedAt:     pgtype.Timestamptz{Time: autoNow.Add(-syncedAgo), Valid: true},
		InFlightRuns: inFlight,
	}
}

// autoFixture is a run-lane claim fixture with an AUTO worker and two named,
// openable credentials beside the owner's default.
type autoFixture struct {
	fs      *fakeStore
	svc     *Service
	worker  store.Worker
	owner   uuid.UUID
	runID   uuid.UUID
	emptyID uuid.UUID // "spare-key"
	fullID  uuid.UUID // "console-key"
}

func newAutoFixture(t *testing.T) autoFixture {
	t.Helper()
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-AUTOSEL-abcdef1234567890"))
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-autosel-abcdef12"))
	sealedEmpty, _ := box.Seal([]byte("anthropic-SPAREKEY-autosel-abcdef1"))
	sealedFull, _ := box.Seal([]byte("anthropic-CONSOLE-autosel-abcdef12"))

	owner, runID := uuid.New(), uuid.New()
	emptyID, fullID := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: runID, UserID: owner, IssueIid: pgtype.Int8{Int64: 7, Valid: true},
			IssueTitle: "t", IssueDescription: "d", Status: "claimed",
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:          sealedDefault,
		defaultSecretLabel: "default",
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			emptyID: {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedEmpty, SealedWith: store.SealedWithMaster},
			fullID:  {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedFull, SealedWith: store.SealedWithMaster},
		},
		byIDLabels: map[uuid.UUID]string{emptyID: "spare-key", fullID: "console-key"},
	}
	svc := New(fs, box, autoParams())
	svc.now = func() time.Time { return autoNow }
	return autoFixture{
		fs: fs, svc: svc, owner: owner, runID: runID, emptyID: emptyID, fullID: fullID,
		worker: store.Worker{ID: uuid.New(), UserID: owner, AnthropicBindMode: BindModeAuto},
	}
}

func (f autoFixture) claim(t *testing.T) *ClaimPayload {
	t.Helper()
	payload, err := f.svc.Claim(context.Background(), f.worker)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return payload
}

// --- the happy path ------------------------------------------------------------

// TestAutoClaimSpendsTheSelectorsPick: an auto worker's claim opens and records the
// credential the ranker chose, and the two are the SAME id — asserted against the
// by-id open the claim actually performed rather than against the fixture's idea of
// which token is emptier, because the fixture agreeing with itself is not the
// property under test.
//
// MUTATION THIS CATCHES: autoChoice returning `rows[0]`'s id instead of the ranked
// pick — the fixture lists the fuller token first, so it reddens on the id.
func TestAutoClaimSpendsTheSelectorsPick(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.fullID, "console-key", true, 20, time.Minute, 0),
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0),
	}

	if payload := f.claim(t); payload == nil {
		t.Fatal("expected a payload, got idle")
	}

	// The query is scoped to the RUN OWNER, once.
	if len(f.fs.autoCandidateLookups) != 1 || f.fs.autoCandidateLookups[0] != f.owner {
		t.Fatalf("candidate lookups = %v, want exactly one for %v", f.fs.autoCandidateLookups, f.owner)
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("recorded %v, want the emptier token %v", uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	// Recorded == opened. The by-id opens are the bot PAT's lane aside: only the
	// Anthropic open goes through GetUserSecretCiphertextByID.
	if len(f.fs.byIDLookups) != 1 || f.fs.byIDLookups[0].ID != f.emptyID {
		t.Fatalf("by-id opens = %+v, want exactly one for %v", f.fs.byIDLookups, f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonAuto) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonAuto)
	}
	if !rec.AnthropicHeadroomPct.Valid || rec.AnthropicHeadroomPct.Int16 != 90 {
		t.Fatalf("headroom = %+v, want 90 — M4 is the first writer of this column", rec.AnthropicHeadroomPct)
	}
	// The label is the one read alongside the id at open time, NOT the one the
	// ranking query returned: the ranking query's copy is a few microseconds older
	// and, more to the point, is not the row that was decrypted (D8's argument).
	if rec.AnthropicSecretLabel.String != "spare-key" {
		t.Fatalf("recorded label = %q, want spare-key", rec.AnthropicSecretLabel.String)
	}
}

// TestAutoClaimRecordsRawHeadroomNotTheRank: the in-flight penalty is a RANKING key,
// never a reported number. Recording the penalised value would show a user a
// percentage that appears nowhere in their own meters and moves when someone else's
// run starts.
//
// MUTATION THIS CATCHES: recording Outcome.Ranked instead of Outcome.Headroom →
// 90 becomes 84.
func TestAutoClaimRecordsRawHeadroomNotTheRank(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 2), // rank 84, raw 90
	}
	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if !rec.AnthropicHeadroomPct.Valid || rec.AnthropicHeadroomPct.Int16 != 90 {
		t.Fatalf("headroom = %+v, want the RAW 90 (the rank is 84 after two in-flight runs)",
			rec.AnthropicHeadroomPct)
	}
}

// --- who does and does not run the selector ------------------------------------

// TestNonAutoWorkerNeverRunsTheSelector is R4 at the wiring level: M4 goes BEHIND
// claimSecretID, and every other mode must be bit-for-bit unchanged. The candidate
// query not running at all is the strongest available statement of that — a mode
// that ranks and then discards the answer would still be a behaviour change waiting
// for its first bug.
//
// MUTATION THIS CATCHES: hoisting the ListAutoSelectCandidates call above the mode
// switch (an "optimisation" that prefetches), which reddens both subtests.
func TestNonAutoWorkerNeverRunsTheSelector(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		bind     bool
		wantWhy  string
		wantOpen func(f autoFixture) uuid.UUID
	}{
		{
			name: "default", mode: BindModeDefault, wantWhy: selectReasonDefault,
			wantOpen: func(f autoFixture) uuid.UUID { return f.fs.defaultCredID() },
		},
		{
			name: "pinned", mode: BindModePinned, bind: true, wantWhy: selectReasonPinned,
			wantOpen: func(f autoFixture) uuid.UUID { return f.fullID },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAutoFixture(t)
			f.worker.AnthropicBindMode = tc.mode
			if tc.bind {
				f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
			}
			// A pool is staged and must be ignored: an empty pool would let this pass
			// for the wrong reason.
			f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
				candRow(f.emptyID, "spare-key", true, 99, time.Minute, 0),
			}
			f.claim(t)
			if len(f.fs.autoCandidateLookups) != 0 {
				t.Fatalf("a %s worker ran the candidate query %d time(s); the selector is behind the "+
					"mode switch and must not be reached", tc.name, len(f.fs.autoCandidateLookups))
			}
			rec := onlyRecord(t, f.fs)
			if uuid.UUID(rec.AnthropicSecretID.Bytes) != tc.wantOpen(f) {
				t.Fatalf("recorded %v, want %v", uuid.UUID(rec.AnthropicSecretID.Bytes), tc.wantOpen(f))
			}
			if rec.AnthropicSelectReason.String != tc.wantWhy {
				t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, tc.wantWhy)
			}
			if rec.AnthropicHeadroomPct.Valid {
				t.Fatalf("a %s claim recorded headroom %+v; it measured none", tc.name, rec.AnthropicHeadroomPct)
			}
		})
	}
}

// TestSelfImproveIgnoresTheWorkerBindMode is D4. A self_improve run is repo-ful and
// rides the ordinary run lane, so it reaches claimSecretID like any other — and the
// kind check is FIRST, so an auto worker never applies its mode to it. Auto is a work
// PLACEMENT decision; billing review separately is the judge binding's entire job,
// and auto-spreading it would defeat that.
//
// MUTATION THIS CATCHES: moving the bind-mode branch above the run-kind branch. The
// staged pool would then win and the claim would record `auto` against spare-key.
func TestSelfImproveIgnoresTheWorkerBindMode(t *testing.T) {
	f := newAutoFixture(t)
	judgeID := uuid.New()
	f.fs.claimRun.Kind = RunKindSelfImprove
	f.fs.judgeSecret = pgtype.UUID{Bytes: judgeID, Valid: true}
	f.fs.byIDSecrets[judgeID] = f.fs.byIDSecrets[f.fullID]
	f.fs.byIDLabels[judgeID] = "review-key"
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 99, time.Minute, 0),
	}

	f.claim(t)
	if len(f.fs.autoCandidateLookups) != 0 {
		t.Fatalf("a self_improve run ran the selector %d time(s); it follows the JUDGE binding (D4)",
			len(f.fs.autoCandidateLookups))
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != judgeID {
		t.Fatalf("recorded %v, want the judge binding %v", uuid.UUID(rec.AnthropicSecretID.Bytes), judgeID)
	}
	if rec.AnthropicSelectReason.String != selectReasonJudge {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, selectReasonJudge)
	}
}

// --- the fallback chain, from the claim path's side ----------------------------

// TestAutoFallsBackToTheOwnerDefaultAndSaysWhy is D7: auto never fails a run. Both
// non-picking outcomes resolve the worker's non-auto binding — nil for an auto
// worker, i.e. the owner's default (D9) — and record the reason, so "auto was on and
// I still got the default" is stated rather than inferred.
//
// MUTATION THIS CATCHES: recording selectReasonDefault on the fallback (the "it IS
// the default, after all" simplification), which collapses pool_empty and pool_stale
// into a value that cannot be told from a plain default-mode worker.
func TestAutoFallsBackToTheOwnerDefaultAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  func(f autoFixture) []store.ListAutoSelectCandidatesRow
		want  autoselect.Reason
		stale bool
	}{
		{
			name: "the user has opted no token in",
			rows: func(f autoFixture) []store.ListAutoSelectCandidatesRow {
				return []store.ListAutoSelectCandidatesRow{candRow(f.emptyID, "spare-key", false, 90, time.Minute, 0)}
			},
			want: autoselect.ReasonPoolEmpty,
		},
		{
			name: "the user holds no anthropic_token rows at all",
			rows: func(autoFixture) []store.ListAutoSelectCandidatesRow { return nil },
			want: autoselect.ReasonPoolEmpty,
		},
		{
			name: "every pooled reading has aged out",
			rows: func(f autoFixture) []store.ListAutoSelectCandidatesRow {
				return []store.ListAutoSelectCandidatesRow{candRow(f.emptyID, "spare-key", true, 90, 99*time.Hour, 0)}
			},
			want: autoselect.ReasonPoolStale,
		},
		{
			name: "a pooled token has never been polled",
			rows: func(f autoFixture) []store.ListAutoSelectCandidatesRow {
				r := candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0)
				r.SyncedAt, r.FiveHourPct, r.SevenDayPct = pgtype.Timestamptz{}, pgtype.Int2{}, pgtype.Int2{}
				return []store.ListAutoSelectCandidatesRow{r}
			},
			want: autoselect.ReasonPoolStale,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAutoFixture(t)
			f.fs.autoCandidates = tc.rows(f)
			if payload := f.claim(t); payload == nil {
				t.Fatal("auto fell back and the claim went idle; D7 says a run never fails for this")
			}
			rec := onlyRecord(t, f.fs)
			if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fs.defaultCredID() {
				t.Fatalf("fell back to %v, want the owner default %v",
					uuid.UUID(rec.AnthropicSecretID.Bytes), f.fs.defaultCredID())
			}
			if rec.AnthropicSelectReason.String != string(tc.want) {
				t.Fatalf("reason = %q, want %q — the fallback must name WHY, not just say `default`",
					rec.AnthropicSelectReason.String, tc.want)
			}
			if rec.AnthropicHeadroomPct.Valid {
				t.Fatalf("a fallback recorded headroom %+v; nothing measured the credential it spent",
					rec.AnthropicHeadroomPct)
			}
		})
	}
}

// TestAutoBestOfPoolSpendsAPooledToken is D10 through the claim path: when every
// pooled token is under the floor, the emptiest of THEM is spent — not the owner
// default, which may itself be the most-throttled credential the user holds and is
// not in the pool at all.
func TestAutoBestOfPoolSpendsAPooledToken(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.fullID, "console-key", true, 2, time.Minute, 0),
		candRow(f.emptyID, "spare-key", true, 11, time.Minute, 0), // still under MinHeadroom 15
	}
	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("recorded %v, want the least-bad pooled token %v",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonBestOfPool) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonBestOfPool)
	}
	if !rec.AnthropicHeadroomPct.Valid || rec.AnthropicHeadroomPct.Int16 != 11 {
		t.Fatalf("headroom = %+v, want 11", rec.AnthropicHeadroomPct)
	}
}

// TestAutoCandidateQueryErrorFailsTheClaim: a broken query must NOT degrade to the
// owner default. "The database blinked" and "you pooled nothing" are different facts,
// and treating the first as the second spends an account the user did not choose
// while raising nothing — the same reasoning judgeSecretID's error path already uses.
// The run is retried; a silent mis-spend is not, because nobody learns it happened.
//
// MUTATION THIS CATCHES: swallowing the query error into a pool_empty fallback.
func TestAutoCandidateQueryErrorFailsTheClaim(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.autoCandidatesErr = errors.New("connection reset")

	_, err := f.svc.Claim(context.Background(), f.worker)
	if err == nil {
		t.Fatal("a failed candidate query produced no error; it must not look like an empty pool")
	}
	if !strings.Contains(err.Error(), "auto-select candidates") {
		t.Fatalf("error = %v, want it to name the candidate query", err)
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential after a failed selection: %+v", f.fs.recordedCreds)
	}
	if f.fs.markedFailed != nil {
		t.Fatal("a transport error must not fail the run terminally; the claim is retried")
	}
}

// --- D14: the open-failure retry ------------------------------------------------

// TestAutoRetriesOnceWhenThePickWillNotOpen is D14, and without it D7's "auto never
// fails a run" is simply false. recoverClaimAssembly maps errCredentialUnavailable to
// a TERMINAL run failure, so a token that clears the gauge gate and then will not
// decrypt — a rotated UZI_SECRET_KEY, a corrupt row, a token deleted between the
// ranking query and the open — kills a run the owner default would have completed.
// That is the optimizer newly failing runs that static binding finished.
//
// MUTATION THIS CATCHES: deleting the retry arm → the run is marked failed and no
// payload is returned.
func TestAutoRetriesOnceWhenThePickWillNotOpen(t *testing.T) {
	f := newAutoFixture(t)
	// The pick's ciphertext is not sealed by this box: secretopen.ErrUndecryptable →
	// errCredentialUnavailable, which is exactly the terminal path D14 intercepts.
	f.fs.byIDSecrets[f.emptyID] = store.GetUserSecretCiphertextByIDRow{
		UserID: f.owner, Kind: store.KindAnthropicToken,
		Ciphertext: []byte("not-sealed-by-this-box"), SealedWith: store.SealedWithMaster,
	}
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0),
	}

	payload := f.claim(t)
	if payload == nil {
		t.Fatal("an undecryptable auto pick went idle; the owner default was openable")
	}
	if f.fs.markedFailed != nil {
		t.Fatalf("the run was failed terminally (%v); D14 exists so auto never does that", f.fs.markedFailed)
	}
	// Two opens, in order: the pick that failed, then the owner default. The second
	// is what proves it retried rather than having skipped the pick.
	if len(f.fs.byIDLookups) != 2 ||
		f.fs.byIDLookups[0].ID != f.emptyID ||
		f.fs.byIDLookups[1].ID != f.fs.defaultCredID() {
		t.Fatalf("by-id opens = %+v, want the pick then the owner default", f.fs.byIDLookups)
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fs.defaultCredID() {
		t.Fatalf("recorded %v, want the fallback %v — the record names what was SPENT",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.fs.defaultCredID())
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonOpenFailed) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonOpenFailed)
	}
	// NULL, not 90. The measured headroom described the credential that would not
	// open; attaching it to the one that did would attribute a reading to a token
	// nothing measured.
	if rec.AnthropicHeadroomPct.Valid {
		t.Fatalf("headroom = %+v, want NULL — 90%% was the FAILED pick's reading", rec.AnthropicHeadroomPct)
	}
}

// TestAutoRetryIsOnceOnly: when the fallback ALSO fails to open, the claim fails
// terminally rather than looping. The bound is structural — the retry target is
// workerSecretID(wkr), which for an auto worker is nil, and a nil choice can never
// satisfy autoPicked — so this test pins the structure, not a counter.
func TestAutoRetryIsOnceOnly(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.byIDSecrets[f.emptyID] = store.GetUserSecretCiphertextByIDRow{
		UserID: f.owner, Kind: store.KindAnthropicToken,
		Ciphertext: []byte("not-sealed-by-this-box"), SealedWith: store.SealedWithMaster,
	}
	f.fs.anthropic = []byte("the-default-is-also-corrupt")
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0),
	}

	if payload := f.claim(t); payload != nil {
		t.Fatal("expected idle when neither credential opens")
	}
	if len(f.fs.byIDLookups) != 2 {
		t.Fatalf("by-id opens = %d, want exactly 2 (the pick, then one retry)", len(f.fs.byIDLookups))
	}
	if f.fs.markedFailed == nil {
		t.Fatal("neither credential opened and the run was not failed; it would sit claimed forever")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential nothing opened: %+v", f.fs.recordedCreds)
	}
}

// TestAutoDoesNotRetryOnALockedVault: D14 is explicitly NOT extended to
// errVaultLocked. That path already requeues the run, which is transient and correct;
// retrying it would convert a WAIT into a SPEND on the wrong account, and the wait
// resolves the moment the owner unlocks.
//
// MUTATION THIS CATCHES: widening the retry's error test to any error (dropping the
// errors.Is(err, errCredentialUnavailable) leg) → the run is not requeued and the
// owner's default is spent while their vault is locked.
func TestAutoDoesNotRetryOnALockedVault(t *testing.T) {
	box := newBox(t)
	owner := uuid.New()
	v := unlockedVault(t, owner, box) // passes Claim's gate...

	sealedPAT, _ := box.Seal([]byte("bot-pat-AUTOLOCK-abcdef1234567890"))
	runID, pickID := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: runID, UserID: owner, IssueTitle: "t", IssueDescription: "d", Status: "claimed",
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:          []byte("the-default-must-never-be-opened-here"),
		defaultSecretLabel: "default",
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			pickID: {
				UserID: owner, Kind: store.KindAnthropicToken,
				Ciphertext: []byte("dek-sealed-never-decrypted"), SealedWith: store.SealedWithDEK,
			},
		},
		byIDLabels: map[uuid.UUID]string{pickID: "spare-key"},
		// ...then the owner locks after ClaimRun, before the token open.
		onClaimRun: func() { v.Lock(owner) },
	}
	fs.autoCandidates = []store.ListAutoSelectCandidatesRow{candRow(pickID, "spare-key", true, 90, time.Minute, 0)}
	svc := New(fs, box, autoParams())
	svc.now = func() time.Time { return autoNow }
	svc.SetVault(v)

	payload, err := svc.Claim(context.Background(), store.Worker{
		ID: uuid.New(), UserID: owner, AnthropicBindMode: BindModeAuto,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle after a lock race, got a payload")
	}
	if fs.requeuedRun == nil || *fs.requeuedRun != runID {
		t.Fatalf("run not requeued: %v — a locked vault is transient, not a reason to spend elsewhere", fs.requeuedRun)
	}
	if fs.markedFailed != nil {
		t.Fatal("a lock race must never fail the run")
	}
	if len(fs.byIDLookups) != 1 {
		t.Fatalf("by-id opens = %+v, want exactly 1 — a locked vault must not trigger D14's retry", fs.byIDLookups)
	}
	if len(fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential during a lock race: %+v", fs.recordedCreds)
	}
}

// TestPinnedOpenFailureStaysTerminal is the other half of D14's scoping, and the one
// that is easy to lose by widening the retry to "any worker". A PINNED worker names
// its credential; if that will not open, silently billing the owner's default instead
// is precisely the wrong-account spend R4 is about. The user chose that token, and
// the failure is how they find out it is broken.
//
// MUTATION THIS CATCHES: gating the retry on the worker's MODE rather than on the
// choice having been auto-picked — this fixture has mode `pinned`, so a mode gate
// would not fire, but the symmetric error (gating on "the choice has an id") would,
// and this is what catches it.
func TestPinnedOpenFailureStaysTerminal(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModePinned
	f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	f.fs.byIDSecrets[f.fullID] = store.GetUserSecretCiphertextByIDRow{
		UserID: f.owner, Kind: store.KindAnthropicToken,
		Ciphertext: []byte("not-sealed-by-this-box"), SealedWith: store.SealedWithMaster,
	}

	if payload := f.claim(t); payload != nil {
		t.Fatal("expected idle: a pinned worker's broken credential is terminal")
	}
	if len(f.fs.byIDLookups) != 1 {
		t.Fatalf("by-id opens = %+v, want exactly 1 — a pinned failure must NOT retry on the default",
			f.fs.byIDLookups)
	}
	if f.fs.markedFailed == nil {
		t.Fatal("a pinned worker's undecryptable credential did not fail the run")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential nothing opened: %+v", f.fs.recordedCreds)
	}
}

// --- the closed vocabulary ------------------------------------------------------

// TestSelectReasonVocabularyMatchesCheck is the guard that keeps two Go homes and one
// SQL constraint in step. runs.anthropic_select_reason is DISPLAY-ONLY — nothing in
// the state machine, the claim path or any sweep reads it — so a value Go writes and
// the CHECK rejects has no failing consumer anywhere except Postgres, at claim time,
// on a user's run. And a value in the CHECK that Go never writes is a promise nothing
// keeps.
//
// It parses the migration rather than restating the list, because a second hand-typed
// copy is the thing it is trying to prevent.
//
// MUTATION THIS CATCHES: adding a constant to either Go half without touching 00089
// (and the reverse). Measured both directions.
func TestSelectReasonVocabularyMatchesCheck(t *testing.T) {
	const path = "../store/migrations/00089_run_select_reason_check.sql"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Strip comments first. The prose above the statement names several reasons, and
	// a regex over the whole file would happily collect them and agree with itself.
	var stmt strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stmt.WriteString(line)
		stmt.WriteString("\n")
	}
	inCheck := regexp.MustCompile(`'([a-z_]+)'`)
	var fromSQL []string
	for _, m := range inCheck.FindAllStringSubmatch(stmt.String(), -1) {
		fromSQL = append(fromSQL, m[1])
	}
	if len(fromSQL) == 0 {
		t.Fatalf("parsed no quoted values out of %s; the guard is reading the wrong thing", path)
	}

	var fromGo []string
	for _, r := range autoselect.AllReasons() {
		fromGo = append(fromGo, string(r))
	}
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the reason vocabulary has drifted.\n  Go:  %v\n  SQL: %v\nA value Go writes and "+
			"00089's CHECK rejects is a constraint violation at claim time — i.e. a failed run — and "+
			"nothing else reads this column, so nothing else would notice.", fromGo, fromSQL)
	}
}

// TestEveryProducedReasonIsInTheVocabulary closes the loop from the other side: the
// constants exist, but a lane could still record a literal. Every reason this package
// can write goes through staticChoice or autoselect.Select, and both are enumerated
// above — so what remains to pin is that the CLAIM PATH's own literal, D14's
// open_failed, is one of them.
func TestEveryProducedReasonIsInTheVocabulary(t *testing.T) {
	known := map[string]bool{}
	for _, r := range autoselect.AllReasons() {
		known[string(r)] = true
	}
	f := newAutoFixture(t)
	f.fs.byIDSecrets[f.emptyID] = store.GetUserSecretCiphertextByIDRow{
		UserID: f.owner, Kind: store.KindAnthropicToken,
		Ciphertext: []byte("not-sealed-by-this-box"), SealedWith: store.SealedWithMaster,
	}
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0),
	}
	f.claim(t)
	got := onlyRecord(t, f.fs).AnthropicSelectReason.String
	if !known[got] {
		t.Fatalf("the claim path recorded %q, which is in neither Go half of the vocabulary and would "+
			"therefore violate 00089's CHECK against a real database", got)
	}
}
