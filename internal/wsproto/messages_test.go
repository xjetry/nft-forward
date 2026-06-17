package wsproto

import (
	"encoding/json"
	"testing"
	"time"

	"nft-forward/internal/nft"
)

func TestUpgradeDataRoundTrip(t *testing.T) {
	in := Upgrade{Version: "v1", SHA256: "abc", Size: 3, DownloadAt: "u", Data: []byte{1, 2, 3}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Upgrade
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if string(out.Data) != string(in.Data) || out.SHA256 != in.SHA256 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

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

func TestRuleCreateRoundtrip(t *testing.T) {
	rc := RuleCreate{Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 80, ListenPort: 12000, Mode: nft.ModeKernel, Comment: "test", Name: "r1"}
	b, err := json.Marshal(rc)
	if err != nil {
		t.Fatal(err)
	}
	var got RuleCreate
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Proto != "tcp" || got.ExitHost != "10.0.0.1" || got.ListenPort != 12000 || got.Name != "r1" {
		t.Fatalf("rule_create roundtrip mismatch: %+v", got)
	}
}

func TestRuleUpdateRoundtrip(t *testing.T) {
	ru := RuleUpdate{RuleID: 5, Proto: "tcp", ExitHost: "10.0.0.2", ExitPort: 443, ListenPort: 15000}
	b, err := json.Marshal(ru)
	if err != nil {
		t.Fatal(err)
	}
	var got RuleUpdate
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.RuleID != 5 || got.ExitHost != "10.0.0.2" || got.ListenPort != 15000 {
		t.Fatalf("rule_update roundtrip mismatch: %+v", got)
	}
}

func TestMigrateRulesRoundtrip(t *testing.T) {
	mr := MigrateRules{Rules: []nft.Rule{
		{Proto: "tcp", SrcPort: 12000, DestHost: "1.2.3.4", DestPort: 80},
	}}
	b, err := json.Marshal(mr)
	if err != nil {
		t.Fatal(err)
	}
	var got MigrateRules
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 1 || got.Rules[0].SrcPort != 12000 {
		t.Fatalf("migrate_rules roundtrip mismatch: %+v", got)
	}
}

func TestNewTypeConstants(t *testing.T) {
	if TypeRuleCreate != "rule_create" || TypeRuleUpdate != "rule_update" || TypeMigrateRules != "migrate_rules" {
		t.Fatalf("unexpected new type constants: %q %q %q", TypeRuleCreate, TypeRuleUpdate, TypeMigrateRules)
	}
}

func TestRuleCommandFramesRoundtrip(t *testing.T) {
	e := RuleHopEdit{RuleID: 7, ListenPort: 21000, Mode: nft.ModeUserspace, Comment: "edge hop"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var ge RuleHopEdit
	if err := json.Unmarshal(b, &ge); err != nil {
		t.Fatal(err)
	}
	if ge.RuleID != 7 || ge.ListenPort != 21000 || ge.Mode != nft.ModeUserspace || ge.Comment != "edge hop" {
		t.Fatalf("rule_hop_edit roundtrip mismatch: %+v", ge)
	}

	d := RuleDelete{RuleID: 9}
	b, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var gd RuleDelete
	if err := json.Unmarshal(b, &gd); err != nil {
		t.Fatal(err)
	}
	if gd.RuleID != 9 {
		t.Fatalf("rule_delete roundtrip mismatch: %+v", gd)
	}

	a := RuleCmdAck{OK: false, Error: "端口被占用", Entry: ""}
	b, err = json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var ga RuleCmdAck
	if err := json.Unmarshal(b, &ga); err != nil {
		t.Fatal(err)
	}
	if ga.OK || ga.Error != "端口被占用" {
		t.Fatalf("rule_cmd_ack roundtrip mismatch: %+v", ga)
	}

	ok := RuleCmdAck{OK: true, Entry: "1.2.3.4:21000"}
	b, err = json.Marshal(ok)
	if err != nil {
		t.Fatal(err)
	}
	var gok RuleCmdAck
	if err := json.Unmarshal(b, &gok); err != nil {
		t.Fatal(err)
	}
	if !gok.OK || gok.Entry != "1.2.3.4:21000" {
		t.Fatalf("rule_cmd_ack success roundtrip mismatch: %+v", gok)
	}
}

func TestRuleCommandTypeConstants(t *testing.T) {
	if TypeRuleHopEdit != "rule_hop_edit" || TypeRuleDelete != "rule_delete" || TypeRuleCmdAck != "rule_cmd_ack" {
		t.Fatalf("unexpected rule type constants: %q %q %q", TypeRuleHopEdit, TypeRuleDelete, TypeRuleCmdAck)
	}
}
