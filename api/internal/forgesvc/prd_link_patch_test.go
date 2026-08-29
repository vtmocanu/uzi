package forgesvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// THE ONE FIXTURE. Every test below varies exactly ONE field of it.
//
// That discipline is the point, not a style: nearly every assertion here is "no
// write happened" or "a settle happened", and both are satisfiable by a dozen
// states other than the one the test names — the candidate was never enumerated,
// the MR was not merged, the marker was already settled, the kind was wrong, the
// binding gate fired instead of the branch under test. A green from any of those
// looks identical to a green from the property. If a test's fixture differs from
// its twin in a second field, it is not testing what its name says.
const (
	fixProject  = int64(4242)
	fixIssueIID = int64(7)
	fixMRIID    = int64(99)
	fixOldPath  = "prds/72-x.md"
	fixNewPath  = "prds/done/72-x.md"
	// The queue-time snapshot: what the issue linked when the run was created.
	fixSnapshot = "Implements " + fixOldPath + " end to end."
	// The LIVE description, deliberately different text so a test can tell which of
	// the two the watcher actually read.
	fixLive = "Implements " + fixOldPath + " end to end. (edited)"
)

func fixCandidate() store.ListPRDLinkPatchCandidatesRow {
	return store.ListPRDLinkPatchCandidatesRow{
		ID:               uuid.New(),
		IssueIid:         pgtype.Int8{Int64: fixIssueIID, Valid: true},
		MrIid:            pgtype.Int8{Int64: fixMRIID, Valid: true},
		PrdDonePath:      pgtype.Text{String: fixNewPath, Valid: true},
		IssueDescription: fixSnapshot,
		Superseded:       false,
	}
}

// newPRDFixture wires the shared fixture. mutate() changes exactly one thing.
func newPRDFixture(t *testing.T, mutate func(c *store.ListPRDLinkPatchCandidatesRow, st *fakeStore, f *fakeForge)) (*Service, *fakeStore, *fakeForge, uuid.UUID) {
	t.Helper()
	c := fixCandidate()
	st := &fakeStore{}
	f := &fakeForge{
		mr:         forge.MergeRequest{State: forge.MRStateMerged},
		issueByIID: map[int64]forge.Issue{fixIssueIID: {Description: fixLive}},
	}
	if mutate != nil {
		mutate(&c, st, f)
	}
	st.prdCandidates = []store.ListPRDLinkPatchCandidatesRow{c}
	return newTestService(st), st, f, c.ID
}

// enumerated asserts the candidate scan actually ran and returned the row. EVERY
// "no settle / candidate survives" assertion needs this first, or an empty
// candidate list satisfies it just as well as the property under test.
func enumerated(t *testing.T, st *fakeStore) {
	t.Helper()
	if len(st.prdCandidateArgs) != 1 {
		t.Fatalf("expected exactly one candidate scan, got %d — the outcome below would be satisfied by never enumerating", len(st.prdCandidateArgs))
	}
}

func runPRDPatch(t *testing.T, s *Service, f *fakeForge) {
	t.Helper()
	if err := s.SyncPRDLinkPatches(context.Background(), uuid.New(), fixProject, f); err != nil {
		t.Fatalf("SyncPRDLinkPatches: %v", err)
	}
}

// --- The attack case, FIRST -------------------------------------------------

