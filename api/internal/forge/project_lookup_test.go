package forge

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type projectLookupDriver struct {
	kind   Type
	lookup string
	branch string
}

var projectLookupDrivers = []projectLookupDriver{
	{TypeGitHub, "/api/v3/repositories/42", "/api/v3/repos/current/repo/branches/main"},
	{TypeGitLab, "/api/v4/projects/42", "/api/v4/projects/42/repository/branches/main"},
	{TypeForgejo, "/api/v1/repositories/42", "/api/v1/repos/current/repo/branches/main"},
}

func (d projectLookupDriver) payload(owner string) map[string]any {
	if d.kind == TypeGitLab {
		return map[string]any{
			"id": 42, "path": "repo", "path_with_namespace": owner + "/repo",
			"namespace": map[string]any{"full_path": owner},
			"web_url":   "https://forge.example/" + owner + "/repo", "default_branch": "main",
		}
	}
	return map[string]any{
		"id": 42, "name": "repo", "full_name": owner + "/repo",
		"owner":    map[string]any{"login": owner},
		"html_url": "https://forge.example/" + owner + "/repo", "default_branch": "main",
	}
}

func (d projectLookupDriver) pathKey() string {
	if d.kind == TypeGitLab {
		return "path_with_namespace"
	}
	return "full_name"
}

func (d projectLookupDriver) urlKey() string {
	if d.kind == TypeGitLab {
		return "web_url"
	}
	return "html_url"
}

func (d projectLookupDriver) new(t *testing.T, h http.HandlerFunc) (Forge, *pathRecorder) {
	t.Helper()
	paths := &pathRecorder{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths.add(r)
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(s.Close)
	f, err := New(d.kind, s.URL, "project-lookup-test-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return f, paths
}

func projectLookupJSON(t *testing.T, p map[string]any, size int) string {
	t.Helper()
	p["padding"] = ""
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		if size < len(b) {
			t.Fatalf("fixture size %d < JSON length %d", size, len(b))
		}
		p["padding"] = strings.Repeat("x", size-len(b))
		b, err = json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != size {
			t.Fatalf("fixture length = %d, want %d", len(b), size)
		}
	}
	return string(b)
}

func projectLookupWant(owner string) Project {
	return Project{ForgeProjectID: 42, PathWithNamespace: owner + "/repo", WebURL: "https://forge.example/" + owner + "/repo", DefaultBranch: "main"}
}

func projectLookupAssertPaths(t *testing.T, paths *pathRecorder, want ...string) {
	t.Helper()
	if got := paths.list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v (no inventory or fallback allowed)", got, want)
	}
}

func (d projectLookupDriver) writeBranch(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": ancHead, "id": ancHead}})
}

func TestGetProjectFreshRefresh(t *testing.T) {
	for _, d := range projectLookupDrivers {
		t.Run(string(d.kind), func(t *testing.T) {
			n := 0
			f, paths := d.new(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case d.lookup:
					if r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("numeric lookup is not bounded")
					}
					n++
					owner := "old"
					if n > 1 {
						owner = "current"
					}
					// ETags must not make numeric metadata reads conditional/cached.
					if r.Header.Get("If-None-Match") != "" {
						t.Error("conditional numeric lookup")
					}
					w.Header().Set("ETag", `"project-v1"`)
					_ = json.NewEncoder(w).Encode(d.payload(owner))
				case d.branch:
					d.writeBranch(w)
				default:
					t.Errorf("unexpected route %s", r.URL)
					w.WriteHeader(404)
				}
			})
			for _, owner := range []string{"old", "current", "current"} {
				p, err := f.GetProject(context.Background(), 42)
				if err != nil || p != projectLookupWant(owner) {
					t.Fatalf("lookup = %+v, %v", p, err)
				}
			}
			sha, err := f.BranchHead(context.Background(), 42, "main")
			if err != nil || sha != ancHead {
				t.Fatalf("BranchHead = %q, %v", sha, err)
			}
			projectLookupAssertPaths(t, paths, d.lookup, d.lookup, d.lookup, d.branch)
		})
	}
}

