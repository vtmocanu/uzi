package issuetrackersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

var (
	linkBot   = issuetracker.Account{ID: "1-1", Login: "bot"}
	linkOther = issuetracker.Account{ID: "1-2", Login: "maintainer"}
	linkAll   = issuetracker.Group{ID: "675-0", Name: "All Users", AllUsers: true}
)

// linkHarness is a harness whose connection is already stored and whose
// tracker lists one project and a set of tags.
type linkHarness struct {
	*harness
	user, conn uuid.UUID
}

func newLinkHarness(t *testing.T) *linkHarness {
	t.Helper()
	h := newHarness(t)
	h.template.projects = []issuetracker.Project{{ID: "0-1", ShortName: "UZI", Name: "Uzi"}}
	h.template.tags = []issuetracker.Tag{
		{ID: "5-1", Name: "uzi-ready", Owner: linkBot, Use: issuetracker.SharingSettings{Groups: []issuetracker.Group{linkAll}}},
	}
	user := uuid.New()
	c, err := h.svc.Connect(context.Background(), user, "youtrack", testHost, testToken("link"))
	if err != nil {
		t.Fatal(err)
	}
	return &linkHarness{harness: h, user: user, conn: c.ID}
}

func (h *linkHarness) link(repo uuid.UUID, project, tag string) (Link, error) {
	return h.svc.LinkRepo(context.Background(), h.user, repo, h.conn, project, tag)
}

func TestLinkRepoStoresTheListedValues(t *testing.T) {
	h := newLinkHarness(t)
	repo := h.q.addRepo(h.user, true)
	l, err := h.link(repo, "0-1", "5-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.RepoID != repo || l.ConnectionID != h.conn || l.ProjectID != "0-1" || l.ProjectShort != "UZI" || l.TagID != "5-1" || l.TagName != "uzi-ready" {
		t.Fatalf("link = %+v", l)
	}
	if r := h.q.repo(repo); r.linkedConn != h.conn || r.projectShort != "UZI" || r.tagName != "uzi-ready" {
		t.Fatalf("stored = %+v", r)
	}
}

func TestLinkRepoRefusesAnUnusableTag(t *testing.T) {
	cases := []struct {
		name    string
		tag     issuetracker.Tag
		wantMsg string
	}{
		{"account cannot apply: owned by someone else, not shared with the account",
			issuetracker.Tag{ID: "5-2", Name: "theirs", Owner: linkOther}, "cannot apply this tag"},
		{"account cannot apply: only a one-member group is named",
			issuetracker.Tag{ID: "5-2", Name: "theirs", Owner: linkOther,
				Use: issuetracker.SharingSettings{Groups: []issuetracker.Group{{ID: "675-2", Name: "Solo", UsersCount: 1}}}}, "cannot apply this tag"},
		{"nobody else can apply: owned by the account and not shared",
			issuetracker.Tag{ID: "5-2", Name: "mine", Owner: linkBot}, "named users"},
		{"nobody else can apply: shared only with the account itself",
			issuetracker.Tag{ID: "5-2", Name: "mine", Owner: linkBot,
				Use: issuetracker.SharingSettings{Users: []issuetracker.Account{linkBot}}}, "named users"},
		{"nobody else can apply: a one-member group may be the account",
			issuetracker.Tag{ID: "5-2", Name: "mine", Owner: linkBot,
				Use: issuetracker.SharingSettings{Groups: []issuetracker.Group{{ID: "675-2", Name: "Solo", UsersCount: 1}}}}, "named users"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinkHarness(t)
			h.template.tags = append(h.template.tags, tc.tag)
			repo := h.q.addRepo(h.user, true)
			_, err := h.link(repo, "0-1", tc.tag.ID)
			e := wantKind(t, err, KindConflict)
			if !strings.Contains(e.Message, tc.wantMsg) {
				t.Errorf("message %q lacks %q", e.Message, tc.wantMsg)
			}
			if h.q.repo(repo).linkedConn != uuid.Nil {
				t.Error("the repo was linked")
			}
		})
	}
}

// The refusal says what to change in the tracker and never claims that the tag's
// sharing shows who can edit issues in the project (PRD #4173 D14).
func TestLinkRepoMessagesDoNotClaimEditAccess(t *testing.T) {
	h := newLinkHarness(t)
	h.template.tags = append(h.template.tags, issuetracker.Tag{ID: "5-2", Name: "mine", Owner: linkBot})
	_, err := h.link(h.q.addRepo(h.user, true), "0-1", "5-2")
	msg := wantKind(t, err, KindConflict).Message
	if !strings.Contains(msg, "does not show who can edit issues in the project") {
		t.Errorf("message %q does not disclaim edit access", msg)
	}
	for _, claim := range []string{"can edit issues in the project.", "proves"} {
		if strings.Contains(msg, claim) {
			t.Errorf("message %q claims %q", msg, claim)
		}
	}
}

