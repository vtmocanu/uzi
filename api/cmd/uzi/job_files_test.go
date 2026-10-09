package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1909 M7: `uzi job files`, `uzi job file get`, `uzi job create --file` and
// `uzi admin products skills`, driven through the real HTTPClient against an httptest server so
// the wire contract (headers, multipart, status mapping) is what is asserted.

type filesServer struct {
	mu        sync.Mutex
	uploads   []uploadSeen
	jobBodies []map[string]any
	fileBody  string
	fileName  string
	fileCode  int
	listing   apitypes.V1JobFilesDTO
	skills    apitypes.ProductSkillsDTO
	products  []apitypes.ProductDTO
	uploadErr int
	// uploadErrAfter, when > 0, lets that many uploads succeed before uploadErr fires.
	uploadErrAfter int
}

type uploadSeen struct {
	size, sha, name, body string
}

func (fs *filesServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/files", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if fs.uploadErr != 0 && len(fs.uploads) >= fs.uploadErrAfter {
			w.WriteHeader(fs.uploadErr)
			_, _ = w.Write([]byte(`{"error":"nope","reason":"x"}`))
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not multipart: %v", err)
			return
		}
		part, err := mr.NextPart()
		if err != nil || part.FormName() != "file" {
			t.Errorf("first part = %v, %v", part, err)
			return
		}
		b, _ := io.ReadAll(part)
		fs.uploads = append(fs.uploads, uploadSeen{
			size: r.Header.Get("X-Uzi-File-Size"), sha: r.Header.Get("X-Uzi-File-Sha256"),
			name: part.FileName(), body: string(b),
		})
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(apitypes.V1FileDTO{ID: "file-" + strconv.Itoa(len(fs.uploads)), State: "unattached"})
	})
	mux.HandleFunc("POST /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		fs.mu.Lock()
		fs.jobBodies = append(fs.jobBodies, m)
		fs.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sampleJob())
	})
	mux.HandleFunc("GET /api/v1/jobs/j1/files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fs.listing)
	})
	mux.HandleFunc("GET /api/v1/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		if fs.fileCode != 0 {
			w.WriteHeader(fs.fileCode)
			_, _ = w.Write([]byte(`{"error":"gone","reason":"file_expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+fs.fileName+`"`)
		_, _ = w.Write([]byte(fs.fileBody))
	})
	mux.HandleFunc("GET /api/admin/products", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"products": fs.products})
	})
	mux.HandleFunc("GET /api/admin/products/{id}/skills", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "p1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(fs.skills)
	})
	return mux
}

