package youtracktest

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Fixed identities of the fake's data.
const (
	// ProjectID and ProjectShortName name the project issues are created in.
	ProjectID        = "0-1"
	ProjectShortName = "UZI"
	// OtherProjectID and OtherProjectShortName name a second project, the
	// target of MoveIssue.
	OtherProjectID        = "0-2"
	OtherProjectShortName = "OTH"
	// BotLogin and BotID are the account the bearer token authenticates as.
	BotLogin = "uzi-bot"
	BotID    = "1-1"
	// OtherLogin and OtherUserID are a second user, who owns the default
	// eligibility tag.
	OtherLogin  = "maintainer"
	OtherUserID = "1-2"
	// The fake's user groups. AllUsersGroupID covers every user; TeamGroupID is
	// a named group of both users; SoloGroupID is a named group of the other
	// user only.
	AllUsersGroupID = "675-0"
	TeamGroupID     = "675-1"
	SoloGroupID     = "675-2"
	// The default seed has an eligibility tag owned by the other user and shared
	// with All Users, and a tag of the same name owned by the bot, shared with
	// nobody. The ids are high so the ids the fake hands out never reach them.
	EligibilityTagID   = "5-9001"
	EligibilityTagName = "uzi-eligible"
	SameNameTagID      = "5-9002"
	// DefaultStateField is the name of the State custom field.
	DefaultStateField = "State"
)

type project struct {
	id, shortName, name string
	next                int64 // last number handed out in this project
}

type user struct{ id, login string }

// group is a user group; members are user ids. An all-users group has every user.
type group struct {
	id, name string
	allUsers bool
	members  []string
}

// fakeUsers and fakeGroups are the fixed identities of the fake.
var (
	fakeUsers  = []user{{BotID, BotLogin}, {OtherUserID, OtherLogin}}
	fakeGroups = []group{
		{id: AllUsersGroupID, name: "All Users", allUsers: true, members: []string{BotID, OtherUserID}},
		{id: TeamGroupID, name: "Team", members: []string{BotID, OtherUserID}},
		{id: SoloGroupID, name: "Solo", members: []string{OtherUserID}},
	}
)

// sharing is who a tag is shared with, by group id and user id.
type sharing struct{ groups, users []string }

type tag struct {
	id, name string
	owner    string // user id
	read     sharing
	use      sharing
}

type comment struct {
	id      string
	text    string
	created int64
	deleted bool
	author  string
}

// Activity categories the fake records and serves.
const (
	catTags        = "TagsCategory"
	catIssueCreate = "IssueCreatedCategory"
	catSummary     = "SummaryCategory"
	catDescription = "DescriptionCategory"
	catCommentText = "CommentTextCategory"
)

type activity struct {
	id        string
	category  string
	timestamp int64
	author    string   // login; empty is a null author
	target    string   // comment id, for catCommentText
	added     []string // tag ids
	removed   []string
}

type stateValue struct {
	id, name string
	resolved bool
	ordinal  int
}

type issue struct {
	id          string
	project     *project
	number      int64
	summary     string
	description string
	created     int64
	updated     int64
	resolved    *int64
	reporter    string
	tags        []string // tag ids, oldest first
	comments    []*comment
	activities  []*activity
}

func (i *issue) readable() string { return fmt.Sprintf("%s-%d", i.project.shortName, i.number) }

// model is the in-memory YouTrack state. Server.mu guards every field.
type model struct {
	projects   []*project
	tags       []*tag
	issues     []*issue
	aliases    map[string]*issue // every id-readable an issue has answered to
	states     []stateValue
	stateField string
	clock      int64
	seq        int64
	textSeq    int64 // separate from seq so recording text activities shifts no other id
}

