package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// ErrHibernationConflict is returned by ReportHibernation when the api answers 409:
// the transition is stale or illegal for the worker's current hibernation state (the
// operation was superseded, the claim moved on, or the state machine refuses the
// step). It is FINAL for the tick that received it: the caller does not retry within
// the tick, and the next poll shows the api's actual state. It is distinct from every
// other failure because those are ambiguous (the write may or may not have landed)
// while a 409 is the api's explicit, definite no.
var ErrHibernationConflict = errors.New("apiclient: hibernation transition conflict")

// ReportHibernation posts one hibernation transition (PRD #4205) to
// /api/controller/workers/{id}/hibernation.
//
// Like RequestDrain this is a CONTROL-WRITE, so it fails closed: a 404 (an api that
// predates the endpoint) is an error, not a success. A 409 maps to
// ErrHibernationConflict; any other non-2xx is a plain error. The response body is
// never read.
func (c *Client) ReportHibernation(ctx context.Context, workerID string, t protocol.HibernationTransition) error {
	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("apiclient: marshal hibernation transition: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/controller/workers/"+url.PathEscape(workerID)+"/hibernation", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("apiclient: build hibernation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("apiclient: hibernation request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusConflict:
		return ErrHibernationConflict
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("apiclient: hibernation request: api returned %d", resp.StatusCode)
	}
	return nil
}
