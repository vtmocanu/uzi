package issuetracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

func acct(id string) Account { return Account{ID: id, Login: "login-" + id} }

func use(groups []Group, users ...string) SharingSettings {
	s := SharingSettings{Groups: groups}
	for _, id := range users {
		s.Users = append(s.Users, acct(id))
	}
	return s
}

var (
	allUsers = Group{ID: "675-0", Name: "All Users", AllUsers: true}
	team2    = Group{ID: "673-1", Name: "Team", UsersCount: 2}
	solo     = Group{ID: "673-2", Name: "Solo", UsersCount: 1}
	noCount  = Group{ID: "673-3", Name: "Unknown size"}
)

func TestAccountCanApply(t *testing.T) {
	me := acct("1-1")
	for _, tc := range []struct {
		name string
		tag  Tag
		who  Account
		want bool
	}{
		{"owner", Tag{Owner: me}, me, true},
		{"named in use users", Tag{Owner: acct("1-2"), Use: use(nil, "1-3", "1-1")}, me, true},
		{"all users group", Tag{Owner: acct("1-2"), Use: use([]Group{allUsers})}, me, true},
		{"named group alone does not count", Tag{Owner: acct("1-2"), Use: use([]Group{team2})}, me, false},
		{"named group of one does not count", Tag{Owner: acct("1-2"), Use: use([]Group{solo})}, me, false},
		{"someone else only", Tag{Owner: acct("1-2"), Use: use(nil, "1-3")}, me, false},
		{"read settings do not grant use", Tag{Owner: acct("1-2"), Read: use([]Group{allUsers}, "1-1")}, me, false},
		{"unshared", Tag{Owner: acct("1-2")}, me, false},
		{"no account never matches an unknown owner", Tag{}, Account{}, false},
		{"no account never matches an empty user", Tag{Owner: acct("1-2"), Use: SharingSettings{Users: []Account{{}}}}, Account{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AccountCanApply(tc.tag, tc.who); got != tc.want {
				t.Errorf("AccountCanApply = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOthersCanApply(t *testing.T) {
	me := acct("1-1")
	for _, tc := range []struct {
		name string
		tag  Tag
		want bool
	}{
		{"owned by another user", Tag{Owner: acct("1-2")}, true},
		{"owned by me and unshared", Tag{Owner: me}, false},
		{"owned by me, shared with another user", Tag{Owner: me, Use: use(nil, "1-2")}, true},
		{"owned by me, use users are only me", Tag{Owner: me, Use: use(nil, "1-1")}, false},
		{"owned by me, shared with all users", Tag{Owner: me, Use: use([]Group{allUsers})}, true},
		{"owned by me, group of two", Tag{Owner: me, Use: use([]Group{team2})}, true},
		{"owned by me, group of one", Tag{Owner: me, Use: use([]Group{solo})}, false},
		{"owned by me, group with no count", Tag{Owner: me, Use: use([]Group{noCount})}, false},
		{"owned by me, only read settings", Tag{Owner: me, Read: use([]Group{allUsers}, "1-2")}, false},
		{"unknown owner is not another user", Tag{}, false},
		{"empty user entry is not another user", Tag{Owner: me, Use: SharingSettings{Users: []Account{{}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OthersCanApply(tc.tag, me); got != tc.want {
				t.Errorf("OthersCanApply = %v, want %v", got, tc.want)
			}
		})
	}
	if OthersCanApply(Tag{Owner: acct("1-2")}, Account{}) {
		t.Error("OthersCanApply with an unknown account must fail closed")
	}
}

// unboundDriver builds a driver with no project over a raw handler.
func unboundDriver(t *testing.T, h http.HandlerFunc) (*youTrack, *requestLog) {
	t.Helper()
	log := &requestLog{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	y, err := newYouTrackOver(Options{BaseURL: srv.URL, Token: credential()}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return y, log
}

// TestListTagsParsesARecordedResponse parses testdata/youtrack_tags_recorded.json
// through the driver's real decoding path. The file is a response of GET
// /api/tags with the fields ListTags asks for, recorded anonymously from the
// public YouTrack instance at youtrack.jetbrains.com. Every login, tag name
// and user or tag id in it is a placeholder; the structure, the $type members,
// the booleans and the absence of usersCount (the server did not return one) are
// as recorded.
func TestListTagsParsesARecordedResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/youtrack_tags_recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	y, log := unboundDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, string(raw)) })
	tags, err := y.ListTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := log.all(); len(got) != 1 || got[0] != "GET /api/tags" {
		t.Errorf("requests = %v, want one GET /api/tags", got)
	}
	if len(tags) != 5 {
		t.Fatalf("got %d tags, want 5", len(tags))
	}
	first := tags[0]
	if first.ID != "68-1001" || first.Name != "tag-1" || first.Owner != (Account{ID: "11-1001", Login: "user-1"}) {
		t.Errorf("first tag = %+v", first)
	}
	if len(first.Read.Groups) != 1 || first.Read.Groups[0] != (Group{ID: "675-0", Name: "All Users", AllUsers: true}) {
		t.Errorf("read groups = %+v, want the All Users group", first.Read.Groups)
	}
	if len(first.Read.Users) != 2 || len(first.Use.Users) != 2 || len(first.Use.Groups) != 0 {
		t.Errorf("sharing = read %+v, use %+v", first.Read, first.Use)
	}
	// One tag is also readable by a named group, which is not an all-users group.
	var named *Group
	for _, tg := range tags {
		for i, g := range tg.Read.Groups {
			if !g.AllUsers {
				named = &tg.Read.Groups[i]
			}
		}
	}
	if named == nil || named.ID == "" || named.Name == "" || named.UsersCount != 0 {
		t.Errorf("named group = %+v, want one with a name and no count", named)
	}
	// The rules read the parsed settings: a user named in the use settings can
	// apply the tag, a stranger cannot, and nobody is judged from the read settings.
	if !AccountCanApply(first, first.Use.Users[0]) || AccountCanApply(first, acct("11-9999")) {
		t.Error("AccountCanApply on the recorded tag does not follow its use settings")
	}
	if !OthersCanApply(first, first.Owner) {
		t.Error("OthersCanApply on the recorded tag: its use users are not its owner")
	}
}

// TestListTagsParsesARecordedPrivateInstanceResponse parses
// testdata/youtrack_tags_recorded_private.json, a response of the same request
// recorded with a permanent token on a private YouTrack instance. Every login,
// tag name, id and the member count in it is a placeholder; the structure, the
// $type members and the booleans are as recorded. Unlike the public recording it
// carries usersCount, an all-users group in the use settings, a tag only its
// owner can apply and a tag only its owner can see.
func TestListTagsParsesARecordedPrivateInstanceResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/youtrack_tags_recorded_private.json")
	if err != nil {
		t.Fatal(err)
	}
	y, _ := unboundDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, string(raw)) })
	tags, err := y.ListTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 {
		t.Fatalf("got %d tags, want 3", len(tags))
	}
	shared, ownerOnly, private := tags[0], tags[1], tags[2]
	stranger := acct("25-9999")

	allUsers := Group{ID: "579-1", Name: "All Users", AllUsers: true, UsersCount: 12}
	if len(shared.Use.Groups) != 1 || shared.Use.Groups[0] != allUsers || len(shared.Read.Groups) != 1 || shared.Read.Groups[0] != allUsers {
		t.Errorf("shared tag sharing = read %+v, use %+v, want the All Users group with its count in both", shared.Read, shared.Use)
	}
	if !AccountCanApply(shared, stranger) || !OthersCanApply(shared, shared.Owner) {
		t.Error("a tag whose use settings cover all users must be applicable by any account and by others")
	}

	if len(ownerOnly.Read.Groups) != 1 || !ownerOnly.Read.Groups[0].AllUsers || len(ownerOnly.Use.Groups) != 0 || len(ownerOnly.Use.Users) != 0 {
		t.Errorf("owner-only tag sharing = read %+v, use %+v, want visible to all and usable by nobody listed", ownerOnly.Read, ownerOnly.Use)
	}
	if AccountCanApply(ownerOnly, stranger) || !AccountCanApply(ownerOnly, ownerOnly.Owner) || OthersCanApply(ownerOnly, ownerOnly.Owner) {
		t.Error("a tag everyone can see but only its owner can apply must not count as applicable by anyone else")
	}

	if len(private.Read.Groups) != 0 || len(private.Read.Users) != 0 || len(private.Use.Groups) != 0 || len(private.Use.Users) != 0 {
		t.Errorf("private tag sharing = read %+v, use %+v, want empty", private.Read, private.Use)
	}
	if AccountCanApply(private, stranger) || OthersCanApply(private, private.Owner) {
		t.Error("a private tag must not count as applicable by anyone but its owner")
	}
}

func TestListTagsNeedsNoProject(t *testing.T) {
	srv := youtracktest.NewServer(t)
	own := srv.SeedOwnTag("mine")
	unbound, err := NewYouTrackOver(Options{BaseURL: srv.URL, Token: srv.Token}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := unbound.ListTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Tag{}
	for _, tg := range tags {
		byID[tg.ID] = tg
	}
	elig := byID[fakeEligibility.ID]
	if elig.Name != fakeEligibility.Name || elig.Owner.Login != "maintainer" || len(elig.Use.Groups) != 1 || !elig.Use.Groups[0].AllUsers || elig.Use.Groups[0].UsersCount != 2 {
		t.Errorf("eligibility tag = %+v", elig)
	}
	if mine := byID[own]; mine.Owner.ID != "1-1" || len(mine.Read.Groups)+len(mine.Use.Groups)+len(mine.Read.Users) != 0 {
		t.Errorf("own tag = %+v", mine)
	}
	// VerifyToken and ListProjects work on the unbound driver too.
	if _, err := unbound.VerifyToken(context.Background()); err != nil {
		t.Error(err)
	}
	if _, err := unbound.ListProjects(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestListTagsRefusesMalformedIDsAndOversizedSharing(t *testing.T) {
	user := func(id string) string { return `{"id":"` + id + `","login":"u"}` }
	group := func(id string) string { return `{"id":"` + id + `","name":"g","allUsersGroup":false,"usersCount":3}` }
	tag := func(id, owner, readGroups, readUsers string) string {
		return `[{"id":"` + id + `","name":"t","owner":` + owner + `,"readSharingSettings":{"permittedGroups":[` + readGroups +
			`],"permittedUsers":[` + readUsers + `]},"tagSharingSettings":{"permittedGroups":[],"permittedUsers":[]}}]`
	}
	for name, body := range map[string]string{
		"tag id":      tag("..", user("1-1"), "", ""),
		"owner id":    tag("5-1", user("../x"), "", ""),
		"group id":    tag("5-1", user("1-1"), group("a/b"), ""),
		"sharer id":   tag("5-1", user("1-1"), "", user("")),
		"too many":    tag("5-1", user("1-1"), strings.TrimSuffix(strings.Repeat(group("673-1")+",", maxNestedItems+1), ","), ""),
		"not objects": `[{"id":"5-1","name":"t","readSharingSettings":{"permittedUsers":[1]}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			y, _ := unboundDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, body) })
			if got, err := y.ListTags(context.Background()); err == nil {
				t.Errorf("ListTags accepted %s: %+v", name, got)
			}
		})
	}
}

func TestListTagsNestedSharingCountsTowardTheItemCeiling(t *testing.T) {
	lowerMaxItems(t, 5)
	// Two tags with three sharers each: two items, eight counting the sharers.
	tag := `{"id":"5-1","name":"t","owner":{"id":"1-1","login":"u"},"readSharingSettings":{"permittedGroups":[],"permittedUsers":[` +
		`{"id":"1-2","login":"a"},{"id":"1-3","login":"b"},{"id":"1-4","login":"c"}]}}`
	y, _ := unboundDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, "["+tag+","+tag+"]") })
	if _, err := y.ListTags(context.Background()); err == nil {
		t.Fatal("nested sharing entries did not count toward the item ceiling")
	}
}