func TestLinkRepoRefusesWhenTheTokenNowStandsForAnotherAccount(t *testing.T) {
	h := newLinkHarness(t)
	h.template.account = linkOther // the stored connection is for linkBot
	repo := h.q.addRepo(h.user, true)
	e := wantKind(t, errOf(h.link(repo, "0-1", "5-1")), KindConflict)
	if !strings.Contains(e.Message, "reconnect") {
		t.Errorf("message %q does not say to reconnect", e.Message)
	}
	if h.q.repo(repo).linkedConn != uuid.Nil {
		t.Error("the repo was linked")
	}
}

func TestLinkRepoRefusesUnknownProjectAndTag(t *testing.T) {
	for name, ids := range map[string][2]string{
		"unknown project": {"0-9", "5-1"},
		"unknown tag":     {"0-1", "5-9"},
		"malformed id":    {"not an id", "5-1"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLinkHarness(t)
			repo := h.q.addRepo(h.user, true)
			_ = wantKind(t, errOf(h.link(repo, ids[0], ids[1])), KindBadRequest)
			if h.q.repo(repo).linkedConn != uuid.Nil {
				t.Error("the repo was linked")
			}
		})
	}
}

func TestLinkRepoRefusesNamesTheStoreCannotHold(t *testing.T) {
	cases := map[string]func(*linkHarness){
		"short name with a dash":  func(h *linkHarness) { h.template.projects[0].ShortName = "A-B" },
		"short name too long":     func(h *linkHarness) { h.template.projects[0].ShortName = "A" + strings.Repeat("b", 44) },
		"tag name with a brace":   func(h *linkHarness) { h.template.tags[0].Name = "uzi{ready" },
		"tag name with a newline": func(h *linkHarness) { h.template.tags[0].Name = "uzi\nready" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newLinkHarness(t)
			mutate(h)
			_ = wantKind(t, errOf(h.link(h.q.addRepo(h.user, true), "0-1", "5-1")), KindBadRequest)
		})
	}
}

func TestLinkRepoRefusesARepoThatIsNotAwaitingOrAlreadyLinked(t *testing.T) {
	h := newLinkHarness(t)
	enabled := h.q.addRepo(h.user, false)
	built := len(h.built)
	_ = wantKind(t, errOf(h.link(enabled, "0-1", "5-1")), KindConflict)
	if len(h.built) != built {
		t.Error("the tracker was contacted for a repo that cannot be linked")
	}

	fresh := h.q.addRepo(h.user, true)
	if _, err := h.link(fresh, "0-1", "5-1"); err != nil {
		t.Fatal(err)
	}
	e := wantKind(t, errOf(h.link(fresh, "0-1", "5-1")), KindConflict)
	if !strings.Contains(e.Message, "already linked") {
		t.Errorf("relink message %q", e.Message)
	}
}

func TestLinkRepoIsOwnerScoped(t *testing.T) {
	h := newLinkHarness(t)
	foreignRepo := h.q.addRepo(uuid.New(), true)
	_ = wantKind(t, errOf(h.link(foreignRepo, "0-1", "5-1")), KindNotFound)

	repo := h.q.addRepo(h.user, true)
	_, err := h.svc.LinkRepo(context.Background(), uuid.New(), repo, h.conn, "0-1", "5-1")
	_ = wantKind(t, err, KindNotFound)
	_, err = h.svc.LinkRepo(context.Background(), h.user, repo, uuid.New(), "0-1", "5-1")
	_ = wantKind(t, err, KindNotFound)
	if h.q.repo(repo).linkedConn != uuid.Nil {
		t.Error("the repo was linked")
	}
}

func TestLinkRepoRefusesARepoThatStoresIssueNumbers(t *testing.T) {
	h := newLinkHarness(t)
	repo := h.q.addRepo(h.user, true)
	h.q.repos[repo].storesNumbers = true
	_ = wantKind(t, errOf(h.link(repo, "0-1", "5-1")), KindConflict)
	if h.q.repo(repo).linkedConn != uuid.Nil {
		t.Error("the repo was linked")
	}
}

func TestLinkRepoTrackerFailuresAreBadGateway(t *testing.T) {
	cases := map[string]func(*linkHarness){
		"verify":   func(h *linkHarness) { h.template.verifyErr = context.DeadlineExceeded },
		"projects": func(h *linkHarness) { h.template.projectErr = context.DeadlineExceeded },
		"tags":     func(h *linkHarness) { h.template.tagErr = context.DeadlineExceeded },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newLinkHarness(t)
			mutate(h)
			_ = wantKind(t, errOf(h.link(h.q.addRepo(h.user, true), "0-1", "5-1")), KindBadGateway)
		})
	}
}

