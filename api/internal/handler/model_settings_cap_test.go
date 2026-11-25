package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The 400 "errors" array must stay small whatever the caller sends: bounded entry count for
// unknown keys and bounded echo length for every caller-supplied string. No database.

func TestPutMyModelSettingsCapsUnknownFieldErrors(t *testing.T) {
	parts := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		parts = append(parts, fmt.Sprintf(`"<>&bogus-%04d":1`, i))
	}
	rec := putModelNoStore(t, "{"+strings.Join(parts, ",")+"}")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if n := rec.Body.Len(); n >= 4096 {
		t.Fatalf("response body is %d bytes, want < 4096", n)
	}
	errs := modelErrList(t, rec)
	if len(errs) != 5 {
		t.Fatalf("got %d errors, want exactly 5", len(errs))
	}
	for i, e := range errs {
		if want := fmt.Sprintf("<>&bogus-%04d", i); e.Field != want {
			t.Fatalf("errors[%d].field = %q, want %q (the first keys in sorted order)", i, e.Field, want)
		}
	}
}

func TestPutMyModelSettingsTruncatesEchoedUnknownKey(t *testing.T) {
	key := strings.Repeat("k", 100*1024)
	rec := putModelNoStore(t, `{"`+key+`":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if n := rec.Body.Len(); n >= 1024 {
		t.Fatalf("response body is %d bytes, want < 1024", n)
	}
	errs := modelErrList(t, rec)
	if len(errs) != 1 || len(errs[0].Field) > 64 || len(errs[0].Message) > 64+len("unknown field ") {
		t.Fatalf("errors = %+v, want one entry with a truncated field and message", errs)
	}
	if !strings.HasPrefix(key, errs[0].Field) || errs[0].Field == "" {
		t.Fatalf("truncated field %q is not a prefix of the key", errs[0].Field)
	}
}

func TestPutMyModelSettingsTruncatesEchoedPinStage(t *testing.T) {
	stage := strings.Repeat("s", 100*1024)
	rec := putModelNoStore(t, `{"cross_check_pins":[{"stage":"`+stage+`","harness":"claude"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if n := rec.Body.Len(); n >= 1024 {
		t.Fatalf("response body is %d bytes, want < 1024", n)
	}
	errs := modelErrList(t, rec)
	if len(errs) != 1 || len(errs[0].Stage) > 64 || len(errs[0].Message) > 200 {
		t.Fatalf("errors = %+v, want one entry with a truncated stage and message", errs)
	}
}

// A multi-byte key is cut on a rune boundary, so the echo stays valid UTF-8.
func TestPutMyModelSettingsTruncationKeepsRuneBoundary(t *testing.T) {
	key := "a" + strings.Repeat("é", 200) // byte 64 is a continuation byte, so a plain s[:64] would split a rune
	rec := putModelNoStore(t, `{"`+key+`":1}`)
	errs := modelErrList(t, rec)
	if len(errs) != 1 || len(errs[0].Field) > 64 || strings.ContainsRune(errs[0].Field, '�') {
		t.Fatalf("errors = %+v, want a valid truncated field", errs)
	}
}

func TestPutMyModelSettingsTruncatesEchoedPinHarness(t *testing.T) {
	harness := strings.Repeat("h", 100*1024)
	rec := putModelNoStore(t, `{"cross_check_pins":[{"stage":"plan","harness":"`+harness+`"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if n := rec.Body.Len(); n >= 1024 {
		t.Fatalf("response body is %d bytes, want < 1024", n)
	}
	errs := modelErrList(t, rec)
	if len(errs) != 1 || len(errs[0].Harness) > 64 || len(errs[0].Message) > 200 {
		t.Fatalf("errors = %+v, want one entry with a truncated harness and message", errs)
	}
}
