package kube

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The API accepts at most 192 UTF-8 bytes for pacing reasons.
func TestPacingReasonFitsAPIByteBound(t *testing.T) {
	got := pacingText(strings.Repeat("界", 190) + "\nend")
	if got == nil || len(*got) > 192 || !utf8.ValidString(*got) || !strings.HasPrefix(*got, "界") {
		t.Fatal("pacing reason exceeds the API byte contract or loses valid text")
	}
}
