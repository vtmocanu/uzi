package settings

import "testing"

func TestValidateGatewaySettings(t *testing.T) {
	for _, ok := range []string{"", "https://gw.example.com", "https://gw.example.com:8443/anthropic"} {
		if err := Validate(KeyAnthropicGatewayBaseURL, ok); err != nil {
			t.Errorf("base URL %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://gw.example.com", " https://gw.example.com", "https://user:pw@gw.example.com",
		"https://gw.example.com/?k=v", "https://gw.example.com/#x", "gw.example.com"} {
		if err := Validate(KeyAnthropicGatewayBaseURL, bad); err == nil {
			t.Errorf("base URL %q accepted", bad)
		}
	}
	for _, v := range []string{"authorization", "x-api-key"} {
		if err := Validate(KeyAnthropicGatewayAuthHeader, v); err != nil {
			t.Errorf("auth header %q refused: %v", v, err)
		}
	}
	if Validate(KeyAnthropicGatewayAuthHeader, "cookie") == nil {
		t.Error("auth header cookie accepted")
	}
}

func TestParseGatewayModels(t *testing.T) {
	m, err := ParseGatewayModels(" opus = gw/claude-opus , haiku=claude-haiku@20250101,fable=bedrock/anthropic.claude:0 ")
	if err != nil || m["opus"] != "gw/claude-opus" || m["haiku"] != "claude-haiku@20250101" || m["fable"] != "bedrock/anthropic.claude:0" || len(m) != 3 {
		t.Fatalf("parse = %v, %v", m, err)
	}
	if m, err := ParseGatewayModels(""); err != nil || len(m) != 0 {
		t.Fatalf("empty = %v, %v", m, err)
	}
	for _, bad := range []string{"opus", "opus=", "gpt=x", "opus=a,opus=b", "opus=has space", "opus=a=b", "opus=gw\u00a0x", "opus=gw\vx"} {
		if _, err := ParseGatewayModels(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
