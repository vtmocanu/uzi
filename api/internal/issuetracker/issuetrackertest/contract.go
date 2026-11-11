package issuetrackertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// Subject is one implementation of forge.IssueTracker under contract test,
// with the seams the suite needs to put state in it that the interface cannot.
type Subject struct {
	IssueTracker forge.IssueTracker
	// ProjectID is passed as the projectID argument of every call.
	ProjectID int64
	// SeedHumanComment adds a comment by username on issue iid, bypassing the
	// IssueTracker under test.
	SeedHumanComment func(t *testing.T, iid int64, username, body string)
	// SeedLabelEvent records that username added ("add") or removed ("remove")
	// label on issue iid, bypassing the IssueTracker under test.
	SeedLabelEvent func(t *testing.T, iid int64, action, label, username string)
	// SeedDeletedComment adds a comment that the issue tracker reports as
	// deleted. Nil means the implementation has no such comment.
	SeedDeletedComment func(t *testing.T, iid int64, username, body string)
	// MoveIssue moves issue iid to another project. Nil means issues do not
	// move on this implementation.
	MoveIssue func(t *testing.T, iid int64)
	// OtherProjectID is a project id the subject is not bound to; the
	// ProjectGuard case calls every method with it. Required.
	OtherProjectID int64
	// WantProjectGuardErr, when set, is a sentinel every guarded call must
	// match with errors.Is; nil only requires some error.
	WantProjectGuardErr error
	// NoteIDUnset says CreateIssueNote returns ID 0, because the issue tracker's
	// comment ids have no numeric form. When false the case requires a non-zero ID.
	NoteIDUnset bool
	// SameName carries the seams of the SameNameTagUsesEligibilityTag case. Nil
	// means labels on this implementation have no owners, so the case is skipped.
	SameName *SameNameSeams
}

// SameNameSeams put server-side state in a subject whose labels are tags with
// owners, and read it back. The subject is bound to an eligibility tag owned by
// another user. Besides it, the connection's own account owns a tag with the
// same name.
type SameNameSeams struct {
	// EligibilityName is the eligibility tag's label name.
	EligibilityName string
	// EligibilityTagID is the id of the eligibility tag the subject is bound to.
	EligibilityTagID string
	// SameNameTagID is the id of the account's own tag that has the eligibility name.
	SameNameTagID string
	// SeedIssue creates an issue carrying the tags with the given ids, bypassing
	// the IssueTracker under test, and returns its number.
	SeedIssue func(t *testing.T, tagIDs ...string) int64
	// IssueTagIDs returns the ids of the tags on issue iid, server side.
	IssueTagIDs func(t *testing.T, iid int64) []string
	// TagsNamed returns every tag with the name, server side.
	TagsNamed func(t *testing.T, name string) []TagView
}

// TagView is one server-side tag, as far as the SameName case judges it.
type TagView struct {
	ID string
	// OwnedByAccount is true when the connection's own account owns the tag.
	OwnedByAccount bool
	// SharedLikeEligibility is true when the tag's read and tag sharing settings
	// equal the eligibility tag's.
	SharedLikeEligibility bool
}

// uniqueSeq makes every name the suite creates unique within the process, so
// a subject whose backing store is shared never sees a collision.
var uniqueSeq atomic.Int64

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, uniqueSeq.Add(1))
}

// RunContract runs the shared behaviour of every issue tracker implementation.
// newSubject is called once per case, so a case starts from the state its
// constructor gives. Assertions look for the data the case created and never
// count everything the subject holds, because a subject may hold other data.
func RunContract(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		run  func(t *testing.T, c *contract)
	}{
		{"EnsureLabelsIsIdempotent", caseEnsureLabels},
		{"CreateIssueRoundTrips", caseCreateIssue},
		{"ListIssuesFilters", caseListIssues},
		{"UpdateLabelsAndDescription", caseUpdate},
		{"SetIssueStateClosesAndReopens", caseSetState},
		{"CommentsOldestFirst", caseComments},
		{"LabelEventsOldestFirst", caseLabelEvents},
		{"MissingIssueIsNotFound", caseMissing},
		{"MovedIssueIsRefused", caseMoved},
		{"ProjectGuard", caseProjectGuard},
		{"SameNameTagUsesEligibilityTag", caseSameNameTag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSubject(t)
			tc.run(t, &contract{Subject: s, ctx: t.Context()})
		})
	}
}

type contract struct {
	Subject
	ctx context.Context
}