func newModel() *model {
	return &model{
		projects: []*project{
			{id: ProjectID, shortName: ProjectShortName, name: "Uzi"},
			{id: OtherProjectID, shortName: OtherProjectShortName, name: "Other"},
		},
		// The bot's own tag comes first, so a driver that takes the first tag
		// of a name picks it rather than the eligibility tag.
		tags: []*tag{
			{id: SameNameTagID, name: EligibilityTagName, owner: BotID},
			{id: EligibilityTagID, name: EligibilityTagName, owner: OtherUserID,
				read: sharing{groups: []string{AllUsersGroupID}}, use: sharing{groups: []string{AllUsersGroupID}}},
		},
		aliases: map[string]*issue{},
		states: []stateValue{
			{id: "10-1", name: "Open", resolved: false, ordinal: 0},
			{id: "10-2", name: "Fixed", resolved: true, ordinal: 1},
			{id: "10-3", name: "Won't fix", resolved: true, ordinal: 2},
		},
		stateField: DefaultStateField,
		clock:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
	}
}

// now returns a strictly increasing epoch-millisecond clock, one second per
// call, so ordering assertions never depend on wall time.
func (m *model) now() int64 {
	m.clock += 1000
	return m.clock
}

func (m *model) nextID(prefix int) string {
	m.seq++
	return fmt.Sprintf("%d-%d", prefix, m.seq)
}

// textActivityID is the id of an issue-created, summary, description or comment
// text activity. It has its own counter so that recording these, which the fake
// began to do after tests pinned the ids it hands out, leaves those ids alone.
func (m *model) textActivityID() string {
	m.textSeq++
	return fmt.Sprintf("3-t%d", m.textSeq)
}

func (m *model) projectByID(id string) *project {
	for _, p := range m.projects {
		if p.id == id {
			return p
		}
	}
	return nil
}

func (m *model) projectByShort(short string) *project {
	for _, p := range m.projects {
		if strings.EqualFold(p.shortName, short) {
			return p
		}
	}
	return nil
}

func (m *model) tagByID(id string) *tag {
	for _, t := range m.tags {
		if t.id == id {
			return t
		}
	}
	return nil
}

// covers reports whether s reaches user id: the user is named, or is in a
// named group, or a group covers all users.
func covers(s sharing, userID string) bool {
	if slices.Contains(s.users, userID) {
		return true
	}
	for _, gid := range s.groups {
		for _, g := range fakeGroups {
			if g.id == gid && (g.allUsers || slices.Contains(g.members, userID)) {
				return true
			}
		}
	}
	return false
}

// visibleToBot reports whether the bot can see the tag.
func (t *tag) visibleToBot() bool { return t.owner == BotID || covers(t.read, BotID) }

// usableBy reports whether the user can apply the tag to an issue.
func (t *tag) usableBy(userID string) bool { return t.owner == userID || covers(t.use, userID) }

// visibleTags returns the tags the bot can see.
func (m *model) visibleTags() []*tag {
	var out []*tag
	for _, t := range m.tags {
		if t.visibleToBot() {
			out = append(out, t)
		}
	}
	return out
}

func (m *model) newIssue(p *project, summary, description string) *issue {
	p.next++
	ts := m.now()
	i := &issue{
		id: m.nextID(2), project: p, number: p.next, summary: summary, description: description,
		created: ts, updated: ts, reporter: BotLogin,
	}
	i.activities = append(i.activities, &activity{id: m.textActivityID(), category: catIssueCreate, timestamp: ts, author: BotLogin})
	m.issues = append(m.issues, i)
	m.aliases[i.readable()] = i
	return i
}

// lookup resolves an entity id or any id-readable the issue has had.
func (m *model) lookup(id string) *issue {
	for _, i := range m.issues {
		if i.id == id {
			return i
		}
	}
	return m.aliases[id]
}

func (m *model) byNumber(n int64) *issue {
	return m.aliases[fmt.Sprintf("%s-%d", ProjectShortName, n)]
}

func (i *issue) hasTag(id string) bool {
	for _, t := range i.tags {
		if t == id {
			return true
		}
	}
	return false
}

