package agentsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func TestAgentSourceHTTPTransportHealth(t *testing.T) {
	firstBudget := &wireBudget{remaining: maxCloneWireBytes}
	secondBudget := &wireBudget{remaining: maxCloneWireBytes}
	first := agentSourceHTTPClient(firstBudget, nil)
	second := agentSourceHTTPClient(secondBudget, func(string) bool { return true })
	firstWrapper, firstOK := first.Transport.(*boundedRoundTripper)
	secondWrapper, secondOK := second.Transport.(*boundedRoundTripper)
	if !firstOK || !secondOK {
		t.Fatal("helper clients must use bounded round trippers")
	}
	if first == second || firstWrapper == secondWrapper || firstWrapper.budget == secondWrapper.budget {
		t.Fatal("clients, wrappers and budgets must be distinct per operation")
	}
	if firstWrapper.budget != firstBudget || secondWrapper.budget != secondBudget {
		t.Fatal("helper must retain each operation's budget")
	}
	pool, ok := firstWrapper.base.(*http.Transport)
	if !ok || pool == nil {
		t.Fatal("helper must use a concrete HTTP connection pool")
	}
	if pool != agentSourceHTTPTransport || secondWrapper.base != pool || firstWrapper.base == http.DefaultTransport {
		t.Fatal("helper clients must share the independent agent-source pool")
	}
	if pool.HTTP2 == nil || pool.HTTP2.SendPingTimeout != 30*time.Second || pool.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("HTTP/2 health configuration = %+v, want 30s send / 15s ping", pool.HTTP2)
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.com/info/refs", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.CheckRedirect(req, nil) == nil || second.CheckRedirect(req, nil) != nil {
		t.Fatal("each helper client must retain its own redirect predicate")
	}
}

func TestAgentSourceHTTPTransportWiring(t *testing.T) {
	// Nonparallel: the dial hook belongs to the persistent pool. Close idle
	// connections before each path so that path must pass through the hook.
	pool := agentSourceHTTPTransport
	pool.CloseIdleConnections()
	originalDial := pool.DialContext
	var dials atomic.Int64
	pool.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return originalDial(ctx, network, address)
	}
	t.Cleanup(func() {
		pool.CloseIdleConnections()
		pool.DialContext = originalDial
	})

	const sha = "1111111111111111111111111111111111111111"
	packet := func(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }
	advertisement := packet("# service=git-upload-pack\n") + "0000" +
		packet(sha+" HEAD\x00symref=HEAD:refs/heads/main\n") +
		packet(sha+" refs/heads/main\n") + "0000"
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/roster.git/info/refs" ||
			r.URL.Query().Get("service") != "git-upload-pack" {
			http.Error(w, "unexpected git request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, advertisement)
	}))
	t.Cleanup(server.Close)
	endpoint, err := transport.NewEndpoint(server.URL + "/roster.git")
	if err != nil {
		t.Fatal(err)
	}
	allowSelf := func(raw string) bool { return strings.HasPrefix(raw, server.URL+"/") }

	for _, path := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"transport sessions and cumulative budgets", func(t *testing.T) {
			first, firstBudget, err := transportForEndpoint(endpoint, allowSelf)
			if err != nil {
				t.Fatal(err)
			}
			second, secondBudget, err := transportForEndpoint(endpoint, allowSelf)
			if err != nil {
				t.Fatal(err)
			}
			if firstBudget == nil || secondBudget == nil || firstBudget == secondBudget {
				t.Fatal("transport operations must have distinct budgets")
			}
			if firstBudget.remaining != maxCloneWireBytes || secondBudget.remaining != maxCloneWireBytes {
				t.Fatal("each operation must start with the full wire budget")
			}
			readRefs := func(tr transport.Transport) error {
				session, err := tr.NewUploadPackSession(endpoint, nil)
				if err != nil {
					return err
				}
				defer func() {
					if err := session.Close(); err != nil {
						t.Errorf("close upload-pack session: %v", err)
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				refs, err := session.AdvertisedReferencesContext(ctx)
				if err == nil && (refs.Head == nil || *refs.Head != plumbing.NewHash(sha)) {
					t.Fatal("session did not decode the advertised HEAD")
				}
				return err
			}
			for i := 1; i <= 2; i++ {
				if err := readRefs(first); err != nil {
					t.Fatalf("first operation advertisement %d: %v", i, err)
				}
				if got, want := atomic.LoadInt64(&firstBudget.remaining), int64(maxCloneWireBytes-i*len(advertisement)); got != want {
					t.Fatalf("cumulative remaining budget = %d, want %d", got, want)
				}
				if secondBudget.tripped() || atomic.LoadInt64(&secondBudget.remaining) != maxCloneWireBytes {
					t.Fatal("first operation consumed the second operation's budget")
				}
			}
			// Reduce only this test operation's allowance to cross its cap on
			// the next local advertisement without sending a 48 MiB fixture.
			atomic.StoreInt64(&firstBudget.remaining, int64(len(advertisement)-1))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/roster.git/info/refs?service=git-upload-pack", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := agentSourceHTTPClient(firstBudget, allowSelf).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if !errors.Is(readErr, errCloneWireBudget) {
				t.Fatalf("over-budget local read error = %v, want wire-budget error", readErr)
			}
			if !firstBudget.tripped() || secondBudget.tripped() ||
				atomic.LoadInt64(&secondBudget.remaining) != maxCloneWireBytes {
				t.Fatal("tripping the first operation must leave the second untouched")
			}
			if err := readRefs(second); err != nil {
				t.Fatalf("independent second operation: %v", err)
			}
			if secondBudget.tripped() || atomic.LoadInt64(&secondBudget.remaining) != int64(maxCloneWireBytes-len(advertisement)) {
				t.Fatal("second operation must read with its own untripped budget")
			}
		}},
		{"ls-remote", func(t *testing.T) {
			refs, err := ListRemoteRefs(context.Background(), CloneOptions{
				CloneURL: endpoint.String(), RedirectAllowed: allowSelf,
			})
			if err != nil || refs.HeadSHA != sha || refs.Branches["main"] != sha {
				t.Fatalf("ListRemoteRefs = %+v, %v; want advertised HEAD and main", refs, err)
			}
		}},
		{"fetch advertisement", func(t *testing.T) {
			_, _, err := FetchRoleFiles(context.Background(), CloneOptions{
				CloneURL: endpoint.String(), Ref: "missing", RedirectAllowed: allowSelf,
			})
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("fetch must decode refs before rejecting missing ref: %v", err)
			}
		}},
	} {
		t.Run(path.name, func(t *testing.T) {
			pool.CloseIdleConnections()
			beforeDials, beforeRequests := dials.Load(), requests.Load()
			path.run(t)
			if requests.Load() <= beforeRequests {
				t.Error("path did not reach the local advertisement server")
			}
			if dials.Load() <= beforeDials {
				t.Error("agent-source pool dial marker was not reached")
			}
		})
	}
}
