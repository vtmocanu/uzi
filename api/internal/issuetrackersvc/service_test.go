package issuetrackersvc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/issuetrackertest"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const testHost = "https://issues.example.com"

// fakeQuerier is an in-memory Querier keyed like the real table.
type fakeQuerier struct {
	mu   sync.Mutex
	rows []store.IssueTrackerConnection
}

func (f *fakeQuerier) UpsertIssueTrackerConnection(_ context.Context, a store.UpsertIssueTrackerConnectionParams) (store.IssueTrackerConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	for i, r := range f.rows {
		if r.UserID == a.UserID && r.Kind == a.Kind && r.BaseUrl == a.BaseUrl {
			f.rows[i].AccountID, f.rows[i].AccountLogin = a.AccountID, a.AccountLogin
			f.rows[i].TokenCiphertext, f.rows[i].LastVerifiedAt = a.TokenCiphertext, now
			return f.rows[i], nil
		}
	}
	r := store.IssueTrackerConnection{
		ID: uuid.New(), UserID: a.UserID, Kind: a.Kind, BaseUrl: a.BaseUrl,
		AccountID: a.AccountID, AccountLogin: a.AccountLogin, TokenCiphertext: a.TokenCiphertext,
		CreatedAt: now, LastVerifiedAt: now,
	}
	f.rows = append(f.rows, r)
	return r, nil
}

func (f *fakeQuerier) ListIssueTrackerConnectionsByUser(_ context.Context, u uuid.UUID) ([]store.IssueTrackerConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.IssueTrackerConnection
	for _, r := range f.rows {
		if r.UserID == u {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeQuerier) GetIssueTrackerConnectionForUser(_ context.Context, a store.GetIssueTrackerConnectionForUserParams) (store.IssueTrackerConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ID == a.ID && r.UserID == a.UserID {
			return r, nil
		}
	}
	return store.IssueTrackerConnection{}, pgx.ErrNoRows
}

func (f *fakeQuerier) DeleteIssueTrackerConnectionForUser(_ context.Context, a store.DeleteIssueTrackerConnectionForUserParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.rows {
		if r.ID == a.ID && r.UserID == a.UserID {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return 1, nil
		}
	}
	return 0, nil
}

// fakeDriver embeds BaseFake and overrides only what the service calls.
type fakeDriver struct {
	issuetrackertest.BaseFake
	opts       issuetracker.Options
	account    issuetracker.Account
	verifyErr  error
	projects   []issuetracker.Project
	projectErr error
	tags       []issuetracker.Tag
	tagErr     error
}

func (d *fakeDriver) ListTags(context.Context) ([]issuetracker.Tag, error) {
	return d.tags, d.tagErr
}

func (d *fakeDriver) VerifyToken(context.Context) (issuetracker.Account, error) {
	return d.account, d.verifyErr
}

func (d *fakeDriver) ListProjects(context.Context) ([]issuetracker.Project, error) {
	return d.projects, d.projectErr
}

// harness wires a Service to the fakes and records every driver build.
type harness struct {
	svc      *Service
	q        *fakeQuerier
	box      *secretbox.Box
	allow    map[string]bool
	built    []issuetracker.Options
	template fakeDriver
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i*7 + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{q: &fakeQuerier{}, box: box, allow: map[string]bool{testHost: true}}
	h.template.account = issuetracker.Account{ID: "1-1", Login: "bot"}
	h.svc = New(h.q, box, func(u string) bool { return h.allow[u] }, func(_ issuetracker.Kind, o issuetracker.Options) (issuetracker.Driver, error) {
		h.built = append(h.built, o)
		d := h.template
		d.opts = o
		return &d, nil
	})
	h.svc.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return h
}

// testToken assembles a token-shaped value at runtime.
// The fixed tail keeps it above the driver's minimum token length.
func testToken(parts ...string) string {
	return strings.Join(append([]string{"perm"}, parts...), "-") + "-0123456789abcdef"
}

// testMaxTokenLen is the longest token the service accepts.
const testMaxTokenLen = 4096

func wantKind(t *testing.T, err error, want Kind) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Kind != want {
		t.Fatalf("err = %v, want *Error of kind %d", err, want)
	}
	return e
}