// httpEnv is an Env whose client is the real HTTPClient pointed at srv.
func httpEnv(srv *httptest.Server) Env {
	env := fakeEnv(nil)
	env.NewClient = func(uzicli.Settings) uzicli.Client {
		return &uzicli.HTTPClient{BaseURL: srv.URL, Token: "uzc_test", HTTP: srv.Client()}
	}
	return env
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestJobFilesRender(t *testing.T) {
	exp := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	src := "https://example.test/paper.pdf"
	fs := &filesServer{listing: apitypes.V1JobFilesDTO{
		Files: []apitypes.V1JobFileDTO{
			{ID: "f-in", DisplayName: "notes.txt", ContentType: "text/plain", ByteSize: 11, Sha256: sha256Hex("hello notes"), Direction: "input", State: "attached"},
			{ID: "f-out", DisplayName: "paper.pdf", ContentType: "application/pdf", ByteSize: 2048, Sha256: sha256Hex("pdf"), Direction: "output", State: "available", ExpiresAt: &exp, SourceURL: &src},
			{ID: "f-old", DisplayName: "gone.png", Direction: "output", State: "expired"},
		},
		RefusedFiles: []apitypes.V1JobRefusedFileDTO{{DisplayName: "big.bin", ByteSize: 99, Reason: "unsupported_file_type"}},
	}}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	out, _, code := runCLI(t, httpEnv(srv), "job", "files", "j1")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	for _, want := range []string{
		"f-in", "notes.txt", "input", "11", sha256Hex("hello notes")[:12], "attached",
		"f-out", "output", "2048", "available", "2026-09-02T10:00:00Z", src,
		"f-old", "expired", "REFUSED", "big.bin", "unsupported_file_type",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, sha256Hex("hello notes")) {
		t.Errorf("the full sha256 should be shortened:\n%s", out)
	}
	jout, _, code := runCLI(t, httpEnv(srv), "job", "files", "j1", "--json")
	var back apitypes.V1JobFilesDTO
	if code != 0 || json.Unmarshal([]byte(jout), &back) != nil || len(back.Files) != 3 {
		t.Errorf("--json = %d %s", code, jout)
	}
}

func TestJobFilesEmpty(t *testing.T) {
	fs := &filesServer{listing: apitypes.V1JobFilesDTO{Files: []apitypes.V1JobFileDTO{}, RefusedFiles: []apitypes.V1JobRefusedFileDTO{}}}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	out, _, code := runCLI(t, httpEnv(srv), "job", "files", "j1")
	if code != 0 || !strings.Contains(out, "no files") {
		t.Errorf("exit = %d\n%s", code, out)
	}
}

func TestJobFilesHostileStringsEscaped(t *testing.T) {
	hostile := func(s string) string { return s + esc2J + oscTitle + "\u202e" + "\nFORGED  input  1" }
	src := hostile("https://e.example/")
	fs := &filesServer{listing: apitypes.V1JobFilesDTO{
		Files: []apitypes.V1JobFileDTO{{ID: "f1", DisplayName: hostile("name"), ByteSize: 1, Sha256: "ab", Direction: "output", State: hostile("st"), SourceURL: &src}},
		RefusedFiles: []apitypes.V1JobRefusedFileDTO{
			{DisplayName: hostile("r"), ByteSize: 2, Reason: hostile("why")},
		},
	}}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	out, _, code := runCLI(t, httpEnv(srv), "job", "files", "j1")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	assertNoTerminalControl(t, "job files", out)
	// header + 1 file row + blank + REFUSED title + header + 1 refused row
	if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n != 6 {
		t.Errorf("rendered %d lines, want 6 (an embedded newline forged a row):\n%q", n, out)
	}
}

func TestJobFileGetWritesAndRefusesOverwrite(t *testing.T) {
	body := "file bytes"
	fs := &filesServer{fileBody: body, fileName: sha256Hex(body) + ".txt"}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	t.Chdir(dir)

	// Default name: the storage name in the current directory.
	out, _, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, fs.fileName)) //nolint:gosec // G304: test reads a file it wrote under t.TempDir()
	if err != nil || string(got) != body {
		t.Fatalf("file = %q, %v", got, err)
	}
	if !strings.Contains(out, "saved") || !strings.Contains(out, sha256Hex(body)) {
		t.Errorf("output = %q", out)
	}

	// A second get of the same name refuses and leaves the file alone.
	if err := os.WriteFile(filepath.Join(dir, fs.fileName), []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1")
	if code != uzicli.ExitUsage || !strings.Contains(errOut, "refusing to overwrite") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, fs.fileName)); string(b) != "precious" { //nolint:gosec // G304: test reads a file it wrote under t.TempDir()
		t.Errorf("existing file was overwritten: %q", b)
	}

	// -o writes there, and refuses an existing -o target too.
	target := filepath.Join(dir, "out.bin")
	if _, _, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1", "-o", target); code != 0 {
		t.Fatalf("-o exit = %d", code)
	}
	if b, _ := os.ReadFile(target); string(b) != body { //nolint:gosec // G304: test reads a file it wrote under t.TempDir()
		t.Errorf("-o file = %q", b)
	}
	if _, _, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1", "-o", target); code != uzicli.ExitUsage {
		t.Errorf("second -o exit = %d, want usage", code)
	}
	// No temp file is left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".uzi-download-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestJobFileGetDigestMismatchSavesNothing(t *testing.T) {
	fs := &filesServer{fileBody: "tampered", fileName: sha256Hex("original") + ".txt"}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	t.Chdir(dir)
	_, errOut, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1")
	if code == 0 || !strings.Contains(errOut, "sha256") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left behind: %v", ents)
	}
}

func TestJobFileGetUnusableServerName(t *testing.T) {
	fs := &filesServer{fileBody: "x", fileName: "../../etc/passwd"}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	t.Chdir(dir)
	_, errOut, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1")
	if code == 0 || !strings.Contains(errOut, "-o") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left behind: %v", ents)
	}
}

