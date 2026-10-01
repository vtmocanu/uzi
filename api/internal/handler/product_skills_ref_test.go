package handler

import (
	"strings"
	"testing"
)

func TestProductSkillsRefUsesDatabaseByteLimit(t *testing.T) {
	for _, c := range []struct {
		name, ref string
		valid     bool
	}{
		{"empty", "", true},
		{"ASCII at cap", strings.Repeat("r", 256), true},
		{"multibyte at cap", strings.Repeat("é", 128), true},
		{"multibyte over cap", strings.Repeat("é", 129), false},
		{"mixed one byte over cap", strings.Repeat("é", 128) + "r", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := validateProductSkillsRef(c.ref); (err == nil) != c.valid {
				t.Fatalf("%d-byte ref validation = %v, want valid=%v", len(c.ref), err, c.valid)
			}
		})
	}
}