func (c *contract) ensure(t *testing.T, names ...string) {
	t.Helper()
	labels := make([]forge.Label, 0, len(names))
	for _, n := range names {
		labels = append(labels, forge.Label{Name: n, Color: "#428bca"})
	}
	if err := c.IssueTracker.EnsureLabels(c.ctx, c.ProjectID, labels); err != nil {
		t.Fatalf("EnsureLabels(%v): %v", names, err)
	}
}

func (c *contract) create(t *testing.T, description string, labels ...string) forge.Issue {
	t.Helper()
	issue, err := c.IssueTracker.CreateIssue(c.ctx, c.ProjectID, unique("contract issue"), description, labels)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.IID <= 0 {
		t.Fatalf("CreateIssue returned IID %d", issue.IID)
	}
	return issue
}

func (c *contract) get(t *testing.T, iid int64) forge.Issue {
	t.Helper()
	issue, err := c.IssueTracker.GetIssue(c.ctx, c.ProjectID, iid)
	if err != nil {
		t.Fatalf("GetIssue(%d): %v", iid, err)
	}
	return issue
}

// listIIDs returns the sorted IIDs ListIssues finds for opts.
func (c *contract) listIIDs(t *testing.T, opts forge.ListIssuesOptions) []int64 {
	t.Helper()
	issues, err := c.IssueTracker.ListIssues(c.ctx, c.ProjectID, opts)
	if err != nil {
		t.Fatalf("ListIssues(%+v): %v", opts, err)
	}
	iids := make([]int64, 0, len(issues))
	for _, i := range issues {
		iids = append(iids, i.IID)
	}
	slices.Sort(iids)
	return iids
}

func wantIIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("ListIssues IIDs = %v, want %v", got, want)
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func caseEnsureLabels(t *testing.T, c *contract) {
	a, b := unique("contract-label"), unique("contract-label")
	c.ensure(t, a, b)
	c.ensure(t, a, b) // idempotent
	labels, err := c.IssueTracker.ListLabels(c.ctx, c.ProjectID)
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	count := map[string]int{}
	for _, l := range labels {
		count[l.Name]++
	}
	for _, name := range []string{a, b} {
		if count[name] != 1 {
			t.Errorf("label %q listed %d times, want once", name, count[name])
		}
	}
}

func caseCreateIssue(t *testing.T, c *contract) {
	l1, l2 := unique("contract-label"), unique("contract-label")
	c.ensure(t, l1, l2)
	created := c.create(t, "the description", l1, l2)
	got := c.get(t, created.IID)
	if got.IID != created.IID {
		t.Errorf("IID = %d, want %d", got.IID, created.IID)
	}
	if got.Title != created.Title || got.Title == "" {
		t.Errorf("Title = %q, created as %q", got.Title, created.Title)
	}
	if got.Description != "the description" {
		t.Errorf("Description = %q", got.Description)
	}
	if !sameSet(got.Labels, []string{l1, l2}) {
		t.Errorf("Labels = %v, want %v and %v", got.Labels, l1, l2)
	}
	if got.State != string(forge.StateOpened) {
		t.Errorf("State = %q, want opened", got.State)
	}
	if got.WebURL == "" || got.WebURL != created.WebURL {
		t.Errorf("WebURL = %q, created as %q", got.WebURL, created.WebURL)
	}
	if got.Assignees == nil {
		t.Error("Assignees is nil, want an empty slice")
	}
}

func caseListIssues(t *testing.T, c *contract) {
	l1, l2 := unique("contract-label"), unique("contract-label")
	c.ensure(t, l1, l2)
	both := c.create(t, "both", l1, l2)
	onlyOne := c.create(t, "one", l1)
	other := c.create(t, "two", l2)

	wantIIDs(t, c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{l1, l2}}), both.IID)
	wantIIDs(t, c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{l1}}), both.IID, onlyOne.IID)
	wantIIDs(t, c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{l2}}), both.IID, other.IID)

	if err := c.IssueTracker.SetIssueState(c.ctx, c.ProjectID, onlyOne.IID, forge.StateClosed); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	wantIIDs(t, c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{l1}, State: forge.StateOpened}), both.IID)
	wantIIDs(t, c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{l1}, State: forge.StateClosed}), onlyOne.IID)
	wantIIDs(t, c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{l1}, State: forge.StateAll}), both.IID, onlyOne.IID)

	limited := c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{l1}, Limit: 1})
	if len(limited) != 1 {
		t.Fatalf("Limit 1 returned %d issues", len(limited))
	}
	if limited[0] != both.IID && limited[0] != onlyOne.IID {
		t.Errorf("Limit 1 returned %d, want one of the matches %d or %d", limited[0], both.IID, onlyOne.IID)
	}
}

