package forge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"
)

type artifactRT func(*http.Request) (*http.Response, error)

func (f artifactRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type artifactCountingBody struct {
	remaining int64
	consumed  int64
	reads     int
	closed    bool
}

func (b *artifactCountingBody) Read(p []byte) (int, error) {
	b.reads++
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.remaining -= n
	b.consumed += n
	return int(n), nil
}
func (b *artifactCountingBody) Close() error { b.closed = true; return nil }

func artifactResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}
}
func artifactDriver(t *testing.T, api artifactRT) *github {
	t.Helper()
	g, err := newGitHub("", "github_pat_"+strings.Repeat("a", 82), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the constructor's redirect policy. No live network in these tests.
	g.slugs[7] = repoSlug{owner: "group", repo: "repo"}
	g.artifactAPI.Transport = api
	g.artifactHooks.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	return g
}
func artifactRedirect(location string) *http.Response {
	r := artifactResponse(302, "")
	r.Header.Set("Location", location)
	return r
}
func artifactJSON(id int, extra string) string {
	return fmt.Sprintf(`{"id":%d,"name":"evidence","size_in_bytes":3,"expired":false,"workflow_run":{"id":8,"repository_id":7,"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}%s}`, id, extra)
}

func TestArtifactAuthorityAndAuthentication(t *testing.T) {
	apiCalls, blobCalls := 0, 0
	g := artifactDriver(t, func(r *http.Request) (*http.Response, error) {
		apiCalls++
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") == "" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("API request: %v %v", r.URL, r.Header)
		}
		switch r.URL.Path {
		case "/repos/group/repo/actions/runs/8/artifacts":
			if r.URL.RawQuery != "per_page=100&page=1" {
				t.Fatal(r.URL)
			}
			return artifactResponse(200, `{"total_count":1,"artifacts":[`+artifactJSON(9, `,"archive_download_url":"https://metadata.invalid/steal"`)+`]}`), nil
		case "/repos/group/repo/actions/artifacts/9/zip":
			return artifactRedirect("https://blob.example/archive?signature=private"), nil
		default:
			t.Fatalf("unexpected API path %v", r.URL)
			return nil, nil
		}
	})
	g.artifactHooks.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "blob.example" {
			t.Fatalf("metadata chose destination %s", host)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	g.artifactHooks.blob = artifactRT(func(r *http.Request) (*http.Response, error) {
		blobCalls++
		if r.URL.Host != "blob.example" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Accept-Encoding") != "identity" || len(r.Header) != 1 {
			t.Fatalf("blob request leaked headers: %v", r.Header)
		}
		return artifactResponse(200, "zip"), nil
	})
	got, err := g.ListRunArtifacts(context.Background(), 7, 8)
	want := []Artifact{{ID: 9, Name: "evidence", SizeBytes: 3, RunID: 8, RepositoryID: 7, SHA: strings.Repeat("a", 40)}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("list: %+v %v", got, err)
	}
	b, err := g.DownloadArtifact(context.Background(), 7, 9, 3)
	if err != nil || string(b) != "zip" || apiCalls != 2 || blobCalls != 1 {
		t.Fatalf("download: %q %v api=%d blob=%d", b, err, apiCalls, blobCalls)
	}
}

func TestArtifactMissingRedirectAndStatuses(t *testing.T) {
	for _, status := range []int{200, 301, 303, 307, 308, 401, 403, 404, 410, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := &artifactCountingBody{remaining: 100000}
			g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
				r := artifactResponse(status, "")
				r.Body = body
				r.Header.Set("Location", "https://blob.example/")
				return r, nil
			})
			g.artifactHooks.lookup = func(context.Context, string) ([]netip.Addr, error) {
				t.Fatal("unexpected destination lookup")
				return nil, nil
			}
			_, err := g.DownloadArtifact(context.Background(), 7, 9, 3)
			if err == nil || body.reads != 0 || !body.closed {
				t.Fatalf("err=%v body=%+v", err, body)
			}
			want := map[int]error{401: ErrArtifactDenied, 403: ErrArtifactDenied, 404: ErrArtifactMissing, 410: ErrArtifactExpired}[status]
			if want != nil && !errors.Is(err, want) {
				t.Fatalf("want %v got %v", want, err)
			}
		})
	}
	for _, locations := range [][]string{nil, {"https://blob.example/", "https://other.example/"}, {""}} {
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			r := artifactResponse(302, "")
			r.Header["Location"] = locations
			return r, nil
		})
		g.artifactHooks.lookup = func(context.Context, string) ([]netip.Addr, error) { t.Fatal("unexpected lookup"); return nil, nil }
		if _, err := g.DownloadArtifact(context.Background(), 7, 9, 3); err == nil {
			t.Fatal("accepted invalid Location")
		}
	}
}