func TestJobFileGetExpired(t *testing.T) {
	fs := &filesServer{fileCode: http.StatusGone}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	t.Chdir(dir)
	_, errOut, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1")
	if code != uzicli.ExitNotFound || !strings.Contains(errOut, "file expired") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left behind: %v", ents)
	}
}

func TestJobCreateFileUploadsThenAttaches(t *testing.T) {
	fs := &filesServer{}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	a := writeTemp(t, "paper.md", "# hello")
	b := writeTemp(t, "data.csv", "a,b\n1,2\n")
	out, _, code := runCLI(t, httpEnv(srv), "job", "create", "--type", "research", "--prompt", "p", "--file", a, "--file", b)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if len(fs.uploads) != 2 {
		t.Fatalf("uploads = %+v", fs.uploads)
	}
	for i, want := range []struct{ name, body string }{{"paper.md", "# hello"}, {"data.csv", "a,b\n1,2\n"}} {
		u := fs.uploads[i]
		if u.name != want.name || u.body != want.body || u.size != strconv.Itoa(len(want.body)) || u.sha != sha256Hex(want.body) {
			t.Errorf("upload %d = %+v, want %+v", i, u, want)
		}
	}
	if len(fs.jobBodies) != 1 {
		t.Fatalf("job creates = %d", len(fs.jobBodies))
	}
	ids, _ := fs.jobBodies[0]["input_file_ids"].([]any)
	if len(ids) != 2 || ids[0] != "file-1" || ids[1] != "file-2" {
		t.Errorf("input_file_ids = %v", fs.jobBodies[0]["input_file_ids"])
	}
}

func TestJobCreateWithoutFileSendsNoIDs(t *testing.T) {
	fs := &filesServer{}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	if _, _, code := runCLI(t, httpEnv(srv), "job", "create", "--type", "research", "--prompt", "p"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if _, has := fs.jobBodies[0]["input_file_ids"]; has || len(fs.uploads) != 0 {
		t.Errorf("unexpected file fields: %v %v", fs.jobBodies[0], fs.uploads)
	}
}

func TestJobCreateFileFailuresCreateNoJob(t *testing.T) {
	good := writeTemp(t, "a.txt", "x")
	empty := writeTemp(t, "e.txt", "")
	for name, tc := range map[string]struct {
		path     string
		serverSt int
		code     int
		want     string
	}{
		"missing":     {path: filepath.Join(t.TempDir(), "nope.txt"), code: uzicli.ExitUsage, want: "cannot read"},
		"empty":       {path: empty, code: uzicli.ExitUsage, want: "empty"},
		"too large":   {path: good, serverSt: http.StatusRequestEntityTooLarge, code: uzicli.ExitUsage, want: "nope"},
		"unsupported": {path: good, serverSt: http.StatusUnsupportedMediaType, code: uzicli.ExitUsage, want: "unsupported file type"},
		"mismatch":    {path: good, serverSt: http.StatusUnprocessableEntity, code: uzicli.ExitUsage, want: "nope"},
		"quota":       {path: good, serverSt: http.StatusInsufficientStorage, code: uzicli.ExitGeneric, want: "storage quota exceeded"},
	} {
		t.Run(name, func(t *testing.T) {
			fs := &filesServer{uploadErr: tc.serverSt}
			srv := httptest.NewServer(fs.handler(t))
			defer srv.Close()
			_, errOut, code := runCLI(t, httpEnv(srv), "job", "create", "--type", "research", "--prompt", "p", "--file", tc.path)
			if code != tc.code || !strings.Contains(errOut, tc.want) {
				t.Errorf("exit = %d (want %d) stderr = %q", code, tc.code, errOut)
			}
			if len(fs.jobBodies) != 0 {
				t.Errorf("a job was created after a failed upload")
			}
		})
	}
}

func TestJobCreateLongFileFailuresCreateNoJob(t *testing.T) {
	for _, kind := range []string{"empty", "directory", "missing"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			// Fixed short components exceed the render cap without exceeding NAME_MAX.
			for range 8 {
				dir = filepath.Join(dir, "nested-long-path-component")
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "input.txt")
			wantReason := "empty file"
			switch kind {
			case "empty":
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path = dir
				wantReason = "not a regular file"
			case "missing":
				_, err := os.Stat(path)
				var pe *os.PathError
				if !errors.As(err, &pe) {
					t.Fatalf("missing child stat = %v, want PathError", err)
				}
				wantReason = "cannot read file: " + pe.Err.Error()
			}
			if utf8.RuneCountInString(path) <= 200 {
				t.Fatalf("fixture path is too short: %q", path)
			}
			fs := &filesServer{}
			srv := httptest.NewServer(fs.handler(t))
			defer srv.Close()
			out, errOut, code := runCLI(t, httpEnv(srv), "job", "create", "--type", "research", "--prompt", "p", "--file", path)
			if code != 2 || out != "" {
				t.Errorf("exit = %d, stdout = %q; want exit 2 and no stdout", code, out)
			}
			if !strings.HasPrefix(errOut, "uzi: "+wantReason+": \"") {
				t.Errorf("reason must precede path: stderr = %q, want reason %q", errOut, wantReason)
			}
			if !strings.HasSuffix(errOut, "…\n") || strings.Count(errOut, "\n") != 1 {
				t.Errorf("stderr must be one truncated line: %q", errOut)
			}
			if n := utf8.RuneCountInString(errOut); n > len("uzi: ")+200+1+1 {
				t.Errorf("stderr has %d runes, exceeds prefix + 200 + ellipsis + newline", n)
			}
			assertNoTerminalControl(t, "long file failure", errOut)
			fs.mu.Lock()
			defer fs.mu.Unlock()
			if len(fs.uploads) != 0 || len(fs.jobBodies) != 0 {
				t.Errorf("failed local validation sent uploads/jobs: %v / %v", fs.uploads, fs.jobBodies)
			}
		})
	}
}

