package uzicli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// CrossCheckHistoryOptions selects one bounded page. Zero limits use API defaults.
// Cursors are opaque: the server validates their filter binding and ownership.
type CrossCheckHistoryOptions struct {
	Lifecycle      string
	Stage          string
	Outcome        string
	Cursor         string
	AttemptCursors []string
	Limit          int
	AttemptLimit   int
}

// Validate checks transport bounds without decoding opaque cursor contents.
func (o CrossCheckHistoryOptions) Validate() error {
	_, err := o.query()
	return err
}

func (o CrossCheckHistoryOptions) query() (string, error) {
	if o.Lifecycle == "" {
		o.Lifecycle = "active"
	}
	if o.Stage == "" {
		o.Stage = "all"
	}
	if o.Outcome == "" {
		o.Outcome = "any"
	}
	if o.Limit == 0 {
		o.Limit = 25
	}
	if o.AttemptLimit == 0 {
		o.AttemptLimit = 10
	}
	bad := func() (string, error) {
		return "", Exitf(ExitUsage, "invalid cross-check history filters, limits or cursors")
	}
	if !slices.Contains([]string{"active", "past"}, o.Lifecycle) ||
		!slices.Contains([]string{"all", "plan", "code"}, o.Stage) ||
		!slices.Contains([]string{"any", "didnt_finish", "raised_issues", "passed", "replaced"}, o.Outcome) ||
		(o.Outcome != "any" && o.Lifecycle != "past") ||
		o.Limit < 1 || o.Limit > 25 || o.AttemptLimit < 2 || o.AttemptLimit > 20 ||
		len(o.AttemptCursors) > 8 || (o.Cursor != "" && len(o.AttemptCursors) > 0) ||
		len(o.Cursor) > 512 || len(url.QueryEscape(o.Cursor)) > 512 {
		return bad()
	}
	v := url.Values{
		"lifecycle": {o.Lifecycle}, "stage": {o.Stage}, "outcome": {o.Outcome},
		"limit": {strconv.Itoa(o.Limit)}, "attempt_limit": {strconv.Itoa(o.AttemptLimit)},
	}
	if o.Cursor != "" {
		v.Set("cursor", o.Cursor)
	}
	seen := map[string]bool{}
	total := 0
	for _, token := range o.AttemptCursors {
		if len(token) > 256 {
			return bad()
		}
		n := len(url.QueryEscape(token))
		total += n
		if token == "" || n > 256 || total > 3072 || seen[token] {
			return bad()
		}
		seen[token] = true
		v.Add("attempt_cursor", token)
	}
	path := "/api/cross-checks?" + v.Encode()
	if len(path) > 4096 {
		return bad()
	}
	return path, nil
}

// CrossCheckHistory makes exactly one GET. It never follows either cursor family.
func (c *HTTPClient) CrossCheckHistory(ctx context.Context, opts CrossCheckHistoryOptions) (apitypes.CrossCheckHistoryDTO, error) {
	var out apitypes.CrossCheckHistoryDTO
	path, err := opts.query()
	if err != nil {
		return out, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return out, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, transportExit(c.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	const envelopeLimit = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, envelopeLimit+1))
	if err != nil {
		return out, Exitf(ExitUnreachable, "cross-check history response could not be read")
	}
	if len(body) > envelopeLimit {
		return out, Exitf(ExitGeneric, "cross-check history response exceeded 2 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Preserve existing status mapping without exposing structured server errors.
		e := statusError(resp.StatusCode, nil, resp.Header.Get("Retry-After"))
		return out, e
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, Exitf(ExitGeneric, "invalid cross-check history response")
	}
	return out, nil
}

func (f *FakeClient) CrossCheckHistory(ctx context.Context, opts CrossCheckHistoryOptions) (apitypes.CrossCheckHistoryDTO, error) {
	if err := opts.Validate(); err != nil {
		return apitypes.CrossCheckHistoryDTO{}, err
	}
	opts.AttemptCursors = slices.Clone(opts.AttemptCursors)
	f.CrossCheckHistoryCalls = append(f.CrossCheckHistoryCalls, opts)
	f.LastCrossCheckHistoryCtx = ctx
	if f.CrossCheckHistoryErr != nil {
		return apitypes.CrossCheckHistoryDTO{}, f.CrossCheckHistoryErr
	}
	if f.Err != nil {
		return apitypes.CrossCheckHistoryDTO{}, f.Err
	}
	out := f.CrossCheckHistoryResult
	if out.Groups == nil {
		out.Groups = []apitypes.CrossCheckHistoryGroupDTO{}
	}
	return out, nil
}
