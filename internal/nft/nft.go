package nft

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"nft-forward/internal/resolver"
)

const (
	TableName   = "nft_forward"
	TableFamily = "inet"

	ModeKernel    = "kernel"
	ModeUserspace = "userspace"
)

// Rule 是一条转发规则。同一个类型既是数据面的输入，也是面板下发与本地规则迁移的
// 线上载荷、状态文件里的元素和 TUI 列表的一行，所以已有字段的 JSON 名字受已部署
// 的 agent、面板和磁盘上的 state.json 约束，不能改。
//
// 关于字段有两个问题，各自只在本文件里回答一次，别处不逐字段列举：
//
//   - 数据面读不读它：Dataplane。
//   - 它是不是面板给的、规则脱离面板时要去掉：WithoutPanelBinding。
//
// 加字段时两处都要归类；TestEveryRuleFieldIsClassified 在有字段没归类时红。
type Rule struct {
	// ID 是本地规则在本机的 id（NewRuleID 生成）；面板下发的规则不带它，面板规则
	// 用 RuleID 标识。数据面不读。
	ID       string `json:"id"`
	Proto    string `json:"proto"`
	SrcPort  int    `json:"src_port"`
	DestIP   string `json:"dest_ip,omitempty"`
	DestHost string `json:"dest_host,omitempty"`
	DestPort int    `json:"dest_port"`
	// Comment 是给人看的备注。数据面不读。
	Comment string `json:"comment,omitempty"`
	// BandwidthMbps 是这条规则自己的限速（Mbit/s），0 是不限。只有用户态转发读它，
	// 而且只在规则没有带着分组与额度时（forward 的 setLimits）；内核数据面只按分组
	// 整形，不读它。面板不填它：面板规则的限速只由 ShapeGroup 与 RateMBytes 表达
	// （server 的 buildRules）。
	BandwidthMbps int `json:"bandwidth_mbps,omitempty"`
	// Mode selects the data plane for this forward: "" / "kernel" = nftables
	// DNAT (zero-copy); "userspace" = the embedded TCP split-relay (TCP only).
	Mode string `json:"mode,omitempty"`
	// RuleID、RuleName、HopCount 是面板给的标识：这一条属于面板上的哪条规则、那条
	// 规则叫什么、整条链有几跳。TUI 用 RuleName 显示所属链路、用 HopCount 决定哪些
	// 字段可在本机改；daemon 用 HopCount 决定本机发起的编辑走哪条命令
	// （handleUpdatePanelRule）。数据面不读。
	RuleID   int64  `json:"rule_id,omitempty"`
	RuleName string `json:"rule_name,omitempty"`
	HopCount int    `json:"hop_count,omitempty"`
	// ShapeGroup 与 RateMBytes 是按授权共享的限速：ShapeGroup 是面板侧的授权 id（0
	// 表示不分组），RateMBytes 是这份授权的额度（MB/s）。同一分组的规则在一个数据面
	// 里合用一个桶：内核数据面是每组一个 tc class，连接两个方向的包都带分组的标记；
	// 用户态转发是每组一个限速器，上下行都走它。
	ShapeGroup int64 `json:"shape_group,omitempty"`
	RateMBytes int   `json:"rate_mbytes,omitempty"`
}

// EffectiveMode 把 Mode 归一：只有 ModeUserspace 是用户态转发，空值与其余任何值都
// 按内核转发。
func (r Rule) EffectiveMode() string {
	if r.Mode == ModeUserspace {
		return ModeUserspace
	}
	return ModeKernel
}

// Dataplane 返回 r 的数据面投影：数据面不读的字段清零，Mode 归一成 EffectiveMode
// （空值与 "kernel" 在数据面上是同一回事）。两条规则的投影相等，数据面对它们的处理
// 就相同。
//
// “哪些字段数据面不读”只在这里列一次。daemon 用这个投影判断数据面要不要重放
// （SameDataplane）。面板给下发载荷算的 rev 不用它：rev 标识的是整份载荷，数据面不
// 读的字段变了也要让节点收到新值（见 internal/server 的 computeRev）。
//
// 写成“清掉不读的”而不是“抄下读的”，是为了让漏归类的后果落在安全的一侧：新加的
// 字段忘了在这里处理时会留在投影里，它的变化多触发一次重放；反过来写，忘了的字段
// 不在投影里，数据面字段的变化会被当成没变，数据面不更新。
func (r Rule) Dataplane() Rule {
	r.ID = ""
	r.Comment = ""
	r.RuleID = 0
	r.RuleName = ""
	r.HopCount = 0
	r.Mode = r.EffectiveMode()
	return r
}

