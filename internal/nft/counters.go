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

func parseCounters(data []byte) ([]Counter, error) {
	var doc struct {
		Nftables []map[string]any `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse nft json: %w", err)
	}
	var out []Counter
	for _, item := range doc.Nftables {
		ruleAny, ok := item["rule"]
		if !ok {
			continue
		}
		rule, ok := ruleAny.(map[string]any)
		if !ok {
			continue
		}
		if chain, _ := rule["chain"].(string); chain != "prerouting" {
			continue
		}
		exprs, _ := rule["expr"].([]any)
		var c Counter
		hasCounter := false
		for _, e := range exprs {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if mt, ok := m["match"].(map[string]any); ok {
				// proto match: left.meta.key=l4proto, right=tcp|udp
				if left, ok := mt["left"].(map[string]any); ok {
					if meta, ok := left["meta"].(map[string]any); ok {
						if key, _ := meta["key"].(string); key == "l4proto" {
							if right, ok := mt["right"].(string); ok {
								c.Proto = right
							}
						}
					}
					if pl, ok := left["payload"].(map[string]any); ok {
						if field, _ := pl["field"].(string); field == "dport" {
							switch v := mt["right"].(type) {
							case float64:
								c.ListenPort = int(v)
							case int:
								c.ListenPort = v
							}
							if proto, _ := pl["protocol"].(string); proto != "" && c.Proto == "" {
								c.Proto = proto
							}
						}
					}
				}
			}
			if ctr, ok := m["counter"].(map[string]any); ok {
				hasCounter = true
				if b, ok := ctr["bytes"].(float64); ok {
					c.Bytes = int64(b)
				}
				if p, ok := ctr["packets"].(float64); ok {
					c.Packets = int64(p)
				}
			}
		}
		if hasCounter && c.Proto != "" && c.ListenPort != 0 {
			out = append(out, c)
		}
	}
	return out, nil
}