func TestReconnectRuleWhenARepoIsLinked(t *testing.T) {
	ctx := context.Background()
	h := newLinkHarness(t)
	repo := h.q.addRepo(h.user, true)
	if _, err := h.link(repo, "0-1", "5-1"); err != nil {
		t.Fatal(err)
	}
	reconnect := func() (Connection, error) {
		return h.svc.Connect(ctx, h.user, "youtrack", testHost, testToken("rotated"))
	}

	h.template.account = linkOther
	e := wantKind(t, errOf(reconnect()), KindConflict)
	if !strings.Contains(e.Message, "different account") {
		t.Errorf("message %q", e.Message)
	}
	if got := h.q.rows[0]; got.AccountID != linkBot.ID {
		t.Fatalf("the refused reconnect changed the stored account to %q", got.AccountID)
	}

	// Token rotation for the same account stays allowed, even with a new login.
	h.template.account = issuetracker.Account{ID: linkBot.ID, Login: "bot-renamed"}
	c, err := reconnect()
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != h.conn || c.AccountLogin != "bot-renamed" {
		t.Fatalf("rotated connection = %+v", c)
	}
}

func TestReconnectWithAnotherAccountIsAllowedWhenNothingIsLinked(t *testing.T) {
	h := newLinkHarness(t)
	h.template.account = linkOther
	c, err := h.svc.Connect(context.Background(), h.user, "youtrack", testHost, testToken("rotated"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != h.conn || c.AccountID != linkOther.ID {
		t.Fatalf("connection = %+v", c)
	}
}

func TestDeleteRefusedWhileARepoIsLinked(t *testing.T) {
	ctx := context.Background()
	h := newLinkHarness(t)
	repo := h.q.addRepo(h.user, true)
	if _, err := h.link(repo, "0-1", "5-1"); err != nil {
		t.Fatal(err)
	}
	_ = wantKind(t, h.svc.Delete(ctx, h.user, h.conn), KindConflict)
	if len(h.q.rows) != 1 {
		t.Fatal("the connection was deleted")
	}
	// A foreign id stays NotFound and never reveals the link count.
	_ = wantKind(t, h.svc.Delete(ctx, uuid.New(), h.conn), KindNotFound)

	// A repo linked between the count and the delete: the foreign key says no.
	other := newLinkHarness(t)
	other.q.deleteErr = &pgconn.PgError{Code: "23503"}
	_ = wantKind(t, other.svc.Delete(ctx, other.user, other.conn), KindConflict)

	// Unlinked, it is deletable.
	free := newLinkHarness(t)
	if err := free.svc.Delete(ctx, free.user, free.conn); err != nil {
		t.Fatal(err)
	}
}

func TestListTagsWithVerdicts(t *testing.T) {
	h := newLinkHarness(t)
	h.template.tags = append(h.template.tags,
		issuetracker.Tag{ID: "5-2", Name: "mine", Owner: linkBot},
		issuetracker.Tag{ID: "5-3", Name: "theirs", Owner: linkOther})
	views, err := h.svc.ListTagsWithVerdicts(context.Background(), h.user, h.conn)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]bool{"uzi-ready": {true, true}, "mine": {true, false}, "theirs": {false, true}}
	if len(views) != len(want) {
		t.Fatalf("got %d views", len(views))
	}
	for _, v := range views {
		if w := want[v.Tag.Name]; v.AccountCanApply != w[0] || v.OthersCanApply != w[1] {
			t.Errorf("%s: verdicts = %v/%v, want %v", v.Tag.Name, v.AccountCanApply, v.OthersCanApply, w)
		}
	}
	_, err = h.svc.ListTagsWithVerdicts(context.Background(), uuid.New(), h.conn)
	_ = wantKind(t, err, KindNotFound)
}

// errOf drops a value and keeps the error, so a refusal reads as one expression.
func errOf[T any](_ T, err error) error { return err }

// The store caps both ids at 128 characters; a longer one is a bad request, not
// a check violation surfacing as an internal error.
func TestLinkRepoRefusesIDsLongerThanTheStoreHolds(t *testing.T) {
	over := "1-" + strings.Repeat("9", 127) // 129 characters
	t.Run("project id", func(t *testing.T) {
		h := newLinkHarness(t)
		h.template.projects = append(h.template.projects, issuetracker.Project{ID: over, ShortName: "LONG", Name: "Long"})
		repo := h.q.addRepo(h.user, true)
		_ = wantKind(t, errOf(h.link(repo, over, "5-1")), KindBadRequest)
		if h.q.repo(repo).linkedConn != uuid.Nil {
			t.Error("the repo was linked")
		}
	})
	t.Run("tag id", func(t *testing.T) {
		h := newLinkHarness(t)
		h.template.tags = append(h.template.tags, issuetracker.Tag{ID: over, Name: "long", Owner: linkBot,
			Use: issuetracker.SharingSettings{Groups: []issuetracker.Group{linkAll}}})
		repo := h.q.addRepo(h.user, true)
		_ = wantKind(t, errOf(h.link(repo, "0-1", over)), KindBadRequest)
		if h.q.repo(repo).linkedConn != uuid.Nil {
			t.Error("the repo was linked")
		}
	})
}
