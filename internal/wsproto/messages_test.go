package wsproto

import (
	"encoding/json"
	"testing"
	"time"

	"nft-forward/internal/nft"
)

func TestEnvelopeRoundtrip(t *testing.T) {
	e := Envelope{Type: TypeHello, ID: "abc", Payload: json.RawMessage(`{"k":"v"}`)}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var got Envelope
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeHello || got.ID != "abc" || string(got.Payload) != `{"k":"v"}` {
		t.Fatalf("envelope roundtrip mismatch: %+v", got)
	}
}

func TestHelloEncode(t *testing.T) {
	h := Hello{NodeToken: "tok", AgentVersion: "v1", OS: "linux", Arch: "amd64", LastAppliedRev: "r1"}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var got Hello
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("hello roundtrip mismatch: %+v != %+v", got, h)
	}
}

func TestApplyRulesetEncodesRules(t *testing.T) {
	ar := ApplyRuleset{
		Rev: "rev42",
		Rules: []nft.Rule{
			{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80},
		},
	}
	b, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	var got ApplyRuleset
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Rev != "rev42" || len(got.Rules) != 1 || got.Rules[0].DestIP != "10.0.0.1" {
		t.Fatalf("apply_ruleset roundtrip mismatch: %+v", got)
	}
}

func TestPingPongCarriesTS(t *testing.T) {
	ts := time.Now().UTC().UnixMilli()
	p := Ping{TS: ts}
	b, _ := json.Marshal(p)
	var got Ping
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.TS != ts {
		t.Fatalf("ts mismatch: %d != %d", got.TS, ts)
	}
}
