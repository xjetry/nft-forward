package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// nft-agent is built reproducibly (no VCS stamping), so the version does not
// live in the binary. It is a label maintained by install.sh (on install) and
// the panel (on push) in agentVersionFile; the binary's identity is its sha256.
const (
	agentVersionFile = "/etc/nft-forward/agent.version"
	agentSHAFile     = "/etc/nft-forward/agent.sha"
)

// agentVersion returns the version label for this agent: the label file, then
// build info, then "dev".
func agentVersion() string {
	if b, err := os.ReadFile(agentVersionFile); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// agentSHA returns the sha256 of the running binary — the authoritative identity
// the panel compares against the agent it would push. Self-computed so a stale
// agentSHAFile cannot lie; the file is refreshed to match when they diverge.
// Returns "" if the executable cannot be read.
func agentSHA() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if cur, _ := os.ReadFile(agentSHAFile); strings.TrimSpace(string(cur)) != sha {
		_ = os.MkdirAll(filepath.Dir(agentSHAFile), 0o755)
		_ = os.WriteFile(agentSHAFile, []byte(sha+"\n"), 0o644)
	}
	return sha
}

// writeAgentIdentity persists the version label and binary sha after an upgrade
// so the restarted process reports the new identity (the label especially,
// which a reproducible binary cannot self-derive).
func writeAgentIdentity(version, sha string) {
	_ = os.MkdirAll(filepath.Dir(agentVersionFile), 0o755)
	if version != "" {
		_ = os.WriteFile(agentVersionFile, []byte(version+"\n"), 0o644)
	}
	if sha != "" {
		_ = os.WriteFile(agentSHAFile, []byte(sha+"\n"), 0o644)
	}
}