// TestPRDLinkPatchRefusesALinkTheIssueNeverCarried is auditor B's attack, and it
// comes before its honest twin deliberately.
//
// The run's SNAPSHOT links only 72-x. The LIVE description also lists an unrelated
// 40-other. A compromised or prompt-injected agent declares prds/done/40-other.md,
// trying to redirect the unrelated entry. The binding must refuse it with NO forge
// write at all.
//
// Note what the PRD's own Verified line would NOT have caught: "a description
// listing three PRDs has two untouched" pins the COUNT, not the IDENTITY.
func TestPRDLinkPatchRefusesALinkTheIssueNeverCarried(t *testing.T) {
	s, st, f, id := newPRDFixture(t, func(c *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
		c.PrdDonePath = pgtype.Text{String: "prds/done/40-other.md", Valid: true}
		f.issueByIID[fixIssueIID] = forge.Issue{Description: "Implements " + fixOldPath + ". Related: prds/40-other.md."}
	})
	runPRDPatch(t, s, f)
	enumerated(t, st)

	if len(f.descriptionUpdates) != 0 {
		t.Fatalf("the agent redirected a link the issue never carried in ITS OWN snapshot: %+v", f.descriptionUpdates)
	}
	// It must not even reach the network: the gate fires before GetIssue.
	if len(f.getIssueCalls) != 0 {
		t.Errorf("the binding gate must fire BEFORE any network access; GetIssue was called %v", f.getIssueCalls)
	}
	// And the edge is consumed, so it does not retry forever.
	if len(st.prdSettled) != 1 || st.prdSettled[0] != id {
		t.Errorf("expected the edge settled once, got %v", st.prdSettled)
	}
}

// The honest twin, differing from the attack ONLY in the declared path.
func TestPRDLinkPatchAcceptsTheIssuesOwnPRD(t *testing.T) {
	s, st, f, id := newPRDFixture(t, func(_ *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
		f.issueByIID[fixIssueIID] = forge.Issue{Description: "Implements " + fixOldPath + ". Related: prds/40-other.md."}
	})
	runPRDPatch(t, s, f)
	enumerated(t, st)

	if len(f.descriptionUpdates) != 1 {
		t.Fatalf("expected exactly one description update, got %d", len(f.descriptionUpdates))
	}
	got := f.descriptionUpdates[0]
	if got.projectID != fixProject || got.issueIID != fixIssueIID {
		t.Errorf("wrote to the wrong issue: %+v", got)
	}
	// BY IDENTITY, not tally: the unrelated link must be byte-identical.
	want := "Implements " + fixNewPath + ". Related: prds/40-other.md."
	if got.description != want {
		t.Errorf("description = %q, want %q", got.description, want)
	}
	if len(st.prdSettled) != 1 || st.prdSettled[0] != id {
		t.Errorf("expected the edge settled once, got %v", st.prdSettled)
	}
}

// A link-less run's snapshot carries no PRD link, so the binding makes Decision 12's
// no-op MECHANICAL rather than prompt-level — no forge write can happen even if the
// lead declares a path anyway. Differs from the honest twin ONLY in the snapshot.
func TestPRDLinkPatchIsAMechanicalNoOpForALinklessRun(t *testing.T) {
	s, st, f, _ := newPRDFixture(t, func(c *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, _ *fakeForge) {
		c.IssueDescription = "An issue with no spec file for this one."
	})
	runPRDPatch(t, s, f)
	enumerated(t, st)
	if len(f.descriptionUpdates) != 0 {
		t.Fatalf("a link-less run must never rewrite a description: %+v", f.descriptionUpdates)
	}
	if len(f.getIssueCalls) != 0 {
		t.Errorf("no network access for a link-less run; got %v", f.getIssueCalls)
	}
}

// --- The terminal-state family ----------------------------------------------
// One fixture, varying ONLY the MR state (or the injected error).

func TestPRDLinkPatchTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		superseded bool
		wantWrite  bool
		wantSettle bool
	}{
		{"merged patches and settles", forge.MRStateMerged, false, true, true},
		{"closed unmerged settles without patching", forge.MRStateClosed, false, false, true},
		{"opened and not superseded leaves the marker", forge.MRStateOpened, false, false, false},
		// Differs from the row above in the SUPERSEDED FLAG ONLY. If it also changed
		// the MR state, neither row would be testing supersession.
		{"opened and superseded settles without patching", forge.MRStateOpened, true, false, true},
		// locked is the case that would be wrong if it were folded in with closed:
		// mr_watch.go records it as TRANSIENT DURING MERGE PROCESSING, so settling on
		// it drops the patch for an MR that is about to merge.
		{"locked leaves the marker", forge.MRStateLocked, false, false, false},
		{"an unknown state leaves the marker", "banana", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, f, _ := newPRDFixture(t, func(c *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
				f.mr = forge.MergeRequest{State: tc.state}
				c.Superseded = tc.superseded
			})
			runPRDPatch(t, s, f)
			enumerated(t, st)

			if got := len(f.descriptionUpdates) > 0; got != tc.wantWrite {
				t.Errorf("wrote = %v, want %v (updates: %+v)", got, tc.wantWrite, f.descriptionUpdates)
			}
			if got := len(st.prdSettled) > 0; got != tc.wantSettle {
				t.Errorf("settled = %v, want %v", got, tc.wantSettle)
			}
		})
	}
}

