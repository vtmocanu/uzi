package issuetracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// ytCall describes one request. path holds unescaped segments; each is
// escaped when the URL is built.
type ytCall struct {
	method string
	path   []string
	query  url.Values
	body   any
	limit  int64
	prov   forge.ResponseProvenance
	// decode, when set, replaces json.Unmarshal into out; ytPages uses it to
	// stream a list page.
	decode func([]byte) error
}

// escapedPath joins the segments, escaping every one.
func escapedPath(segments []string) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// do sends one request and decodes a successful JSON answer into out (nil to
// discard). It returns the HTTP status and the number of body bytes actually
// read. Every error is already redacted; a redirect is refused before any body
// is read; the response is read through a cap and decoded only from the capped
// buffer. There is no retry.
func (y *youTrack) do(ctx context.Context, c ytCall, out any) (status, n int, err error) {
	var reqBody *bytes.Reader
	if c.body != nil {
		b, err := json.Marshal(c.body)
		if err != nil {
			return 0, 0, y.red.error(err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	path := escapedPath(append([]string{"api"}, c.path...))
	target := y.baseURL + path
	if len(c.query) > 0 {
		target += "?" + c.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, c.method, target, reqBody)
	if err != nil {
		return 0, 0, y.red.error(err)
	}
	req.Header.Set("Authorization", "Bearer "+y.token)
	req.Header.Set("Accept", "application/json")
	if c.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := y.client.Do(req)
	if err != nil {
		return 0, 0, y.red.error(err)
	}
	defer func() { _ = resp.Body.Close() }()
	status = resp.StatusCode
	if isRedirect(resp) {
		return status, 0, y.red.sentinel(ErrRedirectRefused, status, fmt.Sprintf("%s %s answered HTTP %d", c.method, path, status))
	}
	if status < 200 || status > 299 {
		body, err := readBounded(resp.Body, errorBodyBytes, forge.ResponseError, status)
		if err != nil {
			return status, 0, y.red.errorStatus(err, status)
		}
		return status, len(body), y.red.errorStatus(fmt.Errorf("youtrack: %s %s: HTTP %d: %s", c.method, path, status, y.errorText(body)), status)
	}
	body, err := readBounded(resp.Body, c.limit, c.prov, status)
	if err != nil {
		return status, 0, y.red.errorStatus(err, status)
	}
	var decodeErr error
	switch {
	case c.decode != nil:
		decodeErr = c.decode(body)
	case out != nil:
		if valueCount(body) > objectValueBudget {
			decodeErr = errValueCeiling
		} else {
			decodeErr = json.Unmarshal(body, out)
		}
	}
	if decodeErr != nil {
		if errors.Is(decodeErr, errItemCeiling) {
			return status, len(body), y.red.sentinel(errItemCeiling, status, fmt.Sprintf("%s %s: decode response", c.method, path))
		}
		return status, len(body), y.red.errorStatus(fmt.Errorf("youtrack: %s %s: decode response: %w", c.method, path, decodeErr), status)
	}
	return status, len(body), nil
}

// errorText renders a YouTrack error body: its error and error_description
// members joined by ": ", or the raw text. The text is scrubbed whole before it
// is cut to rawLimit, so a token that straddles the cut cannot leave a prefix
// behind; the caller's redactor scrubs and truncates the result again.
func (y *youTrack) errorText(body []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil && (e.Error != "" || e.Description != "") {
		var parts []string
		for _, p := range []string{e.Error, e.Description} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, ": ")
	}
	const rawLimit = 200
	return truncate(y.red.scrub(string(body)), rawLimit)
}

// nester is implemented by a wire type that carries nested arrays; ytPages
// counts them toward the maxItems budget. Each array is already capped at
// maxNestedItems while it is decoded.
type nester interface {
	nestedCount() int
}

// ytPages walks every page of a list endpoint under paginate's ceilings and
// calls visit with each page. visit returns true to stop early. Items that
// implement nester count their nested entries toward maxItems. A page is
// counted by a byte scan and refused above pageSize items or the structural
// value budget before any element is decoded. Errors come back redacted.
func ytPages[T any](ctx context.Context, y *youTrack, path []string, query url.Values, visit func([]T) bool) error {
	var fetchErr error
	seen := 0
	_, err := paginate(func(skip, top int) ([]T, int, error) {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("$top", strconv.Itoa(top))
		q.Set("$skip", strconv.Itoa(skip))
		var items []T
		_, n, err := y.do(ctx, ytCall{
			method: http.MethodGet, path: path, query: q, limit: listPageBytes, prov: forge.ResponseSuccess,
			decode: func(b []byte) (err error) { items, err = decodePage[T](b); return err },
		}, nil)
		if err != nil {
			fetchErr = err
			return nil, 0, err
		}
		seen += len(items)
		for _, it := range items {
			if nn, ok := any(it).(nester); ok {
				seen += nn.nestedCount()
			}
		}
		if seen > maxItems {
			fetchErr = y.red.sentinel(errItemCeiling, 0, "")
			return nil, 0, fetchErr
		}
		if visit(items) {
			// A short page ends the walk; the items were already consumed.
			return nil, n, nil
		}
		return items, n, nil
	})
	if fetchErr != nil {
		return fetchErr
	}
	// A ceiling stays identifiable with errors.Is; anything else is scrubbed.
	if isPackageSentinel(err) {
		return y.red.sentinel(err, 0, "")
	}
	return y.red.error(err)
}

// ytList collects every item of a list endpoint.
func ytList[T any](ctx context.Context, y *youTrack, path []string, query url.Values) ([]T, error) {
	var all []T
	err := ytPages(ctx, y, path, query, func(page []T) bool {
		all = append(all, page...)
		return false
	})
	return all, err
}