// addTag adds a tag and records the activity, unless it is already present.
func (m *model) addTag(i *issue, t *tag, author string) {
	if i.hasTag(t.id) {
		return
	}
	ts := m.now()
	i.tags = append(i.tags, t.id)
	i.updated = ts
	i.activities = append(i.activities, &activity{id: m.nextID(3), category: catTags, timestamp: ts, author: author, added: []string{t.id}})
}

func (m *model) removeTag(i *issue, t *tag, author string) {
	if !i.hasTag(t.id) {
		return
	}
	ts := m.now()
	kept := i.tags[:0:0]
	for _, id := range i.tags {
		if id != t.id {
			kept = append(kept, id)
		}
	}
	i.tags = kept
	i.updated = ts
	i.activities = append(i.activities, &activity{id: m.nextID(3), category: catTags, timestamp: ts, author: author, removed: []string{t.id}})
}

func (m *model) stateByName(name string) *stateValue {
	for k := range m.states {
		if m.states[k].name == name {
			return &m.states[k]
		}
	}
	return nil
}

// issueJSON is the full wire object of an issue; callers project it through a
// fields spec.
func (m *model) issueJSON(i *issue) map[string]any {
	tags := make([]map[string]any, 0, len(i.tags))
	for _, id := range i.tags {
		if t := m.tagByID(id); t != nil {
			tags = append(tags, m.tagJSON(t))
		}
	}
	var resolved any
	if i.resolved != nil {
		resolved = *i.resolved
	}
	return map[string]any{
		"$type":           "Issue",
		"id":              i.id,
		"idReadable":      i.readable(),
		"numberInProject": i.number,
		"summary":         i.summary,
		"description":     i.description,
		"created":         i.created,
		"updated":         i.updated,
		"resolved":        resolved,
		"reporter":        userJSON(i.reporter),
		"tags":            tags,
		"project":         map[string]any{"id": i.project.id, "shortName": i.project.shortName, "$type": "Project"},
	}
}

// userJSON is the wire user for a login. A login the fake does not know is
// given the bot's id, as comment authors such as "alice" always were.
func userJSON(login string) map[string]any {
	for _, u := range fakeUsers {
		if u.login == login {
			return map[string]any{"$type": "User", "id": u.id, "login": u.login}
		}
	}
	return map[string]any{"$type": "User", "id": BotID, "login": login}
}

func userJSONByID(id string) map[string]any {
	for _, u := range fakeUsers {
		if u.id == id {
			return userJSON(u.login)
		}
	}
	return map[string]any{"$type": "User", "id": id, "login": id}
}

func (m *model) sharingJSON(s sharing, typ string) map[string]any {
	groups := []map[string]any{}
	for _, gid := range s.groups {
		for _, g := range fakeGroups {
			if g.id != gid {
				continue
			}
			gt := "UserGroup"
			if g.allUsers {
				gt = "AllUsersGroup"
			}
			groups = append(groups, map[string]any{"$type": gt, "id": g.id, "name": g.name, "allUsersGroup": g.allUsers, "usersCount": len(g.members)})
		}
	}
	users := []map[string]any{}
	for _, uid := range s.users {
		users = append(users, userJSONByID(uid))
	}
	return map[string]any{"$type": typ, "permittedGroups": groups, "permittedUsers": users}
}

// tagJSON is the full wire object of a tag; callers project it through a fields spec.
func (m *model) tagJSON(t *tag) map[string]any {
	return map[string]any{
		"$type": "Tag", "id": t.id, "name": t.name, "owner": userJSONByID(t.owner),
		"readSharingSettings": m.sharingJSON(t.read, "WatchFolderSharingSettings"),
		"tagSharingSettings":  m.sharingJSON(t.use, "TagSharingSettings"),
	}
}

func (m *model) tagsJSON(ids []string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if t := m.tagByID(id); t != nil {
			out = append(out, m.tagJSON(t))
		}
	}
	return out
}

// parseDay returns the epoch milliseconds of the start of the UTC day.
func parseDay(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", s)
	return t.UnixMilli(), err
}
