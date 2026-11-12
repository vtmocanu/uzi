package issuetracker

import "testing"

func TestParseKind(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Kind
		ok   bool
	}{
		{"youtrack", KindYouTrack, true},
		{"YouTrack", "", false},
		{" youtrack", "", false},
		{"", "", false},
		{"gitlab", "", false},
	} {
		got, ok := ParseKind(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseKind(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSupportedKindsRoundTrip(t *testing.T) {
	kinds := SupportedKinds()
	if len(kinds) == 0 {
		t.Fatal("no supported kinds")
	}
	for _, k := range kinds {
		if got, ok := ParseKind(string(k)); !ok || got != k {
			t.Errorf("ParseKind(%q) = %q, %v", k, got, ok)
		}
	}
	if got := SupportedKindsList(); got != "youtrack" {
		t.Errorf("SupportedKindsList() = %q", got)
	}
}
