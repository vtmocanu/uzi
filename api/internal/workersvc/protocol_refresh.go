package workersvc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

// Shared registration and refresh validation bands.
const MaxAdvertisedConcurrentRuns = 256

func ValidAdvertisedConcurrentRuns(n int) bool  { return n >= 1 && n <= MaxAdvertisedConcurrentRuns }
func ValidAdvertisedCrossCheckSlots(n int) bool { return n >= 0 && n <= 16 }

// EffectiveConcurrentRuns applies the run-bound worker clamp after raw validation.
func EffectiveConcurrentRuns(n *int, ephemeral bool) *int {
	if ephemeral {
		one := 1
		return &one
	}
	return n
}

// ProtocolRefresh is an additive report fenced by the original registration nonce.
type ProtocolRefresh struct {
	RegisterNonce        string
	ProtocolCapabilities []string
	MaxConcurrentRuns    int
	MaxCrossCheckSlots   int
}

// 64 ASCII bytes can occupy 384 JSON bytes when fully escaped. Allow all 256
// such names plus delimiters and the remaining required members.
const maxProtocolRefreshBytes = 256*(64*6+3) + 1024

// ParseProtocolRefresh isolates invalid soft input from heartbeat liveness. Work
// is bounded by the member ceiling and 256 names; one invalid name drops the report.
func ParseProtocolRefresh(raw json.RawMessage) *ProtocolRefresh {
	if len(raw) == 0 {
		return nil
	}
	reject := func() *ProtocolRefresh {
		slog.Warn("worker protocol refresh rejected; ignoring report")
		return nil
	}
	if len(raw) > maxProtocolRefreshBytes || !utf8.Valid(raw) {
		return reject()
	}
	var wire struct {
		RegisterNonce        *string         `json:"register_nonce"`
		ProtocolCapabilities json.RawMessage `json:"protocol_capabilities"`
		MaxConcurrentRuns    *int            `json:"max_concurrent_runs"`
		MaxCrossCheckSlots   *int            `json:"max_cross_check_slots"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(&struct{}{}) != io.EOF || wire.RegisterNonce == nil ||
		wire.MaxConcurrentRuns == nil || wire.MaxCrossCheckSlots == nil ||
		!ValidAdvertisedConcurrentRuns(*wire.MaxConcurrentRuns) || !ValidAdvertisedCrossCheckSlots(*wire.MaxCrossCheckSlots) {
		return reject()
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(*wire.RegisterNonce)
	if err != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != *wire.RegisterNonce {
		return reject()
	}
	namesRaw := bytes.TrimSpace(wire.ProtocolCapabilities)
	if len(namesRaw) == 0 || namesRaw[0] != '[' {
		return reject()
	}
	var names []json.RawMessage
	if json.Unmarshal(namesRaw, &names) != nil || len(names) > 256 {
		return reject()
	}
	caps := make([]string, 0, len(names))
	for _, name := range names {
		var value *string
		if json.Unmarshal(name, &value) != nil || value == nil || len(*value) > 64 || !utf8.ValidString(*value) {
			return reject()
		}
		caps = append(caps, *value)
	}
	return &ProtocolRefresh{
		RegisterNonce:        *wire.RegisterNonce,
		ProtocolCapabilities: capability.FilterProtocol(caps),
		MaxConcurrentRuns:    *wire.MaxConcurrentRuns,
		MaxCrossCheckSlots:   *wire.MaxCrossCheckSlots,
	}
}
