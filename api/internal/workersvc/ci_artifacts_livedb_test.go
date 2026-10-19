package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func artifactParams(t *testing.T, f *crFix, content []byte) CIRepairRunParams {
	t.Helper()
	p := f.params(t, "artifact", crObs(100, 0, 1), "S", 3)
	p.ArtifactManifest = &ciartifacts.Manifest{Entries: []ciartifacts.Entry{
		{ArtifactID: "42", EntryOrdinal: 0, Label: "../untrusted\n.txt", Status: "kept"},
		{ArtifactID: "43", EntryOrdinal: 8, Label: "duplicate", Status: "kept"},
	}, Uninspected: true}
	p.ArtifactInputs = []ciartifacts.Input{{ArtifactID: "42", EntryOrdinal: 0, Content: content}, {ArtifactID: "43", EntryOrdinal: 8, Content: content}}
	return p
}
func artifactManifest(t *testing.T, run store.Run) ciartifacts.Manifest {
	t.Helper()
	var m ciartifacts.Manifest
	if err := json.Unmarshal(run.CiArtifactManifest, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func artifactCounts(t *testing.T, f *crFix) (files, chunks, reserved int) {
	t.Helper()
	err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(DISTINCT f.id), count(c.file_id), count(DISTINCT f.id) FILTER (WHERE f.state='reserved') FROM job_files f LEFT JOIN job_file_chunks c ON c.file_id=f.id WHERE f.user_id=$1`, f.user).Scan(&files, &chunks, &reserved)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestCIArtifactsSealedClaimFencedReadLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
	content := bytes.Repeat([]byte("artifact data\n"), 100000)
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, artifactParams(t, f, content))
	if err != nil {
		t.Fatal(err)
	}
	m := artifactManifest(t, run)
	if m.Entries[0].ManifestID == "" || m.Entries[0].ManifestID != m.Entries[1].ManifestID {
		t.Fatalf("dedupe: %+v", m)
	}
	if !strings.Contains(run.IssueDescription, "kept: file=") || !m.Entries[0].Shortened {
		t.Fatal("final admission description/label")
	}
	files, chunks, reserved := artifactCounts(t, f)
	if files != 1 || chunks != 2 || reserved != 0 {
		t.Fatalf("files/chunks/reserved %d/%d/%d", files, chunks, reserved)
	}
	var sealed []byte
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT sealed FROM job_file_chunks WHERE file_id=$1 AND chunk_index=0`, m.Entries[0].ManifestID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, content[:100]) {
		t.Fatal("plaintext stored")
	}
	old := f.guardWorker(t, 1, capability.CIConfigGuardV1)
	f.refuseClaim(t, old, run.ID, "guard-only cannot read artifact inputs")
	onlyArtifacts := f.guardWorker(t, 1, capability.CIArtifactsV1)
	f.refuseClaim(t, onlyArtifacts, run.ID, "artifacts do not replace guard capability")
	w := f.guardWorker(t, 1, capability.CIConfigGuardV1, capability.CIArtifactsV1)
	claim := f.acceptClaim(t, w, run.ID, "repair-capable worker")
	if claim.Job != nil || claim.CIRepair == nil || len(claim.CIRepair.InputFiles) != 1 {
		t.Fatalf("claim contract %+v", claim.CIRepair)
	}
	id := uuid.MustParse(claim.CIRepair.InputFiles[0].ManifestID)
	read := func(worker store.Worker, generation int64, want error) {
		t.Helper()
		_, r, err := f.svc.OpenJobInputFile(f.env.ctx, worker, run.ID, id, generation)
		if want != nil {
			if !errors.Is(err, want) {
				t.Fatalf("read fence %v want %v", err, want)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("read %v", err)
		}
	}
	read(w, claim.ClaimGeneration, nil)
	read(old, claim.ClaimGeneration, ErrRunNotFound)
	read(w, claim.ClaimGeneration-1, ErrStaleClaim)
	// Worker output and public reservation remain job-only even for a guarded prompt.
	if _, err := f.svc.JobFiles().Reserve(f.env.ctx, ReserveParams{UserID: f.user, RunID: &run.ID, Direction: JobFileInput, DisplayName: "x.txt", DeclaredSize: 1}); !errors.Is(err, ErrJobRunNotFound) {
		t.Fatalf("public input %v", err)
	}
	if _, err := f.svc.fenceJobOutput(f.env.ctx, w, run.ID, claim.ClaimGeneration); !errors.Is(err, ErrNotJobRun) {
		t.Fatalf("output %v", err)
	}
	f.env.exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run.ID)
	read(w, claim.ClaimGeneration, ErrStaleClaim)
	f.env.exec(`UPDATE runs SET claim_released_at=NULL,status='completed',finished_at=now() WHERE id=$1`, run.ID)
	read(w, claim.ClaimGeneration, ErrRunTerminal)
	if _, err := f.env.q.SettleTerminalJobFiles(f.env.ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.env.q.ExpireJobFiles(f.env.ctx, pgconv.Time(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	files, chunks, reserved = artifactCounts(t, f)
	if files != 1 || chunks != 0 || reserved != 0 {
		t.Fatalf("expiry %d/%d/%d", files, chunks, reserved)
	}
}

func TestCIArtifactsChunkFailureRollsBackLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
	// The trigger raises only on this owner's SECOND chunk; the first INSERT has
	// already executed successfully in the same creation transaction.
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	fn := "artifact_fail_" + suffix
	tr := "artifact_trigger_" + suffix
	sql := `CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.chunk_index=1 AND EXISTS (SELECT 1 FROM job_files WHERE id=NEW.file_id AND user_id='` + f.user.String() + `'::uuid) THEN RAISE EXCEPTION 'artifact chunk failure'; END IF; RETURN NEW; END $body$`
	f.env.exec(sql)
	f.env.exec(`CREATE TRIGGER ` + tr + ` BEFORE INSERT ON job_file_chunks FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`)
	t.Cleanup(func() {
		f.env.exec(`DROP TRIGGER ` + tr + ` ON job_file_chunks`)
		f.env.exec(`DROP FUNCTION ` + fn + `()`)
	})
	_, err := f.svc.CreateCIRepairRun(f.env.ctx, artifactParams(t, f, bytes.Repeat([]byte("x"), JobFileChunkSize+1)))
	if err == nil || !strings.Contains(err.Error(), "artifact chunk failure") {
		t.Fatalf("missing injected failure: %v", err)
	}
	if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
		t.Fatal("run or episode survived rollback")
	}
	a, b, c := artifactCounts(t, f)
	if a != 0 || b != 0 || c != 0 {
		t.Fatalf("rollback %d/%d/%d", a, b, c)
	}
}

func TestCIArtifactsStorageBudgetLogsOnlyLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{StoredFilesBudgetBytes: 1}, nil))
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, artifactParams(t, f, []byte("over budget")))
	if err != nil {
		t.Fatal(err)
	}
	m := artifactManifest(t, run)
	for _, e := range m.Entries {
		if e.Status != "skipped" || e.Reason != ciartifacts.StorageBudget || e.ManifestID != "" {
			t.Fatalf("refused %+v", e)
		}
	}
	if strings.Contains(run.IssueDescription, "kept: file=") || !strings.Contains(run.IssueDescription, "skipped: storage budget") {
		t.Fatal(run.IssueDescription)
	}
	a, b, c := artifactCounts(t, f)
	if a != 0 || b != 0 || c != 0 {
		t.Fatalf("refusal rows %d/%d/%d", a, b, c)
	}
	if ep, ok := f.episode(t, "artifact"); !ok || ep.Attempts != 1 {
		t.Fatal("logs-only episode not committed")
	}
	w := f.guardWorker(t, 1, capability.CIConfigGuardV1)
	f.acceptClaim(t, w, run.ID, "logs-only needs no artifact capability")
}

