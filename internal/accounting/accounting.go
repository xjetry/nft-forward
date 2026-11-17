// Package accounting 把节点上报的计数样本记进流量账本，并负责这些账本的清零。
//
// 各本账的累加与清零只在本包里发生。本包之外，账本的值只会随它所在的行一起消失：
// 规则、授权、节点、落地出口被删除时它们的账随行删除；RegenerateRule 整体删除重建
// 一条规则的跳行，逐跳账随新行从 0 开始。
//
// 三种字节单位，各是一个类型，彼此之间没有隐式转换：
//
//   - Raw（原始）：一个样本的上行 + 下行。
//   - Folded（折算）：按上报样本的那台物理节点的计费方向把上下行折成一个数——双向
//     节点等于原始，单向节点取较大的一侧。
//   - Weighted（加权）：折算 × 规则逻辑入口节点的倍率 × 属主的费率，每个样本各自
//     四舍五入。
//
// 六本账，每本只记一种单位：
//
//	账本                                       单位  记在哪一跳                  记在谁身上
//	node_raw_traffic.raw_bytes                 原始  每个样本，含匹配不到规则跳的  上报的物理节点
//	rule_hops.total_bytes                      折算  每一跳                      这一跳
//	rules.weighted_bytes                       加权  入口跳                      规则
//	users.traffic_used_bytes                   加权  入口跳                      属主
//	user_nodes.traffic_used_bytes              折算  每个逻辑段的段首跳          属主在这个逻辑节点上的授权
//	user_landing_exits.used_bytes(_up/_down)   原始  末跳，出口在属主名下在册    属主的这个落地出口
//
// 计价来源都从一跳的身份——上报节点上的一行 rule_hops——解析，每批解析一次
// （LoadBook）：是否单向取上报样本的物理节点；倍率取规则的逻辑入口节点
// （rules.node_id，组合入口用组合自己的倍率，中转层与组合成员的倍率不叠加）；费率取
// 规则属主；落地出口取属主名下在册的出口。无属主的规则不向任何人计价，倍率与费率
// 都按 1。
//
// 算术是纯函数（Meter），落库是另一步（Record 里的 post）。
package accounting

import (
	"math"
	"sort"

	"nft-forward/internal/wsproto"
)

// Raw 是原始字节：上行 + 下行，不折算、不加权。
type Raw int64

// Folded 是折算字节：按上报节点的计费方向折过的字节数，不加权。
type Folded int64

// Weighted 是加权字节：折算字节乘过规则入口倍率与属主费率之后的字节数。
type Weighted int64

// CounterKey 是节点上报计数时用的键：(协议, 监听端口)。
type CounterKey struct {
	Proto string
	Port  int
}

// Sample 是一台节点上报的一个计数样本：自上一批以来，这个计数键上的上、下行字节。
type Sample struct {
	Proto      string
	ListenPort int
	Up, Down   int64
}

// Key 是样本的计数键。
func (s Sample) Key() CounterKey { return CounterKey{Proto: s.Proto, Port: s.ListenPort} }

// Raw 是样本的原始字节。
func (s Sample) Raw() Raw { return Raw(s.Up + s.Down) }

// SamplesFromWire 把线上的计数样本换成 Sample。v0.33 之前的 agent 不分方向，只报
// BytesDelta：上下行都为 0 而 BytesDelta 为正时把它记作上行，这类节点升级之前流量
// 照样入账。
func SamplesFromWire(ws []wsproto.CounterSample) []Sample {
	out := make([]Sample, len(ws))
	for i, w := range ws {
		up := w.BytesUp
		if w.BytesUp == 0 && w.BytesDown == 0 && w.BytesDelta > 0 {
			up = w.BytesDelta
		}
		out[i] = Sample{Proto: w.Proto, ListenPort: w.ListenPort, Up: up, Down: w.BytesDown}
	}
	return out
}

// Pricing 是一跳的计价条件。LoadBook 解析它时已经落好缺省值（见 pricing），Fold 与
// Weigh 不再做任何判断。
type Pricing struct {
	// Unidirectional：上报样本的物理节点是否单向计费。
	Unidirectional bool
	// Multiplier：规则逻辑入口节点的倍率。0 是免费节点。
	Multiplier float64
	// Rate：规则属主的费率。
	Rate float64
}

// Fold 把一个样本按计费方向折算。单向计费只收较重的一侧：一条流量基本由一个方向
// 主导（多数是下载，备份是上传），取 max(上, 下) 给这一侧计价，而不把较轻的回程
// 再算一遍。
func (p Pricing) Fold(s Sample) Folded {
	if p.Unidirectional {
		return Folded(max(s.Up, s.Down))
	}
	return Folded(s.Up + s.Down)
}

// Weigh 把折算字节加权，四舍五入到整字节。
func (p Pricing) Weigh(f Folded) Weighted {
	return Weighted(math.Round(float64(f) * p.Multiplier * p.Rate))
}

