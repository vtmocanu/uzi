package workersvc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// This adapter opens the fixture's sealed credential and constructs real drivers.
// It does not exercise forgesvc.New (that package imports workersvc).
type prerequisiteDriverBuilder struct {
	s      *Service
	builds int
}

func (b *prerequisiteDriverBuilder) ForgeForConnection(kind, baseURL string, sealed []byte) (forge.Forge, error) {
	b.builds++
	plain, err := b.s.box.Open(sealed)
	if err != nil {
		return nil, err
	}
	return forge.New(forge.Type(kind), baseURL, string(plain), 30*time.Second)
}

type prerequisiteDriverReply struct {
	body     string
	status   int
	location string
}

type prerequisiteDriverCase struct {
	kind   string
	n      int
	mode   string
	count  int
	state  string
	reason string
}

// These are synthetic HTTP fixtures, not forge eligibility probes or production
// latency evidence. In particular they do not extend S0's GitHub-only admission.
type prerequisiteDriverFixture struct {
	s       *Service
	m       *prerequisiteMemory
	h       *prerequisiteHTTP
	builder *prerequisiteDriverBuilder
	mu      sync.Mutex
	entries []string
	want    []string
	replies map[string]prerequisiteDriverReply
}

func prerequisiteDriverEntry(method, path, query string) string {
	if query != "" {
		return method + " " + path + "?" + query
	}
	return method + " " + path
}

func (f *prerequisiteDriverFixture) expect(path, query, body string) string {
	key := prerequisiteDriverEntry(http.MethodGet, path, query)
	f.want = append(f.want, key)
	f.replies[key] = prerequisiteDriverReply{body: body}
	return key
}