// SameDataplane 报告两组规则在数据面上是否相同：条数相同，且同一下标上的规则数据面
// 投影相等。按下标比，所以同样的规则换了顺序算不同；调用方要自己保证顺序稳定。
func SameDataplane(a, b []Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Dataplane() != b[i].Dataplane() {
			return false
		}
	}
	return true
}

// WithoutPanelBinding 返回脱离面板后的 r：面板给的标识（RuleID、RuleName、HopCount）
// 和面板给的分组限速（ShapeGroup、RateMBytes）清零，其余不动。“哪些字段是面板给的”
// 只在这里列一次。
//
// 分组限速跟着去掉，是因为那个额度由面板侧的授权定价，脱离面板后没有谁再管它。
// 规则自己的 BandwidthMbps 不在面板给的字段之列，原样留下。
func (r Rule) WithoutPanelBinding() Rule {
	r.RuleID = 0
	r.RuleName = ""
	r.HopCount = 0
	r.ShapeGroup = 0
	r.RateMBytes = 0
	return r
}

// GroupShapeMark 返回内核数据面给这条规则的连接打的 fwmark；返回 0 的规则不打标记，
// 在内核数据面不限速。内核数据面只按分组整形：RenderRuleset 按它打标记，tc 的
// planClasses 按它建 class，两边调的是这同一个函数。
//
// 标记是 0x10000 | 分组 id。恢复链只把高 16 位等于 0x0001 的 ct mark 恢复成包标记
// （见 RenderRuleset），主机上别的组件写的、不在这个范围里的 ct mark 不受影响。
//
// 分组 id 超过 16 位时返回 0：tc 的 class minor 只有 16 位，这样的分组建不出 class。
// 带着分组与额度却落到这里的规则由 KernelCannotShape 找出来。
func GroupShapeMark(r Rule) uint32 {
	if r.ShapeGroup > 0 && r.RateMBytes > 0 && r.ShapeGroup <= 0xFFFF {
		return uint32(0x10000 | r.ShapeGroup)
	}
	return 0
}

// KernelCannotShape 报告 r 是不是一条要求了分组限速、走内核数据面、而内核数据面
// 限不了速的规则：带着分组与额度，分组 id 却建不出 class（GroupShapeMark 为 0）。
// 这样的规则照常转发、不限速，daemon 应用面板规则集时把它们作为告警报给面板
// （SetPanelRuleset）。用户态转发按分组 id 找限速器，没有 16 位的限制，不在此列。
func KernelCannotShape(r Rule) bool {
	return r.EffectiveMode() == ModeKernel && r.ShapeGroup > 0 && r.RateMBytes > 0 && GroupShapeMark(r) == 0
}

func (r Rule) Display() string {
	target := r.DestIP
	if r.DestHost != "" {
		if r.DestIP != "" {
			target = fmt.Sprintf("%s (→ %s)", r.DestHost, r.DestIP)
		} else {
			target = r.DestHost
		}
	}
	suffix := ""
	if r.Comment != "" {
		suffix = "  # " + r.Comment
	}
	return fmt.Sprintf("%s  %5d  →  %s%s",
		strings.ToUpper(r.Proto), r.SrcPort, net.JoinHostPort(target, fmt.Sprintf("%d", r.DestPort)), suffix)
}

func NewRuleID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func IsIPv6(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.To4() == nil
}

