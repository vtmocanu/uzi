package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestApplyResultNeverPrintsTheToken(t *testing.T) {
	secret := "unit-token-" + uuid.NewString()
	res := CredentialSwitchApplyResult{Outcome: ApplyOutcomeApplied, Token: secret, Ordinal: 2, MessageSeq: 9}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("apply", "result", res)
	log.Info("apply", "result", &res)
	for _, out := range []string{buf.String(), fmt.Sprint(res), fmt.Sprintf("%+v", res), fmt.Sprintf("%#v", res), fmt.Sprintf("%v", &res)} {
		if strings.Contains(out, secret) {
			t.Fatalf("the token leaked into %q", out)
		}
	}
	if !strings.Contains(buf.String(), "REDACTED") {
		t.Fatalf("log line %q should mark the redaction", buf.String())
	}
	// The wire form still carries it: that is the endpoint's whole purpose.
	raw, err := json.Marshal(res)
	if err != nil || !strings.Contains(string(raw), secret) {
		t.Fatalf("wire form = %s (err %v), want the token", raw, err)
	}
}

func TestApplyResultWireShapes(t *testing.T) {
	for _, tc := range []struct {
		res  CredentialSwitchApplyResult
		want string
	}{
		{CredentialSwitchApplyResult{Outcome: ApplyOutcomeApplied, Token: "t", Ordinal: 1, MessageSeq: 5}, `{"outcome":"applied","token":"t","ordinal":1,"message_seq":5,"replay":false}`},
		{CredentialSwitchApplyResult{Outcome: ApplyOutcomeApplied, Token: "t", Ordinal: 1, MessageSeq: 5, Replay: true}, `{"outcome":"applied","token":"t","ordinal":1,"message_seq":5,"replay":true}`},
		{CredentialSwitchApplyResult{Outcome: ApplyOutcomeSuperseded}, `{"outcome":"superseded"}`},
		{CredentialSwitchApplyResult{Outcome: ApplyOutcomeFenced}, `{"outcome":"fenced"}`},
		{CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: "vault_locked"}, `{"outcome":"not_applied","reason":"vault_locked"}`},
		{CredentialSwitchApplyResult{Outcome: ApplyOutcomeRefusedCommitted, Reason: "credential_changed"}, `{"outcome":"refused_committed","reason":"credential_changed"}`},
	} {
		got, err := json.Marshal(tc.res)
		if err != nil || string(got) != tc.want {
			t.Fatalf("marshal = %s (err %v), want %s", got, err, tc.want)
		}
	}
}

func TestValidateCredentialSwitchApplyRequest(t *testing.T) {
	op := uuid.New().String()
	ok := CredentialSwitchApplyRequest{ClaimGeneration: 3, RegisterNonce: "n", OpID: op}
	if _, cum, err := ValidateCredentialSwitchApplyRequest(ok); err != nil || cum != nil {
		t.Fatalf("minimal request: cum=%s err=%v", cum, err)
	}
	withCum := ok
	withCum.OldCumulative = json.RawMessage(`{"input_tokens": 10, "k":0}`)
	if _, cum, err := ValidateCredentialSwitchApplyRequest(withCum); err != nil || !json.Valid(cum) {
		t.Fatalf("old_cumulative: %s %v", cum, err)
	}
	entries := func(n int) json.RawMessage {
		m := map[string]int{}
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("k%d", i)] = i
		}
		raw, _ := json.Marshal(m)
		return raw
	}
	bad := map[string]CredentialSwitchApplyRequest{
		"zero generation": {ClaimGeneration: 0, RegisterNonce: "n", OpID: op},
		"nil op":          {ClaimGeneration: 1, RegisterNonce: "n", OpID: uuid.Nil.String()},
		"not a uuid":      {ClaimGeneration: 1, RegisterNonce: "n", OpID: "x"},
		"no nonce":        {ClaimGeneration: 1, OpID: op},
		"long nonce":      {ClaimGeneration: 1, RegisterNonce: strings.Repeat("n", 129), OpID: op},
		"65 entries":      {ClaimGeneration: 1, RegisterNonce: "n", OpID: op, OldCumulative: entries(65)},
		"long key":        {ClaimGeneration: 1, RegisterNonce: "n", OpID: op, OldCumulative: json.RawMessage(`{"` + strings.Repeat("k", 201) + `":1}`)},
		"negative":        {ClaimGeneration: 1, RegisterNonce: "n", OpID: op, OldCumulative: json.RawMessage(`{"a":-1}`)},
		"float":           {ClaimGeneration: 1, RegisterNonce: "n", OpID: op, OldCumulative: json.RawMessage(`{"a":1.5}`)},
		"string value":    {ClaimGeneration: 1, RegisterNonce: "n", OpID: op, OldCumulative: json.RawMessage(`{"a":"1"}`)},
		"array":           {ClaimGeneration: 1, RegisterNonce: "n", OpID: op, OldCumulative: json.RawMessage(`[1]`)},
	}
	for name, req := range bad {
		if _, _, err := ValidateCredentialSwitchApplyRequest(req); !errors.Is(err, ErrInvalidApplyRequest) {
			t.Fatalf("%s: err = %v, want ErrInvalidApplyRequest", name, err)
		}
	}
	atLimit := ok
	atLimit.OldCumulative = entries(64)
	if _, _, err := ValidateCredentialSwitchApplyRequest(atLimit); err != nil {
		t.Fatalf("64 entries must be accepted: %v", err)
	}
	longKey := ok
	longKey.OldCumulative = json.RawMessage(`{"` + strings.Repeat("k", 200) + `":1}`)
	if _, _, err := ValidateCredentialSwitchApplyRequest(longKey); err != nil {
		t.Fatalf("a 200-byte key must be accepted: %v", err)
	}
}

