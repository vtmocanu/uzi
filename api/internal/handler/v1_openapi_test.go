package handler

import (
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
)

// PRD #1907 D12 / M3: api/openapi/v1.yaml is the stable external contract, and these
// tests bind it to the code in both directions.
//
//   - TestV1OpenAPIRouteParity: the spec's operations and the PRODUCTION router's
//     /api/v1 routes (h.Routes, walked with chi.Walk) are the same set. A route added
//     without a spec entry, or a spec entry with no route, is red.
//   - TestV1OpenAPISchemasMatchDTOs: each operation's 200 schema has exactly the JSON
//     fields (names, required list, nullability, array/object shape) of the Go DTO the
//     handler returns, so the spec cannot drift from the DTO the recorded fixtures pin
//     (fixtures/api-contract/v1_whoami.*.json via apitypes' contract test).
//
// The spec lives inside the api module (api/openapi/), so the test cache hashes it and
// an edit to it re-runs these tests.

const v1SpecPath = "../../openapi/v1.yaml"

// v1ServerURL is the spec's single server url; spec paths are relative to it.
const v1ServerURL = "/api/v1"

type oaSchema struct {
	Ref        string               `yaml:"$ref"`
	Type       yaml.Node            `yaml:"type"`
	Format     string               `yaml:"format"`
	Required   []string             `yaml:"required"`
	Properties map[string]*oaSchema `yaml:"properties"`
	Items      *oaSchema            `yaml:"items"`
	OneOf      []*oaSchema          `yaml:"oneOf"`
	AnyOf      []*oaSchema          `yaml:"anyOf"`
	Enum       []string             `yaml:"enum"`
	// Nullable is OpenAPI 3.0's keyword; 3.1 spells null as a type. Refused if set.
	Nullable *bool `yaml:"nullable"`
}

type oaMedia struct {
	Schema *oaSchema `yaml:"schema"`
}

type oaResponse struct {
	Ref     string             `yaml:"$ref"`
	Content map[string]oaMedia `yaml:"content"`
}

type oaOperation struct {
	OperationID string                `yaml:"operationId"`
	Responses   map[string]oaResponse `yaml:"responses"`
}

type oaSpec struct {
	OpenAPI string `yaml:"openapi"`
	Servers []struct {
		URL string `yaml:"url"`
	} `yaml:"servers"`
	Security []map[string][]string           `yaml:"security"`
	Paths    map[string]map[string]yaml.Node `yaml:"paths"`
	Comps    struct {
		Schemas         map[string]*oaSchema      `yaml:"schemas"`
		Responses       map[string]oaResponse     `yaml:"responses"`
		SecuritySchemes map[string]map[string]any `yaml:"securitySchemes"`
	} `yaml:"components"`
}

// oaMethods are the path-item keys that are operations; every other path-item key
// OpenAPI allows is listed in oaPathItemFields.
var (
	oaMethods        = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
	oaPathItemFields = []string{"summary", "description", "servers", "parameters", "$ref"}
)

func loadV1Spec(t *testing.T) oaSpec {
	t.Helper()
	raw, err := os.ReadFile(v1SpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", v1SpecPath, err)
	}
	var spec oaSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", v1SpecPath, err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.1.") {
		t.Fatalf("openapi = %q, want a 3.1.x document (PRD #1907 D12)", spec.OpenAPI)
	}
	if len(spec.Servers) != 1 || spec.Servers[0].URL != v1ServerURL {
		t.Fatalf("servers = %+v, want exactly one with url %q (paths are relative to it)", spec.Servers, v1ServerURL)
	}
	return spec
}

// v1SpecOperations returns the spec's operations as "METHOD /api/v1/path" keys, each
// mapped to its decoded operation.
func v1SpecOperations(t *testing.T, spec oaSpec) map[string]oaOperation {
	t.Helper()
	ops := map[string]oaOperation{}
	for path, item := range spec.Paths {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("spec path %q does not start with /", path)
			continue
		}
		for key, node := range item {
			if !slices.Contains(oaMethods, key) {
				if !slices.Contains(oaPathItemFields, key) {
					t.Errorf("spec path %q has unknown key %q (neither an HTTP method nor a path-item field)", path, key)
				}
				continue
			}
			var op oaOperation
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decode %s %s: %v", key, path, err)
			}
			ops[strings.ToUpper(key)+" "+v1ServerURL+path] = op
		}
	}
	return ops
}

