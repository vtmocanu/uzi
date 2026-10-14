package workersvc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func refreshWire(names any, runs any, slots any, nonce any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"register_nonce": nonce, "protocol_capabilities": names,
		"max_concurrent_runs": runs, "max_cross_check_slots": slots,
	})
	return raw
}

func TestParseProtocolRefresh(t *testing.T) {
	nonce := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	valid := refreshWire([]string{capability.CrossCheckLaneV1, "unknown", "docker", "jvm", capability.CrossCheckLaneV1}, 256, 16, nonce)
	got := ParseProtocolRefresh(valid)
	if got == nil || got.RegisterNonce != nonce || got.MaxConcurrentRuns != 256 || got.MaxCrossCheckSlots != 16 ||
		!reflect.DeepEqual(got.ProtocolCapabilities, []string{capability.CrossCheckLaneV1}) {
		t.Fatalf("filtered report = %+v", got)
	}
	for _, raw := range []json.RawMessage{
		refreshWire([]string{}, 1, 0, nonce),
		refreshWire([]string{strings.Repeat("é", 32)}, 1, 0, nonce),
	} {
		if ParseProtocolRefresh(raw) == nil {
			t.Fatalf("valid empty/UTF8 report rejected: %s", raw)
		}
	}
	// Escape every byte of all 256 maximum-length names.
	escapedNames := "[" + strings.TrimSuffix(strings.Repeat("\""+strings.Repeat("\\u0061", 64)+"\",", 256), ",") + "]"
	escaped := fmt.Sprintf("{\"register_nonce\":%q,\"protocol_capabilities\":%s,\"max_concurrent_runs\":1,\"max_cross_check_slots\":0}", nonce, escapedNames)
	if ParseProtocolRefresh(json.RawMessage(escaped)) == nil {
		t.Fatal("maximally escaped valid names rejected by byte ceiling")
	}
	cases := map[string]json.RawMessage{
		"absent": nil, "null": []byte("null"), "array": []byte("[]"), "scalar": []byte("1"), "malformed": []byte("{"),
		"missing":            []byte("{}"),
		"trailing object":    append(append([]byte{}, valid...), []byte("{}")...),
		"unknown field":      append(append([]byte{}, valid[:len(valid)-1]...), []byte(`,"capabilities":["docker"]}`)...),
		"runs negative":      refreshWire([]string{}, -1, 0, nonce),
		"slots fractional":   refreshWire([]string{}, 1, 1.5, nonce),
		"caps null":          refreshWire(nil, 1, 0, nonce),
		"caps string":        refreshWire("x", 1, 0, nonce),
		"null name":          refreshWire([]any{nil}, 1, 0, nonce),
		"number name":        refreshWire([]any{1}, 1, 0, nonce),
		"name bytes":         refreshWire([]string{strings.Repeat("é", 33)}, 1, 0, nonce),
		"names count":        refreshWire(make([]string, 257), 1, 0, nonce),
		"runs zero":          refreshWire([]string{}, 0, 0, nonce),
		"runs high":          refreshWire([]string{}, 257, 0, nonce),
		"runs fractional":    refreshWire([]string{}, 1.5, 0, nonce),
		"runs string":        refreshWire([]string{}, "1", 0, nonce),
		"runs null":          refreshWire([]string{}, nil, 0, nonce),
		"slots negative":     refreshWire([]string{}, 1, -1, nonce),
		"slots high":         refreshWire([]string{}, 1, 17, nonce),
		"slots bool":         refreshWire([]string{}, 1, true, nonce),
		"slots null":         refreshWire([]string{}, 1, nil, nonce),
		"nonce short":        refreshWire([]string{}, 1, 0, "short"),
		"nonce padded":       refreshWire([]string{}, 1, 0, nonce+"="),
		"nonce noncanonical": refreshWire([]string{}, 1, 0, nonce[:42]+"B"),
		"oversize":           []byte(strings.Repeat(" ", maxProtocolRefreshBytes+1)),
		"invalid UTF8":       append(append([]byte{}, valid...), 0xff),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if ParseProtocolRefresh(raw) != nil {
				t.Fatal("invalid refresh accepted")
			}
		})
	}
}

func TestEffectiveConcurrentRuns(t *testing.T) {
	n := 256
	if got := EffectiveConcurrentRuns(&n, true); *got != 1 || n != 256 {
		t.Fatalf("ephemeral clamp mutated input or failed: %v, %d", got, n)
	}
	if EffectiveConcurrentRuns(nil, false) != nil {
		t.Fatal("register nullable fallback lost")
	}
}