// Place 是一跳在规则链上的位置，决定它的样本记进哪几本账。
type Place struct {
	// Entry：入口跳（position 0）。同一批字节流过链上每一跳，规则账与属主的全局账
	// 只在这里记一次。
	Entry bool
	// Final：末跳（规则里最大的 position）。落地出口账只在这里记：中间跳的目标是
	// 下一跳的中继地址，即使恰好与出口地址相同也不是属主的目的地。
	Final bool
	// SegmentStart：这一跳是所在逻辑段的第一跳（同一 via_node_id 里最小的
	// position）。一段里每一跳过的是同一批字节，授权账只在段首记一次。入口跳所在的
	// 段从 position 0 开始，所以入口跳总是段首。
	SegmentStart bool
	// Segment：这一跳所在逻辑段的逻辑节点（rule_hops.via_node_id），授权账记在属主
	// 在它上面的授权上。
	Segment int64
}

// GrantKey 是一本授权账：属主在一个逻辑节点上的授权。
type GrantKey struct {
	User int64
	Node int64
}

// ExitKey 是一本落地出口账：属主名下的一个落地出口。
type ExitKey struct {
	User int64
	Host string
	Port int
}

// Hop 是上报节点上的一跳，连同它的计价条件与位置。
type Hop struct {
	ID     int64 // rule_hops.id
	RuleID int64
	// Owner 是规则属主，0 表示无属主。
	Owner   int64
	Place   Place
	Pricing Pricing
	// Exit 非 nil 时，规则的出口是属主名下在册（present=1）的落地出口。
	Exit *ExitKey
}

// Book 是一批计数入账所需的全部计价与位置信息：上报节点上每一跳一个 Hop，按它可能
// 收到样本的每个计数键登记（一个 tcp+udp 的跳登记在 tcp+udp、tcp、udp 三个键上，
// 见 db.CounterProtos）。
type Book struct {
	Hops map[CounterKey]*Hop
}

// OwnerOf 返回计数键所在规则的属主；匹配不到规则跳或无属主时为 0。
func (b *Book) OwnerOf(k CounterKey) int64 {
	if b == nil {
		return 0
	}
	if h := b.Hops[k]; h != nil {
		return h.Owner
	}
	return 0
}

// Directional 是一本落地出口账的增量：上、下行各自的原始字节。
type Directional struct {
	Up, Down int64
}

// Raw 是上下行之和。
func (d Directional) Raw() Raw { return Raw(d.Up + d.Down) }

// Postings 是一批样本在每本账上的增量，键是账本的行。
type Postings struct {
	NodeRaw Raw
	Hops    map[int64]Folded    // rule_hops.id
	Rules   map[int64]Weighted  // rules.id
	Users   map[int64]Weighted  // users.id
	Grants  map[GrantKey]Folded // user_nodes(user_id, node_id)
	Exits   map[ExitKey]Directional
	// Charged 是这一批给其名下账本记了字节的属主（有样本匹配到他的规则跳，且折算
	// 为正）。Record 在落库之前先为他们做周期翻转，这一批记进新周期。
	Charged map[int64]bool
	// Unmatched 是匹配不到任何规则跳的样本，只计入节点的原始流量。
	Unmatched []Sample
}

// QuotaOwners 是这一批向其授权账或落地出口账记过账的属主，升序。全局账只在入口跳
// 记，入口跳总是段首、同时记授权账，所以全局账有增长的属主也都在其中。
func (p *Postings) QuotaOwners() []int64 {
	set := map[int64]bool{}
	for k := range p.Grants {
		set[k.User] = true
	}
	for k := range p.Exits {
		set[k.User] = true
	}
	return sortedIDs(set)
}

func (p *Postings) empty() bool {
	return p.NodeRaw == 0 && len(p.Hops) == 0 && len(p.Rules) == 0 && len(p.Users) == 0 &&
		len(p.Grants) == 0 && len(p.Exits) == 0
}

// Meter 算出一批样本在每本账上的增量。它是纯函数：只看 Book 与样本，不读库。
func Meter(b *Book, samples []Sample) *Postings {
	p := &Postings{
		Hops:    map[int64]Folded{},
		Rules:   map[int64]Weighted{},
		Users:   map[int64]Weighted{},
		Grants:  map[GrantKey]Folded{},
		Exits:   map[ExitKey]Directional{},
		Charged: map[int64]bool{},
	}
	for _, s := range samples {
		raw := s.Raw()
		// 节点原始流量在匹配规则跳之前就记：样本匹配不到规则跳（规则在下发与上报
		// 之间被删了）时，这些字节也是真实转发过的。
		p.NodeRaw += raw
		h := b.Hops[s.Key()]
		if h == nil {
			p.Unmatched = append(p.Unmatched, s)
			continue
		}

		folded := h.Pricing.Fold(s)
		p.Hops[h.ID] += folded
		var weighted Weighted
		if h.Place.Entry {
			weighted = h.Pricing.Weigh(folded)
			p.Rules[h.RuleID] += weighted
		}

		if h.Owner == 0 {
			continue
		}
		// 落地出口账记的是到达目的地的真实字节：不折算、不加权。
		if raw > 0 && h.Place.Final && h.Exit != nil {
			e := p.Exits[*h.Exit]
			e.Up += s.Up
			e.Down += s.Down
			p.Exits[*h.Exit] = e
		}
		if folded <= 0 {
			continue
		}
		p.Charged[h.Owner] = true
		if h.Place.SegmentStart {
			p.Grants[GrantKey{User: h.Owner, Node: h.Place.Segment}] += folded
		}
		if h.Place.Entry && weighted > 0 {
			p.Users[h.Owner] += weighted
		}
	}
	return p
}

func sortedIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
