package uzicli

// client_recovery.go holds the durable-recovery archive client methods (PRD #1296 M5,
// consuming the M2 owner API). Both are owner-scoped (RequireUser + strict GetRun
// owner-or-404 server-side) and reach only the caller's OWN runs. The summary read goes
// through the ordinary JSON path; the download is a raw byte STREAM, deliberately off
// doJSONRead — see DownloadRecoveryArchive.

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// RecoveryArchives fetches a run's owner-scoped recovery summary (metadata only). The
// server returns the RecoveryArchiveSummaryDTO directly (unenveloped), with Archives
// ALWAYS a JSON array (never null) — a run with no captures yields the zero-value
// summary, which callers render as an honest "none/unsupported" rather than a false claim.
func (c *HTTPClient) RecoveryArchives(ctx context.Context, runID string) (apitypes.RecoveryArchiveSummaryDTO, error) {
	var out apitypes.RecoveryArchiveSummaryDTO
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(runID)+"/archives", &out); err != nil {
		return apitypes.RecoveryArchiveSummaryDTO{}, err
	}
	return out, nil
}

// DownloadRecoveryArchive streams one capture's decrypted bundle bytes to w and returns
// the number of bytes copied.
//
// It is built on newRequest (so credentialSafeBase gates the Bearer credential exactly
// like every other request) + c.HTTP.Do + io.Copy, NOT on doJSONRead: doJSONRead caps the
// body at 32 MiB and JSON-decodes it, both fatal for a bundle up to the server's 64 MiB
// ceiling. Nothing here buffers a whole bundle — the bytes flow straight from the response
// body into w.
//
// A non-2xx status is classified BEFORE any byte reaches w (statusError → the documented
// exit code: an expired/unavailable capture is a 409 → exit 5, an unknown one a 404 →
// exit 4, an integrity refusal a 422 → exit 2). Once the 200 stream has begun, the server
// aborts a corrupt/truncated response mid-body rather than serving unauthenticated
// plaintext; that surfaces here as an io.Copy read error, returned as ExitUnreachable with
// the bytes-so-far, so the caller detects the short read and never publishes a partial
// file.
func (c *HTTPClient) DownloadRecoveryArchive(ctx context.Context, runID, captureID string, w io.Writer) (int64, error) {
	path := "/api/runs/" + url.PathEscape(runID) + "/archives/" + url.PathEscape(captureID) + "/download"
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	// The body is an octet-stream, not JSON — override the JSON Accept newRequest set.
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Dial refused, DNS, TLS, timeout, context deadline, or a refused redirect: the
		// server is effectively unreachable, as with doJSONRead.
		return 0, Exitf(ExitUnreachable, "cannot reach uzi at %s: %v", c.BaseURL, transportMsg(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		// A non-2xx download carries a small JSON error body, never bundle bytes — read a
		// capped amount and map the status to the documented exit code, the same contract
		// decode2xx applies to the JSON verbs.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
		return 0, statusError(resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		// A read error here means the 200 stream ended early (Content-Length promised more
		// than arrived, or the connection dropped): an interrupted download. Nonzero exit,
		// and the caller leaves NO file at the destination.
		return n, Exitf(ExitUnreachable, "download interrupted after %d bytes: %v", n, transportMsg(err))
	}
	return n, nil
}

// RecoveryHolds fetches the caller's owner-wide custody holds + aggregate (PRD #1349 M5, D7).
// Owner-scoped server-side; Holds is always a JSON array (never null). `uzi run recovery`
// narrows to one run client-side.
func (c *HTTPClient) RecoveryHolds(ctx context.Context) (apitypes.RecoveryCustodyHoldsDTO, error) {
	var out apitypes.RecoveryCustodyHoldsDTO
	if err := c.get(ctx, "/api/recovery/holds", &out); err != nil {
		return apitypes.RecoveryCustodyHoldsDTO{}, err
	}
	return out, nil
}

// DiscardRecoveryHold discards ONE exact owner-owned open custody hold (PRD #1349 M5, D7/D9).
// It ALWAYS sends ?confirm=discard — the server's only mutating form — so the human
// confirmation (`uzi run discard`'s interactive prompt or --yes) is what precedes this call,
// and the query field is the wire confirmation the server requires. A non-2xx maps to the
// documented exit code (a 404 for a foreign/absent/already-settled hold → ExitNotFound).
func (c *HTTPClient) DiscardRecoveryHold(ctx context.Context, runID, holdID string) error {
	path := "/api/runs/" + url.PathEscape(runID) + "/recovery-holds/" + url.PathEscape(holdID) + "?confirm=discard"
	return c.del(ctx, path)
}