// v1RegexpParam matches a chi {name:regexp} segment so its regexp can be dropped: an
// OpenAPI path template names the parameter only.
var v1RegexpParam = regexp.MustCompile(`\{([^}:]+):[^}]*\}`)

// v1RouterOperations walks the PRODUCTION router (h.Routes, exactly as cmd/server
// builds it) and returns every route inside /api/v1 as "METHOD /api/v1/path".
func v1RouterOperations(t *testing.T) map[string]bool {
	t.Helper()
	lim := func() *mw.Limiter { return mw.NewLimiter(1_000_000, time.Hour, nil) }
	// Hosting on, as in route_limiter_mounts_test.go, so the table is the full one.
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true}}
	router := h.Routes(lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim())
	cr, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("Routes returned %T, not a chi.Routes", router)
	}
	ops := map[string]bool{}
	total := 0
	if err := chi.Walk(cr, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		total++
		if v1IsV1(pattern) {
			ops[method+" "+v1RegexpParam.ReplaceAllString(pattern, "{$1}")] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk Routes: %v", err)
	}
	if total < 100 {
		t.Fatalf("walked %d routes: too few for the production router, the walk is not covering it", total)
	}
	return ops
}

// TestV1OpenAPIRouteParity (PRD #1907 D12): the spec and the chi router list the same
// /api/v1 operations, in both directions.
func TestV1OpenAPIRouteParity(t *testing.T) {
	spec := loadV1Spec(t)
	specOps := v1SpecOperations(t, spec)
	routerOps := v1RouterOperations(t)

	var inRouterOnly, inSpecOnly []string
	for op := range routerOps {
		if _, ok := specOps[op]; !ok {
			inRouterOnly = append(inRouterOnly, op)
		}
	}
	for op := range specOps {
		if !routerOps[op] {
			inSpecOnly = append(inSpecOnly, op)
		}
	}
	sort.Strings(inRouterOnly)
	sort.Strings(inSpecOnly)
	for _, op := range inRouterOnly {
		t.Errorf("route %s is served but not described in %s: document it (the spec is the external contract)", op, v1SpecPath)
	}
	for _, op := range inSpecOnly {
		t.Errorf("%s describes %s but the production router does not serve it", v1SpecPath, op)
	}

	// Non-vacuity: the one B1 endpoint is on both sides (an empty spec and an unmounted
	// subtree would otherwise agree).
	if !routerOps["GET /api/v1/whoami"] || specOps["GET /api/v1/whoami"].OperationID == "" {
		t.Fatalf("GET /api/v1/whoami missing: router=%v spec=%v", routerOps["GET /api/v1/whoami"], specOps["GET /api/v1/whoami"].OperationID != "")
	}

	// Every operation is authenticated by the bearer scheme and documents the 401 that
	// RequireV1Caller answers for every failure (it is mounted on the whole subtree).
	if _, ok := spec.Comps.SecuritySchemes["bearerAuth"]; !ok {
		t.Error("components.securitySchemes.bearerAuth is missing")
	}
	if len(spec.Security) != 1 || spec.Security[0]["bearerAuth"] == nil {
		t.Errorf("top-level security = %v, want [{bearerAuth: []}]", spec.Security)
	}
	ids := map[string]string{}
	for key, op := range specOps {
		if op.OperationID == "" {
			t.Errorf("%s has no operationId", key)
		} else if prev, dup := ids[op.OperationID]; dup {
			t.Errorf("operationId %q used by both %s and %s", op.OperationID, prev, key)
		}
		ids[op.OperationID] = key
		if _, ok := op.Responses["401"]; !ok {
			t.Errorf("%s does not document the 401 RequireV1Caller answers", key)
		}
	}
}