func TestConnectRefusesWhenAllowlistEmpty(t *testing.T) {
	h := newHarness(t)
	h.allow = map[string]bool{}
	_, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", testHost, testToken("a", "b"))
	_ = wantKind(t, err, KindBadRequest)
	if len(h.built) != 0 {
		t.Fatalf("driver factory called %d times for a refused host", len(h.built))
	}
	if len(h.q.rows) != 0 {
		t.Fatal("a row was stored for a refused host")
	}
}

func TestConnectRefusesBadInput(t *testing.T) {
	validToken := testToken("v")
	cases := []struct{ name, kind, url, token string }{
		{"unknown kind", "jira", testHost, validToken},
		{"empty kind", "", testHost, validToken},
		{"empty token", "youtrack", testHost, "  "},
		{"http", "youtrack", "http://issues.example.com", validToken},
		{"path", "youtrack", testHost + "/youtrack", validToken},
		{"userinfo", "youtrack", "https://issues.example.com@evil.example.com", validToken},
		{"userinfo with password", "youtrack", "https://u:p@issues.example.com", validToken},
		{"query", "youtrack", testHost + "?a=b", validToken},
		{"bare question mark", "youtrack", testHost + "?", validToken},
		{"fragment", "youtrack", testHost + "#x", validToken},
		{"bare hash", "youtrack", testHost + "#", validToken},
		{"escaped percent in host", "youtrack", "https://x%25y.example.com", validToken},
		{"empty hostname with port", "youtrack", "https://:443", validToken},
		{"no host", "youtrack", "https://", validToken},
		{"not allowlisted", "youtrack", "https://evil.example.com", validToken},
		{"explicit default port is a different origin string", "youtrack", testHost + ":443", validToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.svc.Connect(context.Background(), uuid.New(), tc.kind, tc.url, tc.token)
			_ = wantKind(t, err, KindBadRequest)
			if len(h.built) != 0 {
				t.Error("driver built for refused input")
			}
		})
	}
}

func TestConnectRefusesBadToken(t *testing.T) {
	cases := map[string]string{
		"8 MiB":          strings.Repeat("a", 8<<20),
		"just too long":  strings.Repeat("a", testMaxTokenLen+1),
		"embedded CRLF":  strings.Join([]string{"perm", "abc\r\nX-Evil: 1", "def"}, "-"),
		"zero width":     strings.Join([]string{"perm", "abc\u200bdef", "ghi"}, "-"),
		"bidi override":  strings.Join([]string{"perm", "abc\u202edef", "ghi"}, "-"),
		"embedded NUL":   strings.Join([]string{"perm", "abc\x00def", "ghi"}, "-"),
		"line separator": strings.Join([]string{"perm", "abc\u2028def", "ghi"}, "-"),
		"too short":      "tok",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", testHost, tok)
			e := wantKind(t, err, KindBadRequest)
			if !strings.HasPrefix(e.Message, "invalid token: ") {
				t.Errorf("message = %q, want the validator's reason after \"invalid token: \"", e.Message)
			}
			// The reason must never echo the token: no 8-byte window of it appears.
			for i := 0; i+8 <= len(tok) && i < 64; i++ {
				if strings.Contains(e.Message, tok[i:i+8]) {
					t.Errorf("message %q contains the token window %q", e.Message, tok[i:i+8])
					break
				}
			}
			if len(h.built) != 0 || len(h.q.rows) != 0 {
				t.Error("driver built or row stored for a refused token")
			}
		})
	}
	h := newHarness(t)
	if _, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", testHost, strings.Repeat("a", testMaxTokenLen)); err != nil {
		t.Errorf("a token of exactly the limit must pass: %v", err)
	}
}