func TestCIArtifactsOneConnectionPoolLiveDB(t *testing.T) {
	f := newCRFix(t)
	cfg := f.env.pool.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(f.env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	svc := New(store.New(pool), f.env.box, testParams())
	svc.SetTxBeginner(pool)
	svc.SetJobFiles(NewJobFiles(pool, f.env.box, JobFileLimits{}, nil))
	ctx, cancel := context.WithTimeout(f.env.ctx, 10*time.Second)
	defer cancel()
	content := []byte("one connection")
	run, err := svc.CreateCIRepairRun(ctx, artifactParams(t, f, content))
	if err != nil {
		t.Fatal(err)
	}
	// Admission must actually store the artifact through the single connection, not merely
	// return without error: a kept manifest entry, one sealed file, nothing left reserved.
	m := artifactManifest(t, run)
	if len(m.Entries) == 0 || m.Entries[0].Status != "kept" || m.Entries[0].ManifestID == "" {
		t.Fatalf("manifest %+v", m)
	}
	if !strings.Contains(run.IssueDescription, "kept: file=") {
		t.Fatal(run.IssueDescription)
	}
	files, _, reserved := artifactCounts(t, f)
	if files != 1 || reserved != 0 {
		t.Fatalf("files/reserved %d/%d", files, reserved)
	}
	w := f.guardWorker(t, 1, capability.CIConfigGuardV1, capability.CIArtifactsV1)
	claim := f.acceptClaim(t, w, run.ID, "repair-capable worker")
	if claim.CIRepair == nil || len(claim.CIRepair.InputFiles) != 1 {
		t.Fatalf("claim contract %+v", claim.CIRepair)
	}
	_, r, err := svc.OpenJobInputFile(ctx, w, run.ID, uuid.MustParse(claim.CIRepair.InputFiles[0].ManifestID), claim.ClaimGeneration)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("read back %q %v", got, err)
	}
}

