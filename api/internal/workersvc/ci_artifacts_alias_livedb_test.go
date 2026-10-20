package workersvc

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
)

func TestCIArtifactsCollectorAliasesAdmissionLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i := 0; i < 302; i++ {
		name := fmt.Sprintf("entry-%d", i)
		text := "same"
		if i == 300 {
			text = "last unique"
		}
		if i == 301 {
			name = "../bad"
		}
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	c := ciartifacts.NewCollector(ciartifacts.Limits{}, nil)
	c.Add(context.Background(), "42", "logs", b.Bytes())
	m, inputs := c.Result()
	p := f.params(t, "aliases", crObs(100, 0, 1), "S", 3)
	p.ArtifactManifest = &m
	p.ArtifactInputs = inputs
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	admitted := artifactManifest(t, run)
	kept, skipped := int64(0), int64(0)
	for _, e := range admitted.Entries {
		if e.Status == "kept" {
			kept++
		} else if !e.Summary {
			skipped++
		}
	}
	if skipped+*admitted.Omitted != 1 || kept+admitted.OmittedKeptAliases != 301 || admitted.OmittedKeptAliases == 0 {
		t.Fatalf("kept=%d skipped=%d omitted=%d aliases=%d", kept, skipped, *admitted.Omitted, admitted.OmittedKeptAliases)
	}
	files, chunks, reserved := artifactCounts(t, f)
	if files != 2 || chunks != 2 || reserved != 0 {
		t.Fatalf("files/chunks/reserved %d/%d/%d", files, chunks, reserved)
	}
	ids := map[string]string{}
	for _, e := range admitted.Entries {
		if e.Status != "kept" {
			continue
		}
		if e.ManifestID == "" {
			t.Fatal("unadmitted alias")
		}
		if id, ok := ids[e.SHA256]; ok && id != e.ManifestID {
			t.Fatal("alias allocated second file")
		}
		ids[e.SHA256] = e.ManifestID
	}
	if len(ids) != 2 || !strings.Contains(run.IssueDescription, "299 additional kept entries") || !strings.Contains(run.IssueDescription, fmt.Sprintf("%d more skipped", *admitted.Omitted)) {
		t.Fatal("incorrect admission render")
	}
}

// Exercise collection through the real SaveCIArtifactAdmission query, including
// JSONB expansion and UUID assignment, with every unique kept file preserved.
func TestCIArtifactsNearCapCollectionAdmissionLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i := 0; i < 400; i++ {
		name, text := fmt.Sprintf("kept-%d", i), fmt.Sprintf("unique evidence %d", i)
		if i >= 50 {
			name = fmt.Sprintf("%d-", i) + strings.Repeat(`"`, 240)
			text = "unique evidence 0" // aliases have the same canonical input
			if i%2 == 0 {
				name = "../" + name
			}
		}
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	c := ciartifacts.NewCollector(ciartifacts.Limits{}, nil)
	c.SetExecution(ciartifacts.Execution{RunID: 100, RepositoryID: 42, WorkflowKey: strings.Repeat("w", 200), Workflow: strings.Repeat("n", 200), SHA: strings.Repeat("a", 40), Branch: strings.Repeat("b", 200), UpdatedAt: "2026-10-15T00:00:00Z"})
	c.Add(f.env.ctx, "42", strings.Repeat("a", 200), b.Bytes())
	m, inputs := c.Result()
	if len(inputs) != 50 || m.Omitted == nil || *m.Omitted == 0 {
		t.Fatalf("collection lost unique files or failed to exercise bound: files=%d manifest=%+v", len(inputs), m)
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) < ciartifacts.ManifestMaxBytes/3 {
		t.Fatalf("fixture not near manifest bound: bytes=%d err=%v", len(raw), err)
	}
	p := f.params(t, "near-cap", crObs(100, 0, 1), "S", 3)
	p.Description = "Opaque prefix\n````\nintact\n````\n" + strings.Repeat("p", MaxIssueDescriptionBytes-(48<<10))
	p.ArtifactManifest, p.ArtifactInputs = &m, inputs
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var storageBytes, textBytes int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT pg_column_size(ci_artifact_manifest), octet_length(ci_artifact_manifest::text) FROM runs WHERE id=$1`, run.ID).Scan(&storageBytes, &textBytes); err != nil {
		t.Fatal(err)
	}
	if storageBytes > ciartifacts.ManifestMaxBytes || textBytes > ciartifacts.ManifestMaxBytes {
		t.Fatalf("JSONB cap: storage=%d text=%d", storageBytes, textBytes)
	}
	saved := artifactManifest(t, run)
	if *saved.Omitted != *m.Omitted || saved.OmittedKeptAliases != m.OmittedKeptAliases {
		t.Fatal("admission changed collection counts")
	}
	files, chunks, reserved := artifactCounts(t, f)
	if files != 50 || chunks != 50 || reserved != 0 {
		t.Fatalf("files/chunks/reserved=%d/%d/%d", files, chunks, reserved)
	}
	if !strings.HasPrefix(run.IssueDescription, p.Description) || len(run.IssueDescription) > MaxIssueDescriptionBytes {
		t.Fatal("description prefix/cap")
	}
	seen := map[string]bool{}
	for _, e := range saved.Entries {
		if e.Status == "kept" {
			seen[e.SHA256] = true
			if len(e.ManifestID) != 36 || !strings.Contains(run.IssueDescription, e.ManifestID) {
				t.Fatal("kept reference lost during summary truncation")
			}
		}
	}
	if len(seen) != 50 || !strings.Contains(run.IssueDescription, "more skipped") {
		t.Fatal("missing kept files or omission notice")
	}
	t.Logf("manifest JSON=%d JSONB storage=%d text=%d files=%d", len(raw), storageBytes, textBytes, files)
}

func TestCIArtifactsAliasRequiresVerifiedCanonicalLiveDB(t *testing.T) {
	for _, tamper := range []string{"digest", "size", "missing-canonical"} {
		t.Run(tamper, func(t *testing.T) {
			f := newCRFix(t)
			f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
			p := artifactParams(t, f, []byte("verified"))
			p.ArtifactInputs = p.ArtifactInputs[:1]
			digest, name := ciartifacts.Identity([]byte("verified"))
			alias := &p.ArtifactManifest.Entries[1]
			alias.SHA256 = digest
			alias.Name = name
			alias.Size = 8
			switch tamper {
			case "digest":
				alias.SHA256 = strings.Repeat("a", 64)
				alias.Name = alias.SHA256 + ".txt"
			case "size":
				alias.Size++
			case "missing-canonical":
				p.ArtifactManifest.Entries = p.ArtifactManifest.Entries[1:]
			}
			if _, err := f.svc.CreateCIRepairRun(f.env.ctx, p); err == nil {
				t.Fatal("unverified alias accepted")
			}
			files, chunks, reserved := artifactCounts(t, f)
			if files != 0 || chunks != 0 || reserved != 0 {
				t.Fatal("failed alias left storage")
			}
		})
	}
}