func TestConnectNilAllowlistRefusesAll(t *testing.T) {
	built := 0
	s := New(&fakeQuerier{}, nil, nil, func(issuetracker.Kind, issuetracker.Options) (issuetracker.Driver, error) {
		built++
		return &fakeDriver{}, nil
	})
	_, err := s.Connect(context.Background(), uuid.New(), "youtrack", testHost, strings.Repeat("a", 32))
	e := wantKind(t, err, KindBadRequest)
	if !strings.Contains(e.Message, "allowed") || built != 0 {
		t.Errorf("message = %q, built = %d", e.Message, built)
	}
}

func TestConnectDriverBuildFailureIsBadRequest(t *testing.T) {
	h := newHarness(t)
	h.svc.newDriver = func(issuetracker.Kind, issuetracker.Options) (issuetracker.Driver, error) {
		return nil, errors.New("issuetracker: token shorter than the minimum length")
	}
	tok := testToken("s")
	_, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", testHost, tok)
	e := wantKind(t, err, KindBadRequest)
	if !strings.Contains(e.Message, "invalid token") || strings.Contains(e.Message, tok) {
		t.Errorf("message = %q", e.Message)
	}
}

func TestConnectNormalizesBeforeAllowlist(t *testing.T) {
	h := newHarness(t)
	c, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", " HTTPS://Issues.Example.com/ ", testToken("x"))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != testHost {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, testHost)
	}
	if len(h.built) != 1 || h.built[0].BaseURL != testHost {
		t.Errorf("driver built with %+v, want the normalized URL", h.built)
	}
}

func TestConnectVerifyFailureIsBadGatewayWithoutToken(t *testing.T) {
	h := newHarness(t)
	tok := testToken("secret", "value")
	h.template.verifyErr = errors.New("401 unauthorized")
	_, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", testHost, tok)
	e := wantKind(t, err, KindBadGateway)
	if strings.Contains(e.Message, tok) || !strings.HasPrefix(e.Message, "token verification failed: ") {
		t.Errorf("message = %q", e.Message)
	}
	if len(h.q.rows) != 0 {
		t.Error("a row was stored after a failed verification")
	}
}

func TestConnectRefusesBadAccount(t *testing.T) {
	bad := map[string]issuetracker.Account{
		"empty id":        {ID: "", Login: "bot"},
		"empty login":     {ID: "1", Login: ""},
		"control in id":   {ID: "1\x00", Login: "bot"},
		"newline login":   {ID: "1", Login: "bo\nt"},
		"bidi override":   {ID: "1", Login: "bot\u202e"},
		"zero width":      {ID: "1", Login: "bo\u200bt"},
		"line separator":  {ID: "1", Login: "bo\u2028t"},
		"para separator":  {ID: "1", Login: "bo\u2029t"},
		"login too long":  {ID: "1", Login: strings.Repeat("a", 256)},
		"id too long":     {ID: strings.Repeat("1", 256), Login: "bot"},
		"control in both": {ID: "\x07", Login: "\x07"},
	}
	for name, acct := range bad {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.template.account = acct
			_, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", testHost, testToken("t"))
			_ = wantKind(t, err, KindBadGateway)
			if len(h.q.rows) != 0 {
				t.Error("row stored for a bad account")
			}
		})
	}
	h := newHarness(t)
	h.template.account = issuetracker.Account{ID: strings.Repeat("1", 255), Login: "Zoë"}
	if _, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", testHost, testToken("t")); err != nil {
		t.Errorf("a 255-byte id and a non-ASCII login must pass: %v", err)
	}
}

