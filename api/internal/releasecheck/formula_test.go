package releasecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/settings"
)

// formulaSrc renders a tap formula the way the brew workflow does: one uzi tag URL.
func formulaSrc(tag string) string {
	return `class UziCliRc < Formula
  desc "uzi CLI"
  homepage "https://github.com/vtmocanu/uzi"
  url "https://github.com/vtmocanu/uzi/archive/refs/tags/` + tag + `.tar.gz"
  sha256 "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  license "MIT"

  def install
    system "go", "build"
  end
end
`
}

func TestParseFormulaTag(t *testing.T) {
	withExtraURL := strings.Replace(formulaSrc("v1.2.3"), "  def install",
		"  resource \"x\" do\n    url \"https://example.test/x.tar.gz\"\n  end\n\n  def install", 1)
	withSecondUziURL := strings.Replace(formulaSrc("v1.2.3"), "  def install",
		"  resource \"x\" do\n    url \"https://github.com/vtmocanu/uzi/archive/refs/tags/v1.2.2.tar.gz\"\n  end\n\n  def install", 1)
	for _, tc := range []struct {
		name       string
		src        string
		stableOnly bool
		want       string
	}{
		{"stable in stable formula", formulaSrc("v1.2.3"), true, "v1.2.3"},
		{"stable in rc formula", formulaSrc("v10.0.0"), false, "v10.0.0"},
		{"rc in rc formula", formulaSrc("v0.87.0-rc.11"), false, "v0.87.0-rc.11"},
		{"rc rejected in stable formula", formulaSrc("v0.87.0-rc.11"), true, ""},
		{"empty", "", false, ""},
		{"no url", "class X < Formula\nend\n", false, ""},
		{"commented url only", "# url \"https://github.com/vtmocanu/uzi/archive/refs/tags/v1.2.3.tar.gz\"\n", false, ""},
		{"second url line", withExtraURL, false, ""},
		{"second uzi url line", withSecondUziURL, false, ""},
		{"version override", formulaSrc("v1.2.3") + "  version \"9.9.9\"\n", false, ""},
		{"version_scheme override", formulaSrc("v1.2.3") + "  version_scheme 1\n", false, ""},
		{"non-uzi host", strings.Replace(formulaSrc("v1.2.3"), "github.com/vtmocanu/uzi/archive", "github.com/evil/uzi/archive", 1), false, ""},
		{"rc.0", formulaSrc("v1.2.3-rc.0"), false, ""},
		{"leading zero", formulaSrc("v01.2.3"), false, ""},
		{"trailing rc suffix", formulaSrc("v1.2.3-rc.1.extra"), false, ""},
		{"beta tag", formulaSrc("v1.2.3-beta.1"), false, ""},
		{"no v prefix", formulaSrc("1.2.3"), false, ""},
		{"unicode-indented second url", formulaSrc("v1.2.3") + " url \"https://example.test/y\"\n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFormulaTag(tc.src, tc.stableOnly)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("parseFormulaTag = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.want != "")
			}
		})
	}
}

// formulaServer serves the release endpoints plus each formula path via the given
// handlers, recording every request's path and Accept header.
type formulaServer struct {
	mu     sync.Mutex
	seen   []string
	accept map[string]string
}

