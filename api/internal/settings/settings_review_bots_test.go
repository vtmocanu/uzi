package settings

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The trusted review-bot allowlist (issue #2347): "<base_url>#<forge_user_id>" entries.

func TestMrReviewTrustedBotsKnownWithEmptyDefault(t *testing.T) {
	if !Known(KeyMrReviewTrustedBots) {
		t.Fatalf("%s must be a known (admin-writable) key", KeyMrReviewTrustedBots)
	}
	if got, ok := Defaults[KeyMrReviewTrustedBots]; !ok || got != "" {
		t.Fatalf("default = %q (present %t), want the empty allowlist", got, ok)
	}
	if err := Validate(KeyMrReviewTrustedBots, ""); err != nil {
		t.Fatalf("the empty value must be valid: %v", err)
	}
}

func TestValidateTrustedBots(t *testing.T) {
	var fifty, fiftyOne []string
	for i := 1; i <= 51; i++ {
		e := fmt.Sprintf("https://github.com#%d", i)
		if i <= 50 {
			fifty = append(fifty, e)
		}
		fiftyOne = append(fiftyOne, e)
	}
	ok := []string{
		"",
		"https://github.com#136622811",
		"https://github.com#1,https://gitlab.example.com#2",
		" https://github.com#1 , https://github.com#2 ",
		"https://api.github.com#5,https://github.com#5", // distinct spellings of one instance are distinct entries
		"https://forge.example.com:8443#9",
		strings.Join(fifty, ","),
		"https://github.com#1,", // a trailing separator is an empty token
	}
	for _, v := range ok {
		if err := Validate(KeyMrReviewTrustedBots, v); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", v, err)
		}
	}
	bad := map[string]string{
		"no id":               "https://github.com",
		"no base":             "#5",
		"http":                "http://github.com#5",
		"not normalized path": "https://github.com/#5",
		"not normalized case": "https://GitHub.com#5",
		"deeper path":         "https://github.com/foo#5",
		"id zero":             "https://github.com#0",
		"id negative":         "https://github.com#-4",
		"id text":             "https://github.com#bot",
		"id with sign":        "https://github.com#+4",
		"id leading zero":     "https://github.com#007",
		"id overflow":         "https://github.com#99999999999999999999",
		"duplicate":           "https://github.com#5,https://github.com#5",
		"too many":            strings.Join(fiftyOne, ","),
		"login not an id":     "https://github.com#coderabbitai[bot]",
	}
	for name, v := range bad {
		if err := Validate(KeyMrReviewTrustedBots, v); err == nil {
			t.Errorf("%s: Validate(%q) = nil, want a rejection", name, v)
		}
	}
}

func TestMrReviewTrustedBotsAccessor(t *testing.T) {
	ctx := context.Background()
	c := New(&fakeStore{}, time.Minute)
	got, err := c.MrReviewTrustedBots(ctx)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("absent row = %v, %v, want a non-nil empty slice", got, err)
	}

	c = New(&fakeStore{rows: []store.AppSetting{row(KeyMrReviewTrustedBots, "https://github.com#136622811, https://gitlab.example.com#7,garbage,https://github.com#136622811")}}, time.Minute)
	got, err = c.MrReviewTrustedBots(ctx)
	want := []TrustedBot{{"https://github.com", 136622811}, {"https://gitlab.example.com", 7}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %v, %v, want %v (junk and duplicates skipped)", got, err, want)
	}

	// A store error is PROPAGATED, never read as "no bots": the callers fail closed on it.
	boom := errors.New("app_settings unavailable")
	c = New(&fakeStore{err: boom}, time.Minute)
	if _, err := c.MrReviewTrustedBots(ctx); !errors.Is(err, boom) {
		t.Fatalf("store error = %v, want it propagated", err)
	}
}

func TestTrustedBotMatches(t *testing.T) {
	bots := []TrustedBot{{"https://github.com", 136622811}, {"https://gitlab.example.com", 7}}
	cases := []struct {
		name string
		base string
		id   int64
		want bool
	}{
		{"same instance and id", "https://github.com", 136622811, true},
		{"connection URL spelling is normalized", "https://GitHub.com/", 136622811, true},
		{"api.github.com is the github.com instance", "https://api.github.com", 136622811, true},
		{"same id on another instance", "https://gitlab.example.com", 136622811, false},
		{"same id on a lookalike host", "https://github.com.evil.test", 136622811, false},
		{"other id on the right instance", "https://github.com", 136622812, false},
		{"second entry matches its own instance", "https://gitlab.example.com", 7, true},
		{"unresolvable id", "https://github.com", 0, false},
		{"negative id", "https://github.com", -1, false},
		{"base that does not normalize", "ftp://github.com", 136622811, false},
		{"empty base", "", 136622811, false},
	}
	for _, tc := range cases {
		if got := TrustedBotMatches(bots, tc.base, tc.id); got != tc.want {
			t.Errorf("%s: TrustedBotMatches(%q, %d) = %t, want %t", tc.name, tc.base, tc.id, got, tc.want)
		}
	}
	// The api.github.com spelling works on the entry side too.
	apiEntry := []TrustedBot{{"https://api.github.com", 5}}
	if !TrustedBotMatches(apiEntry, "https://github.com", 5) {
		t.Error("an api.github.com entry must match a github.com connection")
	}
	if TrustedBotMatches(nil, "https://github.com", 5) {
		t.Error("an empty allowlist matches nothing")
	}
}

