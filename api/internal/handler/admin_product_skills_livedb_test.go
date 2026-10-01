package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M6 (D9): the admin product skill-set routes, driven through the PRODUCTION router
// (h.Routes via v1Routers), with the skills-repo reader replaced by a scripted fake so no network
// clone is needed (agentsource.FetchSkillFiles is tested in its own package, against real git).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const psAllowedBase = "https://skills.example.com"
const psRepoURL = psAllowedBase + "/acme/product-skills.git"

// psFakeFetch scripts agentsource.FetchSkillFiles and records how it was called.
type psFakeFetch struct {
	mu    sync.Mutex
	calls []agentsource.CloneOptions
	maxes []int
	sha   string
	files []agentsource.SkillFile
	notes []agentsource.Note
	err   error
	// hook runs inside the fetch (to interleave a concurrent edit).
	hook func()
}

func (f *psFakeFetch) fetch(_ context.Context, opts agentsource.CloneOptions, maxFileBytes int) (string, []agentsource.SkillFile, []agentsource.Note, error) {
	f.mu.Lock()
	f.calls = append(f.calls, opts)
	f.maxes = append(f.maxes, maxFileBytes)
	hook, sha, files, notes, err := f.hook, f.sha, f.files, f.notes, f.err
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return sha, files, notes, err
}

func (f *psFakeFetch) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *psFakeFetch) last() agentsource.CloneOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func psSkillFile(dir, name, desc, body string) agentsource.SkillFile {
	return agentsource.SkillFile{Dir: dir, Data: []byte(
		"---\nname: " + name + "\ndescription: " + desc + "\nallowed-tools: Bash, WebFetch\nhooks: {PreToolUse: x}\n---\n\n" + body)}
}

func psSHA(c byte) string { return strings.Repeat(string(c), 40) }

type psEnv struct {
	h      *Handler
	pool   *pgxpool.Pool
	routes http.Handler
	admin  uuid.UUID
	jwt    string
	fake   *psFakeFetch
}

func psSetup(t *testing.T) *psEnv {
	t.Helper()
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	h.cfg.ProductSkillsAllowedBaseURLs = []string{psAllowedBase}
	h.cfg.SkillMaxBytes = 65536
	h.cfg.SkillsMaxPerRun = 32
	fake := &psFakeFetch{sha: psSHA('a')}
	h.productSkillsFetch = fake.fetch
	admin := cliSeedUser(t, pool, true)
	return &psEnv{h: h, pool: pool, routes: routes, admin: admin, jwt: cliMintJWT(t, pool, admin), fake: fake}
}

func (e *psEnv) product(t *testing.T) string {
	t.Helper()
	return apCreate(t, e.routes, e.jwt, "Skills "+uuid.NewString(), "").ID
}

func (e *psEnv) patch(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return cookieReq(t, e.routes, http.MethodPatch, "/api/admin/products/"+id, e.jwt, string(raw))
}

func (e *psEnv) configure(t *testing.T, id string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"skills_repo_url": psRepoURL}
	for k, v := range extra {
		body[k] = v
	}
	if rec := e.patch(t, id, body); rec.Code != http.StatusOK {
		t.Fatalf("configure skills source = %d %q", rec.Code, rec.Body.String())
	}
}

func (e *psEnv) sync(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, e.routes, http.MethodPost, "/api/admin/products/"+id+"/skills/sync", e.jwt, "")
}

func (e *psEnv) apply(t *testing.T, id, sha string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"expected_sha": sha})
	return cookieReq(t, e.routes, http.MethodPost, "/api/admin/products/"+id+"/skills/apply", e.jwt, string(raw))
}

func (e *psEnv) view(t *testing.T, id string) apitypes.ProductSkillsDTO {
	t.Helper()
	rec := cookieReq(t, e.routes, http.MethodGet, "/api/admin/products/"+id+"/skills", e.jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET skills = %d %q", rec.Code, rec.Body.String())
	}
	return psDecodeView(t, rec.Body.String())
}

func psDecodeView(t *testing.T, body string) apitypes.ProductSkillsDTO {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var v apitypes.ProductSkillsDTO
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode product skills view %q: %v", body, err)
	}
	return v
}

func (e *psEnv) appliedCount(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM skills WHERE scope = 'product' AND product_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count applied skills: %v", err)
	}
	return n
}

func psNames(s []apitypes.ProductSkillDTO) string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Name)
	}
	return strings.Join(out, ",")
}

// psToken assembles a token-shaped fixture at run time from fragments (never a literal).
func psToken() string { return "fixtok" + "-" + strings.Repeat("Qx7m", 8) }

