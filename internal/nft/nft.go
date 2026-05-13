package nft

import (
	"bytes"
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
	case "tcp", "udp":
	default:
		return fmt.Errorf("协议必须为 tcp 或 udp")
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
	return nil
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
		b.WriteString(fmt.Sprintf("\t\t%s dport %d %scounter dnat to %s:%d\n",
			r.Proto, r.SrcPort, mark, r.DestIP, r.DestPort))
	}
	b.WriteString("\t}\n")
	b.WriteString("\tchain postrouting {\n")
	b.WriteString("\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	for _, r := range rules {
		b.WriteString(fmt.Sprintf("\t\tip daddr %s %s dport %d masquerade\n",
			r.DestIP, r.Proto, r.DestPort))
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
