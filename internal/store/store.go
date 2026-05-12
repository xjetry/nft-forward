package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"nft-forward/internal/nft"
)

const DefaultPath = "/etc/nft-forward/rules.json"

func Path() string {
	if p := os.Getenv("NFT_FORWARD_CONFIG"); p != "" {
		return p
	}
	return DefaultPath
}

func Load() ([]nft.Rule, error) {
	path := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []nft.Rule{}, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return []nft.Rule{}, nil
	}
	var rules []nft.Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	return rules, nil
}

func Save(rules []nft.Rule) error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Proto != rules[j].Proto {
			return rules[i].Proto < rules[j].Proto
		}
		return rules[i].SrcPort < rules[j].SrcPort
	})
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