// v1OperationDTOs names the Go DTO each operation's 200 response serializes. Every
// spec operation must be listed: a new endpoint states its DTO here and the schema
// check below binds the two.
var v1OperationDTOs = map[string]reflect.Type{
	"GET /api/v1/whoami": reflect.TypeFor[apitypes.V1WhoamiDTO](),
}

// TestV1OpenAPISchemasMatchDTOs (PRD #1907 M3): each operation's 200 schema matches
// its DTO's JSON shape, recursively.
func TestV1OpenAPISchemasMatchDTOs(t *testing.T) {
	spec := loadV1Spec(t)
	for key, op := range v1SpecOperations(t, spec) {
		dto, ok := v1OperationDTOs[key]
		if !ok {
			t.Errorf("%s has no entry in v1OperationDTOs: name the DTO its 200 response serializes", key)
			continue
		}
		resp, ok := op.Responses["200"]
		if !ok {
			t.Errorf("%s documents no 200 response", key)
			continue
		}
		media, ok := resp.Content["application/json"]
		if !ok || media.Schema == nil {
			t.Errorf("%s: 200 has no application/json schema", key)
			continue
		}
		(&oaChecker{t: t, spec: spec}).match(key+" 200", media.Schema, dto)
	}

	// The scopes enum is the Go vocabulary, in order.
	whoami := spec.Comps.Schemas["Whoami"]
	if whoami == nil || whoami.Properties["scopes"] == nil || whoami.Properties["scopes"].Items == nil {
		t.Fatal("components.schemas.Whoami.properties.scopes.items is missing")
	}
	if got := whoami.Properties["scopes"].Items.Enum; !slices.Equal(got, producttoken.Scopes) {
		t.Errorf("Whoami scopes enum = %v, want producttoken.Scopes %v", got, producttoken.Scopes)
	}
}

type oaChecker struct {
	t    *testing.T
	spec oaSpec
}

// resolve follows a local $ref.
func (c *oaChecker) resolve(where string, s *oaSchema) *oaSchema {
	for s != nil && s.Ref != "" {
		name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/")
		if !ok {
			c.t.Fatalf("%s: unsupported $ref %q", where, s.Ref)
		}
		next := c.spec.Comps.Schemas[name]
		if next == nil {
			c.t.Fatalf("%s: $ref %q resolves to nothing", where, s.Ref)
		}
		s = next
	}
	return s
}

// types returns the schema's type keyword as a list (it may be a string or a list).
func (c *oaChecker) types(where string, s *oaSchema) []string {
	switch s.Type.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		return []string{s.Type.Value}
	case yaml.SequenceNode:
		var out []string
		if err := s.Type.Decode(&out); err != nil {
			c.t.Fatalf("%s: type: %v", where, err)
		}
		return out
	default:
		c.t.Fatalf("%s: type is neither a string nor a list", where)
		return nil
	}
}

// splitNull reports whether s admits null and returns the non-null schema. Accepted
// 3.1 spellings: type [X, "null"], or oneOf/anyOf of one schema plus {type: "null"}.
func (c *oaChecker) splitNull(where string, s *oaSchema) (*oaSchema, bool) {
	if s.Nullable != nil {
		c.t.Errorf("%s: `nullable` is OpenAPI 3.0; in 3.1 write null as a type", where)
	}
	alts := s.OneOf
	if len(alts) == 0 {
		alts = s.AnyOf
	}
	if len(alts) > 0 {
		var rest []*oaSchema
		null := false
		for _, a := range alts {
			if ts := c.types(where, a); len(ts) == 1 && ts[0] == "null" && a.Ref == "" {
				null = true
				continue
			}
			rest = append(rest, a)
		}
		if len(rest) != 1 {
			c.t.Fatalf("%s: oneOf/anyOf must be exactly one schema plus an optional null, got %d non-null", where, len(rest))
		}
		return rest[0], null
	}
	ts := c.types(where, s)
	if i := slices.Index(ts, "null"); i >= 0 {
		cp := *s
		var node yaml.Node
		if err := node.Encode(slices.Delete(slices.Clone(ts), i, i+1)); err != nil {
			c.t.Fatalf("%s: %v", where, err)
		}
		cp.Type = node
		return &cp, true
	}
	return s, false
}