func TestGetProjectIdentity(t *testing.T) {
	for _, d := range projectLookupDrivers {
		t.Run(string(d.kind), func(t *testing.T) {
			cases := []struct {
				name string
				edit func(map[string]any)
				ok   bool
			}{
				{"complete", func(map[string]any) {}, true},
				{"terminal_url_slash", func(p map[string]any) { p[d.urlKey()] = p[d.urlKey()].(string) + "/" }, true},
				{"double_terminal_url_slash", func(p map[string]any) { p[d.urlKey()] = p[d.urlKey()].(string) + "//" }, false},
				{"optional_default_branch", func(p map[string]any) { delete(p, "default_branch") }, true},
				{"missing_id", func(p map[string]any) { delete(p, "id") }, false},
				{"wrong_id", func(p map[string]any) { p["id"] = 43 }, false},
				{"negative_id", func(p map[string]any) { p["id"] = -42 }, false},
				{"missing_path", func(p map[string]any) { delete(p, d.pathKey()) }, false},
				{"conflicting_path", func(p map[string]any) { p[d.pathKey()] = "other/repo" }, false},
				{"missing_url", func(p map[string]any) { delete(p, d.urlKey()) }, false},
				{"conflicting_url", func(p map[string]any) { p[d.urlKey()] = "https://forge.example/other/repo" }, false},
				{"relative_url", func(p map[string]any) { p[d.urlKey()] = "/current/repo" }, false},
				{"missing_host", func(p map[string]any) { p[d.urlKey()] = "https:///current/repo" }, false},
				{"wrong_scheme", func(p map[string]any) { p[d.urlKey()] = "ftp://forge.example/current/repo" }, false},
				{"url_query", func(p map[string]any) { p[d.urlKey()] = "https://forge.example/current/repo?a=b" }, false},
				{"empty_url_query", func(p map[string]any) { p[d.urlKey()] = "https://forge.example/current/repo?" }, false},
				{"url_fragment", func(p map[string]any) { p[d.urlKey()] = "https://forge.example/current/repo#x" }, false},
				{"empty_url_fragment", func(p map[string]any) { p[d.urlKey()] = "https://forge.example/current/repo#" }, false},
				{"url_userinfo", func(p map[string]any) { p[d.urlKey()] = "https://user@forge.example/current/repo" }, false},
				{"invalid_url_escape", func(p map[string]any) { p[d.urlKey()] = "https://forge.example/current/%zz" }, false},
				{"missing_owner", func(p map[string]any) {
					if d.kind == TypeGitLab {
						delete(p, "namespace")
					} else {
						delete(p, "owner")
					}
				}, false},
				{"conflicting_owner", func(p map[string]any) {
					if d.kind == TypeGitLab {
						p["namespace"] = map[string]any{"full_path": "other"}
					} else {
						p["owner"] = map[string]any{"login": "other"}
					}
				}, false},
				{"missing_owner_path", func(p map[string]any) {
					if d.kind == TypeGitLab {
						p["namespace"] = map[string]any{}
					} else {
						p["owner"] = map[string]any{}
					}
				}, false},
				{"missing_name", func(p map[string]any) {
					if d.kind == TypeGitLab {
						delete(p, "path")
					} else {
						delete(p, "name")
					}
				}, false},
				{"conflicting_name", func(p map[string]any) {
					if d.kind == TypeGitLab {
						p["path"] = "other"
					} else {
						p["name"] = "other"
					}
				}, false},
				{"dot_owner", func(p map[string]any) {
					if d.kind == TypeGitLab {
						p["namespace"] = map[string]any{"full_path": ".."}
					} else {
						p["owner"] = map[string]any{"login": ".."}
					}
					p[d.pathKey()] = "../repo"
					p[d.urlKey()] = "https://forge.example/../repo"
				}, false},
			}
			if d.kind == TypeGitLab {
				cases = append(cases, struct {
					name string
					edit func(map[string]any)
					ok   bool
				}{
					"nested_namespace", func(p map[string]any) {
						p["namespace"] = map[string]any{"full_path": "group/sub/current"}
						p[d.pathKey()] = "group/sub/current/repo"
						p[d.urlKey()] = "https://forge.example/group/sub/current/repo"
					}, true})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					p := d.payload("current")
					tc.edit(p)
					body := projectLookupJSON(t, p, 0)
					projectLookupExercise(t, d, 200, nil, body, tc.ok, false)
				})
			}
		})
	}
}