func TestConnectStoresSealedTokenBoundToIdentity(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	tok := testToken("alpha", "beta")
	c, err := h.svc.Connect(context.Background(), user, "youtrack", testHost, tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserID != user || c.Kind != "youtrack" || c.AccountID != "1-1" || c.AccountLogin != "bot" {
		t.Errorf("connection = %+v", c)
	}
	row := h.q.rows[0]
	if strings.Contains(string(row.TokenCiphertext), tok) || len(row.TokenCiphertext) == 0 {
		t.Fatal("ciphertext is empty or holds the token in the clear")
	}
	if h.built[0].Token != tok {
		t.Error("the driver was not given the token")
	}

	got, err := openToken(h.box, user, issuetracker.KindYouTrack, testHost, row.TokenCiphertext)
	if err != nil || got != tok {
		t.Fatalf("open with the right AAD = %q, %v", got, err)
	}
	if _, err := openToken(h.box, uuid.New(), issuetracker.KindYouTrack, testHost, row.TokenCiphertext); err == nil {
		t.Error("opened with another user's AAD")
	}
	if _, err := openToken(h.box, user, issuetracker.KindYouTrack, "https://other.example.com", row.TokenCiphertext); err == nil {
		t.Error("opened with another base URL's AAD")
	}
	if _, err := openToken(h.box, user, issuetracker.Kind("other"), testHost, row.TokenCiphertext); err == nil {
		t.Error("opened with another kind's AAD")
	}
}

func TestConnectRotatesInPlace(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	a, err := h.svc.Connect(context.Background(), user, "youtrack", testHost, testToken("one"))
	if err != nil {
		t.Fatal(err)
	}
	first := string(h.q.rows[0].TokenCiphertext)
	b, err := h.svc.Connect(context.Background(), user, "youtrack", testHost, testToken("two"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || len(h.q.rows) != 1 || string(h.q.rows[0].TokenCiphertext) == first {
		t.Error("reconnect must rotate the single row in place")
	}
}

func TestConnectionHasNoSecretField(t *testing.T) {
	typ := reflect.TypeOf(Connection{})
	for i := 0; i < typ.NumField(); i++ {
		n := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(n, "token") || strings.Contains(n, "cipher") {
			t.Errorf("Connection field %s could carry the secret", typ.Field(i).Name)
		}
	}
}

func TestReadsAreOwnerScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner, other := uuid.New(), uuid.New()
	c, err := h.svc.Connect(ctx, owner, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.svc.Get(ctx, owner, c.ID); err != nil || got.ID != c.ID {
		t.Fatalf("owner get = %v, %v", got, err)
	}
	_, err = h.svc.Get(ctx, other, c.ID)
	_ = wantKind(t, err, KindNotFound)
	if list, err := h.svc.List(ctx, other); err != nil || len(list) != 0 {
		t.Errorf("foreign list = %v, %v", list, err)
	}
	if list, err := h.svc.List(ctx, owner); err != nil || len(list) != 1 {
		t.Errorf("owner list = %v, %v", list, err)
	}
	_ = wantKind(t, h.svc.Delete(ctx, other, c.ID), KindNotFound)
	if _, err := h.svc.Get(ctx, owner, c.ID); err != nil {
		t.Fatalf("foreign delete removed the row: %v", err)
	}
	if err := h.svc.Delete(ctx, owner, c.ID); err != nil {
		t.Fatal(err)
	}
	_ = wantKind(t, h.svc.Delete(ctx, owner, c.ID), KindNotFound)
}

func TestDriverForOpensTokenAndPassesOptions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	tok := testToken("open", "me")
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, tok)
	if err != nil {
		t.Fatal(err)
	}
	h.built = nil
	binding := issuetracker.Binding{
		Project:        issuetracker.ProjectRef{ID: "0-1", ShortName: "UZI"},
		ProjectID:      4173,
		EligibilityTag: issuetracker.TagRef{ID: "5-3", Name: "uzi"},
	}
	if _, err := h.svc.DriverFor(ctx, user, c.ID, binding, "State", "Open", "Done"); err != nil {
		t.Fatal(err)
	}
	want := issuetracker.Options{
		BaseURL: testHost, Token: tok, Project: binding.Project, ProjectID: 4173, EligibilityTag: binding.EligibilityTag,
		StateField: "State", OpenState: "Open", ClosedState: "Done",
	}
	if len(h.built) != 1 || h.built[0] != want {
		t.Errorf("built = %+v, want %+v", h.built, want)
	}
	_, err = h.svc.DriverFor(ctx, uuid.New(), c.ID, binding, "", "", "")
	_ = wantKind(t, err, KindNotFound)
}

func TestDriverForFailsClosedWhenAllowlistEmptied(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.built = nil
	h.allow = map[string]bool{}
	_, err = h.svc.DriverFor(ctx, user, c.ID, issuetracker.Binding{}, "", "", "")
	_ = wantKind(t, err, KindBadRequest)
	if len(h.built) != 0 {
		t.Error("driver built after the allowlist was emptied")
	}
	// Discovery goes through the same gate.
	_, err = h.svc.ListProjects(ctx, user, c.ID)
	_ = wantKind(t, err, KindBadRequest)
}

func TestDriverForRefusesTamperedRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.built = nil
	// A row whose base URL was rewritten no longer opens: the AAD differs.
	h.allow["https://evil.example.com"] = true
	h.q.rows[0].BaseUrl = "https://evil.example.com"
	_, err = h.svc.DriverFor(ctx, user, c.ID, issuetracker.Binding{}, "", "", "")
	e := wantKind(t, err, KindInternal)
	if e.Message != "internal error" || len(h.built) != 0 {
		t.Errorf("message = %q, built = %d", e.Message, len(h.built))
	}
	// A row with a non-strict URL is refused before opening.
	h.allow["https://evil.example.com/x"] = true
	h.q.rows[0].BaseUrl = "https://evil.example.com/x"
	_, err = h.svc.DriverFor(ctx, user, c.ID, issuetracker.Binding{}, "", "", "")
	_ = wantKind(t, err, KindBadRequest)
}