func TestPRDLinkPatchLeavesTheMarkerOnForgeErrors(t *testing.T) {
	t.Run("GetMergeRequest error", func(t *testing.T) {
		s, st, f, _ := newPRDFixture(t, func(_ *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
			f.mrErr = errors.New("forge down")
		})
		runPRDPatch(t, s, f)
		enumerated(t, st)
		if len(st.prdSettled) != 0 {
			t.Errorf("a failed MR read is not evidence about the MR; must not settle")
		}
		if len(f.descriptionUpdates) != 0 {
			t.Errorf("must not write: %+v", f.descriptionUpdates)
		}
	})

	t.Run("GetIssue error", func(t *testing.T) {
		s, st, f, _ := newPRDFixture(t, func(_ *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
			f.getIssueErrByIID = map[int64]error{fixIssueIID: errors.New("forge down")}
		})
		runPRDPatch(t, s, f)
		enumerated(t, st)
		if len(st.prdSettled) != 0 {
			t.Errorf("a failed issue read is not evidence about the description; must not settle")
		}
	})

	t.Run("UpdateIssueDescription error", func(t *testing.T) {
		s, st, f, _ := newPRDFixture(t, func(_ *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
			f.updateDescErr = errors.New("forge down")
		})
		runPRDPatch(t, s, f)
		enumerated(t, st)
		if len(st.prdSettled) != 0 {
			t.Errorf("the write is what we are retrying; must not settle")
		}
	})
}

// --- Rewrite shapes ---------------------------------------------------------
// One fixture, varying ONLY the live description.

func TestPRDLinkPatchRewriteShapes(t *testing.T) {
	for _, tc := range []struct {
		name, live, want string
		wantWrite        bool
	}{
		{
			name:      "a blob URL keeps its prefix",
			live:      "See https://gl.example.com/g/p/-/blob/main/" + fixOldPath + " for the spec.",
			want:      "See https://gl.example.com/g/p/-/blob/main/" + fixNewPath + " for the spec.",
			wantWrite: true,
		},
		{
			name:      "a line anchor survives",
			live:      "See " + fixOldPath + "#L4 please.",
			want:      "See " + fixNewPath + "#L4 please.",
			wantWrite: true,
		},
		{
			name:      "every occurrence of THIS path, or a twice-linked PRD is left half-broken",
			live:      fixOldPath + " and https://gl/-/blob/main/" + fixOldPath,
			want:      fixNewPath + " and https://gl/-/blob/main/" + fixNewPath,
			wantWrite: true,
		},
		{
			// Covers "already patched", "a human edited it", and "already under
			// prds/done/" in one branch, because the action is identical for all three.
			name:      "no match settles without writing",
			live:      "This description no longer mentions any PRD path.",
			wantWrite: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, f, _ := newPRDFixture(t, func(_ *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
				f.issueByIID[fixIssueIID] = forge.Issue{Description: tc.live}
			})
			runPRDPatch(t, s, f)
			enumerated(t, st)

			if !tc.wantWrite {
				if len(f.descriptionUpdates) != 0 {
					t.Fatalf("expected no write, got %+v", f.descriptionUpdates)
				}
				// Distinguish this terminal state from the binding gate's: the binding
				// gate never reaches GetIssue, this one does. Two distinct no-write
				// states, and "no write" alone cannot tell them apart.
				if len(f.getIssueCalls) == 0 {
					t.Errorf("this must be the no-match terminal state (which reads the issue), not the binding gate (which does not)")
				}
				if len(st.prdSettled) != 1 {
					t.Errorf("the no-match state must settle, got %v", st.prdSettled)
				}
				return
			}
			if len(f.descriptionUpdates) != 1 {
				t.Fatalf("expected exactly one write, got %d", len(f.descriptionUpdates))
			}
			if got := f.descriptionUpdates[0].description; got != tc.want {
				t.Errorf("description = %q, want %q", got, tc.want)
			}
		})
	}
}

