package ciartifacts

import (
	"math"
	"testing"
)

func TestCIArtifactsCombinedOmissionCountCannotOverflow(t *testing.T) {
	digest, name := Identity([]byte("same"))
	omitted := int64(math.MaxInt64 - 1)
	m := Manifest{Omitted: &omitted, OmittedKeptAliases: 1, Entries: []Entry{
		{ArtifactID: "a", Status: "kept", SHA256: digest, Name: name, Size: 4},
	}}
	if _, err := m.Normalize(); err == nil {
		t.Fatal("combined omitted counts could overflow when storage refuses kept evidence")
	}
	omitted--
	if _, err := m.Normalize(); err != nil {
		t.Fatalf("representable combined total refused: %v", err)
	}
}