func caseUpdate(t *testing.T, c *contract) {
	l1, l2 := unique("contract-label"), unique("contract-label")
	c.ensure(t, l1, l2)
	issue := c.create(t, "before", l1)
	if err := c.IssueTracker.UpdateIssueLabels(c.ctx, c.ProjectID, issue.IID, []string{l2}, []string{l1}); err != nil {
		t.Fatalf("UpdateIssueLabels: %v", err)
	}
	if err := c.IssueTracker.UpdateIssueDescription(c.ctx, c.ProjectID, issue.IID, "after"); err != nil {
		t.Fatalf("UpdateIssueDescription: %v", err)
	}
	got := c.get(t, issue.IID)
	if !sameSet(got.Labels, []string{l2}) {
		t.Errorf("Labels = %v, want only %v", got.Labels, l2)
	}
	if got.Description != "after" {
		t.Errorf("Description = %q, want after", got.Description)
	}
}

func caseSetState(t *testing.T, c *contract) {
	label := unique("contract-label")
	c.ensure(t, label)
	issue := c.create(t, "state", label)
	inList := func(state forge.IssueState) bool {
		return slices.Contains(c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{label}, State: state}), issue.IID)
	}
	if err := c.IssueTracker.SetIssueState(c.ctx, c.ProjectID, issue.IID, forge.StateClosed); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := c.get(t, issue.IID).State; got != string(forge.StateClosed) {
		t.Errorf("State after close = %q", got)
	}
	if !inList(forge.StateClosed) || inList(forge.StateOpened) {
		t.Error("ListIssues state filters disagree with a closed issue")
	}
	if err := c.IssueTracker.SetIssueState(c.ctx, c.ProjectID, issue.IID, forge.StateOpened); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := c.get(t, issue.IID).State; got != string(forge.StateOpened) {
		t.Errorf("State after reopen = %q", got)
	}
	if inList(forge.StateClosed) || !inList(forge.StateOpened) {
		t.Error("ListIssues state filters disagree with a reopened issue")
	}
}

func caseComments(t *testing.T, c *contract) {
	issue := c.create(t, "comments")
	human, note := unique("human comment"), unique("bot note")
	c.SeedHumanComment(t, issue.IID, "alice", human)
	created, err := c.IssueTracker.CreateIssueNote(c.ctx, c.ProjectID, issue.IID, note)
	if err != nil {
		t.Fatalf("CreateIssueNote: %v", err)
	}
	if created.Body != note {
		t.Errorf("CreateIssueNote body = %q, want %q", created.Body, note)
	}
	if c.NoteIDUnset && created.ID != 0 {
		t.Errorf("CreateIssueNote ID = %d, want 0 (no numeric comment id)", created.ID)
	}
	if !c.NoteIDUnset && created.ID == 0 {
		t.Errorf("CreateIssueNote ID = 0, want a non-zero ID")
	}
	comments, err := c.IssueTracker.ListIssueComments(c.ctx, c.ProjectID, issue.IID)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	var humanAt, noteAt = -1, -1
	for i, cm := range comments {
		switch cm.Body {
		case human:
			humanAt = i
			if cm.AuthorUsername != "alice" {
				t.Errorf("human comment author = %q, want alice", cm.AuthorUsername)
			}
		case note:
			noteAt = i
		}
	}
	if c.SeedDeletedComment != nil {
		gone := unique("contract deleted")
		c.SeedDeletedComment(t, issue.IID, "carol", gone)
		again, err := c.IssueTracker.ListIssueComments(c.ctx, c.ProjectID, issue.IID)
		if err != nil {
			t.Fatalf("ListIssueComments: %v", err)
		}
		for _, cm := range again {
			if cm.Body == gone {
				t.Errorf("a deleted comment was listed: %+v", cm)
			}
		}
	}
	if humanAt < 0 || noteAt < 0 {
		t.Fatalf("comments %+v are missing the seeded comment or the note", comments)
	}
	if humanAt > noteAt {
		t.Errorf("human comment at %d after the note at %d, want oldest first", humanAt, noteAt)
	}
}

