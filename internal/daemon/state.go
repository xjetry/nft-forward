package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"nft-forward/internal/nft"
)

const stateSchemaVersion = 1

type stateFile struct {
	Version int        `json:"version"`
	Rules   []nft.Rule `json:"rules"`
}

// LoadState reads ruleset state from path. Missing file returns
// (nil, nil) so callers can treat first boot as empty state.
func LoadState(path string) ([]nft.Rule, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if sf.Version != stateSchemaVersion {
		return nil, fmt.Errorf("unsupported state version %d (want %d)", sf.Version, stateSchemaVersion)
	}
	return sf.Rules, nil
}

// SaveState writes the ruleset atomically at the filesystem-rename level:
// write to path+".tmp" first, then rename. A reader seeing path always
// observes a fully written file. On rename failure the temp file is
// removed best-effort so a retried call does not silently overwrite a
// stale leftover.
//
// This is not crash-safe at the OS level: no fsync is performed, so a
// system crash between WriteFile and the next page-cache flush can lose
// the latest contents. For daemon state this is acceptable — a crash
// either way means the kernel ruleset has to be reconciled on recovery.
func SaveState(path string, rules []nft.Rule) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	sf := stateFile{Version: stateSchemaVersion, Rules: rules}
	b, err := json.MarshalIndent(&sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
