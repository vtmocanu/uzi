package youtracktest

import "slices"

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
	a := &activity{id: s.m.nextID(3), timestamp: s.m.now(), author: login}
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
