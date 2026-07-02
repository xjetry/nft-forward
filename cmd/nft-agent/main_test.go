package main

import "testing"

func TestValidateConnectURL(t *testing.T) {
	cases := []struct {
		url           string
		allowInsecure bool
		wantErr       bool
	}{
		{"wss://panel.example.com/v1/agents", false, false},
		{"wss://panel/v1/agents", true, false},
		{"ws://panel/v1/agents", false, true},   // plaintext rejected by default
		{"ws://panel/v1/agents", true, false},   // allowed with opt-in
		{"https://panel/v1/agents", false, true}, // wrong scheme for a WS dial
		{"http://panel/v1/agents", false, true},
		{"panel/v1/agents", false, true}, // no scheme
	}
	for _, c := range cases {
		err := validateConnectURL(c.url, c.allowInsecure)
		if (err != nil) != c.wantErr {
			t.Errorf("validateConnectURL(%q, insecure=%v) err=%v, wantErr=%v", c.url, c.allowInsecure, err, c.wantErr)
		}
	}
}