func caseLabelEvents(t *testing.T, c *contract) {
	label := unique("contract-label")
	c.ensure(t, label)
	issue := c.create(t, "events")
	c.SeedLabelEvent(t, issue.IID, "add", label, "alice")
	c.SeedLabelEvent(t, issue.IID, "remove", label, "bob")
	events, err := c.IssueTracker.ListIssueLabelEvents(c.ctx, c.ProjectID, issue.IID)
	if err != nil {
		t.Fatalf("ListIssueLabelEvents: %v", err)
	}
	var mine []forge.LabelEvent
	for _, e := range events {
		if e.LabelName == label {
			mine = append(mine, e)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("events for %q = %+v, want an add then a remove", label, mine)
	}
	if mine[1].CreatedAt.Before(mine[0].CreatedAt) {
		t.Errorf("events out of order: %v before %v", mine[1].CreatedAt, mine[0].CreatedAt)
	}
	if mine[0].Action != "add" || mine[0].Username != "alice" {
		t.Errorf("first event = %+v, want add by alice", mine[0])
	}
	if mine[1].Action != "remove" || mine[1].Username != "bob" {
		t.Errorf("second event = %+v, want remove by bob", mine[1])
	}
}

func caseMissing(t *testing.T, c *contract) {
	_, err := c.IssueTracker.GetIssue(c.ctx, c.ProjectID, 987654321)
	var notFound *forge.IssueNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("GetIssue of a missing IID = %v, want *forge.IssueNotFoundError", err)
	}
}

func caseMoved(t *testing.T, c *contract) {
	if c.MoveIssue == nil {
		t.Skip("issues do not move on this implementation")
	}
	issue := c.create(t, "will move")
	c.MoveIssue(t, issue.IID)
	it, pid, iid := c.IssueTracker, c.ProjectID, issue.IID
	calls := map[string]func() error{
		"GetIssue": func() error { _, err := it.GetIssue(c.ctx, pid, iid); return err },
		"UpdateIssueLabels": func() error {
			return it.UpdateIssueLabels(c.ctx, pid, iid, []string{unique("contract-label")}, nil)
		},
		"UpdateIssueDescription": func() error { return it.UpdateIssueDescription(c.ctx, pid, iid, "x") },
		"SetIssueState":          func() error { return it.SetIssueState(c.ctx, pid, iid, forge.StateClosed) },
		"ListIssueLabelEvents":   func() error { _, err := it.ListIssueLabelEvents(c.ctx, pid, iid); return err },
		"ListIssueComments":      func() error { _, err := it.ListIssueComments(c.ctx, pid, iid); return err },
		"CreateIssueNote":        func() error { _, err := it.CreateIssueNote(c.ctx, pid, iid, "x"); return err },
	}
	for name, call := range calls {
		var notFound *forge.IssueNotFoundError
		if err := call(); !errors.As(err, &notFound) {
			t.Errorf("%s on a moved issue = %v, want *forge.IssueNotFoundError", name, err)
		}
	}
}

// caseProjectGuard calls every method with a project id the subject is not bound
// to. Each must fail, and none may have changed the issue made under the right id.
func caseProjectGuard(t *testing.T, c *contract) {
	if c.OtherProjectID == 0 || c.OtherProjectID == c.ProjectID {
		t.Fatalf("Subject.OtherProjectID = %d must differ from ProjectID %d and be set", c.OtherProjectID, c.ProjectID)
	}
	label := unique("contract-label")
	c.ensure(t, label)
	issue := c.create(t, "guarded", label)
	it, pid, iid := c.IssueTracker, c.OtherProjectID, issue.IID
	calls := map[string]func() error{
		"ListLabels":   func() error { _, err := it.ListLabels(c.ctx, pid); return err },
		"EnsureLabels": func() error { return it.EnsureLabels(c.ctx, pid, []forge.Label{{Name: unique("contract-label")}}) },
		"ListIssues":   func() error { _, err := it.ListIssues(c.ctx, pid, forge.ListIssuesOptions{}); return err },
		"GetIssue":     func() error { _, err := it.GetIssue(c.ctx, pid, iid); return err },
		"CreateIssue": func() error {
			_, err := it.CreateIssue(c.ctx, pid, unique("contract guarded"), "x", []string{label})
			return err
		},
		"UpdateIssueLabels":      func() error { return it.UpdateIssueLabels(c.ctx, pid, iid, nil, []string{label}) },
		"UpdateIssueDescription": func() error { return it.UpdateIssueDescription(c.ctx, pid, iid, "changed") },
		"SetIssueState":          func() error { return it.SetIssueState(c.ctx, pid, iid, forge.StateClosed) },
		"ListIssueLabelEvents":   func() error { _, err := it.ListIssueLabelEvents(c.ctx, pid, iid); return err },
		"ListIssueComments":      func() error { _, err := it.ListIssueComments(c.ctx, pid, iid); return err },
		"CreateIssueNote":        func() error { _, err := it.CreateIssueNote(c.ctx, pid, iid, "x"); return err },
	}
	if len(calls) != 11 {
		t.Fatalf("table covers %d methods, want 11", len(calls))
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s with another project id succeeded", name)
		} else if c.WantProjectGuardErr != nil && !errors.Is(err, c.WantProjectGuardErr) {
			t.Errorf("%s with another project id = %v, want %v", name, err, c.WantProjectGuardErr)
		}
	}
	got := c.get(t, iid)
	if got.Description != "guarded" || got.State != string(forge.StateOpened) || !sameSet(got.Labels, []string{label}) {
		t.Errorf("a guarded call changed the issue: %+v", got)
	}
	comments, err := it.ListIssueComments(c.ctx, c.ProjectID, iid)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 0 {
		t.Errorf("a guarded call left comments: %+v", comments)
	}
}

