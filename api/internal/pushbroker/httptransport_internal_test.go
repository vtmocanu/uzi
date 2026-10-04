package pushbroker

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
)

func TestBrokerHTTPTransportHealth(t *testing.T) {
	tr, ok := brokerHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("broker transport = %T, want *http.Transport", brokerHTTPClient.Transport)
	}
	if tr == http.DefaultTransport {
		t.Fatal("broker shares the process default connection pool")
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 30*time.Second || tr.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("broker HTTP/2 health = %+v, want 30s/15s", tr.HTTP2)
	}
	for _, protocol := range []string{"http", "https"} {
		ep := &transport.Endpoint{Protocol: protocol, Host: "example.com", Path: "/repo.git"}
		registered, err := client.NewClient(ep)
		if err != nil {
			t.Fatal(err)
		}
		manual, err := transportFor(ep)
		if err != nil {
			t.Fatal(err)
		}
		if registered != httpTransport || manual != httpTransport {
			t.Fatalf("%s registered/manual transports do not share httpTransport", protocol)
		}
	}
}
