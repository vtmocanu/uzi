package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func wrap(data string) string { return `{"data":` + data + `,"expected_revision":null}` }

func TestScanModuleDocRejects(t *testing.T) {
	cases := map[string]string{
		"empty":                        ``,
		"bom":                          "\ufeff" + wrap(`1`),
		"doubled comma":                `{"data":1,,"expected_revision":null}`,
		"trailing comma":               `{"data":1,"expected_revision":null,}`,
		"leading comma":                `{,"data":1,"expected_revision":null}`,
		"missing comma":                `{"data":1 "expected_revision":null}`,
		"bytes after":                  wrap(`1`) + `x`,
		"second object":                wrap(`1`) + wrap(`1`),
		"top null":                     `null`,
		"top array":                    `[]`,
		"top string":                   `"x"`,
		"empty object":                 `{}`,
		"missing data":                 `{"expected_revision":null}`,
		"missing rev":                  `{"data":1}`,
		"extra key":                    `{"data":1,"expected_revision":null,"x":1}`,
		"upper key":                    `{"DATA":1,"expected_revision":null}`,
		"null data":                    wrap(`null`),
		"rev number":                   `{"data":1,"expected_revision":5}`,
		"rev object":                   `{"data":1,"expected_revision":{}}`,
		"unterminated":                 `{"data":[1,2`,
		"unterminated str":             `{"data":"abc`,
		"missing value":                `{"data":,"expected_revision":null}`,
		"missing colon":                `{"data" 1,"expected_revision":null}`,
		"mismatched":                   wrap(`[1}`),
		"lone surrogate k":             `{"\ud800":1,"data":1,"expected_revision":null}`,
		"overflow":                     wrap(`1e309`),
		"neg overflow":                 wrap(`-1e309`),
		"underflow":                    wrap(`[1e-400]`),
		"depth 10001":                  wrap(strings.Repeat("[", 10001) + strings.Repeat("]", 10001)),
		"dup earlier ok but last null": `{"data":1,"data":null,"expected_revision":null}`,
		"invalid utf8 in rev":          "{\"data\":1,\"expected_revision\":\"a\xffb\"}",
		"invalid utf8 in dup data":     "{\"data\":\"a\xffb\",\"data\":1,\"expected_revision\":null}",
		"invalid utf8 in key":          "{\"da\xffta\":1,\"data\":1,\"expected_revision\":null}",
	}
	for name, body := range cases {
		if _, err := scanModuleDoc([]byte(body)); err == nil {
			t.Errorf("%s: accepted %.80q", name, body)
		}
	}
	// The scanner's own depth guard (D2) answers "invalid data"; stdlib's json.Valid would
	// only reject the same body later as "invalid request body".
	deep := wrap(strings.Repeat("[", 10001) + strings.Repeat("]", 10001))
	var de *moduleDocError
	if _, err := scanModuleDoc([]byte(deep)); !errors.As(err, &de) || de.Msg != "invalid data" {
		t.Errorf("depth 10001: err = %v, want the scanner's depth guard (invalid data)", err)
	}
}