func TestDriverForRefusesNonCanonicalStoredURL(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.built = nil
	// Strict-valid and allowlisted, but not the canonical stored form.
	h.allow["https://ISSUES.example.com"] = true
	h.q.rows[0].BaseUrl = "https://ISSUES.example.com"
	_, err = h.svc.DriverFor(ctx, user, c.ID, issuetracker.Binding{}, "", "", "")
	_ = wantKind(t, err, KindBadRequest)
	if len(h.built) != 0 {
		t.Error("driver built for a non-canonical stored URL")
	}
}

func TestDriverForRefusesPartialOrMalformedBinding(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.built = nil
	good := issuetracker.Binding{
		Project:        issuetracker.ProjectRef{ID: "0-1", ShortName: "UZI"},
		ProjectID:      7,
		EligibilityTag: issuetracker.TagRef{ID: "5-3", Name: "uzi"},
	}
	mutate := func(f func(*issuetracker.Binding)) issuetracker.Binding { b := good; f(&b); return b }
	for name, b := range map[string]issuetracker.Binding{
		"project id only":     {ProjectID: 7},
		"eligibility only":    {EligibilityTag: good.EligibilityTag},
		"project only":        {Project: good.Project},
		"no project id":       mutate(func(b *issuetracker.Binding) { b.ProjectID = 0 }),
		"no eligibility":      mutate(func(b *issuetracker.Binding) { b.EligibilityTag = issuetracker.TagRef{} }),
		"no project":          mutate(func(b *issuetracker.Binding) { b.Project = issuetracker.ProjectRef{} }),
		"negative project id": mutate(func(b *issuetracker.Binding) { b.ProjectID = -1 }),
		"half project":        mutate(func(b *issuetracker.Binding) { b.Project.ID = "" }),
		"bad project id":      mutate(func(b *issuetracker.Binding) { b.Project.ID = "x" }),
		"bad short name":      mutate(func(b *issuetracker.Binding) { b.Project.ShortName = "1bad" }),
		"bad tag id":          mutate(func(b *issuetracker.Binding) { b.EligibilityTag.ID = "x" }),
		"bad tag name":        mutate(func(b *issuetracker.Binding) { b.EligibilityTag.Name = "a}b" }),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.DriverFor(ctx, user, c.ID, b, "", "", "")
			_ = wantKind(t, err, KindBadRequest)
		})
	}
	if len(h.built) != 0 {
		t.Error("driver built for a partial or malformed binding")
	}
}

