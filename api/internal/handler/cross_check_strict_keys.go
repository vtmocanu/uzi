package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// jsonWireNames returns the top-level JSON names encoding/json would bind for
// t: tag name before the comma, Go field name when untagged. Unexported fields
// and json:"-" fields are skipped; anonymous embedded structs are flattened.
func jsonWireNames(t reflect.Type) map[string]struct{} {
	names := map[string]struct{}{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag, hasTag := f.Tag.Lookup("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				ft := f.Type
				if ft.Kind() == reflect.Ptr {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			if !hasTag || name == "" {
				name = f.Name
			}
			names[name] = struct{}{}
		}
	}
	walk(t)
	return names
}

type strictKeySet struct {
	once  sync.Once
	typ   reflect.Type
	names map[string]struct{}
}

func (s *strictKeySet) get() map[string]struct{} {
	s.once.Do(func() { s.names = jsonWireNames(s.typ) })
	return s.names
}

var (
	planCrossCheckKeys    = &strictKeySet{typ: reflect.TypeOf(planCrossCheckRequest{})}
	crossCheckVerdictKeys = &strictKeySet{typ: reflect.TypeOf(crossCheckVerdictRequest{})}
)

// requireExactTopLevelKeys refuses a body that is not exactly one JSON object,
// has a top-level key that is not byte-exact one of allowed, or repeats a key
// exactly or case-folded. encoding/json binds keys case-insensitively, so the
// exact-key presence checks in the UnmarshalJSON callers are only sound after
// this passes.
func requireExactTopLevelKeys(raw []byte, allowed map[string]struct{}) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return workersvc.ErrCrossCheckRefused
	}
	seen := map[string]struct{}{}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return workersvc.ErrCrossCheckRefused
		}
		if _, ok := allowed[key]; !ok {
			return workersvc.ErrCrossCheckRefused
		}
		folded := strings.ToLower(key)
		if _, dup := seen[folded]; dup {
			return workersvc.ErrCrossCheckRefused
		}
		seen[folded] = struct{}{}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return workersvc.ErrCrossCheckRefused
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return workersvc.ErrCrossCheckRefused
	}
	if _, err := dec.Token(); err != io.EOF {
		return workersvc.ErrCrossCheckRefused
	}
	return nil
}
