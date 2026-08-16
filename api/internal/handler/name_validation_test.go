package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// Issue #169, the validator half. `uzi admin workers` and `uzi admin cli-tokens` print a
// name its OWNER chose beside a DIFFERENT user's owner_email, so a crafted name is
// terminal control injection into another user's session — from an ordinary non-admin
// account, with no hostile server involved. The render boundary (#180) strips it on the
// way out; these tests pin that it never gets stored in the first place.
//
// EVERY HANDLER HERE IS BUILT WITH A NIL POOL AND A NIL QUERIER, borrowing
// hosted_workers_test.go's device: "this request must not reach the database" stops being
// an assertion about a spy and becomes a mechanical fact. If a validator ever stopped
// gating, the request would dereference nil and PANIC the test rather than quietly
// persisting the name. A 400 plus the absence of a panic is the proof.

// The payloads, named so every test attacks the same bytes the issue reports.
const (
	// ESC-driven: erase-in-display + cursor-home blanks what the admin already read.
	nameWithESC = "worker\x1b[2J\x1b[1;1H-01"
	// U+202E RIGHT-TO-LEFT OVERRIDE — category Cf, so no C0/C1 range test sees it. This
	// is the case that proves the fix is the Cc+Cf pair and not a control-char check.
	nameWithBidi = "safe\u202egnp.exe"
	// The headline: text/tabwriter treats \n as a row terminator, so this one name
	// FORGES a whole row in a listing an admin reads to make decisions.
	nameForgingARow = "mine\nffffffff-0000-0000-0000-000000000000\tvictim@example.com\tworker\trunning"
	// U+200D ZWJ. Refused deliberately — the renderer would break the family glyph into
	// three, so storing it means storing a name that can only ever display wrong.
	nameWithZWJ = "family \U0001F468\u200d\U0001F469\u200d\U0001F467 box"
	// The near-neighbour that must NOT be refused: U+FE0F is Mn, not Cf.
	nameWithVariationSelector = "❤️ favourite"
)

// hostileNames is the reject corpus every create path below is driven with.
var hostileNames = map[string]string{
	"esc":        nameWithESC,
	"bidi":       nameWithBidi,
	"forged-row": nameForgingARow,
	"zwj-emoji":  nameWithZWJ,
	"nul":        "a\x00b",
	"del":        "a\x7fb",
	"zero-width": "a\u200bb",
	"bom":        "\ufeffname",
}

// errorBody reads httpx.Error's {"error": "..."} envelope. A 400 with an empty or
// unrelated message is, from the caller's side, the same failure as no message at all.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, rec.Body.String())
	}
	return env.Error
}

// authedPost drives a handler directly with an authenticated user in context. The route's
// RequireAuth is exercised by the router tests; these are the handler's own gates.
func authedPost(t *testing.T, fn http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	user := store.User{ID: uuid.New(), Email: "owner@uzi.local", IsActive: true}
	req = req.WithContext(mw.ContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// jsonName escapes a name into a JSON body. Building these by concatenation would put
// raw control bytes into the body and fail at the DECODER, which is a different 400 and
// would make every test below pass for the wrong reason.
func jsonName(t *testing.T, field, name string, extra string) string {
	t.Helper()
	b, err := json.Marshal(name)
	if err != nil {
		t.Fatalf("marshal name: %v", err)
	}
	return `{"` + field + `":` + string(b) + extra + `}`
}

// TestCreateWorkerRejectsUnsafeNames is issue #169 item 1: POST /api/workers validated
// LENGTH ONLY, so ESC was storable in a worker name.
//
// THIS IS A BEHAVIOUR CHANGE to a shipped endpoint — names that 201 today now 400.
func TestCreateWorkerRejectsUnsafeNames(t *testing.T) {
	for label, name := range hostileNames {
		t.Run(label, func(t *testing.T) {
			h := &Handler{} // nil pool, nil q, nil wsvc: reaching the store panics.
			rec := authedPost(t, h.CreateWorker, "/api/workers", jsonName(t, "name", name, ""))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q", rec.Code, name)
			}
			if msg := errorBody(t, rec); !strings.HasPrefix(msg, "name ") {
				t.Fatalf("error message does not name the field: %q", msg)
			}
		})
	}
}

// The positive control the rejections above cannot supply: a handler that 400s every
// name would pass all of them. This name is clean, so it must get PAST the validator —
// and with a nil wsvc, getting past it is a PANIC. Recovering that panic is the
// assertion: it proves the request reached CreateWorker rather than being turned away.
func TestCreateWorkerAcceptsCleanNamesPastTheValidator(t *testing.T) {
	clean := []string{
		"laptop-01",
		"Vlad's build box",
		"Ștefan's Mac",
		"工場ワーカー",
		"\U0001F511 key",
		nameWithVariationSelector, // U+FE0F is Mn, so this survives where a ZWJ family emoji does not
	}
	for _, name := range clean {
		t.Run(name, func(t *testing.T) {
			// Sanity-check the fixture against the shared predicate first, so a typo in
			// the literal above cannot silently turn this control into a no-op.
			if err := termsafe.Validate("name", name); err != nil {
				t.Fatalf("fixture is not a clean name: %v", err)
			}
			defer func() {
				if recover() == nil {
					t.Fatalf("no panic: %q was turned away before reaching the service", name)
				}
			}()
			h := &Handler{}
			_ = authedPost(t, h.CreateWorker, "/api/workers", jsonName(t, "name", name, ""))
		})
	}
}

// TestCreateCLITokenRejectsUnsafeNames is the same rule on PRD #64's surface, which
// issue #169 item 2 names: `uzi admin cli-tokens` renders this cell.
func TestCreateCLITokenRejectsUnsafeNames(t *testing.T) {
	for label, name := range hostileNames {
		t.Run(label, func(t *testing.T) {
			h := &Handler{}
			rec := authedPost(t, h.CreateCLIToken, "/api/cli-tokens", jsonName(t, "name", name, ""))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q", rec.Code, name)
			}
			if msg := errorBody(t, rec); !strings.HasPrefix(msg, "name ") {
				t.Fatalf("error message does not name the field: %q", msg)
			}
		})
	}
}