// Each refusal starts with a populated stale slug. The subsequent ancestry read
// must resolve the numeric ID again and use its current path, proving invalidation
// through the existing public reader rather than inspecting the cache.
func projectLookupExercise(t *testing.T, d projectLookupDriver, status int, headers http.Header, body string, ok, large bool) {
	t.Helper()
	step := 0
	f, paths := d.new(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case d.lookup:
			step++
			switch step {
			case 1:
				_ = json.NewEncoder(w).Encode(d.payload("old"))
			case 2:
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("lookup is not bounded")
				}
				for k, vs := range headers {
					for _, v := range vs {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			default:
				_ = json.NewEncoder(w).Encode(d.payload("current"))
			}
		case d.branch:
			d.writeBranch(w)
		default:
			t.Errorf("unexpected route %s", r.URL)
			w.WriteHeader(404)
		}
	})
	if _, err := f.GetProject(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	p, err := f.GetProject(context.Background(), 42)
	if ok {
		if err != nil {
			t.Fatalf("GetProject = %+v, %v", p, err)
		}
		var wire map[string]any
		if err := json.Unmarshal([]byte(body), &wire); err != nil {
			t.Fatal(err)
		}
		want := Project{ForgeProjectID: 42, PathWithNamespace: wire[d.pathKey()].(string), WebURL: wire[d.urlKey()].(string)}
		if b, exists := wire["default_branch"].(string); exists {
			want.DefaultBranch = b
		}
		if p != want {
			t.Fatalf("project = %+v, want %+v", p, want)
		}
		projectLookupAssertPaths(t, paths, d.lookup, d.lookup)
		return
	}
	if err == nil || p != (Project{}) {
		t.Fatalf("refusal = %+v, %v; want zero project and error", p, err)
	}
	var tooLarge *ResponseTooLargeError
	if errors.As(err, &tooLarge) != large {
		t.Fatalf("typed size refusal = %v, want %v: %v", tooLarge, large, err)
	}
	if large {
		wantProvenance := ResponseError
		if statusSDKSuccess(status) {
			wantProvenance = ResponseSuccess
		}
		if !errors.Is(err, ErrResponseTooLarge) || tooLarge.Provenance != wantProvenance {
			t.Fatalf("lost size refusal evidence: %+v", tooLarge)
		}
	}
	if headers.Get("Content-Encoding") != "" && !strings.Contains(err.Error(), "unsupported content encoding") {
		t.Fatalf("lost encoding refusal: %v", err)
	}
	if d.kind == TypeGitLab {
		p, err = f.GetProject(context.Background(), 42)
		if err != nil || p != projectLookupWant("current") {
			t.Fatalf("recovery lookup = %+v, %v", p, err)
		}
		projectLookupAssertPaths(t, paths, d.lookup, d.lookup, d.lookup)
	} else {
		sha, err := f.BranchHead(context.Background(), 42, "main")
		if err != nil || sha != ancHead {
			t.Fatalf("post-refusal ancestry = %q, %v", sha, err)
		}
		projectLookupAssertPaths(t, paths, d.lookup, d.lookup, d.lookup, d.branch)
	}
}

func TestGetProjectResponseBounds(t *testing.T) {
	for _, d := range projectLookupDrivers {
		t.Run(string(d.kind), func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				status, size int
				ok, large    bool
			}{
				{"success_exact", 200, int(objectResponseBytes), true, false},
				{"success_overflow", 200, int(objectResponseBytes) + 1, false, true},
				{"error_exact_lazy", 403, 4096, false, false},
				{"error_overflow_lazy", 403, 4097, false, true},
				{"error_exact_eager", 501, 4096, false, false},
				{"error_overflow_eager", 501, 4097, false, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					p := d.payload("current")
					p["message"] = "lookup refused"
					body := projectLookupJSON(t, p, tc.size)
					projectLookupExercise(t, d, tc.status, nil, body, tc.ok, tc.large)
				})
			}
		})
	}
}

