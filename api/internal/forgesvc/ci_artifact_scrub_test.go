package forgesvc

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func TestCIArtifactScrubberExactConnectionSecret(t *testing.T) {
	box, err := secretbox.New([]byte("0123456789abcdefghijklmnopqrstuv"))
	if err != nil {
		t.Fatal(err)
	}
	service := New(nil, box, time.Second, nil)
	// Intentionally no recognizable provider prefix: shape-only scrubbing fails.
	secret := strings.Join([]string{"fixture", "connection", "credential"}, "-")
	ciphertext, err := service.EncryptToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	scrub, err := service.CIArtifactScrubber(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if got := scrub("name/" + secret + "\nbody " + secret); got != "name/[REDACTED]\nbody [REDACTED]" {
		t.Fatalf("exact connection secret survived: %q", got)
	}
	for _, ciphertext := range [][]byte{nil, []byte("invalid")} {
		if scrub, err := service.CIArtifactScrubber(ciphertext); err == nil || scrub != nil {
			t.Fatal("invalid ciphertext must not yield a shape-only fallback")
		}
	}
	empty, err := service.EncryptToken("")
	if err != nil {
		t.Fatal(err)
	}
	if scrub, err := service.CIArtifactScrubber(empty); err == nil || scrub != nil {
		t.Fatal("empty credential must fail closed")
	}
}