func TestJobFileReadErrorCauseBeforeLongPath(t *testing.T) {
	path := strings.Repeat("long-component/", 20) + "input.txt"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"stat", &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}, os.ErrNotExist.Error()},
		{"open", &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}, os.ErrPermission.Error()},
		{"wrapped", fmt.Errorf("outer: %w", &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}), os.ErrPermission.Error()},
		{"fallback", errors.New("reader unavailable"), "reader unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := jobFileReadError(path, tc.err)
			if code := uzicli.ExitCodeFor(err); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			rendered := cellText(err.Error())
			wantPrefix := "cannot read file: " + tc.want + ": \"long-component/"
			if !strings.HasPrefix(rendered, wantPrefix) {
				t.Errorf("rendered cause/path = %q, want prefix %q", rendered, wantPrefix)
			}
			if !strings.HasSuffix(rendered, "…") || utf8.RuneCountInString(rendered) != 201 {
				t.Errorf("final cellText did not cap long path: %q", rendered)
			}
		})
	}
}

func productSkillsFixture() (apitypes.ProductSkillsDTO, []apitypes.ProductDTO) {
	applied := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	by := "admin@example.com"
	staged := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	return apitypes.ProductSkillsDTO{
		Config:  apitypes.ProductSkillsConfigDTO{SkillsRepoURL: "https://git.example/acme/skills.git", SkillsRef: "main", SkillsTokenSet: true, Enabled: true},
		Applied: apitypes.ProductSkillsAppliedDTO{SHA: "aaaa1111", AppliedAt: &applied, AppliedBy: &by, Skills: []apitypes.ProductSkillDTO{{Name: "triage", Body: "SECRET BODY"}, {Name: "summarize"}}},
		Staged: &apitypes.ProductSkillsStagedDTO{
			SHA: "bbbb2222", StagedAt: staged, StagedBy: &by,
			Skills:  []apitypes.ProductSkillDTO{{Name: "triage"}, {Name: "translate"}},
			Dropped: []apitypes.ProductSkillDropDTO{{Name: "evil", Reason: "secret"}, {Reason: "over_limit", Count: 7}},
			Diff:    apitypes.ProductSkillsDiffDTO{Added: []string{"translate"}, Changed: []string{"triage"}, Removed: []string{"summarize"}, Unchanged: []string{}},
		},
	}, []apitypes.ProductDTO{
		{ID: "p1", Name: "Acme CRM", Enabled: true},
		{ID: "p2", Name: "Other"},
	}
}