// A connection stored with the explicit https default port (config.NormalizeForgeBaseURL
// keeps it) must match an entry written without it, and vice versa; other ports are
// distinct instances.
func TestTrustedBotMatchesDefaultHTTPSPort(t *testing.T) {
	mk := func(entry string) []TrustedBot {
		bots, err := parseTrustedBots(entry)
		if err != nil {
			t.Fatalf("parseTrustedBots(%q): %v", entry, err)
		}
		return bots
	}
	cases := []struct {
		name  string
		entry string
		base  string
		id    int64
		want  bool
	}{
		{"entry without port, connection with :443", "https://gitlab.example.com#7", "https://gitlab.example.com:443", 7, true},
		{"entry with :443, connection without", "https://gitlab.example.com:443#7", "https://gitlab.example.com", 7, true},
		{"both with :443", "https://gitlab.example.com:443#7", "https://gitlab.example.com:443", 7, true},
		{"non-default port is not stripped", "https://gitlab.example.com#7", "https://gitlab.example.com:8443", 7, false},
		{"non-default port entry vs bare connection", "https://gitlab.example.com:8443#7", "https://gitlab.example.com", 7, false},
		{"other id does not match", "https://gitlab.example.com#7", "https://gitlab.example.com:443", 8, false},
		{"other instance does not match", "https://gitlab.example.com#7", "https://other.example.com:443", 7, false},
		{"api.github.com:443 maps to github.com", "https://github.com#7", "https://api.github.com:443", 7, true},
	}
	for _, tc := range cases {
		if got := TrustedBotMatches(mk(tc.entry), tc.base, tc.id); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
}

// trustedBotsRefreshStore answers its first fetch with one allowlisted bot, optionally
// blocking until release, and fails every later fetch.
type trustedBotsRefreshStore struct {
	started, release chan struct{}
	calls            int
}

func (s *trustedBotsRefreshStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	s.calls++
	if s.calls == 1 {
		if s.started != nil {
			close(s.started)
			<-s.release
		}
		return []store.AppSetting{{Key: KeyMrReviewTrustedBots, Value: "https://example.com#77"}}, nil
	}
	return nil, errors.New("settings read unavailable")
}

// A warm cache whose refresh fails must not keep serving the allowlist (#2347): the read is
// strict, so the caller fails closed instead of trusting a list it could not re-read.
func TestMrReviewTrustedBotsExpiredRefreshFailureIsAnError(t *testing.T) {
	s := &trustedBotsRefreshStore{}
	c := New(s, time.Minute)
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }

	bots, err := c.MrReviewTrustedBots(context.Background())
	if err != nil || len(bots) != 1 || bots[0].ForgeUserID != 77 {
		t.Fatalf("warm load = %v, %v; want the one allowlisted bot", bots, err)
	}
	now = now.Add(2 * time.Minute)
	if bots, err := c.MrReviewTrustedBots(context.Background()); err == nil {
		t.Fatalf("an expired cache whose refresh failed returned %v with no error; want the error", bots)
	}
}

// A refresh that read the rows before an admin write must not publish them after the write's
// Invalidate (#2347): otherwise a removed bot is served as trusted until the TTL expires.
func TestCacheRefreshDoesNotPublishAcrossInvalidate(t *testing.T) {
	s := &trustedBotsRefreshStore{started: make(chan struct{}), release: make(chan struct{})}
	c := New(s, time.Hour)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.MrReviewTrustedBots(context.Background())
	}()
	<-s.started
	c.Invalidate() // the admin removal commits while that fetch holds the old rows
	close(s.release)
	<-done

	bots, err := c.MrReviewTrustedBots(context.Background())
	if err == nil {
		t.Fatalf("the pre-invalidation rows were published: got %v with no refetch", bots)
	}
	if s.calls != 2 {
		t.Fatalf("store calls = %d, want 2 (the read after Invalidate must refetch)", s.calls)
	}
}
