package milestonelanes

import "testing"

func TestParseMilestoneTag(t *testing.T) {
	cases := []struct {
		name        string
		description string
		wantID      string
		wantRest    string
		wantOK      bool
	}{
		{
			name:        "well-formed tag with label",
			description: "[m2] Review placement",
			wantID:      "m2",
			wantRest:    "Review placement",
			wantOK:      true,
		},
		{
			name:        "surrounding whitespace trimmed",
			description: "  [m2]  Review ",
			wantID:      "m2",
			wantRest:    "Review",
			wantOK:      true,
		},
		{
			name:        "tag only, empty label",
			description: "[m2]",
			wantID:      "m2",
			wantRest:    "",
			wantOK:      true,
		},
		{
			name:        "untagged description",
			description: "Review placement",
			wantID:      "",
			wantRest:    "Review placement",
			wantOK:      false,
		},
		{
			name:        "empty brackets",
			description: "[] foo",
			wantID:      "",
			wantRest:    "[] foo",
			wantOK:      false,
		},
		{
			name:        "unclosed bracket",
			description: "[m2 Review",
			wantID:      "",
			wantRest:    "[m2 Review",
			wantOK:      false,
		},
		{
			name:        "empty string",
			description: "",
			wantID:      "",
			wantRest:    "",
			wantOK:      false,
		},
		{
			name:        "second bracket stays in rest",
			description: "[m2] [x] y",
			wantID:      "m2",
			wantRest:    "[x] y",
			wantOK:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, rest, ok := ParseMilestoneTag(tc.description)
			if id != tc.wantID || rest != tc.wantRest || ok != tc.wantOK {
				t.Fatalf("ParseMilestoneTag(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.description, id, rest, ok, tc.wantID, tc.wantRest, tc.wantOK)
			}
		})
	}
}

func TestMilestoneTagBinding(t *testing.T) {
	cases := []struct {
		name        string
		description string
		inProgress  map[string]bool
		wantID      string
		wantLabel   string
		wantBound   bool
	}{
		{
			name:        "member tag bound",
			description: "[m2] Review",
			inProgress:  map[string]bool{"m2": true},
			wantID:      "m2",
			wantLabel:   "Review",
			wantBound:   true,
		},
		{
			name:        "non-member tag dropped (D7)",
			description: "[m9] Review",
			inProgress:  map[string]bool{"m2": true},
			wantID:      "",
			wantLabel:   "",
			wantBound:   false,
		},
		{
			name:        "untagged dropped",
			description: "Review",
			inProgress:  map[string]bool{"m2": true},
			wantID:      "",
			wantLabel:   "",
			wantBound:   false,
		},
		{
			name:        "malformed tag dropped",
			description: "[m2 Review",
			inProgress:  map[string]bool{"m2": true},
			wantID:      "",
			wantLabel:   "",
			wantBound:   false,
		},
		{
			name:        "nil map drops every tag",
			description: "[m2] Review",
			inProgress:  nil,
			wantID:      "",
			wantLabel:   "",
			wantBound:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, label, bound := MilestoneTagBinding(tc.description, tc.inProgress)
			if id != tc.wantID || label != tc.wantLabel || bound != tc.wantBound {
				t.Fatalf("MilestoneTagBinding(%q, %v) = (%q, %q, %v), want (%q, %q, %v)",
					tc.description, tc.inProgress, id, label, bound, tc.wantID, tc.wantLabel, tc.wantBound)
			}
		})
	}
}
