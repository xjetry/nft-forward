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
	Left  nftMatchSide `json:"left"`
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
		if item.Rule == nil || item.Rule.Chain != "account" {
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
}

// parseL4Proto reads the right side of a `meta l4proto` match: nft emits a bare
// string ("tcp") for a single protocol or {"set":["tcp","udp"]} for tcp+udp.
func parseL4Proto(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var set struct {
		Set []string `json:"set"`
	}
	if json.Unmarshal(raw, &set) == nil && len(set.Set) > 0 {
		tcp, udp := false, false
		for _, p := range set.Set {
			switch p {
			case "tcp":
				tcp = true
			case "udp":
				udp = true
			}
		}
		if tcp && udp {
			return "tcp+udp"
		}
		return set.Set[0]
	}
	return ""
}