// TestCLIAuthStartRejectsUnsafeClientDesc closes the OTHER way a cli_tokens.name is
// written. browserTokenName turns client_desc into the token's name, so validating only
// the static mint path in cli_tokens.go would leave the same admin column reachable
// through an UNAUTHENTICATED endpoint. Found by the sweep, not by the issue.
func TestCLIAuthStartRejectsUnsafeClientDesc(t *testing.T) {
	for label, desc := range hostileNames {
		t.Run(label, func(t *testing.T) {
			h := &Handler{}
			body := jsonName(t, "client_desc", desc, `,"code_challenge":"aaaa"`)
			req := httptest.NewRequest(http.MethodPost, "/api/auth/cli/start", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.CLIAuthStart(rec, req) // unauthenticated by design
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q", rec.Code, desc)
			}
			if msg := errorBody(t, rec); !strings.HasPrefix(msg, "client_desc ") {
				t.Fatalf("error message does not name the field: %q", msg)
			}
		})
	}
}

// TestProvisionHostedWorkerRejectsUnsafeNames: the hosted path writes the SAME
// workers.name column and reaches the same cross-tenant listing, so a validator on only
// the external path would leave the column reachable. newHostedHandler also builds with
// a nil pool, so the 400 again proves nothing was written.
func TestProvisionHostedWorkerRejectsUnsafeNames(t *testing.T) {
	for label, name := range hostileNames {
		t.Run(label, func(t *testing.T) {
			h := newHostedHandler(true, "2")
			body := jsonName(t, "name", name, `,"template":"base","size":"m"`)
			rec := provisionReq(t, h, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q", rec.Code, name)
			}
			if msg := errorBody(t, rec); !strings.HasPrefix(msg, "name ") {
				t.Fatalf("error message does not name the field: %q", msg)
			}
		})
	}
}

// TestProvisionHostedWorkerDerivedNameStillPasses is the control on the ordering choice
// in that handler: the validator runs AFTER the empty-name fallback, so it sees the
// value that is actually stored. derivedHostedWorkerName composes a curated template id
// with a curated size, so it must never trip the rule it now runs through.
func TestProvisionHostedWorkerDerivedNameStillPasses(t *testing.T) {
	derived, err := derivedHostedWorkerName("base", "m")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if derived == "" {
		t.Fatal("derived name is empty — this control would be vacuous")
	}
	if err := termsafe.Validate("name", derived); err != nil {
		t.Fatalf("the derived default name fails the new validator: %v", err)
	}
}

// TestNameValidatorsAgreeWithTheRenderer is the cross-package half of termsafe's own
// biconditional test: it pins that these HANDLERS reject exactly what the CLI's CellText
// would change, rather than that termsafe's two functions agree with each other. A future
// handler that hand-rolls "its own slightly stricter" check fails here.
func TestNameValidatorsAgreeWithTheRenderer(t *testing.T) {
	corpus := []string{
		"laptop-01", "Ștefan's Mac", "工場ワーカー", nameWithVariationSelector,
		nameWithESC, nameWithBidi, nameForgingARow, nameWithZWJ,
		"a\u200bb", "\ufeffname", "a\x00b", "a\tb",
	}
	for _, name := range corpus {
		wantReject := termsafe.CellText(name) != name
		h := &Handler{}
		var gotReject bool
		func() {
			// A CLEAN name panics on the nil service, so the assignment below never
			// runs and gotReject stays false — which is the right answer for it. A
			// REJECTED name returns normally and sets it true. The recover is load-
			// bearing rather than defensive: it is how "got past the validator" is
			// observed here at all.
			defer func() { _ = recover() }()
			rec := authedPost(t, h.CreateWorker, "/api/workers", jsonName(t, "name", name, ""))
			gotReject = rec.Code == http.StatusBadRequest
		}()
		if gotReject != wantReject {
			t.Fatalf("CreateWorker rejects=%v but the renderer changes=%v for %q",
				gotReject, wantReject, name)
		}
	}
}