// The watcher must read the LIVE description, not the run's queue-time snapshot.
// The two fixtures differ in text precisely so this is decidable.
func TestPRDLinkPatchRewritesTheLiveDescriptionNotTheSnapshot(t *testing.T) {
	s, _, f, _ := newPRDFixture(t, nil)
	runPRDPatch(t, s, f)
	if len(f.descriptionUpdates) != 1 {
		t.Fatalf("expected one write, got %d", len(f.descriptionUpdates))
	}
	want := "Implements " + fixNewPath + " end to end. (edited)"
	if got := f.descriptionUpdates[0].description; got != want {
		t.Errorf("description = %q, want %q — the snapshot's text must not be what gets written", got, want)
	}
}

// --- Edge consumption -------------------------------------------------------

// A second tick must make zero further forge calls. This passes VACUOUSLY against a
// fake whose settle is a no-op, which is why fakeStore.SettlePRDLinkPatch actually
// removes the row: tick 2 returns nothing BECAUSE OF tick 1.
func TestPRDLinkPatchConsumesTheEdgeExactlyOnce(t *testing.T) {
	s, st, f, _ := newPRDFixture(t, nil)
	runPRDPatch(t, s, f)
	if len(f.descriptionUpdates) != 1 {
		t.Fatalf("tick 1: expected one write, got %d", len(f.descriptionUpdates))
	}
	writesAfterTick1 := len(f.descriptionUpdates)
	mrCallsAfterTick1 := len(f.mrCalls)

	runPRDPatch(t, s, f)
	if len(st.prdCandidateArgs) != 2 {
		t.Fatalf("expected a second candidate scan, got %d", len(st.prdCandidateArgs))
	}
	if len(f.descriptionUpdates) != writesAfterTick1 {
		t.Errorf("tick 2 wrote again: %+v", f.descriptionUpdates[writesAfterTick1:])
	}
	if len(f.mrCalls) != mrCallsAfterTick1 {
		t.Errorf("tick 2 made %d further MR reads; the settled edge must not re-enumerate", len(f.mrCalls)-mrCallsAfterTick1)
	}
}

func TestPRDLinkPatchAsksForTheBoundedBatch(t *testing.T) {
	s, st, f, _ := newPRDFixture(t, nil)
	runPRDPatch(t, s, f)
	if len(st.prdCandidateArgs) != 1 {
		t.Fatalf("expected one scan, got %d", len(st.prdCandidateArgs))
	}
	if got := st.prdCandidateArgs[0].Lim; got != PRDLinkPatchBatch {
		t.Errorf("Lim = %d, want PRDLinkPatchBatch (%d)", got, PRDLinkPatchBatch)
	}
}

func TestPRDLinkPatchReturnsEnumerationErrors(t *testing.T) {
	st := &fakeStore{prdCandidatesErr: errors.New("db down")}
	f := &fakeForge{}
	if err := newTestService(st).SyncPRDLinkPatches(context.Background(), uuid.New(), fixProject, f); err == nil {
		t.Fatal("an enumeration failure must surface to the poller")
	}
}

// A stored path that does not validate settles without a forge call. It cannot
// happen through clampWirePRDDonePath, so this pins the watcher's own re-check
// rather than inheriting the assumption from M4.
func TestPRDLinkPatchRefusesAStoredPathThatDoesNotValidate(t *testing.T) {
	s, st, f, _ := newPRDFixture(t, func(c *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, _ *fakeForge) {
		c.PrdDonePath = pgtype.Text{String: "prds/../../../etc/passwd", Valid: true}
	})
	runPRDPatch(t, s, f)
	enumerated(t, st)
	if len(f.descriptionUpdates) != 0 || len(f.getIssueCalls) != 0 {
		t.Errorf("a non-validating stored path must not reach the forge")
	}
	if len(st.prdSettled) != 1 {
		t.Errorf("expected the edge settled, got %v", st.prdSettled)
	}
}
