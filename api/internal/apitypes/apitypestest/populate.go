// Package apitypestest is a stdlib-only reflection populator shared by the JSON
// wire-contract tests (PRD #982). It fills every field of a DTO with a
// deterministic non-zero value so that omitempty fields are present when the
// value is marshaled, and so a hand-recorded full.json exercises every key.
//
// 🔴 THIS PACKAGE MUST NOT IMPORT apitypes. The apitypes contract test is an
// IN-PACKAGE test (package apitypes), so an import of apitypes from a package
// that its own test consumes would be a compile cycle. Keeping this a stdlib-only
// leaf (reflect, time, encoding/json and nothing else) is what lets both the
// apitypes in-package test and the handler internal test share it. The CLI leaf
// guard (api/cmd/uzi/deps_test.go, TestNoServerDeps) is a test-only reachability
// check that this package cannot reach, but the stdlib-only rule is the same
// discipline.
package apitypestest

import (
	"encoding/json"
	"reflect"
	"time"
)

// fixedInstant is the value every time.Time field is set to. It carries ZERO
// nanoseconds on purpose: time.Time marshals RFC3339Nano, so a non-zero
// nanosecond component would make the recorded string depend on the exact value
// and defeat the byte-equal fixture comparison. A fixed instant keeps it stable.
var fixedInstant = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage(nil))
)

// Populate fills the value pointed at by v so that every field is a non-zero
// value: strings become "x", integers 1, floats 1.5, bools true; pointers are
// allocated and their pointee populated; slices and arrays get exactly one
// populated element; maps get exactly one entry (key "x", or 1 for an integer
// key); structs recurse into every settable field, including embedded ones;
// time.Time becomes a fixed instant and json.RawMessage becomes "{}".
//
// v must be a non-nil pointer. An unhandled reflect.Kind panics rather than
// leaving a field zero — a silent zero would drop an omitempty key and make the
// key-set contract green for the wrong reason.
func Populate(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		panic("apitypestest.Populate: v must be a non-nil pointer")
	}
	populate(rv.Elem())
}

func populate(v reflect.Value) {
	// Detect the two by-type special cases before the generic Kind switch:
	// time.Time is a struct we must NOT recurse into, and json.RawMessage is a
	// []byte we must NOT treat as a generic slice (it would populate to a
	// one-byte non-JSON array and fail to marshal as embedded JSON).
	switch v.Type() {
	case timeType:
		v.Set(reflect.ValueOf(fixedInstant))
		return
	case rawType:
		v.Set(reflect.ValueOf(json.RawMessage("{}")))
		return
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		populate(p.Elem())
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		populate(s.Index(0))
		v.Set(s)
	case reflect.Array:
		if v.Len() > 0 {
			populate(v.Index(0))
		}
	case reflect.Map:
		m := reflect.MakeMapWithSize(v.Type(), 1)
		key := reflect.New(v.Type().Key()).Elem()
		populate(key)
		val := reflect.New(v.Type().Elem()).Elem()
		populate(val)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if !f.CanSet() {
				// Unexported field: it cannot carry a json tag and cannot be
				// set, so it contributes nothing to the wire shape.
				continue
			}
			populate(f)
		}
	default:
		panic("apitypestest.Populate: unhandled reflect.Kind " + v.Kind().String() +
			" for type " + v.Type().String())
	}
}