func newFormulaServer(t *testing.T, stable, rc http.HandlerFunc, latestFails bool) *formulaServer {
	t.Helper()
	fs := &formulaServer{accept: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		fs.mu.Lock()
		fs.seen = append(fs.seen, key)
		fs.accept[key] = r.Header.Get("Accept")
		fs.mu.Unlock()
		switch key {
		case formulaStablePath:
			stable(w, r)
		case formulaRCPath:
			rc(w, r)
		case "/repos/vtmocanu/uzi/releases/latest":
			if latestFails {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(releaseJSON("v2.0.0", "stable", "notes", "2026-08-20T10:00:00Z", "https://x")))
		case "/repos/vtmocanu/uzi/releases?per_page=20":
			_, _ = w.Write([]byte("[" + releaseJSON("v2.1.0-rc.1", "rc", "", "2026-08-21T10:00:00Z", "https://y") + "]"))
		default:
			t.Errorf("unexpected request %q", key)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	withBaseURL(t, srv.URL)
	return fs
}

func serve(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func runCheck(t *testing.T, st *fakeStore) Result {
	t.Helper()
	res, err := NewReconciler(st, &fakeSettings{enabled: true}, nil, quietLogger()).CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate err = %v", err)
	}
	return res
}

func TestCheckForUpdateWritesFormulaTags(t *testing.T) {
	fs := newFormulaServer(t, serve(formulaSrc("v2.0.0")), serve(formulaSrc("v2.1.0-rc.1")), false)
	st := newFakeStore(nil)
	res := runCheck(t, st)
	if res.Status != statusOK || res.Partial {
		t.Fatalf("result = %+v", res)
	}
	if st.values[settings.KeyReleaseFormulaStableTag] != "v2.0.0" || st.values[settings.KeyReleaseFormulaRCTag] != "v2.1.0-rc.1" {
		t.Fatalf("formula keys = %q / %q", st.values[settings.KeyReleaseFormulaStableTag], st.values[settings.KeyReleaseFormulaRCTag])
	}
	if res.Facts.FormulaStableTag != "v2.0.0" || res.Facts.FormulaRCTag != "v2.1.0-rc.1" {
		t.Fatalf("facts = %+v", res.Facts)
	}
	for _, p := range []string{formulaStablePath, formulaRCPath} {
		if got := fs.accept[p]; got != "application/vnd.github.raw+json" {
			t.Errorf("Accept for %s = %q", p, got)
		}
	}
	if !strings.HasSuffix(formulaStablePath, "/Formula/uzi-cli.rb?ref=main") || !strings.HasSuffix(formulaRCPath, "/Formula/uzi-cli-rc.rb?ref=main") {
		t.Errorf("formula paths drifted: %q %q", formulaStablePath, formulaRCPath)
	}
}

// A ready check followed by a failure of one formula writes "" for that formula only,
// and leaves Status / Latest* / RC* exactly as in the all-good run.
func TestCheckForUpdateFormulaFailureIsolated(t *testing.T) {
	oversized := serve(strings.Repeat("a", maxFormulaBodyBytes+1))
	redirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}
	failures := map[string]http.HandlerFunc{
		"404":       status(http.StatusNotFound),
		"403":       status(http.StatusForbidden),
		"redirect":  redirect,
		"oversized": oversized,
		"malformed": serve("not a formula"),
	}
	newFormulaServer(t, serve(formulaSrc("v2.0.0")), serve(formulaSrc("v2.1.0-rc.1")), false)
	base := runCheck(t, newFakeStore(nil))

	for name, bad := range failures {
		for _, which := range []string{"stable", "rc"} {
			t.Run(name+"/"+which, func(t *testing.T) {
				good := serve(formulaSrc("v2.0.0"))
				goodRC := serve(formulaSrc("v2.1.0-rc.1"))
				if which == "stable" {
					newFormulaServer(t, bad, goodRC, false)
				} else {
					newFormulaServer(t, good, bad, false)
				}
				st := newFakeStore(map[string]string{
					settings.KeyReleaseFormulaStableTag: "v1.0.0", settings.KeyReleaseFormulaRCTag: "v1.1.0-rc.1",
				})
				res := runCheck(t, st)
				wantStable, wantRC := "v2.0.0", "v2.1.0-rc.1"
				if which == "stable" {
					wantStable = ""
				} else {
					wantRC = ""
				}
				if got := st.values[settings.KeyReleaseFormulaStableTag]; got != wantStable {
					t.Errorf("stable formula key = %q, want %q", got, wantStable)
				}
				if got := st.values[settings.KeyReleaseFormulaRCTag]; got != wantRC {
					t.Errorf("rc formula key = %q, want %q", got, wantRC)
				}
				if res.Status != base.Status || res.Partial != base.Partial || res.Message != base.Message {
					t.Errorf("result = %+v, want status/partial/message of %+v", res, base)
				}
				f, b := res.Facts, base.Facts
				f.FormulaStableTag, f.FormulaRCTag, f.CheckedAt = "", "", ""
				b.FormulaStableTag, b.FormulaRCTag, b.CheckedAt = "", "", ""
				if f != b {
					t.Errorf("non-formula facts changed: %+v vs %+v", f, b)
				}
			})
		}
	}
}

func TestCheckForUpdateStableFailureWritesNoFormulaKeys(t *testing.T) {
	fs := newFormulaServer(t, serve(formulaSrc("v2.0.0")), serve(formulaSrc("v2.1.0-rc.1")), true)
	st := newFakeStore(map[string]string{settings.KeyReleaseFormulaStableTag: "v1.0.0"})
	res := runCheck(t, st)
	if res.Status != statusError || st.writes != 0 || st.values[settings.KeyReleaseFormulaStableTag] != "v1.0.0" {
		t.Fatalf("result=%+v writes=%d values=%v", res, st.writes, st.values)
	}
	if len(fs.seen) != 1 {
		t.Errorf("requests = %v, want only the latest-release call", fs.seen)
	}
}