func TestScanModuleDocAccepts(t *testing.T) {
	deep := strings.Repeat("[", 10000) + strings.Repeat("]", 10000)
	cases := []struct{ name, body, data string }{
		{"scalar", wrap(`7`), `7`},
		{"string with brackets", wrap(`"a]}[{\\\"1e999"`), `"a]}[{\\\"1e999"`},
		{"adjacent numbers", wrap(`[1,{"a":2},3]`), `[1,{"a":2},3]`},
		{"min denormal", wrap(`5e-324`), `5e-324`},
		{"max", wrap(`1e308`), `1e308`},
		{"zero exp", wrap(`[0e-400,0e5]`), `[0,0]`},
		{"whitespace", " \t\r\n{ \"data\" :\n[ 1 , 2 ]\t,\"expected_revision\" : null }\r\n", `[ 1 , 2 ]`},
		{"escaped key", `{"\u0064ata":2,"expected_revision":null}`, `2`},
		{"dup last wins", `{"data":1e400,"expected_revision":null,"data":3}`, `3`},
		{"depth 10000", wrap(deep), deep},
		{"multi zero", `{"data":{"a":[0e-400,1,-0.000,0E+5,0,0.0e-99,12],"0e-9":"x\\\"0.0","k":0.00},"expected_revision":null}`,
			`{"a":[0,1,0,0,0,0,12],"0e-9":"x\\\"0.0","k":0}`},
	}
	for _, tc := range cases {
		d, err := scanModuleDoc([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if string(d.data) != tc.data {
			t.Errorf("%s: data = %.120q, want %.120q", tc.name, d.data, tc.data)
		}
	}
}

func TestScanModuleDocExpectedRevision(t *testing.T) {
	d, err := scanModuleDoc([]byte(`{"data":1,"expected_revision":"12"}`))
	if err != nil || d.expectedRevision == nil || *d.expectedRevision != "12" {
		t.Fatalf("got %+v, %v", d, err)
	}
	d, err = scanModuleDoc([]byte(`{"data":1,"expected_revision":null}`))
	if err != nil || d.expectedRevision != nil {
		t.Fatalf("got %+v, %v", d, err)
	}
}

func FuzzScanModuleDoc(f *testing.F) {
	seeds := []string{
		`{}`, `{"data":0}`, `{"data":-0}`, wrap(`-0`),
		wrap(`[0e-400,1,-0.000,0E+5,0,0.0e-99,12]`),
		`{"data":{"a":[0e-400,1,-0.000,0E+5,0,0.0e-99,12],"0e-9":"x\\\"0.0","k":0.00},"expected_revision":null}`,
		`{"data":1e400,"expected_revision":null,"data":3}`,
		` { "data" : [ 1 , { "a" : -2.5e+3 } ] , "expected_revision" : "7" } `,
		"\t\r\n{\n\"data\"\t:\r1\n,\n\"expected_revision\"\n:\nnull\r}\t",
		`{"\u0064ata":"\\\"]}[1","expected_revision":null}`,
		wrap(`"a\\"`), wrap(`[1]`), wrap(`{"a":1}`), wrap(`1e309`), wrap(`1e-400`), wrap(`5e-324`), wrap(`0e-400`),
		"\ufeff" + wrap(`1`), `{"data":1,,"expected_revision":null}`, wrap(`1`) + `x`, `null`, `[]`, `"x"`,
		wrap(strings.Repeat("[", 50) + strings.Repeat("]", 50)),
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		members, err := scanWrapper(body)
		doc, derr := scanModuleDoc(body)
		if derr == nil && err != nil {
			t.Fatalf("full accept but wrapper reject: %q", body)
		}
		if len(body) >= 9000 {
			return
		}
		trimmed := bytes.TrimLeft(body, " \t\r\n")
		isObj := json.Valid(body) && len(trimmed) > 0 && trimmed[0] == '{'
		if (err == nil) != isObj {
			t.Fatalf("wrapper accept=%v, json.Valid&&object=%v: %q", err == nil, isObj, body)
		}
		if err != nil {
			return
		}
		var m map[string]json.RawMessage
		if e := json.Unmarshal(body, &m); e != nil {
			t.Fatalf("unmarshal: %v", e)
		}
		last := map[string]span{}
		for _, mem := range members {
			k, e := decodeKey(body, mem.key)
			if e != nil {
				t.Fatalf("key: %v", e)
			}
			last[k] = mem.val
		}
		if len(last) != len(m) {
			t.Fatalf("key set differs: %q", body)
		}
		for k, v := range last {
			if !bytes.Equal(body[v.start:v.end], m[k]) {
				t.Fatalf("value of %q: %q vs %q", k, body[v.start:v.end], m[k])
			}
		}
		var want, got []string
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		for {
			tok, e := dec.Token()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatalf("token: %v", e)
			}
			if n, ok := tok.(json.Number); ok {
				want = append(want, n.String())
			}
		}
		for _, mem := range members {
			for _, n := range mem.nums {
				got = append(got, string(body[n.start:n.end]))
			}
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("numbers %q vs %q in %q", got, want, body)
		}
		if derr != nil {
			return
		}
		var orig, out any
		if e := json.Unmarshal(m["data"], &orig); e != nil {
			t.Fatalf("orig: %v", e)
		}
		if e := json.Unmarshal(doc.data, &out); e != nil {
			t.Fatalf("outgoing invalid: %v: %q", e, doc.data)
		}
		if !reflect.DeepEqual(orig, out) {
			t.Fatalf("rewrite changed value: %q -> %q", m["data"], doc.data)
		}
		// Byte-level: only zero literals may change, each replaced by exactly "0".
		var win docMember
		for _, mem := range members {
			if k, _ := decodeKey(body, mem.key); k == "data" {
				win = mem
			}
		}
		var rebuilt []byte
		pos := win.val.start
		for _, n := range win.nums {
			f, _ := strconv.ParseFloat(string(body[n.start:n.end]), 64)
			if f != 0 {
				continue
			}
			rebuilt = append(rebuilt, body[pos:n.start]...)
			rebuilt = append(rebuilt, '0')
			pos = n.end
		}
		rebuilt = append(rebuilt, body[pos:win.val.end]...)
		if !bytes.Equal(rebuilt, doc.data) {
			t.Fatalf("rewrite touched more than zero literals: %q -> %q (want %q)", body[win.val.start:win.val.end], doc.data, rebuilt)
		}
	})
}
