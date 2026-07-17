// Package apiclient is the controller's HTTP client for the uzi api's
// controller-facing protocol (PRD #58 Decision 2).
//
// Outbound only. The controller listens on no port and the api never dials it —
// every exchange is this client's doing, which is what lets the controller live in
// the cluster with no inbound surface at all.
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
)

// maxErrorBodyBytes bounds how much of a non-2xx response we read back for the
// error message, so a misrouted request to something that streams cannot balloon
// the controller's memory.
const maxErrorBodyBytes = 4 << 10

// Client calls the uzi api's controller endpoints.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New constructs a Client. timeout bounds every call end to end (dial through
// body read), so a hung api can never wedge the reconcile loop.
//
// caPool verifies the api's TLS certificate (PRD #58 Decision 4). nil uses the
// system roots — right for a publicly-trusted cert, and the only option for an
// http base URL, where it is moot. It is never a way to skip verification: there
// is no InsecureSkipVerify knob here and there must not be one. The whole reason
// this hop is TLS is that its responses carry a decrypted forge PAT and Anthropic
// token across a shared cluster's pod network; an unverified peer on that hop is
// the exact attack the encryption exists to stop, so "TLS with verification off"
// would be strictly worse than the plain http it replaced — it would look solved.
func New(baseURL, token string, timeout time.Duration, caPool *x509.CertPool) *Client {
	// Clone the default transport rather than building one: it carries the
	// connection pooling, proxy handling and HTTP/2 support net/http has tuned, and
	// only the TLS roots need changing.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caPool != nil {
		tr.TLSClientConfig = &tls.Config{
			RootCAs:    caPool,
			MinVersion: tls.VersionTLS12,
		}
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: timeout, Transport: tr},
	}
}

// Poll returns the desired state of the hosted fleet.
//
// A pure read: this controller asserts nothing to the api. Token delivery is
// settled by the worker's own registration, not by anything sent from here — see
// protocol.go for why an ack from this side was removed rather than made more
// precise.
func (c *Client) Poll(ctx context.Context) (protocol.PollResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/controller/poll", nil)
	if err != nil {
		return protocol.PollResponse{}, fmt.Errorf("apiclient: build poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		// net/http puts the request URL in this error, never the Authorization
		// header, so there is no credential to scrub here.
		return protocol.PollResponse{}, fmt.Errorf("apiclient: poll: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return protocol.PollResponse{}, fmt.Errorf("apiclient: poll: api returned %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var out protocol.PollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// The body may carry join-token plaintext, so the decode error is reported
		// bare — json.Decode errors quote the offending token, and this response is
		// the one place a plaintext could end up in a log line.
		return protocol.PollResponse{}, fmt.Errorf("apiclient: decode poll response")
	}
	return out, nil
}