func TestArtifactInvalidDestinations(t *testing.T) {
	for _, location := range []string{"http://blob.example/", "https://user:pass@blob.example/", "https://blob.example:444/", "https://blob.example:/", "https:///file", "//blob.example/", "https://blob.example/#fragment", "https://127.0.0.1/", "https://[::1]/", "https://10.0.0.1/"} {
		t.Run(location, func(t *testing.T) {
			g := artifactDriver(t, func(*http.Request) (*http.Response, error) { return artifactRedirect(location), nil })
			g.artifactHooks.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
				ip, err := netip.ParseAddr(host)
				if err != nil {
					return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
				}
				return []netip.Addr{ip}, nil
			}
			g.artifactHooks.blob = artifactRT(func(*http.Request) (*http.Response, error) { t.Fatal("invalid destination requested"); return nil, nil })
			if _, err := g.DownloadArtifact(context.Background(), 7, 9, 3); err == nil {
				t.Fatal("accepted invalid destination")
			}
		})
	}
	for _, ips := range [][]string{{"8.8.8.8", "127.0.0.1"}, {"8.8.8.8", "10.0.0.1"}, {"100.64.0.1"}, {"169.254.169.254"}, {"192.0.2.1"}, {"::ffff:127.0.0.1"}, {"2001:db8::1"}, {"fc00::1"}, {}} {
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) { return artifactRedirect("https://blob.example/"), nil })
		g.artifactHooks.lookup = func(context.Context, string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, ip := range ips {
				out = append(out, netip.MustParseAddr(ip))
			}
			return out, nil
		}
		g.artifactHooks.blob = artifactRT(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid DNS destination requested")
			return nil, nil
		})
		if _, err := g.DownloadArtifact(context.Background(), 7, 9, 3); err == nil {
			t.Fatalf("accepted DNS %v", ips)
		}
	}
}

func TestArtifactPublicDNSPinnedDial(t *testing.T) {
	g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
		return artifactRedirect("https://blob.example:443/archive"), nil
	})
	lookups, dials := 0, 0
	g.artifactHooks.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups++
		if host != "blob.example" {
			t.Fatal(host)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2606:4700:4700::1111")}, nil
	}
	g.artifactHooks.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		dials++
		if network != "tcp" || address != "8.8.8.8:443" {
			t.Fatalf("unpinned dial %s %s", network, address)
		}
		return nil, errors.New("signed URL or credentials must never escape")
	}
	_, err := g.DownloadArtifact(context.Background(), 7, 9, 3)
	if err != errArtifactRequest || lookups != 1 || dials != 1 {
		t.Fatalf("err=%v lookup=%d dial=%d", err, lookups, dials)
	}
}

