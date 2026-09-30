package handler

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// v1_specvalidate_test.go is a small JSON Schema validator for the subset of OpenAPI 3.1 that
// api/openapi/v1.yaml uses, so the contract test (v1_jobs_contract_livedb_test.go) can check a
// LIVE response body against the committed spec without a third-party dependency.
//
// Supported keywords: $ref (local, #/components/...), type (string or list, including "null"),
// enum, format (uuid, date-time), required, properties, items, oneOf, minimum, maximum.
// STRICTER THAN THE SPEC on purpose: a property a response object carries that the schema does
// not declare is an error (the spec leaves objects open so clients tolerate additions, but a
// server that emits an undeclared field has drifted from the document it publishes). An
// unsupported keyword in a schema it is asked to enforce is an error too, so the validator can
// never quietly stop checking something the spec starts to say.

var v1SpecKeywordsIgnored = map[string]bool{
	"description": true, "summary": true, "title": true, "example": true, "examples": true,
	"default": true, "deprecated": true,
}

var v1SpecKeywordsSupported = map[string]bool{
	"$ref": true, "type": true, "enum": true, "format": true, "required": true,
	"properties": true, "items": true, "oneOf": true, "minimum": true, "maximum": true,
}

// v1Doc is the spec decoded generically, with the lookups the validator and the contract test need.
type v1Doc struct {
	root map[string]any
}