func (c *oaChecker) wantType(where string, s *oaSchema, want string) {
	c.t.Helper()
	if ts := c.types(where, s); len(ts) != 1 || ts[0] != want {
		c.t.Errorf("%s: schema type %v, want %q", where, ts, want)
	}
}

// match checks that schema s describes Go type typ's JSON encoding.
func (c *oaChecker) match(where string, s *oaSchema, typ reflect.Type) {
	c.t.Helper()
	s = c.resolve(where, s)
	inner, nullable := c.splitNull(where, s)
	inner = c.resolve(where, inner)

	// Nullability: exactly the pointer, slice and map fields encode as null in Go. A
	// slice is the exception the handlers take on themselves: the spec may declare it
	// non-null only because the handler never leaves it nil. For whoami's scopes that
	// normalisation is measured by TestV1WhoamiNilScopesEncodeAsEmptyArray (a nil-scopes
	// principal served through V1Whoami must put "scopes":[] on the wire); the live
	// whoami tests only ever see a non-empty scopes column, so they cannot reach it. This
	// checker itself proves nothing about a slice's nullability.
	isPtr := typ.Kind() == reflect.Pointer
	if isPtr != nullable && typ.Kind() != reflect.Slice {
		c.t.Errorf("%s: spec nullable=%t but Go type %s nullable=%t", where, nullable, typ, isPtr)
	}
	if isPtr {
		typ = typ.Elem()
	}

	if typ == reflect.TypeFor[time.Time]() {
		c.wantType(where, inner, "string")
		if inner.Format != "date-time" {
			c.t.Errorf("%s: a time.Time field needs format date-time, got %q", where, inner.Format)
		}
		return
	}
	switch typ.Kind() {
	case reflect.String:
		c.wantType(where, inner, "string")
	case reflect.Bool:
		c.wantType(where, inner, "boolean")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		c.wantType(where, inner, "integer")
	case reflect.Float32, reflect.Float64:
		c.wantType(where, inner, "number")
	case reflect.Slice:
		c.wantType(where, inner, "array")
		if inner.Items == nil {
			c.t.Errorf("%s: array schema has no items", where)
			return
		}
		c.match(where+"[]", inner.Items, typ.Elem())
	case reflect.Struct:
		c.wantType(where, inner, "object")
		fields := v1JSONFields(typ)
		var goNames, goRequired []string
		for name, f := range fields {
			goNames = append(goNames, name)
			if !f.omitempty {
				goRequired = append(goRequired, name)
			}
		}
		var specNames []string
		for name := range inner.Properties {
			specNames = append(specNames, name)
		}
		sort.Strings(goNames)
		sort.Strings(goRequired)
		sort.Strings(specNames)
		specRequired := slices.Sorted(slices.Values(inner.Required))
		if !slices.Equal(specNames, goNames) {
			c.t.Errorf("%s: spec properties %v, Go %s JSON fields %v", where, specNames, typ, goNames)
		}
		if !slices.Equal(specRequired, goRequired) {
			c.t.Errorf("%s: spec required %v, Go %s always-present fields %v", where, specRequired, typ, goRequired)
		}
		for name, f := range fields {
			if ps, ok := inner.Properties[name]; ok {
				c.match(where+"."+name, ps, f.typ)
			}
		}
	default:
		c.t.Errorf("%s: Go kind %s has no schema mapping in this test; extend match", where, typ.Kind())
	}
}

type v1JSONField struct {
	typ       reflect.Type
	omitempty bool
}

// v1JSONFields lists a struct's JSON-encoded fields by name, flattening embedded
// structs as encoding/json does. A field without a json tag is refused: every wire DTO
// field names its key explicitly.
func v1JSONFields(typ reflect.Type) map[string]v1JSONField {
	out := map[string]v1JSONField{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag, hasTag := f.Tag.Lookup("json")
		if f.Anonymous && !hasTag {
			for k, v := range v1JSONFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if !f.IsExported() || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			panic("wire DTO field " + typ.String() + "." + f.Name + " has no json name")
		}
		out[name] = v1JSONField{typ: f.Type, omitempty: slices.Contains(strings.Split(opts, ","), "omitempty")}
	}
	return out
}
