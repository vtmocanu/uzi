package workersvc

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
)

func TestCIArtifactsRefusalConvertsOmittedAliasesLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{StoredFilesBudgetBytes: 1}, nil))
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	for i := 0; i < 300; i++ {
		w, err := z.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("entry-%d", i), Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("same")); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	c := ciartifacts.NewCollector(ciartifacts.Limits{}, nil)
	c.Add(context.Background(), "42", "logs", data.Bytes())
	m, inputs := c.Result()
	// Keep the canonical row, but replace one optional alias with a source
	// summary: dropped archive summaries must not become skipped-file counts.
	m.Entries = append(m.Entries[:1], m.Entries[2:]...)
	m.OmittedKeptAliases++
	m.Entries = append(m.Entries, ciartifacts.Entry{ArtifactID: "expired-source", EntryOrdinal: 1001, ArtifactName: "old", Status: "skipped", Reason: "expired", Summary: true})
	m.Reasons = []string{"expired"}
	p := f.params(t, "refused-aliases", crObs(100, 0, 1), "S", 3)
	p.ArtifactManifest, p.ArtifactInputs = &m, inputs
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	got := artifactManifest(t, run)
	if got.OmittedKeptAliases != 0 || got.Omitted == nil || *got.Omitted != 100 {
		t.Fatalf("refused files miscounted: aliases=%d omitted=%v", got.OmittedKeptAliases, got.Omitted)
	}
	if !strings.Contains(run.IssueDescription, "100 more skipped") || strings.Contains(run.IssueDescription, "additional kept entries") || !strings.Contains(run.IssueDescription, "skipped: storage budget") {
		t.Fatal("refusal still claims kept aliases or has inaccurate skipped count")
	}
	files, chunks, reserved := artifactCounts(t, f)
	if files != 0 || chunks != 0 || reserved != 0 {
		t.Fatalf("refusal retained evidence: %d/%d/%d", files, chunks, reserved)
	}
}