func TestAdminProductsSkillsRender(t *testing.T) {
	skills, products := productSkillsFixture()
	fs := &filesServer{skills: skills, products: products}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	for _, ref := range []string{"Acme CRM", "p1", "acme crm"} {
		out, _, code := runCLI(t, httpEnv(srv), "admin", "products", "skills", ref)
		if code != 0 {
			t.Fatalf("%s: exit = %d\n%s", ref, code, out)
		}
		for _, want := range []string{
			"Acme CRM", "https://git.example/acme/skills.git", "main", "TOKEN_SET", "yes", "ENABLED",
			"aaaa1111", "2026-09-10T08:00:00Z", "admin@example.com", "triage, summarize",
			"bbbb2222", "translate", "DIFF_ADDED", "DIFF_REMOVED", "summarize", "evil (secret)", "7 files (over_limit)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lost %q:\n%s", ref, want, out)
			}
		}
		if strings.Contains(out, "SECRET BODY") {
			t.Errorf("a skill body was printed:\n%s", out)
		}
	}
}

func TestAdminProductsSkillsUnknownAndNoStaged(t *testing.T) {
	skills, products := productSkillsFixture()
	skills.Staged = nil
	skills.Config.Enabled = false
	skills.Config.SkillsTokenSet = false
	fs := &filesServer{skills: skills, products: products}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	out, _, code := runCLI(t, httpEnv(srv), "admin", "products", "skills", "p1")
	if code != 0 || !strings.Contains(out, "STAGED") || !strings.Contains(out, "none") || !strings.Contains(out, "UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS") {
		t.Errorf("exit = %d\n%s", code, out)
	}
	_, errOut, code := runCLI(t, httpEnv(srv), "admin", "products", "skills", "missing")
	if code != uzicli.ExitNotFound || !strings.Contains(errOut, "no product") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	// products (no subcommand) still lists.
	if out, _, code := runCLI(t, httpEnv(srv), "admin", "products"); code != 0 || !strings.Contains(out, "Acme CRM") {
		t.Errorf("list exit = %d\n%s", code, out)
	}
}

func TestAdminProductsSkillsHostileStringsEscaped(t *testing.T) {
	hostile := func(s string) string { return s + esc2J + oscTitle + "\u202e" + "\nFORGED_ROW  x" }
	skills, products := productSkillsFixture()
	skills.Config.SkillsRepoURL = hostile("https://e.example/")
	skills.Config.SkillsRef = hostile("ref")
	skills.Applied.Skills = []apitypes.ProductSkillDTO{{Name: hostile("a")}}
	skills.Staged.Skills = []apitypes.ProductSkillDTO{{Name: hostile("b")}}
	skills.Staged.Dropped = []apitypes.ProductSkillDropDTO{{Name: hostile("d"), Reason: hostile("r")}}
	skills.Staged.Diff = apitypes.ProductSkillsDiffDTO{Added: []string{hostile("x")}, Changed: []string{hostile("y")}, Removed: []string{hostile("z")}}
	by := hostile("adm")
	skills.Staged.StagedBy = &by
	skills.Applied.AppliedBy = &by
	products[0].Name = "Acme CRM"
	fs := &filesServer{skills: skills, products: products}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	out, _, code := runCLI(t, httpEnv(srv), "admin", "products", "skills", "p1")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	assertNoTerminalControl(t, "admin products skills", out)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FORGED_ROW") {
			t.Errorf("a newline forged a row: %q", line)
		}
	}
}

// truncatedFileServer promises 1000 bytes under a valid storage name, sends 10 and aborts the
// connection (http.ErrAbortHandler), like a server or proxy dying mid-stream.
func truncatedFileServer(t *testing.T, name string, onPartial func()) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		if onPartial != nil {
			onPartial()
		}
		panic(http.ErrAbortHandler)
	}))
}

