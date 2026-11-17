package server

import (
	"testing"

	"nft-forward/internal/db"
)

func TestUpgradeForLabelSyncWhenShaMatches(t *testing.T) {
	art := &agentArtifact{Version: "v1.0.0", Arch: "arm64", SHA: "abc123", Data: []byte("agent-bytes")}

	// Node already on the target binary: label-only sync, no payload.
	match := upgradeFor(&db.Node{Machine: db.Machine{AgentSHA: "abc123"}}, art, "https://panel")
	if len(match.Data) != 0 || match.DownloadAt != "" {
		t.Fatalf("matched sha must be label-only, got data=%d downloadAt=%q", len(match.Data), match.DownloadAt)
	}
	if match.Version != "v1.0.0" || match.SHA256 != "abc123" {
		t.Fatalf("label-only must still carry version+sha: %+v", match)
	}

	// Different binary: full push with inline bytes + fallback URL.
	full := upgradeFor(&db.Node{Machine: db.Machine{AgentSHA: "stale"}}, art, "https://panel")
	if len(full.Data) == 0 || full.Size != int64(len(art.Data)) {
		t.Fatalf("mismatched sha must push bytes: %+v", full)
	}
	// 回退地址必须带上产物的架构，否则 serveBinary 无从返回同一份字节。
	if full.DownloadAt != "https://panel/v1/binary?arch=arm64" {
		t.Fatalf("fallback url = %q, want it to carry arch=arm64", full.DownloadAt)
	}

	// Legacy agent with no reported sha: must push (can't assume it matches).
	legacy := upgradeFor(&db.Node{Machine: db.Machine{AgentSHA: ""}}, art, "https://panel")
	if len(legacy.Data) == 0 {
		t.Fatalf("empty agent sha must push bytes, got label-only")
	}
}