// TestProductSkillsSourceValidationLiveDB: the allowlist, userinfo and token rules of the PATCH.
func TestProductSkillsSourceValidationLiveDB(t *testing.T) {
	e := psSetup(t)
	id := e.product(t)
	tok := psToken()

	bad := []struct {
		name string
		body map[string]any
	}{
		{"userinfo with a token", map[string]any{"skills_repo_url": "https://" + tok + "@skills.example.com/acme/r.git"}},
		{"userinfo user:password", map[string]any{"skills_repo_url": "https://u:" + tok + "@skills.example.com/acme/r.git"}},
		{"userinfo disguising the allowlisted host", map[string]any{"skills_repo_url": "https://skills.example.com@evil.example.org/acme/r.git"}},
		{"http scheme", map[string]any{"skills_repo_url": "http://skills.example.com/acme/r.git"}},
		{"not on the allowlist", map[string]any{"skills_repo_url": "https://evil.example.org/acme/r.git"}},
		{"the allowlisted host on another port", map[string]any{"skills_repo_url": "https://skills.example.com:8443/acme/r.git"}},
		{"a query string", map[string]any{"skills_repo_url": psRepoURL + "?token=" + tok}},
		{"a fragment", map[string]any{"skills_repo_url": psRepoURL + "#x"}},
		{"padded with whitespace", map[string]any{"skills_repo_url": " " + psRepoURL}},
		{"a scheme-less string", map[string]any{"skills_repo_url": "skills.example.com/acme/r.git"}},
		{"a ref with whitespace", map[string]any{"skills_ref": "main branch"}},
		{"a ref over the cap", map[string]any{"skills_ref": strings.Repeat("r", 257)}},
		{"an empty token", map[string]any{"skills_token": "   "}},
		{"a token with whitespace", map[string]any{"skills_token": tok + " " + tok}},
		{"a token with a control character", map[string]any{"skills_token": tok + "\n"}},
		{"a token over the 1024 cap", map[string]any{"skills_token": strings.Repeat("t", 1025)}},
		{"a token and clear_skills_token together", map[string]any{"skills_token": tok, "clear_skills_token": true}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			rec := e.patch(t, id, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PATCH = %d %q, want 400", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), tok) {
				t.Fatalf("the error response echoes the token: %q", rec.Body.String())
			}
		})
	}
	var url string
	var sealed []byte
	if err := e.pool.QueryRow(t.Context(), `SELECT skills_repo_url, skills_token_sealed FROM products WHERE id = $1`, id).Scan(&url, &sealed); err != nil {
		t.Fatal(err)
	}
	if url != "" || sealed != nil {
		t.Fatalf("refused PATCHes changed the product: url=%q sealed=%v", url, sealed != nil)
	}

	t.Run("a token of exactly 1024 characters is accepted", func(t *testing.T) {
		if rec := e.patch(t, id, map[string]any{"skills_token": strings.Repeat("t", 1024)}); rec.Code != http.StatusOK {
			t.Fatalf("PATCH 1024-char token = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("an empty PATCH names nothing to update", func(t *testing.T) {
		if rec := e.patch(t, id, map[string]any{}); rec.Code != http.StatusBadRequest {
			t.Fatalf("empty PATCH = %d, want 400", rec.Code)
		}
	})
	t.Run("the source is saved and read back; a cleared URL is accepted", func(t *testing.T) {
		e.configure(t, id, map[string]any{"skills_ref": "v1.2.0"})
		v := e.view(t, id)
		if v.Config.SkillsRepoURL != psRepoURL || v.Config.SkillsRef != "v1.2.0" || !v.Config.Enabled || !v.Config.SkillsTokenSet {
			t.Fatalf("config = %+v", v.Config)
		}
		if rec := e.patch(t, id, map[string]any{"skills_repo_url": ""}); rec.Code != http.StatusOK {
			t.Fatalf("clearing the URL = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("with the allowlist empty the feature is off: no URL is accepted", func(t *testing.T) {
		e.h.cfg.ProductSkillsAllowedBaseURLs = nil
		t.Cleanup(func() { e.h.cfg.ProductSkillsAllowedBaseURLs = []string{psAllowedBase} })
		rec := e.patch(t, id, map[string]any{"skills_repo_url": psRepoURL})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not enabled") {
			t.Fatalf("PATCH with the feature off = %d %q, want 400 naming that it is not enabled", rec.Code, rec.Body.String())
		}
		if v := e.view(t, id); v.Config.Enabled {
			t.Error("GET skills reports the feature enabled with an empty allowlist")
		}
	})
}

// TestProductSkillsTokenNeverInAnyResponseLiveDB: the clone token is write-only. It is sealed at
// rest under an AAD bound to the product, handed to the fetch only inside the sync, and appears in
// no response body (success or error) and no log line.
func TestProductSkillsTokenNeverInAnyResponseLiveDB(t *testing.T) {
	e := psSetup(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	id := e.product(t)
	tok := psToken()
	var bodies []string
	note := func(label string, rec *httptest.ResponseRecorder) {
		bodies = append(bodies, label+": "+rec.Body.String())
	}

	rec := e.patch(t, id, map[string]any{"skills_repo_url": psRepoURL, "skills_ref": "main", "skills_token": tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %q", rec.Code, rec.Body.String())
	}
	note("PATCH", rec)
	apDecodeProduct(t, rec.Code, rec.Body.String()) // DisallowUnknownFields: no stray field

	// At rest: sealed, not the plaintext, openable only under THIS product's AAD.
	var sealed []byte
	if err := e.pool.QueryRow(t.Context(), `SELECT skills_token_sealed FROM products WHERE id = $1`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 || bytes.Contains(sealed, []byte(tok)) {
		t.Fatalf("the stored token is not sealed: %d bytes, contains plaintext = %v", len(sealed), bytes.Contains(sealed, []byte(tok)))
	}
	pid := uuid.MustParse(id)
	plain, err := e.h.box.OpenWithAAD(sealed, productSkillsTokenAAD(pid))
	if err != nil || string(plain) != tok {
		t.Fatalf("the sealed token does not open under the product's AAD: %v", err)
	}
	if _, err := e.h.box.OpenWithAAD(sealed, productSkillsTokenAAD(uuid.New())); err == nil {
		t.Error("the sealed token opened under ANOTHER product's AAD: it is not bound to its product")
	}
	if got := string(productSkillsTokenAAD(pid)); got != "product_skills_token|"+id {
		t.Errorf("AAD = %q, want product_skills_token|<id>", got)
	}

	// Every read surface says only set / not set.
	list := cookieReq(t, e.routes, http.MethodGet, "/api/admin/products", e.jwt, "")
	note("list products", list)
	get := cookieReq(t, e.routes, http.MethodGet, "/api/admin/products/"+id+"/skills", e.jwt, "")
	note("GET skills", get)
	if v := psDecodeView(t, get.Body.String()); !v.Config.SkillsTokenSet {
		t.Error("GET skills does not report the token as set")
	}

	// A successful sync hands the plaintext to the fetch and only there.
	e.fake.files = []agentsource.SkillFile{psSkillFile("one", "one", "first", "body one")}
	rec = e.sync(t, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %q", rec.Code, rec.Body.String())
	}
	note("sync ok", rec)
	got := e.fake.last()
	if got.Token != tok || got.CloneURL != psRepoURL || got.Ref != "main" {
		t.Fatalf("the fetch was called with url=%q ref=%q token-matches=%v", got.CloneURL, got.Ref, got.Token == tok)
	}
	if got.RedirectAllowed == nil || !got.RedirectAllowed(psRepoURL) || got.RedirectAllowed("https://evil.example.org/x") {
		t.Error("the fetch's redirect guard is missing or does not enforce the product-skills allowlist")
	}
	if e.fake.maxes[len(e.fake.maxes)-1] != 65536 {
		t.Errorf("the fetch was given a file cap of %d, want SKILL_MAX_BYTES", e.fake.maxes[len(e.fake.maxes)-1])
	}

	// The apply responses (success and the stale-set 409) and the stale-source sync 409 are
	// scanned too.
	rec = e.apply(t, id, psSHA('b'))
	note("apply stale sha", rec)
	if rec.Code != http.StatusConflict {
		t.Fatalf("apply with a stale sha = %d %q, want 409", rec.Code, rec.Body.String())
	}
	rec = e.apply(t, id, psSHA('a'))
	note("apply ok", rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %q", rec.Code, rec.Body.String())
	}
	e.fake.mu.Lock()
	e.fake.hook = func() {
		if _, err := e.pool.Exec(t.Context(), `UPDATE products SET skills_ref = 'moved' WHERE id = $1`, id); err != nil {
			t.Errorf("move the ref mid-sync: %v", err)
		}
	}
	e.fake.mu.Unlock()
	rec = e.sync(t, id)
	note("sync source changed", rec)
	if rec.Code != http.StatusConflict {
		t.Fatalf("sync whose source changed mid-flight = %d %q, want 409", rec.Code, rec.Body.String())
	}
	e.fake.mu.Lock()
	e.fake.hook = nil
	e.fake.mu.Unlock()
	if _, err := e.pool.Exec(t.Context(), `UPDATE products SET skills_ref = 'main' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	// A failing fetch: the response is generic and never the remote's error text.
	e.fake.err = errors.New("REMOTE-ERROR-MARKER unable to access the repository")
	rec = e.sync(t, id)
	note("sync failed", rec)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "REMOTE-ERROR-MARKER") {
		t.Fatalf("failed sync = %d %q, want 502 without the fetch error text", rec.Code, rec.Body.String())
	}
	e.fake.err = nil

	// A sealed token copied onto ANOTHER product does not decrypt there (AAD binding), and the
	// refusal is generic.
	other := e.product(t)
	e.configure(t, other, nil)
	if _, err := e.pool.Exec(t.Context(), `UPDATE products SET skills_token_sealed = $1 WHERE id = $2`, sealed, other); err != nil {
		t.Fatal(err)
	}
	before := e.fake.callCount()
	rec = e.sync(t, other)
	note("sync foreign-sealed", rec)
	if rec.Code != http.StatusConflict || e.fake.callCount() != before {
		t.Fatalf("sync with a foreign-sealed token = %d %q (fetch calls %d -> %d), want 409 and no fetch", rec.Code, rec.Body.String(), before, e.fake.callCount())
	}

	// Clearing removes it and a later sync fetches anonymously.
	rec = e.patch(t, id, map[string]any{"clear_skills_token": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d %q", rec.Code, rec.Body.String())
	}
	note("clear", rec)
	if v := e.view(t, id); v.Config.SkillsTokenSet {
		t.Error("the token still reads as set after clear_skills_token")
	}
	if rec := e.sync(t, id); rec.Code != http.StatusOK || e.fake.last().Token != "" {
		t.Fatalf("sync after clear = %d, token passed = %q; want 200 and an anonymous clone", rec.Code, e.fake.last().Token)
	}

	for _, b := range bodies {
		if strings.Contains(b, tok) {
			t.Errorf("a response carries the token: %s", b)
		}
	}
	if strings.Contains(logs.String(), tok) {
		t.Errorf("a log line carries the token:\n%s", logs.String())
	}
}

// TestProductSkillsSyncStageApplyLiveDB is the approval flow: sync stages (frontmatter stripped,
// nothing applied), apply needs the reviewed SHA, replaces the product's skills atomically and
// records the admin and SHA.
func TestProductSkillsSyncStageApplyLiveDB(t *testing.T) {
	e := psSetup(t)
	id := e.product(t)

	t.Run("sync refuses a product with no source, an unknown id, and a deleted product", func(t *testing.T) {
		if rec := e.sync(t, id); rec.Code != http.StatusConflict {
			t.Errorf("sync with no source = %d %q, want 409", rec.Code, rec.Body.String())
		}
		if rec := e.sync(t, uuid.NewString()); rec.Code != http.StatusNotFound {
			t.Errorf("sync unknown = %d, want 404", rec.Code)
		}
		if rec := e.apply(t, uuid.NewString(), psSHA('a')); rec.Code != http.StatusNotFound {
			t.Errorf("apply unknown = %d, want 404", rec.Code)
		}
		if rec := e.apply(t, id, psSHA('a')); rec.Code != http.StatusConflict {
			t.Errorf("apply with nothing staged = %d %q, want 409", rec.Code, rec.Body.String())
		}
		gone := e.product(t)
		e.configure(t, gone, nil)
		if rec := cookieReq(t, e.routes, http.MethodDelete, "/api/admin/products/"+gone, e.jwt, ""); rec.Code != http.StatusOK {
			t.Fatalf("delete = %d", rec.Code)
		}
		if rec := e.sync(t, gone); rec.Code != http.StatusConflict {
			t.Errorf("sync of a deleted product = %d, want 409", rec.Code)
		}
		if rec := e.apply(t, gone, psSHA('a')); rec.Code != http.StatusConflict {
			t.Errorf("apply of a deleted product = %d, want 409", rec.Code)
		}
	})

	e.configure(t, id, map[string]any{"skills_ref": "main"})

	t.Run("a sync stages: frontmatter stripped, nothing applied, drops recorded", func(t *testing.T) {
		secret := "sk-ant-" + "api03-" + strings.Repeat("Aa1_", 12)
		e.fake.sha = psSHA('a')
		e.fake.files = []agentsource.SkillFile{
			psSkillFile("brand-voice", "brand-voice", "How we write", "# Voice\nBe brief.\n"),
			psSkillFile("tone", "tone", "Tone of voice", "Friendly.\n"),
			{Dir: "broken", Data: []byte("no frontmatter at all")},
			psSkillFile("leaky", "leaky", "Has a credential", "key: "+secret),
			psSkillFile("dupe-a", "dupe", "First", "a"),
			psSkillFile("dupe-b", "dupe", "Second", "b"),
		}
		e.fake.notes = []agentsource.Note{{Name: "huge", Reason: agentsource.NoteTooLarge}}
		rec := e.sync(t, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("sync = %d %q", rec.Code, rec.Body.String())
		}
		v := psDecodeView(t, rec.Body.String())
		if v.Staged == nil || v.Staged.SHA != psSHA('a') {
			t.Fatalf("staged = %+v", v.Staged)
		}
		if got := psNames(v.Staged.Skills); got != "brand-voice,dupe,tone" {
			t.Fatalf("staged skills = %s, want brand-voice,dupe,tone", got)
		}
		for _, s := range v.Staged.Skills {
			for _, stripped := range []string{"allowed-tools", "WebFetch", "PreToolUse", "hooks"} {
				if strings.Contains(s.Name+s.Description+s.Body, stripped) {
					t.Errorf("staged skill %q kept stripped frontmatter %q", s.Name, stripped)
				}
			}
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Error("a skill carrying a full credential was staged")
		}
		reasons := map[string]string{}
		for _, d := range v.Staged.Dropped {
			reasons[d.Name] = d.Reason
		}
		for name, want := range map[string]string{"broken": "invalid", "leaky": "secret", "dupe": "duplicate", "huge": "too_large"} {
			if reasons[name] != want {
				t.Errorf("drop %q = %q, want %q (all: %v)", name, reasons[name], want, reasons)
			}
		}
		if len(v.Staged.Diff.Added) != 3 || len(v.Staged.Diff.Removed) != 0 {
			t.Errorf("diff = %+v, want 3 added against an empty applied set", v.Staged.Diff)
		}
		if v.Applied.SHA != "" || len(v.Applied.Skills) != 0 || e.appliedCount(t, id) != 0 {
			t.Fatalf("a sync applied something: %+v", v.Applied)
		}
	})

	t.Run("apply needs the reviewed SHA; a wrong or malformed one applies nothing", func(t *testing.T) {
		for _, sha := range []string{psSHA('b'), "abc", "", strings.ToUpper(psSHA('a'))} {
			rec := e.apply(t, id, sha)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
				t.Errorf("apply(%q) = %d, want 400 or 409", sha, rec.Code)
			}
		}
		if n := e.appliedCount(t, id); n != 0 {
			t.Fatalf("a refused apply wrote %d skills", n)
		}
		if v := e.view(t, id); v.Staged == nil {
			t.Fatal("a refused apply discarded the staged set")
		}
	})

	t.Run("apply replaces the product's skills and records the admin and the SHA", func(t *testing.T) {
		rec := e.apply(t, id, psSHA('a'))
		if rec.Code != http.StatusOK {
			t.Fatalf("apply = %d %q", rec.Code, rec.Body.String())
		}
		v := psDecodeView(t, rec.Body.String())
		if v.Staged != nil {
			t.Error("the staged set survived its apply")
		}
		if v.Applied.SHA != psSHA('a') || v.Applied.AppliedAt == nil || v.Applied.AppliedBy == nil || *v.Applied.AppliedBy != e.admin.String() {
			t.Errorf("applied = %+v, want the SHA, a time and the approving admin %s", v.Applied, e.admin)
		}
		if got := psNames(v.Applied.Skills); got != "brand-voice,dupe,tone" || e.appliedCount(t, id) != 3 {
			t.Errorf("applied skills = %s (%d rows)", got, e.appliedCount(t, id))
		}
		var scope, by string
		if err := e.pool.QueryRow(t.Context(),
			`SELECT scope, updated_by::text FROM skills WHERE product_id = $1 AND name = 'tone'`, id).Scan(&scope, &by); err != nil || scope != "product" || by != e.admin.String() {
			t.Errorf("applied row scope=%q updated_by=%q err=%v", scope, by, err)
		}
		if rec := e.apply(t, id, psSHA('a')); rec.Code != http.StatusConflict {
			t.Errorf("a second apply of the same SHA = %d, want 409 (nothing staged)", rec.Code)
		}
	})

	t.Run("a second sync shows the diff; applying it replaces changed, added and removed skills", func(t *testing.T) {
		e.fake.sha = psSHA('c')
		e.fake.notes = nil
		e.fake.files = []agentsource.SkillFile{
			psSkillFile("brand-voice", "brand-voice", "How we write", "# Voice\nBe brief.\n"), // unchanged
			psSkillFile("tone", "tone", "Tone of voice", "Formal now.\n"),                     // changed
			psSkillFile("newcomer", "newcomer", "Fresh", "new body"),                          // added
			// dupe is gone                                                                          // removed
		}
		rec := e.sync(t, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("sync = %d %q", rec.Code, rec.Body.String())
		}
		d := psDecodeView(t, rec.Body.String()).Staged.Diff
		if fmt.Sprint(d.Added, d.Changed, d.Removed, d.Unchanged) != "[newcomer] [tone] [dupe] [brand-voice]" {
			t.Fatalf("diff = %+v", d)
		}
		// The applied set keeps serving until the approval.
		if n := e.appliedCount(t, id); n != 3 {
			t.Fatalf("a second sync changed the applied set (%d rows)", n)
		}
		// A sync racing the review replaces the stage, so the SHA the admin reviewed is refused.
		e.fake.sha = psSHA('d')
		if rec := e.sync(t, id); rec.Code != http.StatusOK {
			t.Fatalf("racing sync = %d", rec.Code)
		}
		if rec := e.apply(t, id, psSHA('c')); rec.Code != http.StatusConflict {
			t.Fatalf("apply of a superseded SHA = %d, want 409", rec.Code)
		}
		if rec := e.apply(t, id, psSHA('d')); rec.Code != http.StatusOK {
			t.Fatalf("apply of the current SHA = %d %q", rec.Code, rec.Body.String())
		}
		v := e.view(t, id)
		if got := psNames(v.Applied.Skills); got != "brand-voice,newcomer,tone" || v.Applied.SHA != psSHA('d') {
			t.Fatalf("applied = %s @ %s, want brand-voice,newcomer,tone @ d...", got, v.Applied.SHA)
		}
		for _, s := range v.Applied.Skills {
			if s.Name == "tone" && !strings.Contains(s.Body, "Formal now.") {
				t.Errorf("tone was not replaced: %q", s.Body)
			}
		}
	})

	t.Run("an empty repo stages and applies as an empty set (removing the skills)", func(t *testing.T) {
		e.fake.sha = psSHA('e')
		e.fake.files = nil
		if rec := e.sync(t, id); rec.Code != http.StatusOK {
			t.Fatalf("sync of an empty repo = %d", rec.Code)
		}
		if rec := e.apply(t, id, psSHA('e')); rec.Code != http.StatusOK {
			t.Fatalf("apply of an empty set = %d", rec.Code)
		}
		if n := e.appliedCount(t, id); n != 0 {
			t.Fatalf("an empty approved set left %d skills", n)
		}
	})

	t.Run("a failed fetch stages nothing and keeps the earlier stage", func(t *testing.T) {
		e.fake.sha, e.fake.files, e.fake.err = psSHA('f'), []agentsource.SkillFile{psSkillFile("kept", "kept", "d", "b")}, nil
		if rec := e.sync(t, id); rec.Code != http.StatusOK {
			t.Fatal("setup sync failed")
		}
		e.fake.err = errors.New("boom")
		if rec := e.sync(t, id); rec.Code != http.StatusBadGateway {
			t.Fatalf("failing sync = %d, want 502", rec.Code)
		}
		e.fake.err = nil
		if v := e.view(t, id); v.Staged == nil || v.Staged.SHA != psSHA('f') {
			t.Fatalf("staged after a failed sync = %+v, want the earlier f... stage", v.Staged)
		}
	})

	t.Run("changing the repo URL or ref discards the staged set but keeps the applied one", func(t *testing.T) {
		before := e.appliedCount(t, id)
		if rec := e.patch(t, id, map[string]any{"skills_ref": "release"}); rec.Code != http.StatusOK {
			t.Fatalf("PATCH ref = %d", rec.Code)
		}
		v := e.view(t, id)
		if v.Staged != nil {
			t.Error("the staged snapshot of the old ref survived a ref change")
		}
		if e.appliedCount(t, id) != before {
			t.Error("a source change touched the applied skills")
		}
		// Re-sending the same value is not a change.
		e.fake.sha, e.fake.files = psSHA('1'), []agentsource.SkillFile{psSkillFile("x", "x", "d", "b")}
		e.sync(t, id)
		e.patch(t, id, map[string]any{"skills_ref": "release"})
		if e.view(t, id).Staged == nil {
			t.Error("re-saving an unchanged source discarded the stage")
		}
	})

	t.Run("the allowlist is re-checked at sync time: zero egress when the URL is no longer allowed", func(t *testing.T) {
		e.h.cfg.ProductSkillsAllowedBaseURLs = []string{"https://other.example.com"}
		t.Cleanup(func() { e.h.cfg.ProductSkillsAllowedBaseURLs = []string{psAllowedBase} })
		before := e.fake.callCount()
		if rec := e.sync(t, id); rec.Code != http.StatusBadRequest || e.fake.callCount() != before {
			t.Fatalf("sync after the allowlist changed = %d (fetch calls %d -> %d), want 400 and no fetch", rec.Code, before, e.fake.callCount())
		}
	})

	t.Run("a source changed while the clone ran is a 409 and stages nothing", func(t *testing.T) {
		p := e.product(t)
		e.configure(t, p, nil)
		e.fake.sha, e.fake.files = psSHA('2'), []agentsource.SkillFile{psSkillFile("y", "y", "d", "b")}
		e.fake.hook = func() {
			_, _ = e.pool.Exec(context.Background(), `UPDATE products SET skills_ref = 'moved' WHERE id = $1`, p)
		}
		t.Cleanup(func() { e.fake.hook = nil })
		if rec := e.sync(t, p); rec.Code != http.StatusConflict {
			t.Fatalf("sync racing a source edit = %d %q, want 409", rec.Code, rec.Body.String())
		}
		if e.view(t, p).Staged != nil {
			t.Error("a snapshot of a source that is no longer configured was staged")
		}
	})

	t.Run("the skills cap applies at stage time", func(t *testing.T) {
		p := e.product(t)
		e.configure(t, p, nil)
		e.h.cfg.SkillsMaxPerRun = 2
		t.Cleanup(func() { e.h.cfg.SkillsMaxPerRun = 32 })
		e.fake.hook = nil
		e.fake.sha = psSHA('3')
		e.fake.files = []agentsource.SkillFile{
			psSkillFile("c", "c", "d", "b"), psSkillFile("a", "a", "d", "b"), psSkillFile("b", "b", "d", "b"),
		}
		v := psDecodeView(t, e.sync(t, p).Body.String())
		if got := psNames(v.Staged.Skills); got != "a,b" {
			t.Fatalf("staged = %s, want the first 2 by name (a,b)", got)
		}
		if len(v.Staged.Dropped) != 1 || v.Staged.Dropped[0].Name != "c" || v.Staged.Dropped[0].Reason != "over_limit" {
			t.Fatalf("dropped = %+v, want c over_limit", v.Staged.Dropped)
		}
	})
}

// TestProductSkillsRoutesAuthLiveDB: the GET is readable by a uza_ token; every write is
// cookie-only (a Bearer of any class is refused before the handler), and a non-admin is refused.
func TestProductSkillsRoutesAuthLiveDB(t *testing.T) {
	e := psSetup(t)
	id := e.product(t)
	e.configure(t, id, nil)
	member := cliSeedUser(t, e.pool, false)
	memberJWT := cliMintJWT(t, e.pool, member)
	uza := cliMintToken(t, e.pool, e.admin, clitoken.ScopeAdminRO)
	uzc := cliMintToken(t, e.pool, e.admin, clitoken.ScopeUser)

	get := "/api/admin/products/" + id + "/skills"
	if rec := bearerReq(e.routes, http.MethodGet, get, uza); rec.Code != http.StatusOK {
		t.Errorf("uza_ GET skills = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(e.routes, http.MethodGet, get, uzc); rec.Code != http.StatusForbidden {
		t.Errorf("admin's uzc_ GET skills = %d, want 403", rec.Code)
	}
	if rec := cookieReq(t, e.routes, http.MethodGet, get, memberJWT, ""); rec.Code != http.StatusForbidden {
		t.Errorf("member GET skills = %d, want 403", rec.Code)
	}
	if rec := bearerReq(e.routes, http.MethodGet, get, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET skills = %d, want 401", rec.Code)
	}
	if rec := bearerReq(e.routes, http.MethodGet, "/api/admin/products/"+uuid.NewString()+"/skills", uza); rec.Code != http.StatusNotFound {
		t.Errorf("GET skills of an unknown product = %d, want 404", rec.Code)
	}

	e.fake.files = []agentsource.SkillFile{psSkillFile("z", "z", "d", "b")}
	if rec := e.sync(t, id); rec.Code != http.StatusOK {
		t.Fatalf("setup sync = %d", rec.Code)
	}
	calls := e.fake.callCount()
	writes := []struct{ method, path, body string }{
		{http.MethodPatch, "/api/admin/products/" + id, `{"skills_repo_url":"` + psRepoURL + `","skills_token":"` + psToken() + `"}`},
		{http.MethodPost, "/api/admin/products/" + id + "/skills/sync", ""},
		{http.MethodPost, "/api/admin/products/" + id + "/skills/apply", `{"expected_sha":"` + psSHA('a') + `"}`},
	}
	for _, w := range writes {
		for name, token := range map[string]string{"uza_": uza, "uzc_": uzc} {
			if rec := bearerReqBody(e.routes, w.method, w.path, token, w.body); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s %s = %d %q, want 401 (cookie-only)", name, w.method, w.path, rec.Code, rec.Body.String())
			}
		}
		if rec := cookieReq(t, e.routes, w.method, w.path, memberJWT, w.body); rec.Code != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403", w.method, w.path, rec.Code)
		}
	}
	if e.fake.callCount() != calls {
		t.Error("a refused write reached the skills-repo reader")
	}
	if n := e.appliedCount(t, id); n != 0 {
		t.Errorf("a refused write applied %d skills", n)
	}
	if v := e.view(t, id); v.Staged == nil || v.Config.SkillsTokenSet {
		t.Errorf("a refused write changed the product: staged=%v token_set=%v", v.Staged != nil, v.Config.SkillsTokenSet)
	}
}

// TestProductSkillsNeverInSkillsListingsLiveDB: an applied product skill is in no /api/skills
// listing, read, write or allocation, for an admin or a member.
func TestProductSkillsNeverInSkillsListingsLiveDB(t *testing.T) {
	e := psSetup(t)
	id := e.product(t)
	e.configure(t, id, nil)
	e.fake.sha = psSHA('a')
	e.fake.files = []agentsource.SkillFile{psSkillFile("only-for-the-product", "only-for-the-product", "d", "product body")}
	if rec := e.sync(t, id); rec.Code != http.StatusOK {
		t.Fatalf("sync = %d", rec.Code)
	}
	if rec := e.apply(t, id, psSHA('a')); rec.Code != http.StatusOK {
		t.Fatalf("apply = %d", rec.Code)
	}
	var psID string
	if err := e.pool.QueryRow(t.Context(), `SELECT id::text FROM skills WHERE product_id = $1`, id).Scan(&psID); err != nil {
		t.Fatal(err)
	}
	g := uuid.New()
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO skills (id, name, description, body, scope) VALUES ($1, $2, 'd', 'b', 'global')`, g, "g-"+g.String()[:8]); err != nil {
		t.Fatal(err)
	}
	tmpl := uuid.New()
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`, tmpl, "t-"+tmpl.String()[:8]); err != nil {
		t.Fatal(err)
	}
	member := cliSeedUser(t, e.pool, false)
	memberJWT := cliMintJWT(t, e.pool, member)

	for who, jwt := range map[string]string{"admin": e.jwt, "member": memberJWT} {
		rec := cookieReq(t, e.routes, http.MethodGet, "/api/skills", jwt, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET /api/skills = %d", who, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, psID) || strings.Contains(body, "only-for-the-product") || strings.Contains(body, `"scope":"product"`) {
			t.Errorf("%s: /api/skills lists the product skill: %s", who, body)
		}
		if !strings.Contains(rec.Body.String(), g.String()) {
			t.Errorf("%s: /api/skills lost the global skill (positive control)", who)
		}
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/skills/" + psID, ""},
			{http.MethodPut, "/api/skills/" + psID, `{"description":"x","body":"y"}`},
			{http.MethodDelete, "/api/skills/" + psID, ""},
			{http.MethodPost, "/api/skills/" + psID + "/reset", ""},
		} {
			if rec := cookieReq(t, e.routes, c.method, c.path, jwt, c.body); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s %s = %d %q, want 404", who, c.method, c.path, rec.Code, rec.Body.String())
			}
		}
	}
	// Allocation: neither half will take a product skill, and it is not in the allocation view.
	if rec := cookieReq(t, e.routes, http.MethodPut, "/api/agent-templates/"+tmpl.String()+"/skills", e.jwt, `{"shared_skill_ids":["`+psID+`"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("allocating a product skill as shared = %d %q, want 400", rec.Code, rec.Body.String())
	}
	for who, jwt := range map[string]string{"admin": e.jwt, "member": memberJWT} {
		if rec := cookieReq(t, e.routes, http.MethodPut, "/api/agent-templates/"+tmpl.String()+"/skills", jwt, `{"my_skill_ids":["`+psID+`"]}`); rec.Code != http.StatusBadRequest {
			t.Errorf("%s allocating a product skill to their overlay = %d %q, want 400", who, rec.Code, rec.Body.String())
		}
	}
	if rec := cookieReq(t, e.routes, http.MethodGet, "/api/agent-templates/"+tmpl.String()+"/skills", e.jwt, ""); strings.Contains(rec.Body.String(), psID) {
		t.Errorf("the allocation view lists the product skill: %s", rec.Body.String())
	}
	// The skill is still exactly as approved after every refused write.
	var body string
	if err := e.pool.QueryRow(t.Context(), `SELECT body FROM skills WHERE id = $1`, psID).Scan(&body); err != nil || !strings.Contains(body, "product body") {
		t.Errorf("product skill after the refused writes = %q, %v", body, err)
	}
}

// TestProductDTOCarriesNoSkillsTokenMaterial pins the products list/patch DTOs: they are explicit
// field lists, so the sealed token column never reaches them.
func TestProductDTOCarriesNoSkillsTokenMaterial(t *testing.T) {
	raw, err := json.Marshal(productDTO(storeProductWithSealedToken(), 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"skills_token", "sealed", "SEALED-BYTES"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("ProductDTO JSON carries %q: %s", leaked, raw)
		}
	}
}

func storeProductWithSealedToken() store.Product {
	return store.Product{
		ID: uuid.New(), Name: "p", SkillsRepoUrl: psRepoURL, SkillsRef: "main",
		SkillsTokenSealed: []byte("SEALED-BYTES"), SkillsAppliedSha: psSHA('a'),
	}
}

// TestProductSkillsSyncIsSingleFlightLiveDB: one sync clones at a time instance-wide (the clone
// decodes an untrusted pack into memory). A second sync while one is in flight is a 429 with a
// fixed message and makes no fetch; once the first finishes a sync is accepted again.
func TestProductSkillsSyncIsSingleFlightLiveDB(t *testing.T) {
	e := psSetup(t)
	first, second := e.product(t), e.product(t)
	e.configure(t, first, nil)
	e.configure(t, second, nil)
	e.fake.files = []agentsource.SkillFile{psSkillFile("one", "one", "first", "body one")}

	entered, release := make(chan struct{}), make(chan struct{})
	e.fake.mu.Lock()
	e.fake.hook = func() {
		close(entered)
		<-release
	}
	e.fake.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.sync(t, first) }()
	<-entered

	calls := e.fake.callCount()
	rec := e.sync(t, second)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "already running") {
		t.Fatalf("a concurrent sync = %d %q, want 429 naming that one is running", rec.Code, rec.Body.String())
	}
	if e.fake.callCount() != calls {
		t.Fatal("the refused concurrent sync still cloned")
	}

	e.fake.mu.Lock()
	e.fake.hook = nil
	e.fake.mu.Unlock()
	close(release)
	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("the first sync = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if rec := e.sync(t, second); rec.Code != http.StatusOK {
		t.Fatalf("a sync after the first finished = %d %q, want 200 (the flag must be released)", rec.Code, rec.Body.String())
	}
}

// TestProductSkillsTokenClearedOnOriginChangeLiveDB: the sealed clone token belongs to the
// repo's origin. A URL edit that moves to another origin (or clears the URL) drops it; a path-only
// edit keeps it; a PATCH that supplies a new token alongside the new URL keeps the NEW one.
func TestProductSkillsTokenClearedOnOriginChangeLiveDB(t *testing.T) {
	e := psSetup(t)
	other := "https://other.example.com"
	e.h.cfg.ProductSkillsAllowedBaseURLs = []string{psAllowedBase, other}
	tok := psToken()

	tokenSet := func(t *testing.T, id string) bool {
		t.Helper()
		return e.view(t, id).Config.SkillsTokenSet
	}
	fresh := func(t *testing.T) string {
		t.Helper()
		id := e.product(t)
		e.configure(t, id, map[string]any{"skills_token": tok})
		if !tokenSet(t, id) {
			t.Fatal("setup: token not set")
		}
		return id
	}

	t.Run("another origin drops the token", func(t *testing.T) {
		id := fresh(t)
		if rec := e.patch(t, id, map[string]any{"skills_repo_url": other + "/acme/r.git"}); rec.Code != http.StatusOK {
			t.Fatalf("PATCH = %d %q", rec.Code, rec.Body.String())
		}
		if tokenSet(t, id) {
			t.Fatal("the old token survived a move to another origin")
		}
		var sealed []byte
		if err := e.pool.QueryRow(t.Context(), `SELECT skills_token_sealed FROM products WHERE id = $1`, id).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if sealed != nil {
			t.Fatal("the sealed token is still stored")
		}
	})
	t.Run("a path-only change keeps it", func(t *testing.T) {
		id := fresh(t)
		if rec := e.patch(t, id, map[string]any{"skills_repo_url": psAllowedBase + "/acme/another.git"}); rec.Code != http.StatusOK {
			t.Fatalf("PATCH = %d %q", rec.Code, rec.Body.String())
		}
		if !tokenSet(t, id) {
			t.Fatal("a same-origin URL edit dropped the token")
		}
	})
	t.Run("clearing the URL drops it", func(t *testing.T) {
		id := fresh(t)
		if rec := e.patch(t, id, map[string]any{"skills_repo_url": ""}); rec.Code != http.StatusOK {
			t.Fatalf("PATCH = %d %q", rec.Code, rec.Body.String())
		}
		if tokenSet(t, id) {
			t.Fatal("the token survived clearing the URL")
		}
	})
	t.Run("a new token in the same PATCH is kept", func(t *testing.T) {
		id := fresh(t)
		if rec := e.patch(t, id, map[string]any{"skills_repo_url": other + "/acme/r.git", "skills_token": tok + "-new"}); rec.Code != http.StatusOK {
			t.Fatalf("PATCH = %d %q", rec.Code, rec.Body.String())
		}
		if !tokenSet(t, id) {
			t.Fatal("the token supplied with the new URL was dropped")
		}
	})
}