func TestPendingCredentialSwitchSignalCarriesTheOpID(t *testing.T) {
	op := uuid.New()
	run := store.Run{
		ClaimGeneration:             4,
		CredentialSwitchRequestedAt: pgtype.Timestamptz{Valid: true},
		CredentialSwitchGeneration:  pgtype.Int8{Int64: 4, Valid: true},
		CredentialSwitchOpID:        pgtype.UUID{Bytes: op, Valid: true},
	}
	sig := PendingCredentialSwitchSignal(run)
	if sig == nil || sig.OpID != op.String() || sig.Generation != 4 {
		t.Fatalf("signal = %+v, want generation 4 and op %s", sig, op)
	}
	raw, _ := json.Marshal(sig)
	if !strings.Contains(string(raw), `"op_id":"`+op.String()+`"`) {
		t.Fatalf("wire = %s", raw)
	}
	run.CredentialSwitchOpID = pgtype.UUID{}
	if raw, _ := json.Marshal(PendingCredentialSwitchSignal(run)); string(raw) != `{"generation":4}` {
		t.Fatalf("a stamp with no op id keeps the original wire form, got %s", raw)
	}
	run.CredentialSwitchGeneration = pgtype.Int8{Int64: 3, Valid: true}
	if PendingCredentialSwitchSignal(run) != nil {
		t.Fatal("a stamp for another generation is not pending for this claim")
	}
}

func TestInPlaceSwitchDrainsNeedsBothCapabilities(t *testing.T) {
	for _, tc := range []struct {
		caps []string
		want bool
	}{
		{[]string{capability.CredentialSwitchV1, capability.InputReceiptsV1}, false},
		{[]string{capability.CredentialSwitchInPlaceV1}, false},
		{[]string{capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1}, true},
	} {
		if got := inPlaceSwitchDrains(store.Worker{ProtocolCapabilities: tc.caps}); got != tc.want {
			t.Fatalf("%v: got %v want %v", tc.caps, got, tc.want)
		}
	}
}

func TestOptionalSwitchOpID(t *testing.T) {
	if id, err := optionalSwitchOpID(nil, "s"); err != nil || id.Valid {
		t.Fatalf("absent: %v %v", id, err)
	}
	bad := "x"
	if _, err := optionalSwitchOpID(&bad, "s"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("bad: %v", err)
	}
	good := uuid.New().String()
	if id, err := optionalSwitchOpID(&good, "s"); err != nil || !id.Valid || uuid.UUID(id.Bytes).String() != good {
		t.Fatalf("good: %v %v", id, err)
	}
}
