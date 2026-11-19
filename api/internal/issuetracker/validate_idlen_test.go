package issuetracker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// The store holds a project id and a tag id in columns capped at 128
// characters, so the storage-boundary validators refuse a longer one.
func TestStorageIDValidatorsBoundTheLength(t *testing.T) {
	atCap := "1-" + strings.Repeat("9", 126)
	overCap := atCap + "9"
	if len(atCap) != 128 || len(overCap) != 129 {
		t.Fatalf("fixture lengths %d, %d", len(atCap), len(overCap))
	}
	if !ValidProjectID(atCap) || ValidProjectID(overCap) {
		t.Errorf("ValidProjectID: 128 chars = %v, 129 chars = %v; want true, false", ValidProjectID(atCap), ValidProjectID(overCap))
	}
	if err := ValidTagRef(TagRef{ID: atCap, Name: "ready"}); err != nil {
		t.Errorf("ValidTagRef refused a 128-char id: %v", err)
	}
	if err := ValidTagRef(TagRef{ID: overCap, Name: "ready"}); err == nil {
		t.Error("ValidTagRef accepted a 129-char id")
	}
}

func TestListLabelsLeavesOutNamesTheDriverRefuses(t *testing.T) {
	y, srv := newFakeDriver(t)
	srv.SeedOwnTag("mine")
	srv.SeedOwnTag("with{brace")
	srv.SeedOwnTag("with\x07control")
	got, err := y.ListLabels(context.Background(), testProjectID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range got {
		names = append(names, l.Name)
	}
	if want := []string{youtracktest.EligibilityTagName, "mine"}; !slices.Equal(names, want) {
		t.Errorf("ListLabels = %q, want %q", names, want)
	}
}
