package accounting

import (
	"database/sql"
	"reflect"
	"testing"

	"nft-forward/internal/wsproto"
)

// v0.33 之前的 agent 只报不分方向的 BytesDelta：上下行都为 0 时把它记作上行；
// 带方向的样本里的 BytesDelta 不参与。
func TestSamplesFromWire(t *testing.T) {
	got := SamplesFromWire([]wsproto.CounterSample{
		{Proto: "tcp", ListenPort: 1, BytesDelta: 70},
		{Proto: "udp", ListenPort: 2, BytesUp: 3, BytesDown: 4, BytesDelta: 99},
		{Proto: "tcp", ListenPort: 3, BytesDown: 5, BytesDelta: 99},
	})
	want := []Sample{
		{Proto: "tcp", ListenPort: 1, Up: 70},
		{Proto: "udp", ListenPort: 2, Up: 3, Down: 4},
		{Proto: "tcp", ListenPort: 3, Down: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestPricingDefaults(t *testing.T) {
	f := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	null := sql.NullFloat64{}
	for _, c := range []struct {
		name       string
		owned      bool
		mult, rate sql.NullFloat64
		want       Pricing
	}{
		{"有属主：原样取倍率与费率", true, f(2), f(1.5), Pricing{Multiplier: 2, Rate: 1.5}},
		{"倍率 0 是免费节点", true, f(0), f(1.5), Pricing{Multiplier: 0, Rate: 1.5}},
		{"负倍率按 1", true, f(-2), f(1.5), Pricing{Multiplier: 1, Rate: 1.5}},
		{"读不到倍率按 1", true, null, f(1.5), Pricing{Multiplier: 1, Rate: 1.5}},
		{"费率 0 按 1", true, f(2), f(0), Pricing{Multiplier: 2, Rate: 1}},
		{"负费率按 1", true, f(2), f(-1), Pricing{Multiplier: 2, Rate: 1}},
		{"无属主：倍率与费率都按 1", false, f(2), null, Pricing{Multiplier: 1, Rate: 1}},
		{"无属主：免费入口也按 1", false, f(0), null, Pricing{Multiplier: 1, Rate: 1}},
	} {
		for _, uni := range []bool{false, true} {
			want := c.want
			want.Unidirectional = uni
			if got := pricing(uni, c.owned, c.mult, c.rate); got != want {
				t.Errorf("%s（单向=%v）：got %+v, want %+v", c.name, uni, got, want)
			}
		}
	}
}

// 单向节点取较大的一侧，两个方向都要试，免得 max 退化成总取上行或总取下行；双向
// 节点取和。加权四舍五入到整字节，半数远离零。
func TestFoldAndWeigh(t *testing.T) {
	for _, c := range []struct {
		name     string
		uni      bool
		up, down int64
		want     Folded
	}{
		{"单向，上行重", true, 900, 200, 900},
		{"单向，下行重", true, 200, 900, 900},
		{"单向，相等", true, 500, 500, 500},
		{"双向", false, 900, 200, 1100},
	} {
		if got := (Pricing{Unidirectional: c.uni}).Fold(Sample{Up: c.up, Down: c.down}); got != c.want {
			t.Errorf("%s：got %d, want %d", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		f          Folded
		mult, rate float64
		want       Weighted
	}{
		{1000, 2, 1.5, 3000},
		{901, 0.5, 1.5, 676}, // 675.75
		{1, 1.5, 1, 2},       // 1.5 远离零
		{301, 3, 1.5, 1355},  // 1354.5
		{5000, 0, 1.5, 0},
	} {
		if got := (Pricing{Multiplier: c.mult, Rate: c.rate}).Weigh(c.f); got != c.want {
			t.Errorf("Weigh(%d × %v × %v)：got %d, want %d", c.f, c.mult, c.rate, got, c.want)
		}
	}
}

// meterBook 是一台上报节点上的几跳，各占一种位置：
//
//	entry   规则 10 的入口跳（也是段首），有属主 7，双向，倍率 2、费率 1.5
//	middle  规则 20 的第二跳，段首（逻辑段 50），属主 7，单向
//	tail    规则 20 的末跳，非段首，单向，出口在册，属主 7
//	lone    规则 30 唯一的一跳：入口、段首、末跳都是它，出口在册，属主 8
//	orphan  规则 40 的入口跳，无属主
func meterBook() *Book {
	exit7 := &ExitKey{User: 7, Host: "8.8.8.8", Port: 443}
	exit8 := &ExitKey{User: 8, Host: "9.9.9.9", Port: 443}
	b := &Book{Hops: map[CounterKey]*Hop{}}
	add := func(port int, h *Hop) { b.Hops[CounterKey{Proto: "tcp", Port: port}] = h }
	add(1, &Hop{ID: 101, RuleID: 10, Owner: 7,
		Place:   Place{Entry: true, SegmentStart: true, Segment: 5},
		Pricing: Pricing{Multiplier: 2, Rate: 1.5}})
	add(2, &Hop{ID: 202, RuleID: 20, Owner: 7,
		Place:   Place{SegmentStart: true, Segment: 50},
		Pricing: Pricing{Unidirectional: true, Multiplier: 3, Rate: 1.5}})
	add(3, &Hop{ID: 203, RuleID: 20, Owner: 7,
		Place:   Place{Final: true, Segment: 50},
		Pricing: Pricing{Unidirectional: true, Multiplier: 3, Rate: 1.5}, Exit: exit7})
	add(4, &Hop{ID: 301, RuleID: 30, Owner: 8,
		Place:   Place{Entry: true, Final: true, SegmentStart: true, Segment: 6},
		Pricing: Pricing{Multiplier: 1, Rate: 1}, Exit: exit8})
	add(5, &Hop{ID: 401, RuleID: 40,
		Place:   Place{Entry: true, Final: true, SegmentStart: true, Segment: 9},
		Pricing: Pricing{Multiplier: 1, Rate: 1}})
	return b
}

func TestMeterPostsEachPlaceToItsLedgers(t *testing.T) {
	tcp := func(port int, up, down int64) Sample {
		return Sample{Proto: "tcp", ListenPort: port, Up: up, Down: down}
	}
	for _, c := range []struct {
		name    string
		samples []Sample
		want    Postings
	}{
		{
			name:    "入口跳：逐跳折算、规则与全局记加权、段首记授权；没有出口不记出口账",
			samples: []Sample{tcp(1, 300, 700)},
			want: Postings{NodeRaw: 1000,
				Hops: map[int64]Folded{101: 1000}, Rules: map[int64]Weighted{10: 3000},
				Users: map[int64]Weighted{7: 3000}, Grants: map[GrantKey]Folded{{7, 5}: 1000},
				Exits: map[ExitKey]Directional{}, Charged: map[int64]bool{7: true}},
		},
		{
			name:    "非入口的段首跳：只记逐跳与授权，都是折算（单向取 max），不乘倍率",
			samples: []Sample{tcp(2, 900, 100)},
			want: Postings{NodeRaw: 1000,
				Hops: map[int64]Folded{202: 900}, Rules: map[int64]Weighted{},
				Users: map[int64]Weighted{}, Grants: map[GrantKey]Folded{{7, 50}: 900},
				Exits: map[ExitKey]Directional{}, Charged: map[int64]bool{7: true}},
		},
		{
			name:    "非段首的末跳：只记逐跳与落地出口；逐跳记折算（单向取 max），出口记原始的上下行",
			samples: []Sample{tcp(3, 150, 250)},
			want: Postings{NodeRaw: 400,
				Hops: map[int64]Folded{203: 250}, Rules: map[int64]Weighted{},
				Users: map[int64]Weighted{}, Grants: map[GrantKey]Folded{},
				Exits:   map[ExitKey]Directional{{7, "8.8.8.8", 443}: {Up: 150, Down: 250}},
				Charged: map[int64]bool{7: true}},
		},
		{
			name:    "单跳规则：一跳同时是入口、段首、末跳，六本账都记",
			samples: []Sample{tcp(4, 10, 20)},
			want: Postings{NodeRaw: 30,
				Hops: map[int64]Folded{301: 30}, Rules: map[int64]Weighted{30: 30},
				Users: map[int64]Weighted{8: 30}, Grants: map[GrantKey]Folded{{8, 6}: 30},
				Exits:   map[ExitKey]Directional{{8, "9.9.9.9", 443}: {Up: 10, Down: 20}},
				Charged: map[int64]bool{8: true}},
		},
		{
			name:    "无属主：只记节点原始、逐跳与规则账，不记任何人的账",
			samples: []Sample{tcp(5, 10, 30)},
			want: Postings{NodeRaw: 40,
				Hops: map[int64]Folded{401: 40}, Rules: map[int64]Weighted{40: 40},
				Users: map[int64]Weighted{}, Grants: map[GrantKey]Folded{},
				Exits: map[ExitKey]Directional{}, Charged: map[int64]bool{}},
		},
		{
			name:    "匹配不到规则跳：只记节点原始",
			samples: []Sample{tcp(99, 7, 3)},
			want: Postings{NodeRaw: 10,
				Hops: map[int64]Folded{}, Rules: map[int64]Weighted{},
				Users: map[int64]Weighted{}, Grants: map[GrantKey]Folded{},
				Exits: map[ExitKey]Directional{}, Charged: map[int64]bool{},
				Unmatched: []Sample{tcp(99, 7, 3)}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Meter(meterBook(), c.samples)
			if !reflect.DeepEqual(*got, c.want) {
				t.Fatalf("\ngot  %+v\nwant %+v", *got, c.want)
			}
		})
	}
}

// 一跳收到的每个样本各自加权、各自四舍五入再相加（tcp+udp 拆成两个样本扇入同一跳
// 时就是这样）：倍率 1.5 下两个 1 字节的样本记 2 + 2 = 4，而不是先相加再加权的 3。
func TestMeterRoundsEachSample(t *testing.T) {
	h := &Hop{ID: 1, RuleID: 1, Owner: 7,
		Place:   Place{Entry: true, SegmentStart: true, Segment: 1},
		Pricing: Pricing{Multiplier: 1.5, Rate: 1}}
	b := &Book{Hops: map[CounterKey]*Hop{{"tcp", 9}: h, {"udp", 9}: h}}
	p := Meter(b, []Sample{{Proto: "tcp", ListenPort: 9, Up: 1}, {Proto: "udp", ListenPort: 9, Down: 1}})
	if p.Rules[1] != 4 || p.Users[7] != 4 {
		t.Fatalf("规则账 %d、全局账 %d，都应为 4", p.Rules[1], p.Users[7])
	}
	if p.Hops[1] != 2 || p.Grants[GrantKey{7, 1}] != 2 {
		t.Fatalf("逐跳账 %d、授权账 %d，都应为折算的 2", p.Hops[1], p.Grants[GrantKey{7, 1}])
	}
}

// 配额回调的对象是向其授权账或落地出口账记过账的属主。只经过某人非段首、非末跳
// 一跳的批次给他记了逐跳账，但没有配额账增长，不触发回调；周期翻转检查（Charged）
// 仍然包括他。
func TestQuotaOwnersAreGrantOrExitOwners(t *testing.T) {
	h := &Hop{ID: 1, RuleID: 1, Owner: 7, Place: Place{Segment: 3}, Pricing: Pricing{Multiplier: 1, Rate: 1}}
	b := &Book{Hops: map[CounterKey]*Hop{{"tcp", 9}: h}}
	p := Meter(b, []Sample{{Proto: "tcp", ListenPort: 9, Up: 5}})
	if got := p.QuotaOwners(); len(got) != 0 {
		t.Fatalf("中间跳的批次不应触发配额回调，got %v", got)
	}
	if !p.Charged[7] {
		t.Fatal("中间跳的批次也给属主记了账，应做周期翻转检查")
	}

	p = Meter(meterBook(), []Sample{
		{Proto: "tcp", ListenPort: 3, Up: 1}, // 属主 7 的末跳：落地出口账
		{Proto: "tcp", ListenPort: 4, Up: 1}, // 属主 8 的单跳：授权账与出口账
		{Proto: "tcp", ListenPort: 5, Up: 1}, // 无属主
	})
	if got := p.QuotaOwners(); !reflect.DeepEqual(got, []int64{7, 8}) {
		t.Fatalf("QuotaOwners = %v, want [7 8]", got)
	}
}
