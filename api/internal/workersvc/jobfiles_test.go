package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// jobfiles_test.go: the parts of the PRD #1909 M1 job-file store that need no database.

func TestJobFileLimitsDefaults(t *testing.T) {
	l := (JobFileLimits{}).withDefaults()
	want := JobFileLimits{
		InputFileMaxBytes: 25 << 20, InputsMaxFiles: 10, InputsMaxBytes: 50 << 20,
		OutputFileMaxBytes: 25 << 20, OutputsMaxFiles: 50, OutputsMaxBytes: 100 << 20,
		PerOwnerBytes: 256 << 20, InstanceBytes: 1 << 30, StoredFilesBudgetBytes: 4 << 30,
		Retention: 7 * 24 * time.Hour, UploadTTL: time.Hour, RequestDeadline: 120 * time.Second,
	}
	if l != want {
		t.Fatalf("defaults = %+v, want the PRD #1909 D1 table %+v", l, want)
	}
	if got := (JobFileLimits{InputsMaxFiles: 3, UploadTTL: time.Minute}).withDefaults(); got.InputsMaxFiles != 3 || got.UploadTTL != time.Minute {
		t.Fatalf("a set field must survive withDefaults: %+v", got)
	}
}

// TestJobFileChunkAADBindsEveryField: the AAD changes when ANY of file id, owner, index or length
// changes, and is domain-separated from the recovery archive's chunk AAD.
func TestJobFileChunkAADBindsEveryField(t *testing.T) {
	file, owner := uuid.New(), uuid.New()
	base := string(jobFileChunkAAD(file, owner, 3, 1000))
	if got := "job_file_chunk|" + file.String() + "|" + owner.String() + "|3|1000"; base != got {
		t.Fatalf("AAD = %q, want %q", base, got)
	}
	for name, aad := range map[string][]byte{
		"file":   jobFileChunkAAD(uuid.New(), owner, 3, 1000),
		"owner":  jobFileChunkAAD(file, uuid.New(), 3, 1000),
		"index":  jobFileChunkAAD(file, owner, 4, 1000),
		"length": jobFileChunkAAD(file, owner, 3, 1001),
	} {
		if string(aad) == base {
			t.Errorf("changing the %s did not change the AAD", name)
		}
	}
}

func TestJobFileExtAllowlist(t *testing.T) {
	for ct, ext := range map[string]string{
		"application/pdf": "pdf", "image/png": "png", "image/jpeg": "jpg", "text/plain": "txt", "text/markdown": "md",
		"text/csv": "csv", "application/json": "json", "text/html": "html",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       "xlsx",
	} {
		if got, ok := JobFileExt(ct); !ok || got != ext {
			t.Errorf("JobFileExt(%q) = %q, %v; want %q", ct, got, ok, ext)
		}
	}
	if _, ok := JobFileExt("application/x-msdownload"); ok {
		t.Error("a type off the allowlist must have no extension")
	}
}

// TestServiceJobFilesAccessor: the store is reachable from the service once wired, and nil (never a
// panic) before.
func TestServiceJobFilesAccessor(t *testing.T) {
	s := &Service{}
	if s.JobFiles() != nil {
		t.Fatal("an unwired service must report no job-file store")
	}
	jf := NewJobFiles(nil, nil, JobFileLimits{PerOwnerBytes: 7}, nil)
	s.SetJobFiles(jf)
	if s.JobFiles() != jf || s.JobFiles().Limits().PerOwnerBytes != 7 {
		t.Fatalf("SetJobFiles/JobFiles round trip failed: %+v", s.JobFiles())
	}
}

func TestReserveParamsValidate(t *testing.T) {
	run := uuid.New()
	ok := ReserveParams{UserID: uuid.New(), Direction: JobFileInput, DisplayName: "résumé notes.txt", DeclaredSize: 1}
	if err := ok.validate(); err != nil {
		t.Fatalf("a well-formed request was refused: %v", err)
	}
	for name, mut := range map[string]func(*ReserveParams){
		"path separator": func(p *ReserveParams) { p.DisplayName = "a/b" },
		"backslash":      func(p *ReserveParams) { p.DisplayName = `a\b` },
		"NUL":            func(p *ReserveParams) { p.DisplayName = "a\x00b" },
		"newline":        func(p *ReserveParams) { p.DisplayName = "a\nb" },
		"C1 control":     func(p *ReserveParams) { p.DisplayName = "a\u0085b" },
		"invalid utf8":   func(p *ReserveParams) { p.DisplayName = "a\xffb" },
		"too long":       func(p *ReserveParams) { p.DisplayName = string(make([]byte, 256)) },
		"upper-case digest": func(p *ReserveParams) {
			p.DeclaredSHA256 = "BA7816BF8F01CFEA414140DE5DAE2223B00361A396177A9CB410FF61F20015AD"
		},
		"output no run": func(p *ReserveParams) { p.Direction = JobFileOutput },
	} {
		p := ok
		mut(&p)
		if err := p.validate(); err == nil {
			t.Errorf("%s: accepted, want ErrJobFileInvalid", name)
		}
	}
	out := ok
	out.Direction, out.RunID = JobFileOutput, &run
	if err := out.validate(); err != nil {
		t.Fatalf("an output with a run was refused: %v", err)
	}
}
