package schedsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestFireCatalogCIScoutComposesPromptDeliveryAndSnapshot fires the shipped ci-scout
// catalog default (#3225) as an enabled default row: the prompt resolves from the real
// catalog, the NULL output mode resolves to issues, and the CI snapshot follows the
// issues-mode sections.
func TestFireCatalogCIScoutComposesPromptDeliveryAndSnapshot(t *testing.T) {
	h, s := newScoutHarness()
	job, ok := schedtmpl.BySlug(ciScoutSlug)
	if !ok {
		t.Fatal("ci-scout missing from the catalog")
	}
	sched := store.RunSchedule{
		ID: uuid.New(), UserID: h.owner, RepoID: h.repoID, Target: "prompt", Origin: "default",
		CatalogSlug: pgtype.Text{String: ciScoutSlug, Valid: true},
		Timing:      "recurring", CronExpr: pgtype.Text{String: job.Cron, Valid: true}, Timezone: "UTC",
		AutoApprove: true, Status: "active", Enabled: true,
	}
	if _, err := h.sched.firePrompt(context.Background(), sched); err != nil {
		t.Fatalf("firePrompt: %v", err)
	}
	got := h.runs.prompts[0].prompt
	iPrompt := strings.Index(got, "Find ONE CI improvement")
	iDelivery := strings.Index(got, issuesDeliveryInstruction)
	iSnapshot := strings.Index(got, scoutHeader)
	if iPrompt != 0 || iDelivery < 0 || iSnapshot < iDelivery {
		t.Fatalf("want catalog prompt, then the issues delivery, then the snapshot; got prompt@%d delivery@%d snapshot@%d", iPrompt, iDelivery, iSnapshot)
	}
	if len(s.calls) == 0 || !strings.Contains(got, "wf-ci / unit") {
		t.Fatalf("snapshot not collected: calls=%v", s.calls)
	}
}
