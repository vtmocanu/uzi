package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4784 LiveDB suite: the module document routes through the real router.

type mdEnv struct {
	h      *Handler
	router http.Handler
	pool   interface{ Close() }
	user   uuid.UUID
	jwt    string
}

func newMDEnv(t *testing.T) (*mdEnv, func() uuid.UUID, func(uuid.UUID) string) {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	e := &mdEnv{h: h, router: router, pool: pool, user: user, jwt: cliMintJWT(t, pool, user)}
	newUser := func() uuid.UUID { return cliSeedUser(t, pool, false) }
	jwtFor := func(u uuid.UUID) string { return cliMintJWT(t, pool, u) }
	return e, newUser, jwtFor
}

func mdPath(mod string) string { return "/api/modules/" + mod + "/user-data/" }

func mdBody(data, rev string) string {
	return `{"data":` + data + `,"expected_revision":` + rev + `}`
}

func (e *mdEnv) do(t *testing.T, jwt, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, e.router, method, path, jwt, body)
}

func mdRevision(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		Revision *string `json:"revision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Revision == nil {
		t.Fatalf("revision body %q: %v", rec.Body.String(), err)
	}
	return *v.Revision
}

func (e *mdEnv) put(t *testing.T, jwt, mod, data, rev string) string {
	t.Helper()
	rec := e.do(t, jwt, "PUT", mdPath(mod), mdBody(data, rev))
	if rec.Code != 200 {
		t.Fatalf("PUT %s: %d %s", mod, rec.Code, rec.Body.String())
	}
	return mdRevision(t, rec)
}

func wantStatus(t *testing.T, name string, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		n := rec.Body.Len()
		if n > 200 {
			n = 200
		}
		t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, want, rec.Body.String()[:n])
	}
}

type mdGetBody struct {
	Data     json.RawMessage `json:"data"`
	Revision *string         `json:"revision"`
}

func (e *mdEnv) get(t *testing.T, jwt, mod string) mdGetBody {
	t.Helper()
	rec := e.do(t, jwt, "GET", mdPath(mod), "")
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d", mod, rec.Code)
	}
	var b mdGetBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("GET body: %v", err)
	}
	return b
}

func TestModuleUserDataLifecycleLiveDB(t *testing.T) {
	e, newUser, jwtFor := newMDEnv(t)

	// AC1: empty read.
	rec := e.do(t, e.jwt, "GET", mdPath("notes"), "")
	wantStatus(t, "empty get", rec, 200)
	if rec.Body.String() != `{"data":null,"revision":null}`+"\n" {
		t.Errorf("empty body %q", rec.Body.String())
	}

	// AC2: first save, readable from a second session.
	r1 := e.put(t, e.jwt, "notes", `{"a":[1,2]}`, "null")
	second := jwtFor(e.user)
	g := e.get(t, second, "notes")
	if g.Revision == nil || *g.Revision != r1 || string(g.Data) != `{"a": [1, 2]}` {
		t.Errorf("second session read: %+v data %s", g, g.Data)
	}

	// AC3: stale PUT / DELETE conflict with the current revision.
	r2 := e.put(t, e.jwt, "notes", `2`, `"`+r1+`"`)
	rec = e.do(t, e.jwt, "PUT", mdPath("notes"), mdBody(`3`, `"`+r1+`"`))
	wantStatus(t, "stale put", rec, 409)
	if mdRevision(t, rec) != r2 {
		t.Errorf("stale put current = %s want %s", rec.Body.String(), r2)
	}
	rec = e.do(t, e.jwt, "DELETE", mdPath("notes")+"?expected_revision="+r1, "")
	wantStatus(t, "stale delete", rec, 409)
	if mdRevision(t, rec) != r2 {
		t.Errorf("stale delete current = %s", rec.Body.String())
	}
	rec = e.do(t, e.jwt, "PUT", mdPath("notes"), mdBody(`3`, `null`))
	wantStatus(t, "create over existing", rec, 409)

	// AC5: another user's view and attempts.
	other := newUser()
	oj := jwtFor(other)
	if g := e.get(t, oj, "notes"); g.Revision != nil {
		t.Errorf("user B sees A's doc: %+v", g)
	}
	rec = e.do(t, oj, "PUT", mdPath("notes"), mdBody(`9`, `"`+r2+`"`))
	wantStatus(t, "B put with A's revision", rec, 409)
	if !strings.Contains(rec.Body.String(), `"revision":null`) {
		t.Errorf("B conflict leaked A's revision: %s", rec.Body.String())
	}
	rec = e.do(t, oj, "DELETE", mdPath("notes")+"?expected_revision="+r2, "")
	wantStatus(t, "B delete with A's revision", rec, 409)
	if g := e.get(t, e.jwt, "notes"); g.Revision == nil || *g.Revision != r2 || string(g.Data) != `2` {
		t.Errorf("A changed: %+v", g)
	}
	rb := e.put(t, oj, "notes", `"b"`, "null")
	if rb == r2 {
		t.Errorf("revisions collide across users")
	}

	// AC4 + AC7: save, delete, save again; new revision distinct; old ones dead.
	rec = e.do(t, e.jwt, "DELETE", mdPath("notes")+"?expected_revision="+r2, "")
	wantStatus(t, "delete", rec, 204)
	if rec.Body.Len() != 0 {
		t.Errorf("204 body %q", rec.Body.String())
	}
	if g := e.get(t, e.jwt, "notes"); g.Revision != nil || string(g.Data) != "null" {
		t.Errorf("after delete: %+v", g)
	}
	r3 := e.put(t, e.jwt, "notes", `4`, "null")
	if r3 == r1 || r3 == r2 {
		t.Errorf("revision reused: %s %s %s", r1, r2, r3)
	}
	for _, old := range []string{r1, r2} {
		wantStatus(t, "old put", e.do(t, e.jwt, "PUT", mdPath("notes"), mdBody(`5`, `"`+old+`"`)), 409)
		wantStatus(t, "old delete", e.do(t, e.jwt, "DELETE", mdPath("notes")+"?expected_revision="+old, ""), 409)
	}
	// An empty or non-canonical expected_revision never matches.
	wantStatus(t, "empty delete rev", e.do(t, e.jwt, "DELETE", mdPath("notes")+"?expected_revision=", ""), 409)
	wantStatus(t, "padded rev", e.do(t, e.jwt, "PUT", mdPath("notes"), mdBody(`5`, `"0`+r3+`"`)), 409)
	wantStatus(t, "empty rev put", e.do(t, e.jwt, "PUT", mdPath("notes"), mdBody(`5`, `""`)), 409)
	wantStatus(t, "delete without param", e.do(t, e.jwt, "DELETE", mdPath("notes"), ""), 400)
}

func TestModuleUserDataValueContractLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	cases := map[string]string{
		"scalar":  `"hello"`,
		"number":  `12.5`,
		"bool":    `false`,
		"array":   `[1,"two",[3],{"four":4}]`,
		"nested":  `{"a":{"b":{"c":[true,null,{"d":"e"}]}}}`,
		"dupkeys": `{"k":1,"k":2}`,
	}
	for name, data := range cases {
		mod := "v-" + name
		e.put(t, e.jwt, mod, data, "null")
		g := e.get(t, e.jwt, mod)
		var want, got any
		if err := json.Unmarshal([]byte(data), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(g.Data, &got); err != nil {
			t.Fatalf("%s: read-back %s: %v", name, g.Data, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: read back %s, want value-equal to %s", name, g.Data, data)
		}
	}
	if g := e.get(t, e.jwt, "v-dupkeys"); string(g.Data) != `{"k": 2}` {
		t.Errorf("duplicate keys: last must win, got %s", g.Data)
	}
}

func TestModuleUserDataGetShapeLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	rev := e.put(t, e.jwt, "shape", `{"x":[1]}`, "null")
	rec := e.do(t, e.jwt, "GET", mdPath("shape"), "")
	body := rec.Body.Bytes()
	if !json.Valid(body) || !bytes.HasSuffix(body, []byte("}\n")) {
		t.Errorf("body %q", body)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", rec.Header())
	}
	if !strings.HasSuffix(string(body), `"revision":"`+rev+"\"}\n") {
		t.Errorf("revision tail %q", body)
	}
}

func TestModuleUserDataRejectsLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	bad := map[string]string{
		"missing data":    `{"expected_revision":null}`,
		"missing rev":     `{"data":1}`,
		"null data":       mdBody(`null`, `null`),
		"upper DATA":      `{"DATA":1,"expected_revision":null}`,
		"upper REV":       `{"data":1,"EXPECTED_REVISION":null}`,
		"numeric rev":     mdBody(`1`, `5`),
		"object rev":      mdBody(`1`, `{}`),
		"extra key":       `{"data":1,"expected_revision":null,"x":1}`,
		"top null":        `null`,
		"top array":       `[1]`,
		"top string":      `"x"`,
		"bom":             "\xef\xbb\xbf" + mdBody(`1`, `null`),
		"doubled comma":   `{"data":1,,"expected_revision":null}`,
		"after brace":     mdBody(`1`, `null`) + ` x`,
		"malformed":       `{"data":`,
		"nul nested":      mdBody(`{"a":"\u0000"}`, `null`),
		"nul in key":      mdBody(`{"\u0000":1}`, `null`),
		"lone surrogate":  mdBody(`"\ud800"`, `null`),
		"invalid utf8":    mdBody("\"a\xffb\"", `null`),
		"huge fraction":   mdBody(`1.`+strings.Repeat("1", 20000), `null`),
		"overflow":        mdBody(`1e309`, `null`),
		"neg overflow":    mdBody(`-1e309`, `null`),
		"underflow":       mdBody(`1e-400`, `null`),
		"depth 10001":     mdBody(strings.Repeat("[", 10001)+strings.Repeat("]", 10001), `null`),
		"empty":           ``,
		"whitespace only": "  \n",
	}
	for name, body := range bad {
		rec := e.do(t, e.jwt, "PUT", mdPath("rej"), body)
		wantStatus(t, name, rec, 400)
	}
	if g := e.get(t, e.jwt, "rej"); g.Revision != nil {
		t.Errorf("a rejected body stored a document")
	}

	// Invalid module ids.
	for _, mod := range []string{"A", "1a", "a_b", "a%2Fb", "a%20b", strings.Repeat("a", 33), "a.b"} {
		wantStatus(t, "module "+mod, e.do(t, e.jwt, "PUT", "/api/modules/"+mod+"/user-data/", mdBody(`1`, `null`)), 400)
		wantStatus(t, "module get "+mod, e.do(t, e.jwt, "GET", "/api/modules/"+mod+"/user-data/", ""), 400)
	}
	wantStatus(t, "module 32", e.do(t, e.jwt, "PUT", mdPath(strings.Repeat("a", 32)), mdBody(`1`, `null`)), 200)

	// An unstorable body is a 400 even with a stale or malformed revision, and the
	// stored document is untouched.
	rev := e.put(t, e.jwt, "keep", `{"ok":1}`, "null")
	wantStatus(t, "unstorable with 007", e.do(t, e.jwt, "PUT", mdPath("keep"), mdBody(`"\u0000"`, `"007"`)), 400)
	wantStatus(t, "unstorable create over existing", e.do(t, e.jwt, "PUT", mdPath("keep"), mdBody(`"\u0000"`, `null`)), 400)
	wantStatus(t, "unstorable stale", e.do(t, e.jwt, "PUT", mdPath("keep"), mdBody(`"\u0000"`, `"1"`)), 400)
	if g := e.get(t, e.jwt, "keep"); g.Revision == nil || *g.Revision != rev || string(g.Data) != `{"ok": 1}` {
		t.Errorf("keep changed: %+v %s", g, g.Data)
	}
}

func TestModuleUserDataCapLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	revs := map[string]string{}
	for i := 0; i < store.MaxModuleUserDocuments; i++ {
		m := fmt.Sprintf("m%d", i)
		revs[m] = e.put(t, e.jwt, m, `1`, "null")
	}
	rec := e.do(t, e.jwt, "PUT", mdPath("extra"), mdBody(`1`, `null`))
	wantStatus(t, "33rd", rec, 409)
	if !strings.Contains(rec.Body.String(), "too many module documents") {
		t.Errorf("cap body %s", rec.Body.String())
	}
	e.put(t, e.jwt, "m0", `2`, `"`+revs["m0"]+`"`)
	wantStatus(t, "delete frees a slot", e.do(t, e.jwt, "DELETE", mdPath("m1")+"?expected_revision="+revs["m1"], ""), 204)
	e.put(t, e.jwt, "extra", `1`, "null")
}

func TestModuleUserDataZeroRewriteAndDepthLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	doc := `{"a":[0e-400,1,-0.000,0E+5,0,0.0e-99,12],"0e-9":"x\\\"0.0","k":0.00}`
	e.put(t, e.jwt, "zeros", doc, "null")
	g := e.get(t, e.jwt, "zeros")
	var got, want any
	_ = json.Unmarshal(g.Data, &got)
	_ = json.Unmarshal([]byte(`{"a":[0,1,0,0,0,0,12],"0e-9":"x\\\"0.0","k":0}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("zeros read back %s", g.Data)
	}

	// D2: depth 10,000 stores and reads back; 10,001 is refused.
	deep := strings.Repeat("[", 10000) + strings.Repeat("]", 10000)
	e.put(t, e.jwt, "deep", deep, "null")
	rec := e.do(t, e.jwt, "GET", mdPath("deep"), "")
	wantStatus(t, "deep get", rec, 200)
	b := rec.Body.Bytes()
	const prefix = `{"data":`
	i := bytes.LastIndex(b, []byte(`,"revision":"`))
	if !bytes.HasPrefix(b, []byte(prefix)) || i < 0 {
		t.Fatalf("deep body shape")
	}
	data := b[len(prefix):i]
	if !json.Valid(data) || len(bytes.TrimSpace(data)) < 20000 {
		t.Errorf("deep data not valid JSON of depth 10000 (len %d)", len(data))
	}
	wantStatus(t, "depth 10001", e.do(t, e.jwt, "PUT", mdPath("deep"), mdBody(strings.Repeat("[", 10001)+strings.Repeat("]", 10001), `null`)), 400)
}

func TestModuleUserDataReadBackBoundLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	for name, lit := range map[string]string{"max": "1e308", "min": "5e-324"} {
		n := (65536 - 40) / (len(lit) + 1)
		data := "[" + strings.TrimSuffix(strings.Repeat(lit+",", n), ",") + "]"
		e.put(t, e.jwt, name, data, "null")
		rec := e.do(t, e.jwt, "GET", mdPath(name), "")
		wantStatus(t, name+" get", rec, 200)
		if rec.Body.Len() >= 5<<20 {
			t.Errorf("%s: read-back %d bytes", name, rec.Body.Len())
		}
		var body struct {
			Data []float64 `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(body.Data) != n {
			t.Fatalf("%s: %d elements, want %d", name, len(body.Data), n)
		}
		for _, v := range body.Data {
			if v != body.Data[0] {
				t.Fatalf("%s: elements differ", name)
			}
		}
		want := map[string]float64{"max": 1e308, "min": 5e-324}[name]
		if body.Data[0] != want {
			t.Errorf("%s: element %v want %v", name, body.Data[0], want)
		}
	}
}

// countingReader yields up to limit bytes of 'x' and counts what it actually returned.
type countingReader struct {
	limit, n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n >= c.limit {
		return 0, io.EOF
	}
	k := len(p)
	if k > c.limit-c.n {
		k = c.limit - c.n
	}
	for i := 0; i < k; i++ {
		p[i] = 'x'
	}
	c.n += k
	return k, nil
}

func (e *mdEnv) rawPut(t *testing.T, body io.Reader, mod string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", mdPath(mod), body)
	req.ContentLength = -1
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: e.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, e.jwt))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestModuleUserDataSizeCapLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	cr := &countingReader{limit: 1 << 20}
	wantStatus(t, "streamed 1MiB", e.rawPut(t, cr, "big"), 413)
	if cr.n > 65537 {
		t.Errorf("handler read %d bytes of an over-cap body, want <= 65537", cr.n)
	}

	exact := func(total int, tail string) string {
		head := `{"data":"`
		suffix := `","expected_revision":null}` + tail
		return head + strings.Repeat("a", total-len(head)-len(suffix)) + suffix
	}
	b := exact(65536, "")
	if len(b) != 65536 {
		t.Fatalf("len %d", len(b))
	}
	wantStatus(t, "exactly 65536", e.do(t, e.jwt, "PUT", mdPath("exact"), b), 200)
	b = exact(65537, "")
	wantStatus(t, "65537", e.do(t, e.jwt, "PUT", mdPath("exact2"), b), 413)

	pad := strings.Repeat(" ", 70000)
	valid := mdBody(`1`, `null`)
	for name, body := range map[string]string{
		"early syntax error": `{,` + pad,
		"trailing value":     valid + pad + `1`,
		"wrong keys":         `{"x":1}` + pad,
		"valid plus ws":      valid + pad,
	} {
		wantStatus(t, "over cap "+name, e.do(t, e.jwt, "PUT", mdPath("padded"), body), 413)
	}
	under := strings.Repeat(" ", 1000)
	wantStatus(t, "under cap valid+ws", e.do(t, e.jwt, "PUT", mdPath("padded"), valid+under), 200)
	wantStatus(t, "under cap malformed", e.do(t, e.jwt, "PUT", mdPath("padded2"), `{,`+under), 400)
}

func TestModuleUserDataAuthBoundaryLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	rev := e.put(t, e.jwt, "auth", `1`, "null")

	for _, tc := range []struct{ method, body string }{
		{"PUT", mdBody(`2`, `"`+rev+`"`)},
		{"DELETE", ""},
	} {
		path := mdPath("auth")
		if tc.method == "DELETE" {
			path += "?expected_revision=" + rev
		}
		req := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: e.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		wantStatus(t, tc.method+" without CSRF", rec, 403)
	}
	if g := e.get(t, e.jwt, "auth"); g.Revision == nil || *g.Revision != rev {
		t.Errorf("a CSRF-less write changed the document")
	}

	_, _, pool := cliLiveDB(t)
	uzc := cliMintToken(t, pool, e.user, clitoken.ScopeUser)
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		wantStatus(t, "bearer "+m, bearerReq(e.router, m, mdPath("auth")+"?expected_revision=1", uzc), 401)
	}
}

func TestModuleUserDataNoStoreLiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	rev := e.put(t, e.jwt, "ns", `1`, "null")
	type probe struct {
		name string
		rec  *httptest.ResponseRecorder
		want int
	}
	noCookie := httptest.NewRecorder()
	e.router.ServeHTTP(noCookie, httptest.NewRequest("GET", mdPath("ns"), nil))
	noCSRF := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", mdPath("ns"), strings.NewReader(mdBody(`1`, `null`)))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: e.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	e.router.ServeHTTP(noCSRF, req)
	probes := []probe{
		{"200", e.do(t, e.jwt, "GET", mdPath("ns"), ""), 200},
		{"400", e.do(t, e.jwt, "GET", mdPath("BAD"), ""), 400},
		{"401", noCookie, 401},
		{"403", noCSRF, 403},
		{"409", e.do(t, e.jwt, "PUT", mdPath("ns"), mdBody(`1`, `null`)), 409},
		{"413", e.do(t, e.jwt, "PUT", mdPath("ns"), strings.Repeat(" ", 70000)), 413},
		{"405 POST", e.do(t, e.jwt, "POST", mdPath("ns"), ""), 405},
		{"405 PATCH", e.do(t, e.jwt, "PATCH", mdPath("ns"), ""), 405},
		{"405 PROPFIND", e.do(t, e.jwt, "PROPFIND", mdPath("ns"), ""), 405},
		{"204", e.do(t, e.jwt, "DELETE", mdPath("ns")+"?expected_revision="+rev, ""), 204},
	}
	for _, p := range probes {
		wantStatus(t, p.name, p.rec, p.want)
		if got := p.rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q", p.name, got)
		}
	}

	// Seam failures through the real router.
	e.h.moduleData = &fakeModuleData{err: errors.New("boom")}
	rec := e.do(t, e.jwt, "GET", mdPath("ns"), "")
	wantStatus(t, "seam 500", rec, 500)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("500 lacks no-store")
	}
	e.h.moduleData = &fakeModuleData{err: &store.ModuleDataCommitError{Err: errors.New("x")}}
	rec = e.do(t, e.jwt, "PUT", mdPath("ns"), mdBody(`1`, `null`))
	wantStatus(t, "seam 503", rec, 503)
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), "outcome unknown") {
		t.Errorf("503: %q %s", rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	e.h.moduleData = nil
}

func TestModuleUserDataAuthLayer503LiveDB(t *testing.T) {
	e, _, _ := newMDEnv(t)
	e.pool.Close()
	rec := e.do(t, e.jwt, "GET", mdPath("ns"), "")
	wantStatus(t, "closed pool", rec, 503)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("auth-layer 503 lacks no-store")
	}
}