func TestArtifactPinnedDialRetainsTLSVerification(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"blob.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
	type handshakeResult struct {
		name string
		err  error
	}
	results := make(chan handshakeResult, 1)
	g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
		return artifactRedirect("https://blob.example/archive"), nil
	})
	g.artifactHooks.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Fatal(address)
		}
		client, server := net.Pipe()
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		_ = server.SetDeadline(time.Now().Add(2 * time.Second))
		go func() {
			defer func() { _ = server.Close() }()
			name := ""
			connection := tls.Server(server, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(info *tls.ClientHelloInfo) (*tls.Certificate, error) { name = info.ServerName; return &cert, nil }})
			err := connection.Handshake()
			results <- handshakeResult{name, err}
		}()
		return client, nil
	}
	_, err = g.DownloadArtifact(context.Background(), 7, 9, 3)
	if err != errArtifactRequest {
		t.Fatalf("TLS failure was not sanitized: %v", err)
	}
	select {
	case result := <-results:
		if result.name != "blob.example" || result.err == nil {
			t.Fatalf("TLS hostname/verification lost: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TLS handshake did not finish")
	}
}

func TestArtifactActualReadBounds(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		size, cap, declared, wantRead int64
		want                          error
	}{
		{"exact", 3, 3, -1, 3, nil},
		{"overflow", 4, 3, -1, 4, ErrArtifactBudget},
		{"hostile infinite", 100000, 3, -1, 4, ErrArtifactBudget},
		{"length refused", 100000, 3, 4, 0, ErrArtifactBudget},
		{"length understated", 4, 3, 2, 4, ErrArtifactBudget},
		{"length mismatch", 2, 3, 3, 2, errArtifactResponse},
		{"absolute cap", artifactArchiveBytes + 2, artifactArchiveBytes + 100, -1, artifactArchiveBytes + 1, ErrArtifactBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &artifactCountingBody{remaining: tc.size}
			g := artifactDriver(t, func(*http.Request) (*http.Response, error) { return artifactRedirect("https://blob.example/"), nil })
			g.artifactHooks.blob = artifactRT(func(*http.Request) (*http.Response, error) {
				r := artifactResponse(200, "")
				r.Body = body
				r.ContentLength = tc.declared
				return r, nil
			})
			b, err := g.DownloadArtifact(context.Background(), 7, 9, tc.cap)
			if !errors.Is(err, tc.want) || body.consumed != tc.wantRead || !body.closed {
				t.Fatalf("len=%d err=%v body=%+v", len(b), err, body)
			}
		})
	}
}

func TestArtifactEncodingAndBlobRedirectRefusal(t *testing.T) {
	// Inspect the actual production transport as well as the response refusal seam.
	transport := artifactBlobTransport(nil, "8.8.8.8:443")
	if !transport.DisableCompression {
		t.Fatal("blob transport must preserve raw Content-Encoding and byte accounting")
	}
	for _, enc := range [][]string{{"gzip"}, {"identity", "identity"}, {"identity, gzip"}, {"br"}, {""}} {
		for _, stage := range []string{"list", "blob"} {
			t.Run(stage+fmt.Sprint(enc), func(t *testing.T) {
				body := &artifactCountingBody{remaining: 100}
				response := func() *http.Response {
					r := artifactResponse(200, "")
					r.Body = body
					r.Header["Content-Encoding"] = enc
					return r
				}
				g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
					if stage == "list" {
						return response(), nil
					}
					return artifactRedirect("https://blob.example/"), nil
				})
				g.artifactHooks.blob = artifactRT(func(*http.Request) (*http.Response, error) { return response(), nil })
				var err error
				if stage == "list" {
					_, err = g.ListRunArtifacts(context.Background(), 7, 8)
				} else {
					_, err = g.DownloadArtifact(context.Background(), 7, 9, 3)
				}
				if err == nil || body.reads != 0 || !body.closed {
					t.Fatalf("err=%v body=%+v", err, body)
				}
			})
		}
	}
	for _, status := range []int{200, 302} {
		body := &artifactCountingBody{remaining: 100}
		calls := 0
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			return artifactRedirect("https://blob.example/?signature=secret"), nil
		})
		g.artifactHooks.blob = artifactRT(func(*http.Request) (*http.Response, error) {
			calls++
			r := artifactResponse(status, "")
			r.Body = body
			r.Uncompressed = true
			r.Header.Set("Location", "https://other.example/")
			return r, nil
		})
		_, err := g.DownloadArtifact(context.Background(), 7, 9, 3)
		if err == nil || calls != 1 || body.reads != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("err=%v calls=%d body=%+v", err, calls, body)
		}
	}
}

