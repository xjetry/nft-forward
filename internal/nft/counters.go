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

// nftMatchSide holds the "left" operand which can be a meta reference or
// a payload reference (or something we don't care about).
type nftMatchSide struct {
	Meta    *nftMeta    `json:"meta,omitempty"`
	Payload *nftPayload `json:"payload,omitempty"`
}

type nftMeta struct {
	Key string `json:"key"`
}

type nftPayload struct {
	Protocol string `json:"protocol"`
	Field    string `json:"field"`
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
		if item.Rule == nil || item.Rule.Chain != "prerouting" {
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

// extractMatch pulls proto and listen-port info from one match expression.
func extractMatch(m *nftMatch, c *Counter) {
	// meta match: left.meta.key == "l4proto", right is a string like "tcp"
	if m.Left.Meta != nil && m.Left.Meta.Key == "l4proto" {
		var proto string
		if json.Unmarshal(m.Right, &proto) == nil {
			c.Proto = proto
		}
		return
	}
	// payload match: left.payload.field == "dport"
	if m.Left.Payload != nil && m.Left.Payload.Field == "dport" {
		// right is a numeric port
		var port float64
		if json.Unmarshal(m.Right, &port) == nil {
			c.ListenPort = int(port)
		}
		// The protocol on the payload tells us the transport layer.
		// The tcp+udp set form uses a generic transport header ("th dport")
		// which nft reports as protocol "th". Map it to "tcp+udp" so it
		// stays consistent with how the rule is represented elsewhere.
		if proto := m.Left.Payload.Protocol; proto != "" && c.Proto == "" {
			if proto == "th" {
				c.Proto = "tcp+udp"
			} else {
				c.Proto = proto
			}
		}
	}
}
