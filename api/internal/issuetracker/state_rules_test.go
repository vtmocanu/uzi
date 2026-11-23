package issuetracker

import "testing"

func TestClosableState(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    State
		want bool
	}{
		{"Fixed", State{Name: "Fixed", Resolved: true}, true},
		{"Done", State{Name: "Done", Resolved: true}, true},
		// A tab is a control character stateNameProblem refuses, so use spaces.
		{"normalised", State{Name: "  FIXED  ", Resolved: true}, true},
		{"separators collapse", State{Name: "Won_t--fix", Resolved: true}, false},
		{"Won't fix", State{Name: "Won't fix", Resolved: true}, false},
		{"Duplicate", State{Name: "Duplicate", Resolved: true}, false},
		{"Obsolete", State{Name: "Obsolete", Resolved: true}, false},
		{"Can't Reproduce", State{Name: "Can't Reproduce", Resolved: true}, false},
		{"archived Fixed", State{Name: "Fixed", Resolved: true, Archived: true}, false},
		{"unresolved Done", State{Name: "Done", Resolved: false}, false},
		// Closability is decided on Name alone: the localized name is free text
		// set by the same bundle admin, so a non-ASCII name labelled "Fixed" is
		// refused (this row expected true before), as is a done state named
		// only in another language.
		{"non-ASCII name, localized Fixed", State{Name: "Готово", LocalizedName: "Fixed", Resolved: true}, false},
		{"Won't fix localized as Fixed", State{Name: "Won't fix", LocalizedName: "Fixed", Resolved: true}, false},
		{"curly apostrophe, localized Fixed", State{Name: "Won\u2019t fix", LocalizedName: "Fixed", Resolved: true}, false},
		{"NBSP, localized Fixed", State{Name: "Won't\u00a0fix", LocalizedName: "Fixed", Resolved: true}, false},
		{"Russian won't fix, localized Fixed", State{Name: "Не будет исправлено", LocalizedName: "Fixed", Resolved: true}, false},
		{"Cyrillic o, localized Fixed", State{Name: "W\u043en't fix", LocalizedName: "Fixed", Resolved: true}, false},
		{"ASCII name, localized Fixed", State{Name: "Erledigt", LocalizedName: "Fixed", Resolved: true}, false},
		{"Erledigt", State{Name: "Erledigt", Resolved: true}, false},
		{"upper case FIXED", State{Name: "FIXED", Resolved: true}, true},
		{"Fixed with a trailing NBSP", State{Name: "fixed\u00a0", Resolved: true}, true},
		{"Fixed with a filler", State{Name: "Fixed\u3164", Resolved: true}, false},
		{"empty", State{Resolved: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClosableState(tc.s); got != tc.want {
				t.Errorf("ClosableState(%+v) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}

func TestOpenableState(t *testing.T) {
	for name, tc := range map[string]struct {
		s    State
		want bool
	}{
		"unresolved":                     {State{Name: "In Progress"}, true},
		"resolved":                       {State{Name: "Fixed", Resolved: true}, false},
		"archived unresolved":            {State{Name: "Old", Archived: true}, false},
		"filler in the name":             {State{Name: "Open\u3164"}, false},
		"variation selector":             {State{Name: "Open\uFE0F"}, false},
		"ideographic variation selector": {State{Name: "Open\U000E0100"}, false},
	} {
		if got := OpenableState(tc.s); got != tc.want {
			t.Errorf("%s: OpenableState = %v, want %v", name, got, tc.want)
		}
	}
}
