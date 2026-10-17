package runpresentation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestPauseOrigin(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	base := apitypes.RunDTO{Status: "paused", StatusSince: &at}
	marker := func(seq int32, payload string, stamp time.Time) apitypes.MessageDTO {
		return apitypes.MessageDTO{Seq: seq, Kind: "paused", Payload: json.RawMessage(payload), CreatedAt: stamp}
	}
	for _, tc := range []struct{ name, payload, want string }{
		{"lead", `{"origin":"lead_park"}`, "lead_park"},
		{"owner", `{"origin":"owner"}`, "owner"},
		{"absent", `{}`, ""},
		{"unknown", `{"origin":"lead"}`, ""},
		{"wrong key case", `{"Origin":"owner"}`, ""},
		{"hostile", `{"origin":"owner\u001b[31m"}`, ""},
		{"number", `{"origin":1}`, ""},
		{"array", `[]`, ""},
		{"malformed", `{`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []apitypes.MessageDTO{marker(9, tc.payload, at), marker(8, `{"origin":"owner"}`, at)}
			if got := PauseOrigin(base, messages, true); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	valid := []apitypes.MessageDTO{marker(9, `{"origin":"lead_park"}`, at)}
	for _, tc := range []struct {
		name     string
		run      apitypes.RunDTO
		ready    bool
		messages []apitypes.MessageDTO
	}{
		{"unread", base, false, valid},
		{"missing marker", base, true, nil},
		{"stale", base, true, []apitypes.MessageDTO{marker(9, `{"origin":"owner"}`, at.Add(-time.Nanosecond))}},
		{"missing timestamp", base, true, []apitypes.MessageDTO{marker(9, `{"origin":"owner"}`, time.Time{})}},
		{"missing epoch", apitypes.RunDTO{Status: "paused"}, true, valid},
		{"zero epoch", apitypes.RunDTO{Status: "paused", StatusSince: new(time.Time)}, true, valid},
		{"resumed", apitypes.RunDTO{Status: "running", StatusSince: &at}, true, valid},
		{"hold", apitypes.RunDTO{Status: "paused", StatusSince: &at, HoldReason: ptr("budget_exhausted")}, true, valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PauseOrigin(tc.run, tc.messages, tc.ready); got != "" {
				t.Fatalf("got %q", got)
			}
		})
	}
	if got := PauseOrigin(base, append(valid, valid...), true); got != "lead_park" {
		t.Fatalf("dedup/equality got %q", got)
	}
	later := at.Add(time.Second)
	base.StatusSince = &later
	if got := PauseOrigin(base, valid, true); got != "" {
		t.Fatalf("new epoch got %q", got)
	}
}

func ptr(s string) *string { return &s }

func TestPauseCoverage(t *testing.T) {
	if ContiguousMessages(nil, 102, 100) {
		t.Fatal("higher floor proved unloaded history")
	}
	if ContiguousMessages([]apitypes.MessageDTO{{Seq: 1}, {Seq: 2147483647}}, 0, 2147483647) {
		t.Fatal("hostile jump accepted")
	}
	if !ContiguousMessages(nil, 100, 100) {
		t.Fatal("empty current interval rejected")
	}
	messages := []apitypes.MessageDTO{{Seq: 102}, {Seq: 100}, {Seq: 102}}
	if ContiguousMessages(messages, 99, 102) {
		t.Fatal("hole accepted")
	}
	messages = append(messages, apitypes.MessageDTO{Seq: 101})
	if !ContiguousMessages(messages, 99, 102) {
		t.Fatal("reordered deduped suffix rejected")
	}
}
