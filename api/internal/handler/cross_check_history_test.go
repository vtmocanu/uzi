package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func historyTestCursor(kind, filter string) crossCheckHistoryCursor {
	c := crossCheckHistoryCursor{Version: 1, Kind: kind, Filter: filter, Parent: uuid.New()}
	if kind == "group" {
		c.Time = "2026-11-02T01:02:03.123456Z"
	} else {
		c.Round = 4
	}
	return c
}

func TestCrossCheckHistoryParser(t *testing.T) {
	for _, tc := range []struct {
		query string
		valid bool
	}{
		{"", true}, {"lifecycle=past&stage=code&outcome=replaced&limit=25&attempt_limit=2", true},
		{"lifecycle=past&stage=plan&outcome=raised_issues&attempt_limit=20", true},
		{"stage=plan&outcome=raised_issues", false},
		{"outcome=passed", false},
		{"outcome=didnt_finish", false},
		{"lifecycle=unknown", false}, {"stage=unknown", false}, {"outcome=unknown", false},
		{"outcome=replaced", false}, {"limit=0", false}, {"limit=26", false}, {"attempt_limit=1", false},
		{"attempt_limit=21", false}, {"limit=x", false}, {"limit=9999999999999", false},
		{"stage=all&stage=code", false}, {"unknown=x", false}, {"cursor=", false},
		{"attempt_cursor=", false}, {"stage=%xx", false}, {"cursor=bad", false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			p, err := parseCrossCheckHistoryRequest(httptest.NewRequest("GET", "/api/cross-checks?"+tc.query, nil))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if tc.query == "" && (p.limit != 25 || p.attemptLimit != 10 || p.lifecycle != "active" || p.stage != "all" || p.outcome != "any") {
				t.Fatal("wrong defaults")
			}
		})
	}
	group := encodeCrossCheckHistoryCursor(historyTestCursor("group", "active/all/any"))
	attempt := encodeCrossCheckHistoryCursor(historyTestCursor("attempt", "active/all/any"))
	for _, q := range []string{
		"cursor=" + group + "&attempt_cursor=" + attempt,
		"attempt_cursor=" + attempt + "&attempt_cursor=" + attempt,
		"stage=code&cursor=" + group,
		"lifecycle=past&attempt_cursor=" + attempt,
		"cursor=" + attempt,
		"attempt_cursor=" + group,
		"attempt_cursor=" + strings.Repeat("a", 257),
		"cursor=" + strings.Repeat("a", 513),
	} {
		if _, err := parseCrossCheckHistoryRequest(httptest.NewRequest("GET", "/api/cross-checks?"+q, nil)); err == nil {
			t.Fatalf("accepted malformed %s", q)
		}
	}
	values := url.Values{}
	for i := 0; i < 8; i++ {
		values.Add("attempt_cursor", encodeCrossCheckHistoryCursor(historyTestCursor("attempt", "active/all/any")))
	}
	if p, err := parseCrossCheckHistoryRequest(httptest.NewRequest("GET", "/api/cross-checks?"+values.Encode(), nil)); err != nil || len(p.continuations) != 8 {
		t.Fatalf("valid eight parents: %v", err)
	}
	values.Add("attempt_cursor", encodeCrossCheckHistoryCursor(historyTestCursor("attempt", "active/all/any")))
	if _, err := parseCrossCheckHistoryRequest(httptest.NewRequest("GET", "/api/cross-checks?"+values.Encode(), nil)); err == nil {
		t.Fatal("accepted nine parents")
	}
	req := httptest.NewRequest("GET", "/api/cross-checks", nil)
	req.RequestURI = "/api/cross-checks?" + strings.Repeat("x", 4096)
	if _, err := parseCrossCheckHistoryRequest(req); err == nil {
		t.Fatal("accepted oversized target")
	}
}

func TestCrossCheckHistoryRawCursorBounds(t *testing.T) {
	for _, kind := range []string{"group", "attempt"} {
		c := historyTestCursor(kind, "past/plan/raised_issues")
		if kind == "group" {
			c.Time = "2026-11-02T01:02:03.123456789Z"
		}
		token := encodeCrossCheckHistoryCursor(c)
		var escaped strings.Builder
		for _, b := range []byte(token) {
			fmt.Fprintf(&escaped, "%%%02X", b)
		}
		key, maxBytes := "cursor", 512
		if kind == "attempt" {
			key, maxBytes = "attempt_cursor", 256
		}
		if escaped.Len() <= maxBytes {
			t.Fatal("fixture must exceed encoded-byte cap")
		}
		req := httptest.NewRequest("GET", "/api/cross-checks?lifecycle=past&stage=plan&outcome=raised_issues&"+key+"="+escaped.String(), nil)
		if _, err := parseCrossCheckHistoryRequest(req); err == nil {
			t.Fatal("accepted oversized raw encoded cursor")
		}
	}
}

