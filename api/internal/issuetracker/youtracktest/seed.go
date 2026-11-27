package youtracktest

import (
	"fmt"
	"slices"
)

// Sharing says who a tag is shared with, by group id and user id.
type Sharing struct {
	Groups []string
	Users  []string
}

// TagInfo is a tag as the fake holds it, for seeding and assertions.
type TagInfo struct {
	ID         string
	Name       string
	OwnerLogin string
	Read       Sharing
	Use        Sharing
}

// SeedTag adds a tag owned by the user with login ownerLogin and returns its
// id. Group and user ids must be ones the fake knows.
func (s *Server) SeedTag(name, ownerLogin string, read, use Sharing) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var owner string
	for _, u := range fakeUsers {
		if u.login == ownerLogin {
			owner = u.id
		}
	}
	if owner == "" {
		s.t.Fatalf("youtracktest: no user %q", ownerLogin)
	}
	t := &tag{id: s.m.nextID(5), name: name, owner: owner,
		read: sharing{groups: read.Groups, users: read.Users}, use: sharing{groups: use.Groups, users: use.Users}}
	s.m.tags = append(s.m.tags, t)
	return t.id
}

// SeedOwnTag adds a tag owned by the bot and shared with nobody, and returns its id.
func (s *Server) SeedOwnTag(name string) string {
	s.t.Helper()
	return s.SeedTag(name, BotLogin, Sharing{}, Sharing{})
}

// RenameTag changes the live name of tag id, as its owner would in YouTrack.
func (s *Server) RenameTag(id, name string) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.m.tagByID(id)
	if t == nil {
		s.t.Fatalf("youtracktest: no tag %q", id)
	}
	t.name = name
}

// TagsNamed returns every tag called name, in creation order, whoever owns it
// and whether or not the bot can see it.
func (s *Server) TagsNamed(name string) []TagInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TagInfo
	for _, t := range s.m.tags {
		if t.name == name {
			out = append(out, s.m.tagInfo(t))
		}
	}
	return out
}

// TagByID returns the tag with id, failing the test when there is none.
func (s *Server) TagByID(id string) TagInfo {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.m.tagByID(id)
	if t == nil {
		s.t.Fatalf("youtracktest: no tag %q", id)
	}
	return s.m.tagInfo(t)
}

func (m *model) tagInfo(t *tag) TagInfo {
	info := TagInfo{ID: t.id, Name: t.name,
		Read: Sharing{Groups: t.read.groups, Users: t.read.users}, Use: Sharing{Groups: t.use.groups, Users: t.use.users}}
	for _, u := range fakeUsers {
		if u.id == t.owner {
			info.OwnerLogin = u.login
		}
	}
	return info
}

// SeedIssue creates an issue in the main project and returns its number. The
// tags, given by id, are added without recording an activity.
func (s *Server) SeedIssue(summary, description string, tagIDs ...string) int64 {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.newIssue(s.m.projectByID(ProjectID), summary, description)
	for _, id := range tagIDs {
		if s.m.tagByID(id) == nil {
			s.t.Fatalf("youtracktest: no tag %q", id)
		}
		i.tags = append(i.tags, id)
	}
	return i.number
}

// SeedResolvedIssue creates an issue in the main project that is resolved but
// has no State value recorded, as an issue resolved before the State field
// existed, and returns its number.
func (s *Server) SeedResolvedIssue(summary string) int64 {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.newIssue(s.m.projectByID(ProjectID), summary, "")
	ts := s.m.now()
	i.resolved = &ts
	return i.number
}

// SetReporter makes login the reporter of issue number n. A seeded issue is
// reported by BotLogin (the account the token authenticates as) until this says
// otherwise, so a test of an author that is not the connection's account sets it.
func (s *Server) SetReporter(n int64, login string) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	i.reporter = login
	// The creation is recorded under the reporter, so the two never disagree.
	for _, a := range i.activities {
		if a.category == catIssueCreate {
			a.author = login
		}
	}
}

// DropCreationActivity removes the creation activity of issue number n, leaving
// an issue whose history does not say who created it.
func (s *Server) DropCreationActivity(n int64) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	kept := i.activities[:0]
	for _, a := range i.activities {
		if a.category != catIssueCreate {
			kept = append(kept, a)
		}
	}
	i.activities = kept
}

// SeedComment adds a comment by login on issue number n.
func (s *Server) SeedComment(n int64, login, text string) {
	s.seedComment(n, login, text, false)
}

// SeedDeletedComment adds a comment that the API reports as deleted.
func (s *Server) SeedDeletedComment(n int64, login, text string) {
	s.seedComment(n, login, text, true)
}

func (s *Server) seedComment(n int64, login, text string, deleted bool) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	ts := s.m.now()
	i.updated = ts
	i.comments = append(i.comments, &comment{id: s.m.nextID(4), text: text, created: ts, deleted: deleted, author: login})
}

// SeedTagActivity records that login added and removed the tags with the given
// ids on issue number n. The issue's own tag list is left alone, so a test can
// seed history that no longer matches the present.
func (s *Server) SeedTagActivity(n int64, login string, added, removed []string) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	a := &activity{id: s.m.nextID(3), category: catTags, timestamp: s.m.now(), author: login}
	for _, id := range append(slices.Clone(added), removed...) {
		if s.m.tagByID(id) == nil {
			s.t.Fatalf("youtracktest: no tag %q", id)
		}
	}
	a.added, a.removed = added, removed
	i.activities = append(i.activities, a)
}

