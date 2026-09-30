package workersvc

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// jobfiles_test.go: the parts of the PRD #1909 M1 job-file store that need no database.

func TestJobFileLimitsDefaults(t *testing.T) {
	l := (JobFileLimits{}).withDefaults()
	want := JobFileLimits{
		InputFileMaxBytes: 25 << 20, InputsMaxFiles: 10, InputsMaxBytes: 50 << 20,
		OutputFileMaxBytes: 25 << 20, OutputsMaxFiles: 50, OutputsMaxBytes: 100 << 20,
		PerOwnerBytes: 256 << 20, InstanceBytes: 1 << 30, StoredFilesBudgetBytes: 4 << 30,
		Retention: 7 * 24 * time.Hour, UploadTTL: time.Hour, RequestDeadline: 120 * time.Second,
		MaxConcurrentWrites: 4, MaxConcurrentWritesPerOwner: 2,
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

// TestJobFilesAcquireWrite: the concurrent-write slots are bounded process-wide and per owner, a
// refused acquire holds nothing, and release is idempotent and returns the slot.
func TestJobFilesAcquireWrite(t *testing.T) {
	jf := NewJobFiles(nil, nil, JobFileLimits{MaxConcurrentWrites: 3, MaxConcurrentWritesPerOwner: 2}, nil)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	ra1, err := jf.AcquireWrite(a)
	if err != nil {
		t.Fatal(err)
	}
	ra2, _ := jf.AcquireWrite(a)
	if _, err := jf.AcquireWrite(a); !errors.Is(err, ErrJobFileUploadsBusy) {
		t.Fatalf("a third slot for one owner: %v, want ErrJobFileUploadsBusy", err)
	}
	rb, err := jf.AcquireWrite(b)
	if err != nil {
		t.Fatalf("another owner's first slot: %v", err)
	}
	if _, err := jf.AcquireWrite(c); !errors.Is(err, ErrJobFileUploadsBusy) {
		t.Fatalf("a fourth slot process-wide: %v, want ErrJobFileUploadsBusy", err)
	}
	ra1()
	ra1() // idempotent: must not free a second slot.
	if _, err := jf.AcquireWrite(c); err != nil {
		t.Fatalf("a freed slot was not reusable: %v", err)
	}
	if _, err := jf.AcquireWrite(uuid.New()); !errors.Is(err, ErrJobFileUploadsBusy) {
		t.Fatalf("a double release freed more than one slot: %v", err)
	}
	ra2()
	rb()
	// Defaults: 4 in all, 2 per owner.
	d := NewJobFiles(nil, nil, JobFileLimits{}, nil)
	if l := d.Limits(); l.MaxConcurrentWrites != 4 || l.MaxConcurrentWritesPerOwner != 2 {
		t.Errorf("default write slots = %d / %d, want 4 / 2", l.MaxConcurrentWrites, l.MaxConcurrentWritesPerOwner)
	}
}

// beginRecorder is a JobFilesDB that only records Begin: it lets a test see when Write first wants
// a connection.
type beginRecorder struct {
	store.DBTX
	begins chan struct{}
}

// Exec absorbs the best-effort reservation release a failed Write issues.
func (b *beginRecorder) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (b *beginRecorder) Begin(context.Context) (pgx.Tx, error) {
	b.begins <- struct{}{}
	return nil, errors.New("no database in this test")
}

// TestJobFilesWriteReadsFirstChunkBeforeBegin: a client that has sent nothing holds no database
// connection; the transaction begins only once the first chunk (or the end of the body) is in.
func TestJobFilesWriteReadsFirstChunkBeforeBegin(t *testing.T) {
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &beginRecorder{begins: make(chan struct{}, 1)}
	jf := NewJobFiles(rec, box, JobFileLimits{}, nil)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := jf.Write(context.Background(), uuid.New(), uuid.New(), pr, WriteOptions{ContentType: "text/plain"})
		done <- err
	}()
	select {
	case <-rec.begins:
		t.Fatal("Write began a transaction before any body byte arrived")
	case <-time.After(300 * time.Millisecond):
	}
	_, _ = pw.Write([]byte("hello"))
	_ = pw.Close()
	select {
	case <-rec.begins:
	case <-time.After(3 * time.Second):
		t.Fatal("Write never began its transaction after the body arrived")
	}
	if err := <-done; err == nil {
		t.Fatal("Write succeeded with no database")
	}

	// A body that fails before the first chunk never reaches the database at all.
	rec2 := &beginRecorder{begins: make(chan struct{}, 1)}
	jf2 := NewJobFiles(rec2, box, JobFileLimits{}, nil)
	_, err = jf2.Write(context.Background(), uuid.New(), uuid.New(), errReader{io.ErrClosedPipe}, WriteOptions{ContentType: "text/plain"})
	var be *JobFileBodyError
	if !errors.As(err, &be) || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("a failing body: %v, want a JobFileBodyError wrapping the cause", err)
	}
	select {
	case <-rec2.begins:
		t.Fatal("a body that failed before its first chunk still began a transaction")
	default:
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
