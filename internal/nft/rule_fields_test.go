package nft_test

import (
	"testing"

	"nft-forward/internal/nft"
	"nft-forward/internal/nft/nfttest"
)

// ruleFieldClasses 是 nft.Rule 每个字段的归类：数据面读不读它，它是不是面板给的。
// 实现里这两件事分别由 Rule.Dataplane 与 Rule.WithoutPanelBinding 定义；这张表是
// 对它们的独立核对，也是加字段时必须过的一道：表里没有的字段会让
// TestEveryRuleFieldIsClassified 红。
var ruleFieldClasses = map[string]struct{ dataplane, fromPanel bool }{
	"ID":            {dataplane: false, fromPanel: false},
	"Proto":         {dataplane: true, fromPanel: false},
	"SrcPort":       {dataplane: true, fromPanel: false},
	"DestIP":        {dataplane: true, fromPanel: false},
	"DestHost":      {dataplane: true, fromPanel: false},
	"DestPort":      {dataplane: true, fromPanel: false},
	"Comment":       {dataplane: false, fromPanel: false},
	"BandwidthMbps": {dataplane: true, fromPanel: false},
	"Mode":          {dataplane: true, fromPanel: false},
	"RuleID":        {dataplane: false, fromPanel: true},
	"RuleName":      {dataplane: false, fromPanel: true},
	"HopCount":      {dataplane: false, fromPanel: true},
	"ShapeGroup":    {dataplane: true, fromPanel: true},
	"RateMBytes":    {dataplane: true, fromPanel: true},
}

// 每个字段都归了类，且两个函数对每个字段的处理与归类一致：数据面读的字段变了，
// 数据面投影就变，不读的变了投影不变；面板给的字段在脱离面板时被去掉，其余原样
// 留下。空规则和每个字段都有值的规则各核对一遍。
func TestEveryRuleFieldIsClassified(t *testing.T) {
	populated := nft.Rule{
		ID: "aabbccdd", Proto: "tcp", SrcPort: 12000, DestIP: "10.0.0.1", DestHost: "exit.example", DestPort: 443,
		Comment: "c", BandwidthMbps: 84, Mode: nft.ModeKernel,
		RuleID: 7, RuleName: "r", HopCount: 2, ShapeGroup: 5, RateMBytes: 10,
	}
	for baseName, base := range map[string]nft.Rule{"空规则": {}, "每个字段都有值的规则": populated} {
		variants := nfttest.FieldVariants(base)
		for field, variant := range variants {
			class, ok := ruleFieldClasses[field]
			if !ok {
				t.Errorf("字段 %s 没有归类：在 ruleFieldClasses 里写明数据面读不读它、它是不是面板给的，并相应处理 Rule.Dataplane 与 Rule.WithoutPanelBinding", field)
				continue
			}
			if changed := variant.Dataplane() != base.Dataplane(); changed != class.dataplane {
				t.Errorf("%s：只改 %s，数据面投影变了=%v，按归类应为 %v", baseName, field, changed, class.dataplane)
			}
			// 脱离面板：面板给的字段被去掉，于是只在这个字段上不同的两条规则变得
			// 相同；不是面板给的字段原样留下，两条规则仍然不同。
			if dropped := variant.WithoutPanelBinding() == base.WithoutPanelBinding(); dropped != class.fromPanel {
				t.Errorf("%s：只改 %s，脱离面板后这个差别被去掉了=%v，按归类应为 %v", baseName, field, dropped, class.fromPanel)
			}
		}
		for field := range ruleFieldClasses {
			if _, ok := variants[field]; !ok {
				t.Errorf("ruleFieldClasses 里的 %s 已不是 nft.Rule 的字段", field)
			}
		}
	}
}

// 脱离面板后，面板给的字段是零值，不是别的什么值。
func TestWithoutPanelBindingZeroesPanelFields(t *testing.T) {
	in := nft.Rule{
		ID: "aabbccdd", Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.1", DestPort: 8443, Comment: "c",
		BandwidthMbps: 84, Mode: nft.ModeUserspace,
		RuleID: 7, RuleName: "r", HopCount: 2, ShapeGroup: 5, RateMBytes: 10,
	}
	want := nft.Rule{
		ID: "aabbccdd", Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.1", DestPort: 8443, Comment: "c",
		BandwidthMbps: 84, Mode: nft.ModeUserspace,
	}
	if got := in.WithoutPanelBinding(); got != want {
		t.Fatalf("脱离面板后的规则不对:\n got %+v\nwant %+v", got, want)
	}
}

// Mode 的空值与 "kernel" 在数据面上是同一回事，投影里不区分；"userspace" 不同。
func TestDataplaneTreatsEmptyModeAsKernel(t *testing.T) {
	base := nft.Rule{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}
	kernel, userspace := base, base
	kernel.Mode, userspace.Mode = nft.ModeKernel, nft.ModeUserspace
	if base.Dataplane() != kernel.Dataplane() {
		t.Errorf("空 Mode 与 kernel 的数据面投影应相同")
	}
	if base.Dataplane() == userspace.Dataplane() {
		t.Errorf("kernel 与 userspace 的数据面投影应不同")
	}
}

func TestSameDataplane(t *testing.T) {
	a := nft.Rule{RuleID: 1, RuleName: "a", Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}
	b := nft.Rule{RuleID: 2, RuleName: "b", Proto: "tcp", SrcPort: 81, DestIP: "10.0.0.2", DestPort: 81}
	renamed := a
	renamed.RuleName, renamed.Comment, renamed.HopCount = "a2", "note", 3
	retargeted := a
	retargeted.DestPort = 8080

	cases := []struct {
		name string
		x, y []nft.Rule
		want bool
	}{
		{"两个空集", nil, []nft.Rule{}, true},
		{"逐条相同", []nft.Rule{a, b}, []nft.Rule{a, b}, true},
		{"只差数据面不读的字段", []nft.Rule{a, b}, []nft.Rule{renamed, b}, true},
		{"数据面读的字段不同", []nft.Rule{a, b}, []nft.Rule{retargeted, b}, false},
		{"条数不同", []nft.Rule{a, b}, []nft.Rule{a}, false},
		{"同样的规则换了顺序", []nft.Rule{a, b}, []nft.Rule{b, a}, false},
	}
	for _, c := range cases {
		if got := nft.SameDataplane(c.x, c.y); got != c.want {
			t.Errorf("%s：SameDataplane = %v，应为 %v", c.name, got, c.want)
		}
	}
}
