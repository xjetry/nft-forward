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

func TestPanelSegmentEditRoundtrip(t *testing.T) {
	p := PanelSegmentEdit{Forwards: []Forward{
		{Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.9", TargetPort: 443, Comment: "edge", Mode: nft.ModeKernel},
	}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got PanelSegmentEdit
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Forwards) != 1 || got.Forwards[0].ListenPort != 30000 || got.Forwards[0].TargetIP != "10.0.0.9" {
		t.Fatalf("panel_segment_edit roundtrip mismatch: %+v", got)
	}
}

func TestPanelSegmentEditTypeConstant(t *testing.T) {
	if TypePanelSegmentEdit != "panel_segment_edit" {
		t.Fatalf("unexpected type constant %q", TypePanelSegmentEdit)
	}
}

func TestChainCommandFramesRoundtrip(t *testing.T) {
	e := ChainHopEdit{ChainID: 7, ListenPort: 21000, Mode: nft.ModeUserspace, Comment: "edge hop"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var ge ChainHopEdit
	if err := json.Unmarshal(b, &ge); err != nil {
		t.Fatal(err)
	}
	if ge.ChainID != 7 || ge.ListenPort != 21000 || ge.Mode != nft.ModeUserspace || ge.Comment != "edge hop" {
		t.Fatalf("chain_hop_edit roundtrip mismatch: %+v", ge)
	}

	d := ChainDelete{ChainID: 9}
	b, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var gd ChainDelete
	if err := json.Unmarshal(b, &gd); err != nil {
		t.Fatal(err)
	}
	if gd.ChainID != 9 {
		t.Fatalf("chain_delete roundtrip mismatch: %+v", gd)
	}

	a := ChainCmdAck{OK: false, Error: "端口被占用", Entry: ""}
	b, err = json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var ga ChainCmdAck
	if err := json.Unmarshal(b, &ga); err != nil {
		t.Fatal(err)
	}
	if ga.OK || ga.Error != "端口被占用" {
		t.Fatalf("chain_cmd_ack roundtrip mismatch: %+v", ga)
	}

	ok := ChainCmdAck{OK: true, Entry: "1.2.3.4:21000"}
	b, err = json.Marshal(ok)
	if err != nil {
		t.Fatal(err)
	}
	var gok ChainCmdAck
	if err := json.Unmarshal(b, &gok); err != nil {
		t.Fatal(err)
	}
	if !gok.OK || gok.Entry != "1.2.3.4:21000" {
		t.Fatalf("chain_cmd_ack success roundtrip mismatch: %+v", gok)
	}
}

func TestChainCommandTypeConstants(t *testing.T) {
	if TypeChainHopEdit != "chain_hop_edit" || TypeChainDelete != "chain_delete" || TypeChainCmdAck != "chain_cmd_ack" {
		t.Fatalf("unexpected chain type constants: %q %q %q", TypeChainHopEdit, TypeChainDelete, TypeChainCmdAck)
	}
}