func newPrerequisiteDriverFixture(t *testing.T, c prerequisiteDriverCase) *prerequisiteDriverFixture {
	t.Helper()
	s, m, h, _ := newPrerequisiteMemory(t, c.n)
	f := &prerequisiteDriverFixture{s: s, m: m, h: h, replies: map[string]prerequisiteDriverReply{}}
	f.builder = &prerequisiteDriverBuilder{s: s}
	s.SetForges(f.builder)
	// Only the paging selectors are reused, never newPrerequisiteNativeFixture.
	s.SetPrerequisiteStore(&prerequisiteNativeStore{m}, m.transaction)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := prerequisiteDriverEntry(r.Method, r.URL.Path, r.URL.Query().Encode())
		f.mu.Lock()
		f.entries = append(f.entries, key)
		f.mu.Unlock()
		if m.inTX.Load() {
			t.Error("HTTP entry inside transaction")
		}
		if strings.HasPrefix(r.URL.Path, "/acme/widgets.git/") {
			user, pat, ok := r.BasicAuth()
			if !ok || user != "bot" || pat != "fixture-credential" {
				t.Error("broker credential mismatch")
			}
			// prerequisiteHTTP.serve serializes its mutable tip/posts under h.mu.
			h.serve(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		switch c.kind {
		case "github":
			if auth != "Bearer fixture-credential" {
				t.Error("GitHub credential mismatch")
			}
		case "gitlab":
			if r.Header.Get("PRIVATE-TOKEN") != "fixture-credential" {
				t.Error("GitLab credential mismatch")
			}
		case "forgejo":
			if auth != "token fixture-credential" {
				t.Error("Forgejo credential mismatch")
			}
		}
		reply, ok := f.replies[key]
		if !ok {
			t.Errorf("unexpected HTTP entry %s", key)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if reply.location != "" {
			w.Header().Set("Location", reply.location)
		}
		if reply.status != 0 {
			w.WriteHeader(reply.status)
		}
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(server.Close)
	m.conn.ForgeType, m.conn.BaseUrl = c.kind, server.URL
	m.repo.WebUrl = server.URL + "/acme/widgets"
	s.SetForgeBaseURLAllowed(func(base string) bool { return base == server.URL })

	// Publish the immutable reply table under the same mutex each handler takes
	// before reading it; clients start only after this constructor returns.
	f.mu.Lock()
	defer f.mu.Unlock()
	upload := prerequisiteDriverEntry("GET", "/acme/widgets.git/info/refs", "service=git-upload-pack")
	prefix := "/api/v3"
	if c.kind == "forgejo" {
		prefix = "/api/v1"
	}
	lookupPath := prefix + "/repositories/1"
	metadata := fmt.Sprintf(`{"id":1,"name":"widgets","full_name":"acme/widgets","path":"widgets","path_with_namespace":"acme/widgets","web_url":%q,"html_url":%q,"namespace":{"full_path":"acme"},"owner":{"login":"acme"}}`, m.repo.WebUrl, m.repo.WebUrl)
	if c.kind == "gitlab" {
		lookupPath = "/api/v4/projects/1"
	}
	lookup := f.expect(lookupPath, "", metadata)
	switch c.mode {
	case "lookup-error":
		f.replies[lookup] = prerequisiteDriverReply{status: http.StatusNotFound, body: `{}`}
		return f
	case "lookup-redirect":
		f.replies[lookup] = prerequisiteDriverReply{status: http.StatusFound, location: server.URL + "/lookup-target"}
		f.replies["GET /lookup-target"] = prerequisiteDriverReply{body: metadata}
	}
	f.want = append(f.want, upload, lookup)
	for i, sha := range m.row.PrerequisiteShas {
		var key string
		switch c.kind {
		case "github":
			key = f.expect(prefix+"/repos/acme/widgets/compare/"+prerequisiteTestTip+"..."+sha, "", `{"status":"behind"}`)
		case "gitlab":
			query := url.Values{"refs[]": []string{prerequisiteTestTip, sha}}.Encode()
			key = f.expect("/api/v4/projects/1/repository/merge_base", query, fmt.Sprintf(`{"id":%q}`, sha))
		case "forgejo":
			key = f.expect(prefix+"/repos/acme/widgets/compare/"+prerequisiteTestTip+"..."+sha, "", `{"total_commits":0}`)
		}
		last := i == c.n-1
		if last {
			switch c.mode {
			case "ahead", "diverged":
				f.replies[key] = prerequisiteDriverReply{body: fmt.Sprintf(`{"status":%q}`, c.mode)}
			case "negative":
				f.replies[key] = prerequisiteDriverReply{body: fmt.Sprintf(`{"id":%q}`, prerequisiteTestTip)}
			case "missing":
				f.replies[key] = prerequisiteDriverReply{body: `{}`}
			case "bad-json":
				f.replies[key] = prerequisiteDriverReply{body: `{`}
			case "http-error":
				f.replies[key] = prerequisiteDriverReply{status: http.StatusUnprocessableEntity, body: `{}`}
			case "compare-redirect":
				f.replies[key] = prerequisiteDriverReply{status: http.StatusFound, location: server.URL + "/compare-target"}
				// Serve a positive target if erroneously followed; it must never appear.
				f.replies["GET /compare-target"] = prerequisiteDriverReply{body: `{"status":"behind","id":"` + sha + `","total_commits":0}`}
			case "a-nonzero":
				f.replies[key] = prerequisiteDriverReply{body: `{"total_commits":1}`}
			}
		}
		if c.kind == "forgejo" && (!last || c.mode != "a-nonzero" && c.mode != "http-error" && c.mode != "compare-redirect") {
			key = f.expect(prefix+"/repos/acme/widgets/compare/"+sha+"..."+prerequisiteTestTip, "", `{"total_commits":1}`)
			if last {
				switch c.mode {
				case "b-zero":
					f.replies[key] = prerequisiteDriverReply{body: `{"total_commits":0}`}
				case "b-negative":
					f.replies[key] = prerequisiteDriverReply{body: `{"total_commits":-1}`}
				case "b-error":
					f.replies[key] = prerequisiteDriverReply{status: http.StatusUnprocessableEntity, body: `{}`}
				}
			}
		}
		f.want = append(f.want, lookup)
	}
	if c.state == "pinned" {
		f.want = append(f.want, lookup,
			prerequisiteDriverEntry("GET", "/acme/widgets.git/info/refs", "service=git-receive-pack"),
			"POST /acme/widgets.git/git-receive-pack", upload, lookup)
	}
	if c.mode == "lookup-redirect" {
		expanded := make([]string, 0, len(f.want)*2)
		for _, key := range f.want {
			expanded = append(expanded, key)
			if key == lookup {
				expanded = append(expanded, "GET /lookup-target")
			}
		}
		f.want = expanded
	}
	return f
}

func (f *prerequisiteDriverFixture) sweep(t *testing.T, c prerequisiteDriverCase, attempt int) {
	t.Helper()
	start, fakeStart := time.Now(), f.m.now
	_, err := f.s.SweepPrerequisiteClosures(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := append([]string(nil), f.entries...)
	f.mu.Unlock()
	// Traffic is the primary cache/reuse instrument, before factory call counts.
	if len(got) != len(f.want)*attempt {
		t.Fatalf("HTTP requests=%d want=%d (attempt %d); got %v", len(got), len(f.want)*attempt, attempt, got)
	}
	want := make([]string, 0, len(f.want)*attempt)
	for i := 0; i < attempt; i++ {
		want = append(want, f.want...)
	}
	if len(got) != len(want) {
		t.Fatalf("sequence lengths got=%d want=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("first HTTP mismatch at %d: got %q want %q", i, got[i], want[i])
		}
	}
	if f.builder.builds != attempt {
		t.Fatalf("driver builds=%d want=%d", f.builder.builds, attempt)
	}
	m := f.m
	if m.row.State != c.state || m.row.Reason != c.reason || int64(m.row.Attempts) != int64(attempt) {
		t.Fatalf("state=%s reason=%s attempts=%d", m.row.State, m.row.Reason, m.row.Attempts)
	}
	f.h.mu.Lock()
	sends, tip := f.h.sends, f.h.tip
	f.h.mu.Unlock()
	if c.state == "pinned" {
		if sends != 1 || m.marked != 1 || !m.row.LiveRunID.Valid || m.row.LiveRunID.Bytes != m.row.RunID ||
			!m.row.PinTip.Valid || m.row.PinTip.String != prerequisiteTestTip || tip != prerequisiteTestTip {
			t.Fatalf("pinned evidence: sends=%d markers=%d live=%v pin=%s tip=%s", sends, m.marked, m.row.LiveRunID.Valid, m.row.PinTip.String, tip)
		}
	} else {
		if sends != 0 || m.marked != 0 || m.row.LiveRunID.Valid || tip != "" {
			t.Fatalf("unproved authority: sends=%d markers=%d live=%v tip=%s", sends, m.marked, m.row.LiveRunID.Valid, tip)
		}
		if c.state == "pending" && (!m.row.NextAttemptAt.Time.After(m.now) || len(m.retries) != attempt || m.retries[attempt-1]) {
			t.Fatal("unknown proof must retry with positive backoff, not budget exhaustion")
		}
	}
	if f.h.ioInTX.Load() {
		t.Fatal("broker I/O inside transaction")
	}
	if !m.now.Equal(fakeStart) {
		t.Fatal("HTTP fixture advanced the fake clock")
	}
	t.Logf("synthetic-server observed HTTP requests=%d; fixture wall elapsed=%s; fake-clock duration=%s (not production latency or E4 evidence)", len(got), elapsed, m.now.Sub(fakeStart))
}

func TestPrerequisiteRealDriverColdMatrix(t *testing.T) {
	for _, kind := range []string{"github", "gitlab", "forgejo"} {
		for _, n := range []int{1, 6, 16, 64} {
			t.Run(fmt.Sprintf("%s/%d", kind, n), func(t *testing.T) {
				// Source-derived expectations; observed traffic is checked by sweep.
				count := n + 5
				if kind == "gitlab" {
					count = n + 4
				}
				if kind == "forgejo" {
					count = 2*n + 5
				}
				c := prerequisiteDriverCase{kind: kind, n: n, count: count, state: "pinned"}
				newPrerequisiteDriverFixture(t, c).sweep(t, c, 1)
			})
		}
	}
}

func TestPrerequisiteRealDriverDiscriminatingTail(t *testing.T) {
	const unknown = "prerequisite_ancestry_unknown"
	const negative = "prerequisite_not_on_default"
	cases := []prerequisiteDriverCase{
		{kind: "github", n: 64, mode: "ahead", count: 66, state: "refused", reason: negative},
		{kind: "github", n: 64, mode: "diverged", count: 66, state: "refused", reason: negative},
		{kind: "github", n: 64, mode: "missing", count: 66, state: "pending", reason: unknown},
		{kind: "github", n: 64, mode: "http-error", count: 66, state: "pending", reason: unknown},
		{kind: "gitlab", n: 64, mode: "negative", count: 65, state: "refused", reason: negative},
		{kind: "gitlab", n: 64, mode: "bad-json", count: 65, state: "pending", reason: unknown},
		{kind: "gitlab", n: 64, mode: "http-error", count: 65, state: "pending", reason: unknown},
		{kind: "forgejo", n: 64, mode: "a-nonzero", count: 129, state: "pending", reason: unknown},
		{kind: "forgejo", n: 64, mode: "http-error", count: 129, state: "pending", reason: unknown},
		{kind: "forgejo", n: 64, mode: "b-zero", count: 130, state: "pending", reason: unknown},
		{kind: "forgejo", n: 64, mode: "b-negative", count: 130, state: "pending", reason: unknown},
		{kind: "forgejo", n: 64, mode: "b-error", count: 130, state: "pending", reason: unknown},
	}
	for _, c := range cases {
		t.Run(c.kind+"/"+c.mode, func(t *testing.T) {
			newPrerequisiteDriverFixture(t, c).sweep(t, c, 1)
		})
	}
}

func TestPrerequisiteRealDriverRedirectsAndLookupErrors(t *testing.T) {
	const unknown = "prerequisite_ancestry_unknown"
	cases := []prerequisiteDriverCase{
		{kind: "github", n: 1, mode: "lookup-redirect", count: 15, state: "pinned"},
		{kind: "forgejo", n: 1, mode: "lookup-redirect", count: 15, state: "pinned"},
		{kind: "github", n: 1, mode: "lookup-error", count: 2, state: "pending", reason: unknown},
		{kind: "forgejo", n: 1, mode: "lookup-error", count: 2, state: "pending", reason: unknown},
		{kind: "github", n: 1, mode: "compare-redirect", count: 3, state: "pending", reason: unknown},
		{kind: "gitlab", n: 1, mode: "compare-redirect", count: 2, state: "pending", reason: unknown},
		{kind: "forgejo", n: 1, mode: "compare-redirect", count: 3, state: "pending", reason: unknown},
	}
	for _, c := range cases {
		t.Run(c.kind+"/"+c.mode, func(t *testing.T) {
			newPrerequisiteDriverFixture(t, c).sweep(t, c, 1)
		})
	}
}

func TestPrerequisiteRealDriverRetryStartsCold(t *testing.T) {
	for _, kind := range []string{"github", "gitlab", "forgejo"} {
		t.Run(kind, func(t *testing.T) {
			count := 8
			if kind == "gitlab" {
				count = 7
			}
			if kind == "forgejo" {
				count = 13
			}
			c := prerequisiteDriverCase{kind: kind, n: 6, mode: "http-error", count: count, state: "pending", reason: "prerequisite_ancestry_unknown"}
			f := newPrerequisiteDriverFixture(t, c)
			f.sweep(t, c, 1)
			f.m.now = f.m.row.NextAttemptAt.Time
			f.sweep(t, c, 2)
		})
	}
}
