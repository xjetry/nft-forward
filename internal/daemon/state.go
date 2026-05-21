package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"nft-forward/internal/nft"
)

// stateSchemaVersion is bumped whenever the on-disk layout changes in a way
// that requires migration. LoadState accepts older versions and converts in
// memory; SaveState always writes the current version.
const stateSchemaVersion = 2

// OwnerRuleset is the in-memory representation: each known controller
// ("tui", "panel", future additions) owns a slice of rules. The daemon
// merges all owners into one ruleset before calling Applier.Apply.
type OwnerRuleset map[string][]nft.Rule

// stateFile is the on-disk JSON layout for the current schema version.
// New fields go here; reading older versions converts into this shape.
type stateFile struct {
	Version int          `json:"version"`
	Owners  OwnerRuleset `json:"owners"`
}

// legacyV1File is the pre-v2 on-disk layout where rules were stored as a
// single flat slice keyed by "rules". We keep this type defined purely
// to recognize and migrate it; we do not write v1 anymore.
type legacyV1File struct {
	Version int        `json:"version"`
	Rules   []nft.Rule `json:"rules"`
}

// LoadState reads ruleset state from path. Missing file returns an empty
// OwnerRuleset (not nil, so callers can range / index without nil checks).
// v1 files are read transparently and exposed as a single "tui" segment —
// in practice v1 only ever contained rules submitted through the bare
// /v1/ruleset endpoint, which was used by manual smoke tests; assigning
// them to "tui" preserves the data without forcing users to re-submit.
func LoadState(path string) (OwnerRuleset, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return OwnerRuleset{}, nil
	}
	if err != nil {
		return nil, err
	}

	// Peek at the version field first so we don't decode v1 into a v2 shape
	// (which would silently drop the "rules" field).
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("parse state version: %w", err)
	}

	switch probe.Version {
	case stateSchemaVersion:
		var sf stateFile
		if err := json.Unmarshal(b, &sf); err != nil {
			return nil, fmt.Errorf("parse v%d state: %w", stateSchemaVersion, err)
		}
		if sf.Owners == nil {
			sf.Owners = OwnerRuleset{}
		}
		return sf.Owners, nil
	case 1:
		var v1 legacyV1File
		if err := json.Unmarshal(b, &v1); err != nil {
			return nil, fmt.Errorf("parse v1 state: %w", err)
		}
		out := OwnerRuleset{}
		if len(v1.Rules) > 0 {
			out["tui"] = v1.Rules
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported state version %d (want %d or 1)", probe.Version, stateSchemaVersion)
	}
}

// SaveState writes owners atomically at the filesystem-rename level: write
// to path+".tmp" first, then rename. A reader seeing path always observes a
// fully written file. On rename failure the temp file is removed best-effort
// so a retried call does not silently overwrite a stale leftover.
//
// This is not crash-safe at the OS level: no fsync is performed, so a
// system crash between WriteFile and the next page-cache flush can lose
// the latest contents. For daemon state this is acceptable — a crash
// either way means the kernel ruleset has to be reconciled on recovery.
func SaveState(path string, owners OwnerRuleset) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if owners == nil {
		owners = OwnerRuleset{}
	}
	sf := stateFile{Version: stateSchemaVersion, Owners: owners}
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