func loadV1Doc(t *testing.T) v1Doc {
	t.Helper()
	raw, err := os.ReadFile(v1SpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", v1SpecPath, err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse %s: %v", v1SpecPath, err)
	}
	return v1Doc{root: root}
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// get walks nested map keys; ok is false at the first miss.
func (d v1Doc) get(keys ...string) (any, bool) {
	var cur any = d.root
	for _, k := range keys {
		m, ok := asMap(cur)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// resolve follows a local $ref chain.
func (d v1Doc) resolve(v any) (any, error) {
	for range 10 {
		m, ok := asMap(v)
		if !ok {
			return v, nil
		}
		ref, has := m["$ref"].(string)
		if !has {
			return v, nil
		}
		path, ok := strings.CutPrefix(ref, "#/")
		if !ok {
			return nil, fmt.Errorf("unsupported $ref %q", ref)
		}
		next, found := d.get(strings.Split(path, "/")...)
		if !found {
			return nil, fmt.Errorf("$ref %q resolves to nothing", ref)
		}
		v = next
	}
	return nil, fmt.Errorf("$ref chain too deep")
}

// responseSchema returns the application/json schema of the documented response for
// (method, path template, status), and whether the response documents that status at all.
func (d v1Doc) responseSchema(method, path, status string) (schema any, documented bool, err error) {
	op, ok := d.get("paths", path, strings.ToLower(method))
	if !ok {
		return nil, false, fmt.Errorf("no operation %s %s in the spec", method, path)
	}
	responses, ok := asMap(op.(map[string]any)["responses"])
	if !ok {
		return nil, false, fmt.Errorf("%s %s has no responses", method, path)
	}
	resp, ok := responses[status]
	if !ok {
		return nil, false, nil
	}
	resp, err = d.resolve(resp)
	if err != nil {
		return nil, true, err
	}
	rm, _ := asMap(resp)
	content, _ := asMap(rm["content"])
	media, _ := asMap(content["application/json"])
	if media == nil || media["schema"] == nil {
		return nil, true, fmt.Errorf("%s %s %s documents no application/json schema", method, path, status)
	}
	return media["schema"], true, nil
}

// documentedStatuses lists the response codes an operation documents, sorted.
func (d v1Doc) documentedStatuses(method, path string) []string {
	op, _ := d.get("paths", path, strings.ToLower(method))
	om, _ := asMap(op)
	responses, _ := asMap(om["responses"])
	out := make([]string, 0, len(responses))
	for k := range responses {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var v1UUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validate checks value (as decoded by encoding/json into any) against schema and returns every
// violation, each prefixed with its JSON path. An empty result means the value conforms.
func (d v1Doc) validate(schema, value any) []string {
	var errs []string
	d.check(schema, value, "$", &errs)
	return errs
}

func (d v1Doc) check(schema, value any, path string, errs *[]string) {
	fail := func(format string, args ...any) { *errs = append(*errs, path+": "+fmt.Sprintf(format, args...)) }
	schema, err := d.resolve(schema)
	if err != nil {
		fail("%v", err)
		return
	}
	s, ok := asMap(schema)
	if !ok {
		fail("schema is not an object: %v", schema)
		return
	}
	for k := range s {
		if !v1SpecKeywordsSupported[k] && !v1SpecKeywordsIgnored[k] {
			fail("schema uses keyword %q, which the test validator does not enforce: extend it", k)
		}
	}

	if alts, has := s["oneOf"].([]any); has {
		matched := 0
		var all []string
		for i, alt := range alts {
			var sub []string
			d.check(alt, value, path, &sub)
			if len(sub) == 0 {
				matched++
			}
			all = append(all, fmt.Sprintf("alt %d: %v", i, sub))
		}
		if matched != 1 {
			fail("oneOf matched %d alternatives, want exactly 1 (%s)", matched, strings.Join(all, "; "))
		}
		return
	}

	types := schemaTypes(s["type"])
	if len(types) > 0 && !slices.ContainsFunc(types, func(ty string) bool { return jsonTypeIs(value, ty) }) {
		fail("value %v (%T) is not of type %v", value, value, types)
		return
	}
	if enum, has := s["enum"].([]any); has {
		if !slices.ContainsFunc(enum, func(e any) bool { return e == value }) {
			fail("value %v is not in the enum %v", value, enum)
		}
	}
	if value == nil {
		return
	}
	switch v := value.(type) {
	case string:
		switch s["format"] {
		case "uuid":
			if !v1UUIDRe.MatchString(v) {
				fail("%q is not a uuid", v)
			} else if _, err := uuid.Parse(v); err != nil {
				fail("%q is not a uuid: %v", v, err)
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
				fail("%q is not an RFC 3339 date-time", v)
			}
		case nil:
		default:
			fail("format %v is not enforced by the test validator: extend it", s["format"])
		}
	case float64:
		if lo, has := numberOf(s["minimum"]); has && v < lo {
			fail("%v is below the minimum %v", v, lo)
		}
		if hi, has := numberOf(s["maximum"]); has && v > hi {
			fail("%v is above the maximum %v", v, hi)
		}
	case []any:
		items, has := s["items"]
		if !has {
			fail("array schema has no items")
			return
		}
		for i, el := range v {
			d.check(items, el, fmt.Sprintf("%s[%d]", path, i), errs)
		}
	case map[string]any:
		props, _ := asMap(s["properties"])
		if req, has := s["required"].([]any); has {
			for _, r := range req {
				if _, present := v[r.(string)]; !present {
					fail("required property %q is missing", r)
				}
			}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ps, declared := props[k]
			if !declared {
				fail("property %q is not declared in the spec", k)
				continue
			}
			d.check(ps, v[k], path+"."+k, errs)
		}
	}
}

func schemaTypes(t any) []string {
	switch v := t.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func jsonTypeIs(value any, ty string) bool {
	switch ty {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == math.Trunc(f)
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	return false
}

// TestV1SpecValidatorDetectsDrift is the validator's own negative control: a validator that
// accepted everything would make the contract test vacuous. Each case is a real way a live
// response could drift from api/openapi/v1.yaml.
func TestV1SpecValidatorDetectsDrift(t *testing.T) {
	d := loadV1Doc(t)
	job := map[string]any{
		"id": uuid.NewString(), "type": "research", "status": "queued", "title": "t",
		"requested_by_label": nil, "failure_reason": nil, "wall_seconds": nil,
		"created_at": "2026-09-29T10:00:00Z", "started_at": nil, "finished_at": nil,
	}
	schema, ok := d.get("components", "schemas", "Job")
	if !ok {
		t.Fatal("components.schemas.Job missing")
	}
	if errs := d.validate(schema, job); len(errs) != 0 {
		t.Fatalf("control: a conforming job was rejected: %v", errs)
	}
	clone := func(edit func(m map[string]any)) map[string]any {
		m := map[string]any{}
		for k, v := range job {
			m[k] = v
		}
		edit(m)
		return m
	}
	for name, bad := range map[string]map[string]any{
		"missing required":      clone(func(m map[string]any) { delete(m, "title") }),
		"undeclared property":   clone(func(m map[string]any) { m["owner_email"] = "x" }),
		"wrong type":            clone(func(m map[string]any) { m["title"] = 5.0 }),
		"null where not null":   clone(func(m map[string]any) { m["title"] = nil }),
		"status outside enum":   clone(func(m map[string]any) { m["status"] = "paused" }),
		"bad uuid":              clone(func(m map[string]any) { m["id"] = "not-a-uuid" }),
		"bad date-time":         clone(func(m map[string]any) { m["created_at"] = "yesterday" }),
		"integer as fraction":   clone(func(m map[string]any) { m["wall_seconds"] = 1.5 }),
		"nullable member typed": clone(func(m map[string]any) { m["failure_reason"] = 7.0 }),
	} {
		if errs := d.validate(schema, bad); len(errs) == 0 {
			t.Errorf("%s: the validator accepted a drifted job %v", name, bad)
		}
	}
	// oneOf: the result member is a body or null, never a string.
	res, _ := d.get("components", "schemas", "JobResult")
	if errs := d.validate(res, map[string]any{"job_status": "queued", "result": nil}); len(errs) != 0 {
		t.Errorf("control: a null result was rejected: %v", errs)
	}
	if errs := d.validate(res, map[string]any{"job_status": "queued", "result": "x"}); len(errs) == 0 {
		t.Error("oneOf accepted a string result")
	}
	// An unsupported keyword must fail loudly rather than be skipped.
	if errs := d.validate(map[string]any{"type": "string", "pattern": "^a$"}, "b"); len(errs) == 0 {
		t.Error("an unsupported keyword was silently ignored")
	}
}