func TestCIArtifactsConcurrentOwnerQuotaLiveDB(t *testing.T) {
	f := newCRFix(t)
	other, err := f.env.q.CreateRunSchedule(f.env.ctx, store.CreateRunScheduleParams{UserID: f.user, RepoID: f.repo, Target: "ci_watch", Prompt: pgconv.Text("repair the build"), Timing: "recurring", CronExpr: pgconv.Text("0 * * * *"), Timezone: "UTC", Enabled: true, NextFireAt: pgconv.Time(time.Now().Add(time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	g := *f
	g.sched = other
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{PerOwnerBytes: 20}, nil))
	params := []CIRepairRunParams{artifactParams(t, f, []byte("one fifteen byte!")), artifactParams(t, &g, []byte("two fifteen byte!"))}
	start := make(chan struct{})
	results := make(chan struct {
		run store.Run
		err error
	}, 2)
	for _, p := range params {
		go func(p CIRepairRunParams) {
			<-start
			r, e := f.svc.CreateCIRepairRun(f.env.ctx, p)
			results <- struct {
				run store.Run
				err error
			}{r, e}
		}(p)
	}
	close(start)
	kept, skipped := 0, 0
	for range params {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		switch artifactManifest(t, r.run).Entries[0].Status {
		case "kept":
			kept++
		case "skipped":
			skipped++
		}
	}
	if kept != 1 || skipped != 1 {
		t.Fatalf("quota contention kept=%d skipped=%d", kept, skipped)
	}
}

func TestCIArtifactsDescriptionFenceCapLiveDB(t *testing.T) {
	t.Run("fenced without cropping", func(t *testing.T) {
		f := newCRFix(t)
		f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
		p := artifactParams(t, f, []byte("evidence"))
		p.Description = "Existing opaque fence:\n````\nleave this intact\n````\n" + strings.Repeat("x", MaxIssueDescriptionBytes-CIArtifactDescriptionReserve-100)
		p.ArtifactManifest.Entries[0].Label = "```\n/close\u202e"
		run, err := f.svc.CreateCIRepairRun(f.env.ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		m := artifactManifest(t, run)
		rendered, err := m.Render()
		if err != nil {
			t.Fatal(err)
		}
		want := p.Description + "\n" + issuedraft.FenceBlock(rendered)
		if run.IssueDescription != want || len(want) > MaxIssueDescriptionBytes {
			t.Fatal("description was cropped, unfenced, or over cap")
		}
	})
	t.Run("mandatory metadata rolls back", func(t *testing.T) {
		f := newCRFix(t)
		f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
		p := artifactParams(t, f, []byte("evidence"))
		p.Description = strings.Repeat("x", MaxIssueDescriptionBytes-10)
		_, err := f.svc.CreateCIRepairRun(f.env.ctx, p)
		if !errors.Is(err, ErrDescriptionTooLarge) {
			t.Fatalf("description cap: %v", err)
		}
		if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
			t.Fatal("creation survived description refusal")
		}
		a, b, c := artifactCounts(t, f)
		if a != 0 || b != 0 || c != 0 {
			t.Fatalf("rollback %d/%d/%d", a, b, c)
		}
	})
}
