package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

type transitionRefusal struct{ status int }

func (e transitionRefusal) Error() string {
	return fmt.Sprintf("apiclient: DinD transition refused: HTTP %d", e.status)
}
func (e transitionRefusal) HTTPStatus() int { return e.status }

// TransitionDinD performs one bounded control write. A refusal, including 409,
// leaves reconciliation waiting for the next poll; it never authorizes deletion.
func (c *Client) TransitionDinD(ctx context.Context, workerID string, op protocol.DindMaintenance) (protocol.DindMaintenance, error) {
	body, err := json.Marshal(op)
	if err != nil {
		return protocol.DindMaintenance{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/controller/workers/"+url.PathEscape(workerID)+"/dind-maintenance", bytes.NewReader(body))
	if err != nil {
		return protocol.DindMaintenance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return protocol.DindMaintenance{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return protocol.DindMaintenance{}, transitionRefusal{status: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return protocol.DindMaintenance{}, fmt.Errorf("apiclient: invalid DinD response")
	}
	var out protocol.DindMaintenance
	if json.Unmarshal(raw, &out) != nil || !validDinDResponse(op, out) {
		return protocol.DindMaintenance{}, fmt.Errorf("apiclient: invalid DinD binding")
	}
	return out, nil
}

func validDinDResponse(in, out protocol.DindMaintenance) bool {
	for _, v := range []string{out.ID, out.Nonce, out.RegisterNonce, out.DeploymentUID, out.PVCUID} {
		if v == "" || len(v) > 128 || v != strings.TrimSpace(v) || strings.ContainsAny(v, "\x00\r\n\t") {
			return false
		}
	}
	if out.DeploymentUID != in.DeploymentUID || out.PVCUID != in.PVCUID || out.Phase != in.Phase {
		return false
	}
	if in.ID != "" && out.ID != in.ID {
		return false
	}
	// Ready can atomically refresh the old registration binding after a legacy roll.
	if in.Phase != "requested" && in.Phase != "ready" && (out.Nonce != in.Nonce || out.RegisterNonce != in.RegisterNonce) {
		return false
	}
	switch out.Phase {
	case "requested":
		return true
	case "ready", "stopping", "recycling":
		return out.Fenced
	case "complete", "cancelled":
		return !out.Fenced
	default:
		return false
	}
}
