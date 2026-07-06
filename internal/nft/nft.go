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

type Rule struct {
	ID            string `json:"id"`
	Proto         string `json:"proto"`
	SrcPort       int    `json:"src_port"`
	DestIP        string `json:"dest_ip,omitempty"`
	DestHost      string `json:"dest_host,omitempty"`
	DestPort      int    `json:"dest_port"`
	Comment       string `json:"comment,omitempty"`
	BandwidthMbps int    `json:"bandwidth_mbps,omitempty"`
	// Mode selects the data plane for this forward: "" / "kernel" = nftables
	// DNAT (zero-copy); "userspace" = the embedded TCP split-relay (TCP only).
	Mode string `json:"mode,omitempty"`
	// RuleID/RuleName/OwnerName are panel-side metadata: when a rule
	// belongs to a relay rule they identify it so the TUI can show the
	// owning rule and gate which fields are locally editable; OwnerName
	// names the owning user for display. The data plane (DNAT / userspace /
	// MergedRuleset / DNS) never reads them.
	RuleID    int64  `json:"rule_id,omitempty"`
	RuleName  string `json:"rule_name,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
	HopCount  int    `json:"hop_count,omitempty"`
	// ShapeGroup/RateMBytes carry the per-grant shared rate limit: every rule
	// in the same group (one user's rules on one panel node, priced by one
	// grant) shares a single RateMBytes MB/s bucket, both directions combined.
	// ShapeGroup is the panel-side grant id; 0 = no group. When the group is
	// valid the data plane ignores the legacy per-rule BandwidthMbps, which
	// new panels still fill so pre-group agents degrade to an approximate
	// per-rule cap.
	ShapeGroup int64 `json:"shape_group,omitempty"`
	RateMBytes int   `json:"rate_mbytes,omitempty"`
}

// EffectiveMode normalizes the mode: an empty or unrecognized value means
// kernel, so old state files and old-panel pushes (no mode field) default to
// the existing zero-copy behavior. This is the single source of the default.
func (r Rule) EffectiveMode() string {
	if r.Mode == ModeUserspace {
		return ModeUserspace
	}
	return ModeKernel
}

// GroupShapeMark returns the fwmark for a validly group-shaped rule, or 0.
// The 0x10000 offset keeps group marks disjoint from legacy per-port marks
// (ports are ≤ 0xFFFF). Groups whose id exceeds 16 bits cannot become a tc
// class minor and fall back to legacy shaping — callers must treat 0 as "not
// group-shaped", never as "unshaped".
func GroupShapeMark(r Rule) uint32 {
	if r.ShapeGroup > 0 && r.RateMBytes > 0 && r.ShapeGroup <= 0xFFFF {
		return uint32(0x10000 | r.ShapeGroup)
	}
	return 0
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
			return fmt.Errorf("目标 IP 格式非法")
		}
	}
	if hasHost {
		if !resolver.IsHostname(r.DestHost) {
			return fmt.Errorf("目标域名格式非法")
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
		} else if r.BandwidthMbps > 0 {
			mark = fmt.Sprintf("meta mark set %d ", r.SrcPort)
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

func IPv6ForwardEnabled() bool {
	return sysctl("/proc/sys/net/ipv6/conf/all/forwarding")
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
//     resolve, but out still contains the best-effort state — failed entries
//     keep their previous DestIP so live traffic isn't torn down by DNS hiccups)
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
