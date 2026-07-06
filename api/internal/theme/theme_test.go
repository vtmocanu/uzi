package theme

import "testing"

func TestValid(t *testing.T) {
	for _, id := range []string{"ember", "mission"} {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "neon", "Ember", "dark", "mission "} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("mission"); err != nil {
		t.Errorf("Validate(mission) = %v, want nil", err)
	}
	if err := Validate("neon"); err == nil {
		t.Error("Validate(neon) = nil, want an error for an unknown theme")
	}
}

func TestResolveChain(t *testing.T) {
	cases := []struct {
		name            string
		override        string
		instanceDefault string
		want            string
	}{
		{"override wins", "mission", "ember", "mission"},
		{"falls to instance default when no override", "", "mission", "mission"},
		{"falls to ember when nothing set", "", "", "ember"},
		{"invalid override falls through to default", "neon", "mission", "mission"},
		{"invalid override and default fall to ember", "neon", "bogus", "ember"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Resolve(c.override, c.instanceDefault); got != c.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", c.override, c.instanceDefault, got, c.want)
			}
		})
	}
}

func TestDefaultIsEmber(t *testing.T) {
	if Default != "ember" {
		t.Fatalf("Default = %q, want ember (a no-op theme must render the original look)", Default)
	}
	if !Valid(Default) {
		t.Fatal("Default must itself be a valid theme")
	}
}