func IsLoopback(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

// Validate 判断一条本地规则合不合法：协议、两个端口的范围、目标（IP 字面量或可能
// 解析得出来的域名）、转发模式，以及模式与协议的搭配。
//
// daemon 的本机控制口在把规则写进本地名下之前调用它（建，以及改完之后的整条规则），
// 本地规则的合法性只在这里定义；TUI 的表单只做粗检。面板下发的规则不经过这里：它们
// 合不合法由面板在建规则时裁决。
func Validate(r Rule) error {
	switch r.Proto {
	case "tcp", "udp", "tcp+udp":
	default:
		return fmt.Errorf("协议必须为 tcp、udp 或 tcp+udp")
	}
	if r.SrcPort < 0 || r.SrcPort > 65535 {
		return fmt.Errorf("监听端口必须在 0-65535 之间")
	}
	if r.DestPort < 1 || r.DestPort > 65535 {
		return fmt.Errorf("目标端口必须在 1-65535 之间")
	}
	hasHost := r.DestHost != ""
	hasIP := r.DestIP != ""
	if !hasHost && !hasIP {
		return fmt.Errorf("目标必须填 IP 或域名")
	}
	if hasIP {
		if net.ParseIP(r.DestIP) == nil {
			return fmt.Errorf("目标 %q 不是合法的 IP", r.DestIP)
		}
	}
	if hasHost {
		if !resolver.PlausibleHostname(r.DestHost) {
			return fmt.Errorf("目标 %q 不是合法的 IP 或域名", r.DestHost)
		}
	}
	switch r.Mode {
	case "", ModeKernel, ModeUserspace:
	default:
		return fmt.Errorf("转发模式必须为 kernel 或 userspace")
	}
	if r.Mode == ModeUserspace && r.Proto == "udp" {
		return fmt.Errorf("UDP 不支持用户态转发")
	}
	return nil
}

// ProtoDportMatch returns the nft match clause for a proto + dport. For
// "tcp+udp" it uses the l4proto set syntax so nft accepts the multi-protocol
// match; otherwise a plain "<proto> dport <port>".
func ProtoDportMatch(proto string, port int) string {
	if proto == "tcp+udp" {
		return fmt.Sprintf("meta l4proto { tcp, udp } th dport %d", port)
	}
	return fmt.Sprintf("%s dport %d", proto, port)
}

// dnatTarget formats the nft DNAT target for an inet-family table.
// IPv4: "dnat ip to 1.2.3.4:port"  —  IPv6: "dnat ip6 to [addr]:port"
func dnatTarget(ip string, port int) string {
	if IsIPv6(ip) {
		return fmt.Sprintf("dnat ip6 to [%s]:%d", ip, port)
	}
	return fmt.Sprintf("dnat ip to %s:%d", ip, port)
}

// daddrMatch returns "ip daddr <addr>" or "ip6 daddr <addr>" per IP family.
func daddrMatch(ip string) string {
	if IsIPv6(ip) {
		return "ip6 daddr " + ip
	}
	return "ip daddr " + ip
}

func RenderRuleset(rules []Rule) string {
	var b strings.Builder
	hasLoopback := false
	for _, r := range rules {
		if r.DestIP != "" && IsLoopback(r.DestIP) {
			hasLoopback = true
			break
		}
	}

	b.WriteString(fmt.Sprintf("table %s %s {\n", TableFamily, TableName))
	b.WriteString("\tchain prerouting {\n")
	b.WriteString("\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	for _, r := range rules {
		if r.DestIP == "" {
			continue
		}
		mark := ""
		if m := GroupShapeMark(r); m != 0 {
			// Stamp the first packet and its conntrack entry in one go; the
			// restore_mark chain re-stamps every later packet (both
			// directions) so the whole connection lands in the group's tc
			// class. nat prerouting only sees a connection's first packet,
			// which is why the mark must be persisted via ct mark.
			mark = fmt.Sprintf("meta mark set 0x%x ct mark set meta mark ", m)
		}
		if IsLoopback(r.DestIP) && IsIPv6(r.DestIP) {
			// IPv6 has no route_localnet equivalent; redirect delivers the
			// packet locally without needing to route to ::1.
			b.WriteString(fmt.Sprintf("\t\tmeta nfproto ipv6 %s %sredirect to :%d\n",
				ProtoDportMatch(r.Proto, r.SrcPort), mark, r.DestPort))
		} else {
			b.WriteString(fmt.Sprintf("\t\t%s %s%s\n",
				ProtoDportMatch(r.Proto, r.SrcPort), mark, dnatTarget(r.DestIP, r.DestPort)))
		}
	}
	b.WriteString("\t}\n")
	hasGroup := false
	for _, r := range rules {
		if GroupShapeMark(r) != 0 {
			hasGroup = true
			break
		}
	}
	if hasGroup {
		b.WriteString("\tchain restore_mark {\n")
		b.WriteString("\t\ttype filter hook prerouting priority mangle; policy accept;\n")
		// Restrict the restore to marks in our group space (high 16 bits ==
		// 0x0001, per the 0x10000 offset in GroupShapeMark). An unmasked
		// "ct mark != 0" would also restore ct marks set by unrelated host
		// components (policy routing, other QoS), hijacking their packets
		// into our tc classification.
		b.WriteString("\t\tct mark and 0xffff0000 == 0x10000 meta mark set ct mark\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("\tchain postrouting {\n")
	b.WriteString("\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	for _, r := range rules {
		if r.DestIP == "" || IsLoopback(r.DestIP) {
			continue
		}
		b.WriteString(fmt.Sprintf("\t\t%s %s ct status dnat masquerade\n",
			daddrMatch(r.DestIP), ProtoDportMatch(r.Proto, r.DestPort)))
	}
	b.WriteString("\t}\n")
	b.WriteString("\tchain account {\n")
	b.WriteString("\t\ttype filter hook forward priority filter; policy accept;\n")
	// Two counter rules per forwarded rule (original + reply) are intentional:
	// upload and download are billed and displayed separately. Do not collapse
	// them into one direction-agnostic counter — that would lose the up/down
	// split the panel and unidirectional billing rely on.
	for _, r := range rules {
		if r.DestIP == "" || IsLoopback(r.DestIP) {
			continue
		}
		for _, p := range accountProtos(r.Proto) {
			b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct direction original counter\n", p, r.SrcPort))
			b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct direction reply counter\n", p, r.SrcPort))
		}
	}
	b.WriteString("\t}\n")
	if hasLoopback {
		b.WriteString("\tchain account_local {\n")
		b.WriteString("\t\ttype filter hook input priority filter; policy accept;\n")
		for _, r := range rules {
			if r.DestIP == "" || !IsLoopback(r.DestIP) {
				continue
			}
			for _, p := range accountProtos(r.Proto) {
				b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct status dnat ct direction original counter\n", p, r.SrcPort))
			}
		}
		b.WriteString("\t}\n")
		b.WriteString("\tchain account_local_reply {\n")
		b.WriteString("\t\ttype filter hook output priority filter; policy accept;\n")
		for _, r := range rules {
			if r.DestIP == "" || !IsLoopback(r.DestIP) {
				continue
			}
			for _, p := range accountProtos(r.Proto) {
				b.WriteString(fmt.Sprintf("\t\tmeta l4proto %s ct original proto-dst %d ct status dnat ct direction reply counter\n", p, r.SrcPort))
			}
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// accountProtos lists the concrete l4protos a rule's accounting counter is
// emitted under. A tcp+udp rule gets one counter per protocol so ct original
// proto-dst keeps its inet_service type: nftables cannot type a proto-dst under
// an l4proto set, and v1.0.6 then serialises the port as an untyped hex
// "[invalid type]" that the counter parser cannot read.
func accountProtos(proto string) []string {
	if proto == "tcp+udp" {
		return []string{"tcp", "udp"}
	}
	return []string{proto}
}

func Apply(rules []Rule) error {
	var script strings.Builder
	// Remove leftover ip-family table from pre-v0.32 installations.
	script.WriteString(fmt.Sprintf("add table ip %s\ndelete table ip %s\n", TableName, TableName))
	script.WriteString(fmt.Sprintf("add table %s %s\n", TableFamily, TableName))
	script.WriteString(fmt.Sprintf("delete table %s %s\n", TableFamily, TableName))
	script.WriteString(RenderRuleset(rules))

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft 应用失败: %v: %s", err, strings.TrimSpace(stderr.String()))
	}

	for _, r := range rules {
		if r.DestIP != "" && IsLoopback(r.DestIP) && !IsIPv6(r.DestIP) {
			enableRouteLocalnet()
			break
		}
	}
	return nil
}

func enableRouteLocalnet() {
	_ = os.WriteFile("/proc/sys/net/ipv4/conf/all/route_localnet", []byte("1\n"), 0o644)
}

func Available() bool {
	_, err := exec.LookPath("nft")
	return err == nil
}

func Probe() error {
	cmd := exec.Command("nft", "list", "tables")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft 检测失败: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func sysctl(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

func IPForwardEnabled() bool {
	return sysctl("/proc/sys/net/ipv4/ip_forward")
}

func EnableIPForward() error {
	for _, p := range []struct{ proc, sysctl string }{
		{"/proc/sys/net/ipv4/ip_forward", "net.ipv4.ip_forward = 1"},
		{"/proc/sys/net/ipv6/conf/all/forwarding", "net.ipv6.conf.all.forwarding = 1"},
	} {
		_ = os.WriteFile(p.proc, []byte("1\n"), 0o644)
	}
	conf := "/etc/sysctl.d/99-nft-forward.conf"
	body := "net.ipv4.ip_forward = 1\nnet.ipv6.conf.all.forwarding = 1\n"
	return os.WriteFile(conf, []byte(body), 0o644)
}

// ResolveHosts walks rules; for any rule with DestHost set it asks r to look
// up the IPv4 and writes it into a copy. Returns:
//   - out: the resolved rule slice (callers should use this in nft.Apply)
//   - changed: true when at least one DestIP differs from the input
//   - err: aggregated lookup failure (non-nil when at least one host failed to
//     resolve, but out still contains the best-effort state — a failed entry
//     keeps the DestIP it came in with, which is empty unless the caller put
//     one there. A caller that wants a DNS hiccup to leave the live address in
//     place must pass that address in; the daemon's reconcileOwners does)
func ResolveHosts(ctx context.Context, rules []Rule, r *resolver.Resolver) ([]Rule, bool, error) {
	out := make([]Rule, len(rules))
	copy(out, rules)
	changed := false
	var errs []string
	for i := range out {
		if out[i].DestHost == "" {
			continue
		}
		ip, err := r.LookupIPv4(ctx, out[i].DestHost)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", out[i].DestHost, err))
			continue
		}
		if ip != out[i].DestIP {
			changed = true
			out[i].DestIP = ip
		}
	}
	if len(errs) > 0 {
		return out, changed, fmt.Errorf("dns: %s", strings.Join(errs, "; "))
	}
	return out, changed, nil
}
