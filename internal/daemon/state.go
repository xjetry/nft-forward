package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"nft-forward/internal/nft"
)

// stateSchemaVersion 是状态文件的版本号，磁盘上的布局变了就加一。LoadState 只读这
// 一版，SaveState 只写这一版。
const stateSchemaVersion = 4

// OwnerRuleset 是按归属段分开的规则集，内存里与 state.json 里都是这个形状。键只有
// 两个：segLocal 与 segPanel（见 owners.go）。daemon 在调 dp.Reconcile 之前把各段
// 合成一个规则集（MergedRuleset）。
type OwnerRuleset map[string][]nft.Rule

// stateFile 是状态文件在磁盘上的 JSON 布局。
type stateFile struct {
	Version   int          `json:"version"`
	Owners    OwnerRuleset `json:"owners"`
	AgentMeta AgentMeta    `json:"agent_meta"`
}

// LoadState 从 path 读出规则集与 agent 元数据。文件不存在时返回空的 OwnerRuleset
// （不是 nil，调用方可以直接遍历、取下标）。
//
// 只读 stateSchemaVersion 这一版，别的版本一律报错，不做转换；第 1–3 版是 v0.5.0
// 之前的版本写下的。报错而不是当成空状态：Bootstrap 拿到空状态会带着空规则集起来，
// 之后的第一次 SaveState 就把原文件盖掉。
func LoadState(path string) (OwnerRuleset, AgentMeta, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return OwnerRuleset{}, AgentMeta{}, nil
	}
	if err != nil {
		return nil, AgentMeta{}, err
	}

	// 先只看版本号：别的版本的布局不同，直接按当前布局解出来的内容不可信。
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, AgentMeta{}, fmt.Errorf("parse state version: %w", err)
	}
	if probe.Version != stateSchemaVersion {
		return nil, AgentMeta{}, fmt.Errorf(
			"state file %s is version %d, and only version %d is read (versions 1 to 3 were written by releases before v0.5.0)",
			path, probe.Version, stateSchemaVersion)
	}

	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, AgentMeta{}, fmt.Errorf("parse v%d state: %w", stateSchemaVersion, err)
	}
	if sf.Owners == nil {
		sf.Owners = OwnerRuleset{}
	}
	return sf.Owners, sf.AgentMeta, nil
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
func SaveState(path string, owners OwnerRuleset, meta AgentMeta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if owners == nil {
		owners = OwnerRuleset{}
	}
	sf := stateFile{Version: stateSchemaVersion, Owners: owners, AgentMeta: meta}
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
