package pushbroker_test

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// These tests mutate the broker pool's dial hook and must remain nonparallel.
func countedBrokerDial(t *testing.T) (*http.Transport, *atomic.Int64) {
	t.Helper()
	tr, ok := pushbroker.BrokerHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("broker transport = %T, want *http.Transport", pushbroker.BrokerHTTPClient.Transport)
	}
	tr.CloseIdleConnections()
	previous := tr.DialContext
	var calls atomic.Int64
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls.Add(1)
		return previous(ctx, network, addr)
	}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		tr.DialContext = previous
	})
	return tr, &calls
}

func TestBrokerHTTPTransportWiring(t *testing.T) {
	backend := requireGitHTTPBackend(t)
	tr, calls := countedBrokerDial(t)
	f := newGitFixture(t)
	tip := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	r := newGitHTTPRemote(t, f, backend)
	// Plain loopback HTTP makes a bypassed pool fail the dial marker assertion,
	// rather than failing earlier because the default pool lacks the test CA.
	plain := httptest.NewServer(r.srv.Config.Handler)
	t.Cleanup(plain.Close)
	cloneURL := strings.Replace(r.cloneURL(f), r.srv.URL, plain.URL, 1)

	for _, path := range []string{"registered", "manual"} {
		t.Run(path, func(t *testing.T) {
			tr.CloseIdleConnections()
			before := calls.Load()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if path == "registered" {
				remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
					Name: "origin", URLs: []string{cloneURL},
				})
				refs, err := remote.ListContext(ctx, &git.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, ref := range refs {
					if ref.Name() == plumbing.Main && ref.Hash().String() == tip {
						found = true
					}
				}
				if !found {
					t.Fatalf("registered list did not advertise main at %s: %v", tip, refs)
				}
			} else {
				ep, err := transport.NewEndpoint(cloneURL)
				if err != nil {
					t.Fatal(err)
				}
				c, err := pushbroker.TransportFor(ep)
				if err != nil {
					t.Fatal(err)
				}
				s, err := c.NewReceivePackSession(ep, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				}()
				refs, err := s.AdvertisedReferencesContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if refs.References["refs/heads/main"].String() != tip {
					t.Fatalf("manual discovery refs = %v, want main at %s", refs.References, tip)
				}
			}
			if calls.Load() <= before {
				t.Fatal("smart-HTTP succeeded without dialing through the broker pool marker")
			}
			svc := transport.UploadPackServiceName
			if path == "manual" {
				svc = transport.ReceivePackServiceName
			}
			r.mu.Lock()
			seen := r.seen[svc]
			r.mu.Unlock()
			if seen != 1 {
				t.Fatalf("server discoveries for %s = %d, want 1", svc, seen)
			}
			tr.CloseIdleConnections()
		})
	}
}

// assertSessionPool checks go-git's option-specific clone without unsafe access
// or a production seam. Its session client is private, so reflect reads only the
// concrete transport type and exported HTTP2 scalar settings. A dependency layout
// change fails loudly here rather than silently weakening this contract check.
func assertSessionPool(t *testing.T, session transport.ReceivePackSession, base *http.Transport) {
	t.Helper()
	pool := reflect.ValueOf(session).Elem().FieldByName("session").Elem().
		FieldByName("client").Elem().FieldByName("Transport").Elem()
	if pool.Type() != reflect.TypeFor[*http.Transport]() {
		t.Fatalf("session pool type = %v, want *http.Transport", pool.Type())
	}
	if pool.Pointer() == reflect.ValueOf(base).Pointer() {
		t.Fatal("endpoint options reused the broker pool instead of cloning it")
	}
	h2 := pool.Elem().FieldByName("HTTP2")
	if h2.IsNil() || time.Duration(h2.Elem().FieldByName("SendPingTimeout").Int()) != 30*time.Second ||
		time.Duration(h2.Elem().FieldByName("PingTimeout").Int()) != 15*time.Second {
		t.Fatal("session pool lost the 30s/15s HTTP/2 health settings")
	}
	if h2.Pointer() == reflect.ValueOf(base.HTTP2).Pointer() {
		t.Fatal("session clone shares mutable HTTP/2 configuration with the broker pool")
	}
}

func TestBrokerHTTPTransportEndpointOptions(t *testing.T) {
	backend := requireGitHTTPBackend(t)
	tr, calls := countedBrokerDial(t)
	f := newGitFixture(t)
	tip := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	r := newGitHTTPRemote(t, f, backend)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.srv.Certificate().Raw})
	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxyHits.Add(1)
		r.srv.Config.Handler.ServeHTTP(w, req)
	}))
	t.Cleanup(proxy.Close)

	for _, option := range []string{"CA", "TLS", "proxy"} {
		for _, path := range []string{"registered", "manual"} {
			t.Run(option+"/"+path, func(t *testing.T) {
				tr.CloseIdleConnections()
				before, beforeProxy := calls.Load(), proxyHits.Load()
				ep, err := transport.NewEndpoint(r.cloneURL(f))
				if err != nil {
					t.Fatal(err)
				}
				switch option {
				case "CA":
					ep.CaBundle = ca
				case "TLS":
					ep.InsecureSkipTLS = true
				case "proxy":
					ep, err = transport.NewEndpoint("http://unreachable.invalid/" + strings.TrimPrefix(ep.Path, "/"))
					if err != nil {
						t.Fatal(err)
					}
					ep.Proxy = transport.ProxyOptions{URL: proxy.URL}
				}
				var c transport.Transport
				if path == "registered" {
					c, err = client.NewClient(ep)
				} else {
					c, err = pushbroker.TransportFor(ep)
				}
				if err != nil {
					t.Fatal(err)
				}
				s, err := c.NewReceivePackSession(ep, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				}()
				assertSessionPool(t, s, tr)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				refs, err := s.AdvertisedReferencesContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if refs.References["refs/heads/main"].String() != tip {
					t.Fatalf("option discovery refs = %v, want main at %s", refs.References, tip)
				}
				if calls.Load() <= before {
					t.Fatal("option-specific clone bypassed the broker dial marker")
				}
				if option == "proxy" && proxyHits.Load() != beforeProxy+1 {
					t.Fatal("proxy option did not reach the configured proxy")
				}
				if tr.TLSClientConfig != nil && (tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.RootCAs != nil) {
					t.Fatal("endpoint TLS options leaked into the broker pool")
				}
				if tr.HTTP2.SendPingTimeout != 30*time.Second || tr.HTTP2.PingTimeout != 15*time.Second {
					t.Fatal("endpoint options changed broker health settings")
				}
			})
		}
	}
}
