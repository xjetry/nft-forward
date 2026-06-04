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
	TableFamily = "ip"

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
	// ChainID/ChainName/TenantName are panel-side metadata: when a rule
	// belongs to a relay chain they identify it so the TUI can show the
	// owning chain and gate which fields are locally editable; TenantName
	// names the owning tenant for display. The data plane (DNAT / userspace /
	// MergedRuleset / DNS) never reads them.
	ChainID    int64  `json:"chain_id,omitempty"`
	ChainName  string `json:"chain_name,omitempty"`
	TenantName string `json:"tenant_name,omitempty"`
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
	return fmt.Sprintf("%s  %5d  →  %s:%d%s",
		strings.ToUpper(r.Proto), r.SrcPort, target, r.DestPort, suffix)
}

func NewRuleID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func Validate(r Rule) error {
	switch r.Proto {
	case "tcp", "udp", "tcp+udp":
	default:
		return fmt.Errorf("协议必须为 tcp、udp 或 tcp+udp")
	}
	if r.SrcPort < 1 || r.SrcPort > 65535 {
		return fmt.Errorf("监听端口必须在 1-65535 之间")
	}
	if r.DestPort < 1 || r.DestPort > 65535 {
		return fmt.Errorf("目标端口必须在 1-65535 之间")
	}
	hasHost := r.DestHost != ""
	hasIP := r.DestIP != ""
	if !hasHost && !hasIP {
		return fmt.Errorf("目标必须填 IPv4 或域名")
	}
	if hasIP {
		ip := net.ParseIP(r.DestIP)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("目标 IP 必须为有效的 IPv4")
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

func RenderRuleset(rules []Rule) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("table %s %s {\n", TableFamily, TableName))
	b.WriteString("\tchain prerouting {\n")
	b.WriteString("\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	for _, r := range rules {
		mark := ""
		if r.BandwidthMbps > 0 {
			// Mark = listen port. The tc HTB tree uses the same value as the
			// minor class id, so packets are routed to the rate-limited class.
			mark = fmt.Sprintf("meta mark set %d ", r.SrcPort)
		}
		b.WriteString(fmt.Sprintf("\t\t%s %scounter dnat to %s:%d\n",
			ProtoDportMatch(r.Proto, r.SrcPort), mark, r.DestIP, r.DestPort))
	}
	b.WriteString("\t}\n")
	b.WriteString("\tchain postrouting {\n")
	b.WriteString("\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	for _, r := range rules {
		b.WriteString(fmt.Sprintf("\t\tip daddr %s %s masquerade\n",
			r.DestIP, ProtoDportMatch(r.Proto, r.DestPort)))
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

func Apply(rules []Rule) error {
	// Single nft -f input wraps flush + redefine so the kernel either accepts
	// the whole set or keeps the previous state intact.
	var script strings.Builder
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
	return nil
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

func IPForwardEnabled() bool {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}

func EnableIPForward() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return err
	}
	conf := "/etc/sysctl.d/99-nft-forward.conf"
	body := "net.ipv4.ip_forward = 1\n"
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
