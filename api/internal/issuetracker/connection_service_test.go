package issuetracker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// memQuerier is a minimal in-memory issuetrackersvc.Querier: only the upsert
// Connect uses is real, the rest are unreachable from these tests.
type memQuerier struct {
	mu   sync.Mutex
	rows []store.IssueTrackerConnection
}

func (m *memQuerier) UpsertIssueTrackerConnection(_ context.Context, a store.UpsertIssueTrackerConnectionParams) (store.IssueTrackerConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := store.IssueTrackerConnection{
		ID: uuid.New(), UserID: a.UserID, Kind: a.Kind, BaseUrl: a.BaseUrl,
		AccountID: a.AccountID, AccountLogin: a.AccountLogin, TokenCiphertext: a.TokenCiphertext,
	}
	m.rows = append(m.rows, r)
	return r, nil
}

func (m *memQuerier) ListIssueTrackerConnectionsByUser(context.Context, uuid.UUID) ([]store.IssueTrackerConnection, error) {
	return nil, errors.New("not used")
}

func (m *memQuerier) GetIssueTrackerConnectionForUser(context.Context, store.GetIssueTrackerConnectionForUserParams) (store.IssueTrackerConnection, error) {
	return store.IssueTrackerConnection{}, errors.New("not used")
}

func (m *memQuerier) DeleteIssueTrackerConnectionForUser(context.Context, store.DeleteIssueTrackerConnectionForUserParams) (int64, error) {
	return 0, errors.New("not used")
}

func (m *memQuerier) stored() []store.IssueTrackerConnection {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.IssueTrackerConnection(nil), m.rows...)
}

type svcHarness struct {
	svc    *issuetrackersvc.Service
	q      *memQuerier
	builds *atomic.Int64
}

// newSvcHarness wires a Service whose driver factory builds the real YouTrack
// driver over base, counting factory calls.
func newSvcHarness(t *testing.T, base http.RoundTripper, allowed func(string) bool) svcHarness {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i*5 + 3)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	q := &memQuerier{}
	builds := &atomic.Int64{}
	svc := issuetrackersvc.New(q, box, allowed, func(_ issuetracker.Kind, o issuetracker.Options) (issuetracker.Driver, error) {
		builds.Add(1)
		return issuetracker.NewYouTrackOver(o, base)
	})
	return svcHarness{svc: svc, q: q, builds: builds}
}

func allowOnly(u string) func(string) bool {
	return func(got string) bool { return got == u }
}

// assertSvcScrubbed checks that err is a scrubbed *issuetrackersvc.Error of the
// given kind and returns it.
func assertSvcScrubbed(t *testing.T, err error, kind issuetrackersvc.Kind) {
	t.Helper()
	var se *issuetrackersvc.Error
	if !errors.As(err, &se) {
		t.Fatalf("want *issuetrackersvc.Error, got %T: %v", err, err)
	}
	if se.Kind != kind {
		t.Errorf("Kind = %d, want %d", se.Kind, kind)
	}
	for _, f := range issuetracker.TokenForms() {
		if strings.Contains(se.Message, f) {
			t.Errorf("message contains token form %q: %q", f, se.Message)
		}
	}
	for _, r := range se.Message {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("message contains control or format character %U: %q", r, se.Message)
			break
		}
	}
}

func TestConnectYouTrackHappyPath(t *testing.T) {
	srv := youtracktest.NewServer(t)
	h := newSvcHarness(t, srv.Client().Transport, allowOnly(srv.URL))

	conn, err := h.svc.Connect(context.Background(), uuid.New(), string(issuetracker.KindYouTrack), srv.URL, srv.Token)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.AccountLogin != youtracktest.BotLogin {
		t.Errorf("AccountLogin = %q, want %q", conn.AccountLogin, youtracktest.BotLogin)
	}
	rows := h.q.stored()
	if len(rows) != 1 {
		t.Fatalf("stored %d rows, want 1", len(rows))
	}
	if len(rows[0].TokenCiphertext) == 0 || bytes.Contains(rows[0].TokenCiphertext, []byte(srv.Token)) {
		t.Error("ciphertext is empty or contains the token")
	}
}

func TestConnectTokenEchoIsScrubbed(t *testing.T) {
	c := issuetracker.Credential()
	j, _ := json.Marshal(c)
	echo := strings.Join([]string{
		c, url.QueryEscape(c), string(j[1 : len(j)-1]),
	}, " | ") + "\r\n\x1b[31m\u202e"
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(echo))
	}))
	t.Cleanup(srv.Close)
	h := newSvcHarness(t, srv.Client().Transport, allowOnly(srv.URL))

	_, err := h.svc.Connect(context.Background(), uuid.New(), string(issuetracker.KindYouTrack), srv.URL, c)
	if err == nil {
		t.Fatal("want an error")
	}
	assertSvcScrubbed(t, err, issuetrackersvc.KindBadGateway)
	if n := hits.Load(); n != 1 {
		t.Errorf("issue tracker got %d requests, want exactly the verification call", n)
	}
	if n := len(h.q.stored()); n != 0 {
		t.Errorf("stored %d rows, want 0", n)
	}
}

func TestConnectRefusesRedirect(t *testing.T) {
	var hits, primary atomic.Int64
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(other.Close)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primary.Add(1)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound) //nolint:gosec // G710: test redirect to a second httptest server, the redirect-refusal behaviour under test
	}))
	t.Cleanup(srv.Close)
	h := newSvcHarness(t, srv.Client().Transport, allowOnly(srv.URL))

	_, err := h.svc.Connect(context.Background(), uuid.New(), string(issuetracker.KindYouTrack), srv.URL, issuetracker.Credential())
	if err == nil {
		t.Fatal("want an error")
	}
	assertSvcScrubbed(t, err, issuetrackersvc.KindBadGateway)
	if n := primary.Load(); n != 1 {
		t.Errorf("primary server got %d requests, want exactly one", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("redirect target got %d hits, want 0", n)
	}
	if n := len(h.q.stored()); n != 0 {
		t.Errorf("stored %d rows, want 0", n)
	}
}

func TestConnectEmptyAllowlistRefusesBeforeDriver(t *testing.T) {
	srv := youtracktest.NewServer(t)
	// The real predicate with no allowlist configured.
	h := newSvcHarness(t, srv.Client().Transport, config.Config{}.IssueTrackerBaseURLAllowed)

	_, err := h.svc.Connect(context.Background(), uuid.New(), string(issuetracker.KindYouTrack), srv.URL, srv.Token)
	var se *issuetrackersvc.Error
	if !errors.As(err, &se) || se.Kind != issuetrackersvc.KindBadRequest {
		t.Fatalf("want a bad-request *issuetrackersvc.Error, got %v", err)
	}
	if n := h.builds.Load(); n != 0 {
		t.Errorf("driver factory called %d times, want 0", n)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Errorf("server got %d requests, want 0", n)
	}
	if n := len(h.q.stored()); n != 0 {
		t.Errorf("stored %d rows, want 0", n)
	}
}
