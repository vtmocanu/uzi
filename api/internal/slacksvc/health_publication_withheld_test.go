package slacksvc

import "testing"

// Issue #4257: publication_withheld is a mirrored health enum with its own root label and
// nudge head. The head is fixed text; it never carries finding content.
func TestPublicationWithheldHealthWording(t *testing.T) {
	if healthPublicationWithheld != "publication_withheld" {
		t.Fatalf("mirrored enum = %q, want the workersvc value publication_withheld", healthPublicationWithheld)
	}
	if got, want := healthRootLabel(healthPublicationWithheld), "checkpoint publication withheld"; got != want {
		t.Fatalf("root label = %q, want %q", got, want)
	}
	const head = "🔒 This run's checkpoint publication is withheld by a secret-scan restriction."
	if got := healthNudgeHead(healthPublicationWithheld, ""); got != head {
		t.Fatalf("nudge head = %q, want %q", got, head)
	}
	// A reason belonging to another enum must not move the head.
	if got := healthNudgeHead(healthPublicationWithheld, reasonPersistFailing); got != head {
		t.Fatalf("nudge head with a foreign reason = %q, want %q", got, head)
	}
	if got := healthContextLabel(healthRun("running", healthPublicationWithheld)); got != "checkpoint publication withheld" {
		t.Fatalf("context label on a running run = %q", got)
	}
}
