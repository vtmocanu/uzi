package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// labeledIssue builds a cached issue row carrying the given labels (as the stored
// JSON) and has_prd_link flag, for exercising SetIssueLabel (PRD #22 M4).
func labeledIssue(iid int64, hasPRD bool, labels ...string) store.Issue {
	j, _ := json.Marshal(labels)
	return store.Issue{
		RepoID:        uuid.New(),
		ForgeIssueIid: iid,
		Title:         "T",
		State:         "opened",
		Labels:        j,
		HasPrdLink:    hasPRD,
	}
}

func assertLabels(t *testing.T, raw []byte, want []string) {
	t.Helper()
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal cached labels: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cached labels = %v, want %v", got, want)
	}
}

func newLabelSvc(st *fakeStore) *Service {
	// SetIssueLabel never consults the LabelConfig (the caller passes the resolved
	// label name), so a nil resolver is fine.
	return New(st, nil, time.Second, nil)
}

func TestSetIssueLabelApplyCreatesLabelAndCaches(t *testing.T) {
	st := &fakeStore{}
	svc := newLabelSvc(st)
	f := &fakeForge{}
	issue := labeledIssue(4, false, "PRD", "In Progress")

	got, err := svc.SetIssueLabel(context.Background(), f, 7, issue, "PRDLESS", PrdlessLabelColor, true)
	if err != nil {
		t.Fatalf("SetIssueLabel: %v", err)
	}

	// EnsureLabels auto-creates the label once, pinned to the prdless color.
	if len(f.ensureCalls) != 1 || len(f.ensureCalls[0]) != 1 {
		t.Fatalf("EnsureLabels calls = %+v, want one call with one label", f.ensureCalls)
	}
	if l := f.ensureCalls[0][0]; l.Name != "PRDLESS" || l.Color != PrdlessLabelColor {
		t.Fatalf("ensured label = %+v, want {PRDLESS %s}", l, PrdlessLabelColor)
	}
	// Exactly one UpdateIssueLabels: add the one label, remove nothing.
	if len(f.updateCalls) != 1 || !slices.Equal(f.updateCalls[0].add, []string{"PRDLESS"}) || len(f.updateCalls[0].remove) != 0 {
		t.Fatalf("UpdateIssueLabels calls = %+v, want one add=[PRDLESS] remove=[]", f.updateCalls)
	}
	// Cache: the one label appended to the existing set (order preserved), and
	// has_prd_link carried through verbatim (NOT recomputed to true).
	if len(st.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(st.upserts))
	}
	assertLabels(t, st.upserts[0].Labels, []string{"PRD", "In Progress", "PRDLESS"})
	if st.upserts[0].HasPrdLink {
		t.Fatal("has_prd_link must be preserved verbatim (false), never re-derived")
	}
	assertLabels(t, got.Labels, []string{"PRD", "In Progress", "PRDLESS"})
}

func TestSetIssueLabelApplyIdempotentSkipsForge(t *testing.T) {
	st := &fakeStore{}
	svc := newLabelSvc(st)
	f := &fakeForge{}
	issue := labeledIssue(4, true, "PRD", "PRDLESS")

	got, err := svc.SetIssueLabel(context.Background(), f, 7, issue, "PRDLESS", PrdlessLabelColor, true)
	if err != nil {
		t.Fatalf("SetIssueLabel: %v", err)
	}
	// Already present: no forge call at all, no cache write, row returned unchanged.
	if len(f.ensureCalls) != 0 || len(f.updateCalls) != 0 {
		t.Fatalf("idempotent apply must not touch the forge: ensure=%+v update=%+v", f.ensureCalls, f.updateCalls)
	}
	if len(st.upserts) != 0 {
		t.Fatal("idempotent apply must not write the cache")
	}
	assertLabels(t, got.Labels, []string{"PRD", "PRDLESS"})
}

func TestSetIssueLabelRemovePreservesOtherLabels(t *testing.T) {
	st := &fakeStore{}
	svc := newLabelSvc(st)
	f := &fakeForge{}
	issue := labeledIssue(4, true, "PRD", "PRDLESS", "In Progress")

	if _, err := svc.SetIssueLabel(context.Background(), f, 7, issue, "PRDLESS", PrdlessLabelColor, false); err != nil {
		t.Fatalf("SetIssueLabel: %v", err)
	}
	// Remove never creates a label.
	if len(f.ensureCalls) != 0 {
		t.Fatalf("remove must not call EnsureLabels, got %+v", f.ensureCalls)
	}
	if len(f.updateCalls) != 1 || len(f.updateCalls[0].add) != 0 || !slices.Equal(f.updateCalls[0].remove, []string{"PRDLESS"}) {
		t.Fatalf("UpdateIssueLabels calls = %+v, want one add=[] remove=[PRDLESS]", f.updateCalls)
	}
	// Cache: only the one label dropped, everything else kept in order; has_prd_link
	// preserved (true).
	assertLabels(t, st.upserts[0].Labels, []string{"PRD", "In Progress"})
	if !st.upserts[0].HasPrdLink {
		t.Fatal("has_prd_link must be preserved verbatim (true)")
	}
}

func TestSetIssueLabelRemoveIdempotentSkipsForge(t *testing.T) {
	st := &fakeStore{}
	svc := newLabelSvc(st)
	f := &fakeForge{}
	issue := labeledIssue(4, false, "PRD")

	if _, err := svc.SetIssueLabel(context.Background(), f, 7, issue, "PRDLESS", PrdlessLabelColor, false); err != nil {
		t.Fatalf("SetIssueLabel: %v", err)
	}
	if len(f.updateCalls) != 0 || len(st.upserts) != 0 {
		t.Fatal("removing an already-absent label must be a local no-op with no forge or cache write")
	}
}

func TestSetIssueLabelForgeFailureLeavesCacheUntouched(t *testing.T) {
	st := &fakeStore{}
	svc := newLabelSvc(st)
	f := &fakeForge{updateErr: errors.New("forge down")}
	issue := labeledIssue(4, false, "PRD")

	if _, err := svc.SetIssueLabel(context.Background(), f, 7, issue, "PRDLESS", PrdlessLabelColor, true); err == nil {
		t.Fatal("expected the forge error to propagate")
	}
	if len(st.upserts) != 0 {
		t.Fatal("a failed UpdateIssueLabels must leave the cache untouched")
	}
}

func TestSetIssueLabelEnsureFailureAbortsBeforeUpdate(t *testing.T) {
	st := &fakeStore{}
	svc := newLabelSvc(st)
	f := &fakeForge{ensureErr: errors.New("cannot create label")}
	issue := labeledIssue(4, false, "PRD")

	if _, err := svc.SetIssueLabel(context.Background(), f, 7, issue, "PRDLESS", PrdlessLabelColor, true); err == nil {
		t.Fatal("expected the EnsureLabels error to propagate")
	}
	if len(f.updateCalls) != 0 {
		t.Fatal("must not call UpdateIssueLabels after EnsureLabels failed")
	}
	if len(st.upserts) != 0 {
		t.Fatal("a failed EnsureLabels must leave the cache untouched")
	}
}
