package issuetracker

import (
	"regexp"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// fields parameters. YouTrack returns only what is asked for.
const (
	issueFields     = "id,idReadable,numberInProject,summary,description,created,updated,resolved,reporter(id,login),tags(id,name),project(id,shortName)"
	tagFields       = "id,name"
	ownedTagFields  = "id,name,owner(id)"
	sharingFields   = "permittedGroups(id,name,allUsersGroup,usersCount),permittedUsers(id,login)"
	sharedTagFields = "id,name,owner(id,login),readSharingSettings(" + sharingFields + "),tagSharingSettings(" + sharingFields + ")"
	commentFields   = "id,text,created,deleted,author(id,login)"
	activityFields  = "id,timestamp,author(id,login),added(id,name),removed(id,name)"
)

// idReadableRE matches an id-readable such as "UZI-15"; it ends up in WebURL.
var idReadableRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-\d+$`)

type ytUser struct {
	ID    string `json:"id"`
	Login string `json:"login"`
}

// ytTag is a wire tag. Owner and the sharing settings are present only where
// the request asked for them (see ownedTagFields and sharedTagFields).
type ytTag struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Owner *ytUser    `json:"owner"`
	Read  *ytSharing `json:"readSharingSettings"`
	Use   *ytSharing `json:"tagSharingSettings"`
}

// ytSharing is YouTrack's TagSharingSettings (and the read settings, which have
// the same members).
type ytSharing struct {
	Groups []ytGroup `json:"permittedGroups"`
	Users  []ytUser  `json:"permittedUsers"`
}

type ytGroup struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AllUsers   bool   `json:"allUsersGroup"`
	UsersCount int    `json:"usersCount"`
}

// nestedCount is the number of users and groups in the tag's sharing settings.
func (t ytTag) nestedCount() int {
	n := 0
	for _, s := range []*ytSharing{t.Read, t.Use} {
		if s != nil {
			n += len(s.Groups) + len(s.Users)
		}
	}
	return n
}

type ytProject struct {
	ID        string `json:"id"`
	ShortName string `json:"shortName"`
	Name      string `json:"name"`
}

type ytIssue struct {
	ID              string     `json:"id"`
	IDReadable      string     `json:"idReadable"`
	NumberInProject int64      `json:"numberInProject"`
	Summary         string     `json:"summary"`
	Description     string     `json:"description"`
	Created         int64      `json:"created"`
	Updated         int64      `json:"updated"`
	Resolved        *int64     `json:"resolved"`
	Reporter        *ytUser    `json:"reporter"`
	Tags            ytTags     `json:"tags"`
	Project         *ytProject `json:"project"`
}

func (w ytIssue) nestedCount() int { return len(w.Tags) }

func (a ytActivity) nestedCount() int { return len(a.Added) + len(a.Removed) }

type ytComment struct {
	ID      string  `json:"id"`
	Text    string  `json:"text"`
	Created int64   `json:"created"`
	Deleted bool    `json:"deleted"`
	Author  *ytUser `json:"author"`
}

type ytActivity struct {
	ID        string  `json:"id"`
	Timestamp int64   `json:"timestamp"`
	Author    *ytUser `json:"author"`
	Added     ytTags  `json:"added"`
	Removed   ytTags  `json:"removed"`
}

// checkIssue refuses an issue whose entity id, id-readable or tag ids are not
// well formed: those ids end up in request paths and in WebURL. The tag count is
// capped while decoding (ytTags).
func (y *youTrack) checkIssue(w ytIssue) error {
	if !entityIDRE.MatchString(w.ID) || !idReadableRE.MatchString(w.IDReadable) {
		return y.fail("youtrack: issue with a malformed id in the response")
	}
	for _, t := range w.Tags {
		if !entityIDRE.MatchString(t.ID) {
			return y.fail("youtrack: tag with a malformed id in the response")
		}
	}
	return nil
}

// toIssue maps a wire issue to the neutral type. The issue must carry a valid
// entity id and id-readable.
func (y *youTrack) toIssue(w ytIssue) (forge.Issue, error) {
	if err := y.checkIssue(w); err != nil {
		return forge.Issue{}, err
	}
	issue := forge.Issue{
		IID:         w.NumberInProject,
		Title:       w.Summary,
		State:       string(forge.StateOpened),
		Labels:      make([]string, 0, len(w.Tags)),
		Description: w.Description,
		WebURL:      y.baseURL + "/issue/" + w.IDReadable,
		UpdatedAt:   time.UnixMilli(w.Updated).UTC(),
		Assignees:   []int64{},
	}
	if w.Resolved != nil {
		issue.State = string(forge.StateClosed)
	}
	if w.Reporter != nil {
		issue.Author = w.Reporter.Login
	}
	for _, t := range w.Tags {
		if y.foreignEligibility(t) {
			continue
		}
		issue.Labels = append(issue.Labels, t.Name)
	}
	return issue, nil
}

// foreignEligibility reports whether t carries the eligibility name without
// being the eligibility tag. Reads leave it out, so the eligibility name in a
// result always means the stored tag.
func (y *youTrack) foreignEligibility(t ytTag) bool {
	return y.eligibility.Name != "" && t.Name == y.eligibility.Name && t.ID != y.eligibility.ID
}

// inBoundProject reports whether the issue sits in the bound project.
func (y *youTrack) inBoundProject(w ytIssue) bool {
	return w.Project != nil && w.Project.ID == y.project.ID && w.NumberInProject > 0
}

func login(u *ytUser) string {
	if u == nil {
		return ""
	}
	return u.Login
}