func TestArtifactListCapsPaginationAndProvenance(t *testing.T) {
	t.Run("pages ignore link authority", func(t *testing.T) {
		calls := 0
		g := artifactDriver(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Query().Get("page") != fmt.Sprint(calls) || r.URL.Host != "api.github.com" {
				t.Fatal(r.URL)
			}
			resp := artifactResponse(200, fmt.Sprintf(`{"total_count":2,"artifacts":[%s]}`, artifactJSON(calls, "")))
			resp.Header.Set("Link", `<https://evil.invalid/steal>; rel="next"`)
			return resp, nil
		})
		got, err := g.ListRunArtifacts(context.Background(), 7, 8)
		if err != nil || len(got) != 2 || calls != 2 {
			t.Fatalf("%+v %v calls=%d", got, err, calls)
		}
	})
	for _, body := range []string{
		`{}`,
		`{"total_count":0}`,
		`{"total_count":null,"artifacts":[]}`,
		`{"total_count":101,"artifacts":[]}`,
		`{"total_count":1,"artifacts":[]}`,
		`{"total_count":0,"artifacts":[` + artifactJSON(1, "") + `]}`,
		`{"total_count":1,"artifacts":[` + strings.Replace(artifactJSON(1, ""), `"id":8`, `"id":88`, 1) + `]}`,
		`{"total_count":1,"artifacts":[` + strings.Replace(artifactJSON(1, ""), `"repository_id":7`, `"repository_id":77`, 1) + `]}`,
		`{"total_count":1,"artifacts":[` + strings.Replace(artifactJSON(1, ""), "evidence", strings.Repeat("n", 1025), 1) + `]}`,
		`{"total_count":2,"artifacts":[` + artifactJSON(1, "") + "," + artifactJSON(1, "") + `]}`,
		`{"total_count":0,"unknown":"` + strings.Repeat("x", int(artifactPageBytes)) + `"}`,
	} {
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) { return artifactResponse(200, body), nil })
		if got, err := g.ListRunArtifacts(context.Background(), 7, 8); err == nil || got != nil {
			t.Fatalf("accepted invalid list: %+v %v", got, err)
		}
	}
	t.Run("changing totals", func(t *testing.T) {
		calls := 0
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			calls++
			return artifactResponse(200, fmt.Sprintf(`{"total_count":%d,"artifacts":[%s]}`, 4-calls, artifactJSON(calls, ""))), nil
		})
		if _, err := g.ListRunArtifacts(context.Background(), 7, 8); err == nil || calls != 2 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
	t.Run("exact count limit", func(t *testing.T) {
		items := make([]string, 100)
		for i := range items {
			items[i] = artifactJSON(i+1, "")
		}
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			return artifactResponse(200, `{"total_count":100,"artifacts":[`+strings.Join(items, ",")+`]}`), nil
		})
		got, err := g.ListRunArtifacts(context.Background(), 7, 8)
		if err != nil || len(got) != 100 || got[99].ID != 100 {
			t.Fatalf("len=%d err=%v", len(got), err)
		}
	})
	t.Run("finite pages", func(t *testing.T) {
		calls := 0
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			calls++
			return artifactResponse(200, fmt.Sprintf(`{"total_count":100,"artifacts":[%s]}`, artifactJSON(calls, ""))), nil
		})
		if _, err := g.ListRunArtifacts(context.Background(), 7, 8); !errors.Is(err, ErrArtifactBudget) || calls != artifactMaxPages {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
	t.Run("aggregate bytes", func(t *testing.T) {
		calls := 0
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			calls++
			return artifactResponse(200, fmt.Sprintf(`{"total_count":100,"artifacts":[%s],"unknown":"%s"}`, artifactJSON(calls, ""), strings.Repeat("x", 900000))), nil
		})
		if _, err := g.ListRunArtifacts(context.Background(), 7, 8); !errors.Is(err, ErrArtifactBudget) || calls != 5 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
}

