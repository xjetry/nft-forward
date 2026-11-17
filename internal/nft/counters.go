package nft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type Counter struct {
	Proto      string `json:"proto"`
	ListenPort int    `json:"listen_port"`
	Direction  string `json:"direction"` // "original" (upload) or "reply" (download)
	Bytes      int64  `json:"bytes"`
	Packets    int64  `json:"packets"`
}

// Counters parses the prerouting chain of our table and returns one entry per
// rule. We look only at rules with a counter expression; the postrouting chain
// is masquerade-only and would duplicate the totals.
func Counters() ([]Counter, error) {
	cmd := exec.Command("nft", "-j", "list", "table", TableFamily, TableName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// table absent ≡ no rules; not an error from the caller's POV
		if strings.Contains(stderr.String(), "No such file or directory") ||
			strings.Contains(stderr.String(), "does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("nft -j list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseCounters(stdout.Bytes())
}

// nft JSON schema types — typed structs that mirror the subset of nft's
// JSON output we parse, replacing fragile map[string]any walks with
// json.Unmarshal into concrete fields.

// nftDoc is the top-level wrapper: {"nftables": [...]}.
type nftDoc struct {
	Nftables []nftItem `json:"nftables"`
}

// nftItem is one element of the top-level array. Only the "rule" key is
// relevant; metainfo, chain, table entries are ignored via omitempty.
type nftItem struct {
	Rule *nftRule `json:"rule,omitempty"`
}

type nftRule struct {
	Chain string            `json:"chain"`
	Expr  []json.RawMessage `json:"expr"`
}

// nftExpr is the union envelope for a single expression object. At most
// one field is non-nil per element.
type nftExpr struct {
	Match   *nftMatch   `json:"match,omitempty"`
	Counter *nftCounter `json:"counter,omitempty"`
}

type nftMatch struct {
	Left  nftMatchSide    `json:"left"`
	Right json.RawMessage `json:"right"`
}

// nftMatchSide holds the "left" operand of a match: a meta reference
// (l4proto) or a conntrack reference (the original tuple's proto-dst).
type nftMatchSide struct {
	Meta *nftMeta `json:"meta,omitempty"`
	Ct   *nftCt   `json:"ct,omitempty"`
}

type nftMeta struct {
	Key string `json:"key"`
}

type nftCt struct {
	Key string `json:"key"`
	Dir string `json:"dir"`
}

type nftCounter struct {
	Bytes   int64 `json:"bytes"`
	Packets int64 `json:"packets"`
}

func parseCounters(data []byte) ([]Counter, error) {
	var doc nftDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse nft json: %w", err)
	}
	var out []Counter
	for _, item := range doc.Nftables {
		if item.Rule == nil {
			continue
		}
		switch item.Rule.Chain {
		case "account", "account_local", "account_local_reply":
		default:
			continue
		}
		var c Counter
		hasCounter := false
		for _, raw := range item.Rule.Expr {
			var expr nftExpr
			if err := json.Unmarshal(raw, &expr); err != nil {
				continue
			}
			if expr.Match != nil {
				extractMatch(expr.Match, &c)
			}
			if expr.Counter != nil {
				hasCounter = true
				c.Bytes = expr.Counter.Bytes
				c.Packets = expr.Counter.Packets
			}
		}
		if hasCounter && c.Proto != "" && c.ListenPort != 0 {
			out = append(out, c)
		}
	}
	return out, nil
}

// extractMatch pulls proto and listen-port info from one match expression in
// the accounting chain: `meta l4proto <p>` gives the protocol, and
// `ct original proto-dst <port>` gives the listen port (the pre-DNAT dport
// preserved by conntrack).
func extractMatch(m *nftMatch, c *Counter) {
	if m.Left.Meta != nil && m.Left.Meta.Key == "l4proto" {
		c.Proto = parseL4Proto(m.Right)
		return
	}
	if m.Left.Ct != nil && m.Left.Ct.Key == "proto-dst" {
		var port float64
		if json.Unmarshal(m.Right, &port) == nil {
			c.ListenPort = int(port)
		}
	}
	if m.Left.Ct != nil && m.Left.Ct.Key == "direction" {
		var dir string
		if json.Unmarshal(m.Right, &dir) == nil {
			c.Direction = dir
		}
	}
}

// l4protoName 把 nft 输出里的一个传输层协议值归一成协议名。
//
// 同一条 `meta l4proto tcp` 规则，nft 列出来时协议可能是名字 "tcp"，也可能是 IANA
// 协议号 6：名字取自机器上的 /etc/protocols，没有这个文件（极简镜像、没装 netbase）
// 时 nft 就原样输出数字——在 nftables 1.0.6 上实测，把该文件挪走后同一张表的输出从
// "tcp" / "udp" 变成 6 / 17。规则的协议只有 tcp、udp、tcp+udp 三种（Validate），计数链
// 里 tcp+udp 又展开成前两种（accountProtos），所以协议号只需认 6 和 17；认不出的返回
// 空串，调用方据此丢弃这条计数。
func l4protoName(raw json.RawMessage) string {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name
	}
	var number int
	if json.Unmarshal(raw, &number) == nil {
		switch number {
		case 6:
			return "tcp"
		case 17:
			return "udp"
		}
	}
	return ""
}

// parseL4Proto 读 `meta l4proto` 匹配的右值：单个协议，或 tcp+udp 的集合写法
// {"set":[…]}。两种写法里的每个协议值都经 l4protoName 归一，名字与协议号等价。
func parseL4Proto(raw json.RawMessage) string {
	if name := l4protoName(raw); name != "" {
		return name
	}
	var set struct {
		Set []json.RawMessage `json:"set"`
	}
	if json.Unmarshal(raw, &set) == nil && len(set.Set) > 0 {
		tcp, udp := false, false
		for _, member := range set.Set {
			switch l4protoName(member) {
			case "tcp":
				tcp = true
			case "udp":
				udp = true
			}
		}
		if tcp && udp {
			return "tcp+udp"
		}
		return l4protoName(set.Set[0])
	}
	return ""
}
