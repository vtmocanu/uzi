package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCodexAdmissionSignalValidation(t *testing.T) {
	valid := CodexAdmissionSignal{ClaimGeneration: 1, AttemptID: uuid.NewString(), Class: "rate_limit"}
	coordinate := func(v string) *string { return &v }
	cases := []struct {
		name   string
		change func(*CodexAdmissionSignal)
	}{
		{"missing generation", func(s *CodexAdmissionSignal) { s.ClaimGeneration = 0 }},
		{"negative generation", func(s *CodexAdmissionSignal) { s.ClaimGeneration = -1 }},
		{"unsafe generation", func(s *CodexAdmissionSignal) { s.ClaimGeneration = 1 << 53 }},
		{"unknown class", func(s *CodexAdmissionSignal) { s.Class = "rate" }},
		{"uppercase uuid", func(s *CodexAdmissionSignal) { s.AttemptID = strings.ToUpper(valid.AttemptID) }},
		{"uuid braces", func(s *CodexAdmissionSignal) { s.AttemptID = "{" + valid.AttemptID + "}" }},
		{"nil uuid", func(s *CodexAdmissionSignal) { s.AttemptID = uuid.Nil.String() }},
		{"long coordinate", func(s *CodexAdmissionSignal) { s.ProviderThreadID = coordinate(strings.Repeat("a", 129)) }},
		{"multibyte bytes", func(s *CodexAdmissionSignal) { s.ProviderThreadID = coordinate(strings.Repeat("é", 65)) }},
		{"empty coordinate", func(s *CodexAdmissionSignal) { s.ProviderTurnID = coordinate("") }},
		{"ansi", func(s *CodexAdmissionSignal) { s.ProviderTurnID = coordinate("\u001b[31mred") }},
		{"newline", func(s *CodexAdmissionSignal) { s.ProviderTurnID = coordinate("a\nb") }},
		{"bidi", func(s *CodexAdmissionSignal) { s.ProviderTurnID = coordinate("a\u202eb") }},
		{"invalid utf8", func(s *CodexAdmissionSignal) { s.ProviderTurnID = coordinate(string([]byte{0xff})) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.change(&s)
			if s.Validate() != ErrCodexAdmissionInvalid {
				t.Fatal("invalid signal accepted")
			}
		})
	}
	valid.ClaimGeneration = 1<<53 - 1
	valid.ProviderThreadID = coordinate(strings.Repeat("a", 128))
	valid.ProviderTurnID = coordinate("turn-α")
	for _, class := range []string{"overload", "rate_limit"} {
		valid.Class = class
		if err := valid.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