func TestArtifactRateErrorsSanitizedWithoutBodyRead(t *testing.T) {
	for _, status := range []int{403, 429} {
		body := &artifactCountingBody{remaining: 10000}
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			r := artifactResponse(status, "")
			r.Body = body
			r.Header.Set("X-RateLimit-Remaining", "0")
			r.Header.Set("Retry-After", "12")
			return r, nil
		})
		_, err := g.ListRunArtifacts(context.Background(), 7, 8)
		var rate *RateLimitError
		if !errors.As(err, &rate) || rate.Retry != 12*time.Second || body.reads != 0 || !body.closed {
			t.Fatalf("err=%v body=%+v", err, body)
		}
	}
}

func TestArtifactTransportErrorsDoNotExposeCredentialsOrSignedURLs(t *testing.T) {
	secret := "github_pat_" + strings.Repeat("b", 82)
	signed := "https://blob.example/archive?signature=" + secret
	for _, stage := range []string{"api", "blob"} {
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			if stage == "api" {
				return nil, errors.New("upstream " + secret + " " + signed)
			}
			return artifactRedirect(signed), nil
		})
		g.artifactHooks.blob = artifactRT(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("upstream " + secret + " " + signed)
		})
		_, err := g.DownloadArtifact(context.Background(), 7, 9, 3)
		if err != errArtifactRequest || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), signed) {
			t.Fatalf("%s transport error was not sanitized", stage)
		}
	}
}

func TestArtifactInvalidArgumentsNeverRequest(t *testing.T) {
	g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid arguments sent request")
		return nil, nil
	})
	for _, args := range [][3]int64{{0, 9, 3}, {7, 0, 3}, {7, 9, 0}, {7, 9, -1}} {
		if _, err := g.DownloadArtifact(context.Background(), args[0], args[1], args[2]); err == nil {
			t.Fatal("accepted invalid download arguments")
		}
	}
	for _, args := range [][2]int64{{0, 8}, {7, 0}, {-1, 8}} {
		if _, err := g.ListRunArtifacts(context.Background(), args[0], args[1]); err == nil {
			t.Fatal("accepted invalid list arguments")
		}
	}
}

func TestArtifactUnsupportedDrivers(t *testing.T) {
	for _, driver := range []Forge{&gitLab{}, &forgejo{}} {
		if _, err := driver.ListRunArtifacts(context.Background(), 7, 8); !errors.Is(err, ErrForgeVersionUnsupported) {
			t.Fatal(err)
		}
		if _, err := driver.DownloadArtifact(context.Background(), 7, 9, 3); !errors.Is(err, ErrForgeVersionUnsupported) {
			t.Fatal(err)
		}
	}
}

func TestArtifactWorkflowAttemptAndIdentity(t *testing.T) {
	for _, attempt := range []string{"", `,"run_attempt":2`} {
		// Decode through the existing public single-run seam and its SDK mapping.
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			t.Fatal("artifact transport used by workflow seam")
			return nil, nil
		})
		g.slugs[7] = repoSlug{owner: "acme", repo: "widgets"}
		var clientErr error
		g.client, clientErr = gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: artifactRT(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/repos/acme/widgets/actions/runs/8" {
				t.Fatal(r.URL)
			}
			return artifactResponse(200, `{"id":8,"workflow_id":5,"repository":{"id":7},"head_repository":{"id":7}`+attempt+`}`), nil
		})}))
		if clientErr != nil {
			t.Fatal(clientErr)
		}
		got, err := g.GetWorkflowRun(context.Background(), 7, 8)
		if err != nil || got.RepositoryID != 7 || got.HeadRepositoryID != 7 || got.WorkflowKey != "5" {
			t.Fatalf("run=%+v err=%v", got, err)
		}
		if attempt == "" {
			if got.RunAttempt != nil {
				t.Fatal("unknown attempt became known")
			}
		} else if got.RunAttempt == nil || *got.RunAttempt != 2 {
			t.Fatalf("attempt %v", got.RunAttempt)
		}
	}
}