// caseSameNameTag shows that the eligibility label name always means the
// eligibility tag, though the connection's account owns another tag of that
// name, and that other names only ever mean tags the account owns.
func caseSameNameTag(t *testing.T, c *contract) {
	s := c.SameName
	if s == nil {
		t.Skip("labels have no owners on this implementation")
	}
	elig := s.EligibilityName
	fresh := unique("contract-label")

	// EnsureLabels: no eligibility tag is created, the account's same-named tag
	// is left alone, and a new tag is the account's, shared like the eligibility tag.
	c.ensure(t, elig, fresh)
	c.ensure(t, elig, fresh)
	var ids []string
	for _, tv := range s.TagsNamed(t, elig) {
		ids = append(ids, tv.ID)
	}
	if !sameSet(ids, []string{s.EligibilityTagID, s.SameNameTagID}) {
		t.Errorf("tags named %q = %v after EnsureLabels, want only the eligibility tag and the account's own", elig, ids)
	}
	made := s.TagsNamed(t, fresh)
	if len(made) != 1 || !made[0].OwnedByAccount || !made[0].SharedLikeEligibility {
		t.Errorf("tags named %q = %+v, want one owned by the account and shared like the eligibility tag", fresh, made)
	}

	// CreateIssue attaches the eligibility tag, not the account's tag.
	created := c.create(t, "eligible", elig)
	if got := s.IssueTagIDs(t, created.IID); !slices.Equal(got, []string{s.EligibilityTagID}) {
		t.Errorf("created issue carries tags %v, want only the eligibility tag %s", got, s.EligibilityTagID)
	}
	if got := c.get(t, created.IID).Labels; !slices.Equal(got, []string{elig}) {
		t.Errorf("created issue labels = %v, want [%s]", got, elig)
	}

	// ListIssues by the eligibility name finds the eligibility issue and not
	// one that carries only the same-named tag; GetIssue of that one does not
	// report the eligibility name.
	lookalike := s.SeedIssue(t, s.SameNameTagID)
	wantIIDs(t, c.listIIDs(t, forge.ListIssuesOptions{Labels: []string{elig}}), created.IID)
	if got := c.get(t, lookalike).Labels; slices.Contains(got, elig) {
		t.Errorf("an issue with only a same-named tag reports labels %v", got)
	}

	// UpdateIssueLabels touches only the eligibility id.
	if err := c.IssueTracker.UpdateIssueLabels(c.ctx, c.ProjectID, lookalike, []string{elig}, nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := s.IssueTagIDs(t, lookalike); !sameSet(got, []string{s.SameNameTagID, s.EligibilityTagID}) {
		t.Errorf("after add the issue carries %v, want the same-named tag and the eligibility tag", got)
	}
	if err := c.IssueTracker.UpdateIssueLabels(c.ctx, c.ProjectID, lookalike, nil, []string{elig}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := s.IssueTagIDs(t, lookalike); !slices.Equal(got, []string{s.SameNameTagID}) {
		t.Errorf("after remove the issue carries %v, want only the same-named tag", got)
	}
}