func TestJobFileGetAbortedDownloadLeavesNothingAndNextGetSucceeds(t *testing.T) {
	body := "file bytes"
	name := sha256Hex(body) + ".txt"
	srv := truncatedFileServer(t, name, nil)
	dir := t.TempDir()
	t.Chdir(dir)
	_, _, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1")
	srv.Close()
	if code == 0 {
		t.Fatalf("an aborted download exited 0")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left behind after an aborted download: %v", ents)
	}
	fs := &filesServer{fileBody: body, fileName: name}
	good := httptest.NewServer(fs.handler(t))
	defer good.Close()
	if out, _, code := runCLI(t, httpEnv(good), "job", "file", "get", "f1"); code != 0 {
		t.Fatalf("the next get exit = %d\n%s", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != body { //nolint:gosec // G304: test reads a file it wrote under t.TempDir()
		t.Errorf("file = %q, %v", b, err)
	}
}

// A file another process puts at the target while the download streams is never replaced.
func TestJobFileGetNeverReplacesFileAppearingMidDownload(t *testing.T) {
	body := "file bytes"
	name := sha256Hex(body) + ".txt"
	dir := t.TempDir()
	t.Chdir(dir)
	target := filepath.Join(dir, name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body[:4]))
		w.(http.Flusher).Flush()
		// Wait until the client has its temp file open, so the seed lands mid-download.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			found := false
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				found = found || strings.HasPrefix(e.Name(), ".uzi-download-")
			}
			if found {
				break
			}
		}
		if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
			t.Errorf("seed: %v", err)
		}
		_, _ = w.Write([]byte(body[4:]))
	}))
	defer srv.Close()
	_, errOut, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1")
	if code != uzicli.ExitUsage || !strings.Contains(errOut, "refusing to overwrite") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	if b, _ := os.ReadFile(target); string(b) != "precious" { //nolint:gosec // G304: test reads a file it wrote under t.TempDir()
		t.Errorf("a file that appeared mid-download was replaced: %q", b)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("temp file left behind: %v", ents)
	}
}

// TestHelperJobFileGetStalled is the child half of TestJobFileGetInterruptedLeavesNothingAtTarget:
// it runs `job file get` against the URL in the environment and is killed by the parent.
func TestHelperJobFileGetStalled(t *testing.T) {
	url := os.Getenv("UZI_TEST_HELPER_URL")
	if url == "" {
		t.Skip("helper process only")
	}
	if err := os.Chdir(os.Getenv("UZI_TEST_HELPER_DIR")); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(nil)
	env.NewClient = func(uzicli.Settings) uzicli.Client {
		return &uzicli.HTTPClient{BaseURL: url, Token: "uzc_test", HTTP: http.DefaultClient}
	}
	runCLI(t, env, "job", "file", "get", "f1")
}

// Ctrl-C (SIGINT) kills the CLI without running deferred cleanup, so nothing may exist at the
// target name while the body is still streaming.
func TestJobFileGetInterruptedLeavesNothingAtTarget(t *testing.T) {
	name := sha256Hex("whole file") + ".txt"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperJobFileGetStalled$") //nolint:gosec // G204: re-runs this test binary with a fixed argument (helper-process pattern)
	cmd.Env = append(os.Environ(), "UZI_TEST_HELPER_URL="+srv.URL, "UZI_TEST_HELPER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	// Wait until the child has started writing (something exists in the directory), then interrupt.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ents, _ := os.ReadDir(dir); len(ents) > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the child never began the download")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the child survived SIGINT")
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
		t.Fatalf("an interrupted download left %s at the target name", name)
	}
	// A later get into the same directory is unaffected by whatever the interrupt left.
	fs := &filesServer{fileBody: "whole file", fileName: name}
	good := httptest.NewServer(fs.handler(t))
	defer good.Close()
	t.Chdir(dir)
	if out, _, code := runCLI(t, httpEnv(good), "job", "file", "get", "f1"); code != 0 {
		t.Fatalf("the next get exit = %d\n%s", code, out)
	}
}

func TestJobFileGetRefusesDashOutput(t *testing.T) {
	fs := &filesServer{fileBody: "x", fileName: sha256Hex("x")}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	t.Chdir(dir)
	_, errOut, code := runCLI(t, httpEnv(srv), "job", "file", "get", "f1", "-o", "-")
	if code != uzicli.ExitUsage || !strings.Contains(errOut, "not standard output") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left behind: %v", ents)
	}
}

func TestJobCreateFilePartialUploadNamesUnattachedIDs(t *testing.T) {
	a := writeTemp(t, "a.txt", "x")
	b := writeTemp(t, "b.txt", "y")
	fs := &filesServer{uploadErr: http.StatusUnsupportedMediaType, uploadErrAfter: 1}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	_, errOut, code := runCLI(t, httpEnv(srv), "job", "create", "--type", "research", "--prompt", "p", "--file", a, "--file", b)
	if code != uzicli.ExitUsage || !strings.Contains(errOut, "file-1") || !strings.Contains(errOut, "unattached") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
	if len(fs.jobBodies) != 0 {
		t.Errorf("a job was created after a failed upload")
	}
}
