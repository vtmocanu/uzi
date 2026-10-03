package workersvc

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The seam contract for issue #2014: testdata/usage_tail_contract.json freezes the /usage route
// body, the init/result stamp keys and the run DTO field. This decodes each part through the REAL
// types with DisallowUnknownFields, so a renamed or dropped key on either side fails here, and the
// agent and web consumers code against the same file's shapes.

type usageTailContract struct {
	Request            json.RawMessage `json:"request"`
	InitFramePayload   json.RawMessage `json:"init_frame_payload"`
	ResultFramePayload json.RawMessage `json:"result_frame_payload"`
	DTO                json.RawMessage `json:"dto"`
}

func strictDecode(t *testing.T, raw []byte, dst any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("strict decode into %T: %v", dst, err)
	}
}

func loadUsageTailContract(t *testing.T) usageTailContract {
	t.Helper()
	raw, err := os.ReadFile("testdata/usage_tail_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var c usageTailContract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUsageTailContractRequestDecodesThroughRealTypes(t *testing.T) {
	c := loadUsageTailContract(t)
	var body UsagePostBody
	strictDecode(t, c.Request, &body)
	if body.ClaimGeneration == nil || *body.ClaimGeneration != 3 {
		t.Fatalf("claim_generation = %v, want 3", body.ClaimGeneration)
	}
	if len(body.Legs) != 2 || body.Legs[0].ClosedThrough == nil || *body.Legs[0].ClosedThrough != 5 || body.Legs[1].ClosedThrough != nil {
		t.Fatalf("legs decoded wrong: %+v", body.Legs)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(body.Messages))
	}
	m := body.Messages[0]
	if m.MessageID != "msg_01ABC" || m.Ordinal != 1 || m.Model != "claude-sonnet-5-5" || !m.OutputFinal ||
		m.CacheCreation5mInputTokens == nil || *m.CacheCreation5mInputTokens != 200 ||
		m.CacheCreation1hInputTokens == nil || *m.CacheCreation1hInputTokens != 100 ||
		m.ServiceTier != "standard" || m.Speed != "standard" || m.InferenceGeo != "global" {
		t.Fatalf("message decoded wrong: %+v", m)
	}
	// The decoded body must survive the server-side validation unchanged.
	if _, err := sanitizeUsageRequest(body.UsageRequest); err != nil {
		t.Fatalf("contract request rejected by sanitizeUsageRequest: %v", err)
	}
}

func TestUsageTailContractStampKeysAreReadByTheFold(t *testing.T) {
	c := loadUsageTailContract(t)
	msgs := []IncomingMessage{
		{Seq: 7, Kind: "status", Payload: c.InitFramePayload},
		{Seq: 9, Kind: "status", Payload: c.ResultFramePayload},
	}
	got := collectUsageStamps(msgs)
	if len(got) != 2 {
		t.Fatalf("stamps = %+v, want an init and a result stamp", got)
	}
	init, res := got[0], got[1]
	if init.kind != usageStampInit || init.seq != 7 || init.sessionID != "c0ffee00-0000-4000-8000-000000000001" ||
		init.legID.String() != "6f1d2a40-7c1e-4a0e-9f0b-1a2b3c4d5e01" {
		t.Fatalf("init stamp = %+v", init)
	}
	if res.kind != usageStampResult || res.through != 3 || !res.cumulative ||
		res.legID.String() != "6f1d2a40-7c1e-4a0e-9f0b-1a2b3c4d5e01" {
		t.Fatalf("result stamp = %+v", res)
	}
}

func TestUsageTailContractDTOShape(t *testing.T) {
	c := loadUsageTailContract(t)
	var wrap struct {
		Tail apitypes.UsageTailDTO `json:"usage_estimated_tail"`
	}
	strictDecode(t, c.DTO, &wrap)
	// Re-encoding through the real type must reproduce the frozen JSON exactly (null cost_usd
	// included, never 0), so the DTO neither gains nor loses a key unnoticed.
	var want, got any
	var frozen struct {
		Tail json.RawMessage `json:"usage_estimated_tail"`
	}
	if err := json.Unmarshal(c.DTO, &frozen); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frozen.Tail, &want); err != nil {
		t.Fatal(err)
	}
	enc, err := json.Marshal(wrap.Tail)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(enc, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("DTO re-encode differs from the contract:\n got %s\nwant %s", enc, frozen.Tail)
	}
	if wrap.Tail.CostUSD != nil {
		t.Fatal("unpriced tail must decode a null cost_usd to nil")
	}
}
