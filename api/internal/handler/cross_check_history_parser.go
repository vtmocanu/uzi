package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var errCrossCheckHistoryRequest = errors.New("invalid cross-check history request")

type crossCheckHistoryCursor struct {
	Version int       `json:"v"`
	Filter  string    `json:"f"`
	Parent  uuid.UUID `json:"p"`
	Time    string    `json:"t,omitempty"`
	Rank    int32     `json:"s,omitempty"`
	Round   int32     `json:"r,omitempty"`
	Kind    string    `json:"k"`
}

type crossCheckHistoryRequest struct {
	lifecycle, stage, outcome string
	limit, attemptLimit       int32
	cursor                    *crossCheckHistoryCursor
	continuations             []crossCheckHistoryCursor
}

func (p crossCheckHistoryRequest) filter() string {
	return p.lifecycle + "/" + p.stage + "/" + p.outcome
}

func parseCrossCheckHistoryRequest(r *http.Request) (crossCheckHistoryRequest, error) {
	p := crossCheckHistoryRequest{lifecycle: "active", stage: "all", outcome: "any", limit: 25, attemptLimit: 10}
	target := r.RequestURI
	if target == "" {
		target = r.URL.RequestURI()
	}
	if len(target) > 4096 {
		return p, errCrossCheckHistoryRequest
	}
	if !historyRawCursorBounds(r.URL.RawQuery) {
		return p, errCrossCheckHistoryRequest
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return p, err
	}
	for key, v := range values {
		if key != "attempt_cursor" && len(v) != 1 {
			return p, errCrossCheckHistoryRequest
		}
		switch key {
		case "lifecycle":
			p.lifecycle = v[0]
		case "stage":
			p.stage = v[0]
		case "outcome":
			p.outcome = v[0]
		case "limit", "attempt_limit":
			n, e := strconv.ParseInt(v[0], 10, 32)
			if e != nil {
				return p, errCrossCheckHistoryRequest
			}
			if key == "limit" {
				p.limit = int32(n)
			} else {
				p.attemptLimit = int32(n)
			}
		case "cursor", "attempt_cursor":
		default:
			return p, errCrossCheckHistoryRequest
		}
	}
	if !historyMember(p.lifecycle, "active", "past") || !historyMember(p.stage, "all", "plan", "code") ||
		!historyMember(p.outcome, "any", "didnt_finish", "raised_issues", "passed", "replaced") ||
		(p.outcome != "any" && p.lifecycle != "past") ||
		p.limit < 1 || p.limit > 25 || p.attemptLimit < 2 || p.attemptLimit > 20 {
		return p, errCrossCheckHistoryRequest
	}
	if v, ok := values["cursor"]; ok {
		c, _, e := decodeCrossCheckHistoryCursor(v[0], 512, p.filter(), "group")
		if e != nil {
			return p, e
		}
		p.cursor = &c
	}
	raw := values["attempt_cursor"]
	if len(raw) > 8 || (len(raw) > 0 && p.cursor != nil) {
		return p, errCrossCheckHistoryRequest
	}
	seen := map[uuid.UUID]bool{}
	total := 0
	for _, v := range raw {
		c, n, e := decodeCrossCheckHistoryCursor(v, 256, p.filter(), "attempt")
		total += n
		if e != nil || total > 3072 || seen[c.Parent] {
			return p, errCrossCheckHistoryRequest
		}
		seen[c.Parent] = true
		p.continuations = append(p.continuations, c)
	}
	return p, nil
}

// Check raw percent-encoded cursor bytes before ParseQuery decodes values.
func historyRawCursorBounds(raw string) bool {
	total := 0
	for _, pair := range strings.Split(raw, "&") {
		key, value, _ := strings.Cut(pair, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			return false
		}
		switch decodedKey {
		case "cursor":
			if len(value) > 512 {
				return false
			}
		case "attempt_cursor":
			if len(value) > 256 {
				return false
			}
			total += len(value)
			if total > 3072 {
				return false
			}
		}
	}
	return true
}

func historyMember(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

func encodeCrossCheckHistoryCursor(c crossCheckHistoryCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCrossCheckHistoryCursor(s string, max int, filter, kind string) (crossCheckHistoryCursor, int, error) {
	var c crossCheckHistoryCursor
	if s == "" || len(s) > max {
		return c, 0, errCrossCheckHistoryRequest
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != s {
		return c, 0, errCrossCheckHistoryRequest
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, len(raw), errCrossCheckHistoryRequest
	}
	if dec.Decode(new(any)) != io.EOF || c.Version != 1 || c.Filter != filter || c.Kind != kind || c.Parent == uuid.Nil {
		return c, len(raw), errCrossCheckHistoryRequest
	}
	if kind == "group" {
		t, e := time.Parse(time.RFC3339Nano, c.Time)
		if e != nil || t.IsZero() || t.Year() < 1 || t.Year() > 9999 || c.Rank != 0 || c.Round != 0 {
			return c, len(raw), errCrossCheckHistoryRequest
		}
	} else if c.Time != "" || c.Round < 1 || c.Rank < 0 || c.Rank > 1 || (c.Rank == 1 && c.Round != 1) {
		return c, len(raw), errCrossCheckHistoryRequest
	}
	// The token is a keyset, not authority. Every parent is reauthorized in SQL.
	if encodeCrossCheckHistoryCursor(c) != s || strings.TrimSpace(c.Filter) != c.Filter {
		return c, len(raw), errCrossCheckHistoryRequest
	}
	return c, len(raw), nil
}