func TestGetProjectIncompleteResponses(t *testing.T) {
	for _, d := range projectLookupDrivers {
		t.Run(string(d.kind), func(t *testing.T) {
			body := projectLookupJSON(t, d.payload("current"), 0)
			var compressed strings.Builder
			zw := gzip.NewWriter(&compressed)
			if _, err := zw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name    string
				status  int
				headers http.Header
				body    string
			}{
				{"partial_status", 206, nil, body},
				{"accepted", 202, nil, body},
				{"created", 201, nil, body},
				{"no_content", 204, nil, ""},
				{"content_range", 200, http.Header{"Content-Range": {"bytes 0-10/100"}}, body},
				{"empty_content_range", 200, http.Header{"Content-Range": {""}}, body},
				{"compressed_success", 200, http.Header{"Content-Encoding": {"gzip"}}, compressed.String()},
				{"compressed_error", 403, http.Header{"Content-Encoding": {"gzip"}}, compressed.String()},
				{"compressed_server_error", 501, http.Header{"Content-Encoding": {"br"}}, body},
				{"multiple_encodings", 200, http.Header{"Content-Encoding": {"identity", "gzip"}}, body},
				{"malformed", 200, nil, "{"},
				{"trailing_malformed", 200, nil, body + "x"},
				{"second_json_object", 200, nil, body + "{}"},
				{"truncated_json", 200, nil, body[:len(body)-1]},
				{"truncated_transfer", 200, http.Header{"Content-Length": {strconv.Itoa(len(body) + 1)}}, body},
				{"null", 200, nil, "null"},
				{"empty", 200, nil, ""},
				{"not_found", 404, nil, "{\"message\":\"not found\"}"},
			} {
				t.Run(tc.name, func(t *testing.T) { projectLookupExercise(t, d, tc.status, tc.headers, tc.body, false, false) })
			}
		})
	}
}

func TestGetProjectInheritedReadPolicy(t *testing.T) {
	for _, d := range projectLookupDrivers {
		t.Run(string(d.kind), func(t *testing.T) {
			body := projectLookupJSON(t, d.payload("current"), int(objectResponseBytes)+1)
			f, paths := d.new(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != d.lookup {
					t.Errorf("unexpected route %s", r.URL)
				}
				_, _ = w.Write([]byte(body))
			})
			ctx := markCIJSON(WithCIRepairReadLimits(context.Background()))
			p, err := f.GetProject(ctx, 42)
			var large *ResponseTooLargeError
			if p != (Project{}) || !errors.As(err, &large) || large.Provenance != ResponseSuccess {
				t.Fatalf("inherited read policy bypassed project limit: %+v, %v", p, err)
			}
			projectLookupAssertPaths(t, paths, d.lookup)
		})
	}
}

func TestGetProjectInvalidID(t *testing.T) {
	for _, d := range projectLookupDrivers {
		t.Run(string(d.kind), func(t *testing.T) {
			f, paths := d.new(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid ID sent a request") })
			for _, id := range []int64{0, -1} {
				p, err := f.GetProject(context.Background(), id)
				if err == nil || p != (Project{}) {
					t.Fatalf("GetProject(%d) = %+v, %v", id, p, err)
				}
			}
			if got := paths.list(); len(got) != 0 {
				t.Fatalf("requests = %v", got)
			}
		})
	}
}

func TestGetProjectCancellation(t *testing.T) {
	for _, d := range projectLookupDrivers {
		t.Run(string(d.kind), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			step := 0
			f, paths := d.new(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case d.lookup:
					step++
					if step == 2 {
						cancel()
						return
					}
					owner := "old"
					if step > 2 {
						owner = "current"
					}
					_ = json.NewEncoder(w).Encode(d.payload(owner))
				case d.branch:
					d.writeBranch(w)
				default:
					t.Errorf("unexpected route %s", r.URL)
					w.WriteHeader(404)
				}
			})
			if _, err := f.GetProject(context.Background(), 42); err != nil {
				t.Fatal(err)
			}
			p, err := f.GetProject(ctx, 42)
			if err == nil || p != (Project{}) {
				t.Fatalf("cancelled lookup = %+v, %v", p, err)
			}
			if d.kind == TypeGitLab {
				p, err = f.GetProject(context.Background(), 42)
				if err != nil || p != projectLookupWant("current") {
					t.Fatalf("next call inherited cancellation: %+v, %v", p, err)
				}
				projectLookupAssertPaths(t, paths, d.lookup, d.lookup, d.lookup)
			} else {
				sha, err := f.BranchHead(context.Background(), 42, "main")
				if err != nil || sha != ancHead {
					t.Fatalf("post-cancel ancestry = %q, %v", sha, err)
				}
				projectLookupAssertPaths(t, paths, d.lookup, d.lookup, d.lookup, d.branch)
			}
		})
	}
}