func TestCrossCheckHistoryCursor(t *testing.T) {
	for _, kind := range []string{"group", "attempt"} {
		c := historyTestCursor(kind, "past/all/any")
		encoded := encodeCrossCheckHistoryCursor(c)
		max := 256
		if kind == "group" {
			max = 512
		}
		if len(encoded) > max {
			t.Fatal("producer exceeds token limit")
		}
		decoded, _, err := decodeCrossCheckHistoryCursor(encoded, max, c.Filter, kind)
		if err != nil || decoded != c {
			t.Fatalf("roundtrip: %+v %v", decoded, err)
		}
		for _, change := range []func(*crossCheckHistoryCursor){
			func(c *crossCheckHistoryCursor) { c.Version = 2 },
			func(c *crossCheckHistoryCursor) { c.Parent = uuid.Nil },
			func(c *crossCheckHistoryCursor) { c.Kind = "other" },
			func(c *crossCheckHistoryCursor) { c.Filter = "past/code/any" },
			func(c *crossCheckHistoryCursor) { c.Time = "invalid" },
			func(c *crossCheckHistoryCursor) { c.Rank = 3 },
			func(c *crossCheckHistoryCursor) { c.Round = -1 },
		} {
			bad := c
			change(&bad)
			if _, _, err := decodeCrossCheckHistoryCursor(encodeCrossCheckHistoryCursor(bad), max, c.Filter, kind); err == nil {
				t.Fatalf("accepted %+v", bad)
			}
		}
		raw, _ := json.Marshal(c)
		for _, bad := range []string{string(raw) + "{}", string(raw[:len(raw)-1]) + "," + `"secret":"value"}`, string(raw[:len(raw)-1]) + "," + `"v":1}`} {
			if _, _, err := decodeCrossCheckHistoryCursor(base64.RawURLEncoding.EncodeToString([]byte(bad)), max, c.Filter, kind); err == nil {
				t.Fatal("accepted noncanonical cursor")
			}
		}
	}
}

func TestCrossCheckHistoryMapper(t *testing.T) {
	child := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	for _, tc := range []struct {
		name, stage, result, category, reason, status, wantReview, wantExecution string
		child                                                                    bool
		count                                                                    int32
	}{
		{"reviseFinished", "plan", "revise", "", "revise", "completed", "changes_requested", "finished", true, 2},
		{"blockFinished", "plan", "block", "", "block", "completed", "blocked", "finished", true, 2},
		{"approvalACKLag", "plan", "approve", "", "approve", "running", "approved", "running", true, 0},
		{"unstoredApproval", "plan", "failed", "replaced", "approved_not_stored", "completed", "approved", "replaced", true, 0},
		{"invalidApprovalReason", "plan", "approve", "", "", "completed", "none", "unknown", true, 0},
		{"deletedChild", "plan", "approve", "", "approve", "", "approved", "unknown", false, 0},
		{"missingFailure", "plan", "failed", "", "checker_unavailable", "", "none", "didnt_finish", false, -1},
		{"prestart", "code", "failed", "", "snapshot_failed", "", "none", "couldnt_start", false, -1},
		{"pendingNoChild", "plan", "pending", "", "", "", "none", "pending", false, -1},
		{"codeIgnoresLegacyVerdict", "code", "completed", "", "", "completed", "changes_requested", "finished", true, 1},
		{"codePass", "code", "completed", "", "", "completed", "approved", "finished", true, 0},
		{"codeInvalidFindings", "code", "completed", "", "", "completed", "none", "unknown", true, -1},
		{"interrupted", "plan", "approve", "replaced", "approve", "completed", "approved", "replaced", true, 0},
		{"replaced", "plan", "failed", "replaced", "superseded", "cancelled", "none", "replaced", true, -1},
		{"unknownReason", "plan", "failed", "", "private provider error", "", "none", "didnt_finish", false, -1},
		{"unknownEnums", "bad", "private value", "", "", "", "none", "unknown", false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := store.CrossCheckHistoryAttemptsRow{Stage: tc.stage, Result: tc.result, Category: tc.category, ReasonClass: tc.reason, FindingsCount: tc.count}
			if tc.child {
				row.ChildID = child
				row.ChildStatus = pgtype.Text{String: tc.status, Valid: true}
			}
			got := crossCheckHistoryAttempt(row)
			if got.Execution != tc.wantExecution || got.Review != tc.wantReview {
				t.Fatalf("execution=%s review=%s", got.Execution, got.Review)
			}
			if !tc.child && (got.ChildID != nil || got.ChildContext != "No checker run available") {
				t.Fatal("guessed missing child")
			}
			if got.Effect != "Effect not recorded" || got.Reason == tc.reason || (tc.category == "replaced" && !got.Historical) {
				t.Fatal("invented authority or reflected raw reason")
			}
			if tc.reason == "private provider error" && got.Reason != "Cause not recorded for this attempt" {
				t.Fatal("reflected unknown reason")
			}
		})
	}
}

func TestCrossCheckHistorySanitizationAndEnvelope(t *testing.T) {
	token := "glpat-" + strings.Repeat("a", 20)
	in := "hello\n\t\u202e\u200d\x1b" + token + " world"
	want := "hello  [redacted] world"
	if got := crossCheckHistoryText(in, 512); got != want {
		t.Fatalf("sanitized=%q want=%q", got, want)
	}
	// A credential straddling the display boundary is scrubbed before truncation.
	if got := crossCheckHistoryText("abcd"+token, 9); got != "abcd[reda" {
		t.Fatalf("scrub before cap: %q", got)
	}
	if got := crossCheckHistoryText("a界界", 5); got != "a界" {
		t.Fatalf("UTF-8 cap: %q", got)
	}
	if got := crossCheckHistoryText(strings.Repeat("界", 4), 0); got != "" {
		t.Fatal("zero cap")
	}
	w := httptest.NewRecorder()
	writeCrossCheckHistory(w, map[string]string{"value": strings.Repeat("<", crossCheckHistoryEnvelopeLimit/5)})
	if w.Code != 500 || w.Body.Len() > crossCheckHistoryEnvelopeLimit {
		t.Fatal("escaping exceeded envelope cap")
	}
	w = httptest.NewRecorder()
	writeCrossCheckHistory(w, map[string]any{"groups": []any{}, "time": time.Unix(0, 0)})
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("bounded valid response")
	}
}