// MoveIssue moves issue number n to the project toProjectID. Like YouTrack it
// takes a new number there, and the old id-readable keeps resolving to it. It
// returns the new id-readable.
func (s *Server) MoveIssue(n int64, toProjectID string) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	to := s.m.projectByID(toProjectID)
	if i == nil || to == nil {
		s.t.Fatalf("youtracktest: cannot move issue %d to project %q", n, toProjectID)
	}
	to.next++
	i.project, i.number = to, to.next
	i.updated = s.m.now()
	s.m.aliases[i.readable()] = i
	return i.readable()
}

// IssueTagIDs returns the ids of the tags currently on issue number n in the
// main project, oldest first, for assertions on server-side state.
func (s *Server) IssueTagIDs(n int64) []string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	return slices.Clone(i.tags)
}

// SeedTextEdit records that login changed the summary and/or the description of
// issue number n, as the activity log would, and sets the new text. An empty
// login records a null author, as YouTrack does for an edit it cannot attribute.
func (s *Server) SeedTextEdit(n int64, login string, summary, description *string) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	if summary != nil {
		i.summary = *summary
		i.updated = s.m.now()
		i.activities = append(i.activities, &activity{id: s.m.textActivityID(), category: catSummary, timestamp: i.updated, author: login})
	}
	if description != nil {
		i.description = *description
		i.updated = s.m.now()
		i.activities = append(i.activities, &activity{id: s.m.textActivityID(), category: catDescription, timestamp: i.updated, author: login})
	}
}

// SeedCommentEdit records that login changed the text of the most recent
// comment on issue number n whose text is text, and sets it to newText. An
// empty login records a null author.
func (s *Server) SeedCommentEdit(n int64, login, text, newText string) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	for k := len(i.comments) - 1; k >= 0; k-- {
		if c := i.comments[k]; c.text == text {
			c.text = newText
			i.updated = s.m.now()
			i.activities = append(i.activities, &activity{id: s.m.textActivityID(), category: catCommentText, timestamp: i.updated, author: login, target: c.id})
			return
		}
	}
	s.t.Fatalf("youtracktest: no comment %q on issue %d", text, n)
}

// SeedState adds an unarchived State value to the bundle and returns its id.
func (s *Server) SeedState(name string, resolved bool) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Not nextID: its counter would hand out an id the default states hold.
	id := fmt.Sprintf("10-%d", 100+len(s.m.states))
	s.m.states = append(s.m.states, stateValue{id: id, name: name, resolved: resolved, ordinal: len(s.m.states)})
	return id
}

// RenameState renames the State value called from, as a bundle administrator
// would after a mapping was saved. Issues in that state follow the new name.
func (s *Server) RenameState(from, to string) {
	s.t.Helper()
	s.editState(from, func(v *stateValue) { v.name = to })
}

// SetStateResolved sets the resolved flag of the State value called name.
func (s *Server) SetStateResolved(name string, resolved bool) {
	s.t.Helper()
	s.editState(name, func(v *stateValue) { v.resolved = resolved })
}

// SetStateArchived sets the archived flag of the State value called name.
func (s *Server) SetStateArchived(name string, archived bool) {
	s.t.Helper()
	s.editState(name, func(v *stateValue) { v.archived = archived })
}

// SetStateLocalizedName sets the localized name of the State value called name.
func (s *Server) SetStateLocalizedName(name, localized string) {
	s.t.Helper()
	s.editState(name, func(v *stateValue) { v.localizedName = localized })
}

func (s *Server) editState(name string, edit func(*stateValue)) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.m.stateByName(name)
	if v == nil {
		s.t.Fatalf("youtracktest: no state %q", name)
	}
	edit(v)
}

// IssueState returns the name of the State value of issue number n in the main
// project, for assertions on server-side state.
func (s *Server) IssueState(n int64) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	// Read from the model, not the wire, so an issue listed in a non-plain
	// State field shape still reports the State it holds.
	if v := s.m.issueStateValue(i); v != nil {
		return v.name
	}
	return ""
}

// SeedStateFieldShape sets how issue number n of the main project lists its
// State field. The non-plain shapes are this fake's own (see StateFieldShape);
// a State value write to such an issue answers 400.
func (s *Server) SeedStateFieldShape(n int64, shape StateFieldShape) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	switch shape {
	case StateFieldPlain, StateFieldAbsent, StateFieldUntyped:
	default:
		s.t.Fatalf("youtracktest: unknown State field shape %q", shape)
	}
	i.stateShape = shape
}

// SeedIDReadable makes issue number n of the main project serve id as its
// "idReadable", empty included. This is the fake's own: it lets a test serve an
// over-long or empty id-readable. The issue still resolves by its real one.
func (s *Server) SeedIDReadable(n int64, id string) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m.byNumber(n)
	if i == nil {
		s.t.Fatalf("youtracktest: no issue %d", n)
	}
	i.idReadableSet, i.idReadableOverride = true, id
}

// SetStateMachine makes the State field of the project with id projectID one a
// state-machine workflow runs (or a plain one again). Its issues then list a
// StateMachineIssueCustomField, and a value write to one answers 400.
func (s *Server) SetStateMachine(projectID string, on bool) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m.projectByID(projectID)
	if p == nil {
		s.t.Fatalf("youtracktest: no project %q", projectID)
	}
	p.stateMachine = on
}

// SetExtraStateMachineField adds to every issue of the project with id
// projectID a second custom field, named name and run by a state machine, next
// to its plain State field; an empty name removes it.
func (s *Server) SetExtraStateMachineField(projectID, name string) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m.projectByID(projectID)
	if p == nil {
		s.t.Fatalf("youtracktest: no project %q", projectID)
	}
	p.extraMachineField = name
}

// FailIssueListing makes every issues search (GET /api/issues) answer status,
// for example 403 or 404; 0 restores normal answers.
func (s *Server) FailIssueListing(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.listFailure = status
}
