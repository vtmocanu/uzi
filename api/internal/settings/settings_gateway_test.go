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
	m, err := ParseGatewayModels(" opus = gw/claude-opus , haiku=gw-haiku,fable=f ")
	if err != nil || m["opus"] != "gw/claude-opus" || m["haiku"] != "gw-haiku" || m["fable"] != "f" || len(m) != 3 {
		t.Fatalf("parse = %v, %v", m, err)
	}
	if m, err := ParseGatewayModels(""); err != nil || len(m) != 0 {
		t.Fatalf("empty = %v, %v", m, err)
	}
	for _, bad := range []string{"opus", "opus=", "gpt=x", "opus=a,opus=b", "opus=has space", "opus=a=b"} {
		if _, err := ParseGatewayModels(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