func TestListStatesUsesEmptyProjectRefAndValidatesID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.built = nil
	// The double's ListStates is unstubbed and errors; only the build matters here.
	_, _ = h.svc.ListStates(ctx, user, c.ID, "0-1")
	if len(h.built) != 1 || h.built[0].Project != (issuetracker.ProjectRef{}) {
		t.Errorf("built = %+v, want one build with an empty Project", h.built)
	}
	h.built = nil
	for _, bad := range []string{"", "abc", "0-1/../x", "1-", "0-1\n"} {
		_, err = h.svc.ListStates(ctx, user, c.ID, bad)
		_ = wantKind(t, err, KindBadRequest)
	}
	if len(h.built) != 0 {
		t.Error("driver built for a bad project id")
	}
}

func TestListProjectsAndStates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.template.projects = []issuetracker.Project{{ID: "0-1", ShortName: "UZI", Name: "Uzi"}}
	got, err := h.svc.ListProjects(ctx, user, c.ID)
	if err != nil || len(got) != 1 || got[0].ShortName != "UZI" {
		t.Fatalf("ListProjects = %v, %v", got, err)
	}
	h.template.projectErr = errors.New("boom")
	_, err = h.svc.ListProjects(ctx, user, c.ID)
	_ = wantKind(t, err, KindBadGateway)
	// ListStates is not stubbed on the double: its error surfaces as a bad gateway, not a panic.
	_, err = h.svc.ListStates(ctx, user, c.ID, "0-1")
	_ = wantKind(t, err, KindBadGateway)
	_, err = h.svc.ListStates(ctx, uuid.New(), c.ID, "0-1")
	_ = wantKind(t, err, KindNotFound)
}

func TestListTags(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()
	c, err := h.svc.Connect(ctx, user, "youtrack", testHost, testToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.built = nil
	h.template.tags = []issuetracker.Tag{{ID: "5-1", Name: "uzi", Owner: issuetracker.Account{ID: "1-1", Login: "bot"}}}
	got, err := h.svc.ListTags(ctx, user, c.ID)
	if err != nil || len(got) != 1 || got[0].Name != "uzi" {
		t.Fatalf("ListTags = %v, %v", got, err)
	}
	if len(h.built) != 1 || h.built[0].Project != (issuetracker.ProjectRef{}) || h.built[0].ProjectID != 0 || h.built[0].EligibilityTag != (issuetracker.TagRef{}) {
		t.Errorf("ListTags built %+v, want one unbound driver", h.built)
	}
	h.template.tagErr = errors.New("boom")
	_, err = h.svc.ListTags(ctx, user, c.ID)
	_ = wantKind(t, err, KindBadGateway)
	_, err = h.svc.ListTags(ctx, uuid.New(), c.ID)
	_ = wantKind(t, err, KindNotFound)
}

func TestNewDefaultsToRegistry(t *testing.T) {
	s := New(&fakeQuerier{}, nil, nil, nil)
	if s.newDriver == nil {
		t.Fatal("nil newDriver must default to issuetracker.New")
	}
	if _, err := s.Connect(context.Background(), uuid.New(), "youtrack", testHost, "tok"); err == nil {
		t.Fatal("a nil allowlist must refuse")
	}
}

// TestConnectRefusesAnInvisibleHostCharacterBeforeTheAllowlist uses an
// allowlist that accepts anything, so only the base URL check can refuse.
func TestConnectRefusesAnInvisibleHostCharacterBeforeTheAllowlist(t *testing.T) {
	for name, u := range map[string]string{
		"zero width":     "https://issues.exam\u200bple.com",
		"bidi override":  "https://issues.example.com\u202e",
		"line separator": "https://issues.exam\u2028ple.com",
		"hangul filler":  "https://issues.exam\u3164ple.com",
		"unicode idn":    "https://issues.exampl\u00e9.com",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			asked := 0
			h.svc.allowed = func(string) bool { asked++; return true }
			_, err := h.svc.Connect(context.Background(), uuid.New(), "youtrack", u, testToken("v"))
			_ = wantKind(t, err, KindBadRequest)
			if asked != 0 || len(h.built) != 0 {
				t.Errorf("allowlist asked %d times, driver built %d times", asked, len(h.built))
			}
		})
	}
}
