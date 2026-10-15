package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// This external-package test only uses the production Forge interface. The Node
// launcher seeds real Git repositories and starts the shared HTTPS fake before
// starting this fresh process with SSL_CERT_FILE, before TLS roots are loaded.
func TestForgeFakeOperationContracts(t *testing.T) {
	if os.Getenv("UZI_FORGE_FAKE_REQUIRED") == "" {
		t.Skip("standalone contract requires task test:forge-fake-contract")
	}
	if os.Getenv("UZI_FORGE_FAKE_REQUIRED") != "1" {
		t.Fatal("invalid required-mode marker")
	}
	type branch struct {
		Project int64
		Name    string
		SHA     string
	}
	type comparison struct {
		Head, Candidate, Name string
		Ancestor              bool
	}
	type mrFixture struct {
		Dialect, WebURL, Source, Target, Head, State string
		IID                                          int64
	}
	type comment struct {
		AuthorForgeUserID int64
		AuthorUsername    string
		Body              string
		CreatedAt         time.Time
	}
	type thread struct {
		Project, Issue int64
		GitlabOnly     bool
		Comments       []comment
	}
	var manifest struct {
		Version     int
		Endpoint    string
		Branches    []branch
		Checkpoint  string
		Comparisons []comparison
		MRs         []mrFixture
		Threads     []thread
	}
	file, err := os.Open(os.Getenv("UZI_FORGE_FAKE_MANIFEST"))
	if err != nil {
		t.Fatal("required manifest:", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal("invalid manifest:", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatal("manifest must contain exactly one JSON object")
	}
	endpoint, err := url.Parse(manifest.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" || endpoint.Path != "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		t.Fatal("manifest endpoint must be loopback HTTPS with an explicit port")
	}
	shaPattern := regexp.MustCompile("^[0-9a-f]{40}$")
	validSHA := func(s string) bool { return shaPattern.MatchString(s) }
	if manifest.Version != 1 || len(manifest.Branches) != 4 || len(manifest.Comparisons) != 4 || len(manifest.MRs) != 3 || len(manifest.Threads) != 4 || !validSHA(manifest.Checkpoint) {
		t.Fatal("manifest schema/array cardinality invalid")
	}
	for i, b := range manifest.Branches {
		wantProject := int64(1 + i/2)
		wantName := []string{"simple", "agent/slash"}[i%2]
		if b.Project != wantProject || b.Name != wantName || !validSHA(b.SHA) {
			t.Fatalf("invalid branch fixture: %+v", b)
		}
	}
	if manifest.Branches[0].SHA == manifest.Branches[2].SHA {
		t.Fatal("second project must have a distinct head")
	}
	for i, c := range manifest.Comparisons {
		if !validSHA(c.Head) || !validSHA(c.Candidate) || c.Name != []string{"ahead", "behind", "equal", "diverged"}[i] || c.Ancestor != (i == 0 || i == 2) {
			t.Fatalf("invalid independent Git relation: %+v", c)
		}
	}
	for i, mr := range manifest.MRs {
		u, parseErr := url.Parse(mr.WebURL)
		if mr.Dialect != []string{"gitlab", "github", "forgejo"}[i] || mr.IID <= 0 || !validSHA(mr.Head) || mr.Source != "agent/"+mr.Dialect || mr.Target != "main" || mr.State != forge.MRStateOpened || parseErr != nil || u.Scheme != "https" || u.Host != "configured.invalid" {
			t.Fatalf("invalid MR fixture: %+v", mr)
		}
	}
	for i, thread := range manifest.Threads {
		if thread.Project <= 0 || thread.Issue <= 0 || len(thread.Comments) != 105 || thread.GitlabOnly != (i == 3) {
			t.Fatalf("invalid comment thread %d", i)
		}
		for _, c := range thread.Comments {
			if c.AuthorForgeUserID <= 0 || c.AuthorUsername == "" || c.Body == "" || c.CreatedAt.IsZero() {
				t.Fatal("invalid expected comment")
			}
		}
	}

	for _, mr := range manifest.MRs {
		t.Run(mr.Dialect, func(t *testing.T) {
			var client forge.Forge
			client, err = forge.New(forge.Type(mr.Dialect), manifest.Endpoint, "contract-fixture", 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			t.Log("MR create and description update are covered by production worker ForgeClient; Go Forge has no such operations")
			t.Run("BranchHead", func(t *testing.T) {
				for _, b := range manifest.Branches {
					got, err := client.BranchHead(ctx, b.Project, b.Name)
					if err != nil || got != b.SHA {
						t.Errorf("project=%d branch=%s: got %q, %v; want %s", b.Project, b.Name, got, err, b.SHA)
					}
				}
				if _, err := client.BranchHead(ctx, 1, "missing"); !errors.Is(err, forge.ErrRefNotFound) {
					t.Errorf("missing branch: %v", err)
				}
			})
			t.Run("RefHead", func(t *testing.T) {
				for _, b := range manifest.Branches {
					got, err := client.RefHead(ctx, b.Project, "refs/heads/"+b.Name)
					if err != nil || got != b.SHA {
						t.Errorf("project=%d ref=%s: %q, %v; want %s", b.Project, b.Name, got, err, b.SHA)
					}
				}
				got, err := client.RefHead(ctx, 1, "refs/uzi-checkpoints/agent/slash")
				if err != nil || got != manifest.Checkpoint {
					t.Errorf("checkpoint: %q, %v; want %s", got, err, manifest.Checkpoint)
				}
				if _, err := client.RefHead(ctx, 1, "refs/heads/missing"); !errors.Is(err, forge.ErrRefNotFound) {
					t.Errorf("missing ref: %v", err)
				}
			})
			t.Run("CompareAncestry", func(t *testing.T) {
				for _, c := range manifest.Comparisons {
					name := c.Name
					if mr.Dialect == "forgejo" && !c.Ancestor {
						name += "_unsupported_conclusive_negative"
					}
					t.Run(name, func(t *testing.T) {
						want := forge.AncestryNotAncestor
						wantError := mr.Dialect == "forgejo" && !c.Ancestor
						if c.Ancestor {
							want = forge.AncestryAncestor
						} else if wantError {
							want = forge.AncestryUnknown
						}
						got, err := client.CompareAncestry(ctx, 1, c.Head, c.Candidate)
						if got != want || (err != nil) != wantError {
							t.Errorf("head=%s candidate=%s: %v, %v; want %v error=%v", c.Head, c.Candidate, got, err, want, wantError)
						}
					})
				}
				t.Run("equal_without_HTTP", func(t *testing.T) {
					// A closed loopback port cannot answer HTTP. Equality is local.
					offline, err := forge.New(forge.Type(mr.Dialect), "https://127.0.0.1:1", "contract-fixture", time.Second)
					if err != nil {
						t.Fatal(err)
					}
					head := manifest.Comparisons[0].Head
					got, err := offline.CompareAncestry(ctx, 1, head, head)
					if err != nil || got != forge.AncestryAncestor {
						t.Errorf("equal without HTTP: %v, %v", got, err)
					}
				})
				t.Run("missing_commit", func(t *testing.T) {
					got, err := client.CompareAncestry(ctx, 1, manifest.Comparisons[0].Head, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
					if got != forge.AncestryUnknown || err == nil {
						t.Errorf("missing commit: %v, %v", got, err)
					}
				})
			})
			t.Run("GetMergeRequest", func(t *testing.T) {
				got, err := client.GetMergeRequest(ctx, 1, mr.IID)
				want := forge.MergeRequest{IID: mr.IID, State: mr.State, WebURL: mr.WebURL}
				if err != nil || got != want {
					t.Errorf("got %+v, %v; want %+v", got, err, want)
				}
				// Basic Get promises a generic error, not a not-found sentinel.
				if _, err := client.GetMergeRequest(ctx, 1, 99999); err == nil {
					t.Error("missing basic MR must fail")
				}
			})
			t.Run("GetMergeRequestSummary", func(t *testing.T) {
				got, err := client.GetMergeRequestSummary(ctx, 1, mr.IID)
				if err != nil || got.IID != mr.IID || got.State != mr.State || got.WebURL != mr.WebURL || got.HeadSHA != mr.Head || got.SourceBranch != mr.Source || got.TargetBranch != mr.Target {
					t.Errorf("summary %+v, %v; fixture %+v", got, err, mr)
				}
				if _, err := client.GetMergeRequestSummary(ctx, 1, 99999); !errors.Is(err, forge.ErrMergeRequestNotFound) {
					t.Errorf("missing summary: %v", err)
				}
			})
			name := "ListIssueComments"
			if mr.Dialect == "gitlab" {
				name += "/GitLab_notes"
			}
			t.Run(name, func(t *testing.T) {
				for _, thread := range manifest.Threads {
					if thread.GitlabOnly && mr.Dialect != "gitlab" {
						continue // Other dialect fixtures deliberately contain human comments only.
					}
					got, err := client.ListIssueComments(ctx, thread.Project, thread.Issue)
					if err != nil {
						t.Errorf("project=%d issue=%d: %v", thread.Project, thread.Issue, err)
						continue
					}
					want := make([]forge.IssueComment, len(thread.Comments))
					for i, c := range thread.Comments {
						want[i] = forge.IssueComment{AuthorForgeUserID: c.AuthorForgeUserID, AuthorUsername: c.AuthorUsername, Body: c.Body, CreatedAt: c.CreatedAt}
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("project=%d issue=%d whole ordered comments differ:\ngot %+v\nwant %+v", thread.Project, thread.Issue, got, want)
					}
				}
				got, err := client.ListIssueComments(ctx, 1, 9)
				if err != nil || len(got) != 0 {
					t.Errorf("empty existing issue: %+v, %v", got, err)
				}
			})
		})
	}
}
