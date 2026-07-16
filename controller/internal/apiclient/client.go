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
func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: timeout},
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
