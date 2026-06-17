package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"nft-forward/internal/wsproto"
)

func TestUpgradeBinaryFromData(t *testing.T) {
	payload := []byte("hello-binary")
	sum := sha256.Sum256(payload)
	good := wsproto.Upgrade{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Data: payload}
	got, err := upgradeBinary(good)
	if err != nil {
		t.Fatalf("good data: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}

	bad := wsproto.Upgrade{SHA256: "deadbeef", Size: int64(len(payload)), Data: payload}
	if _, err := upgradeBinary(bad); err == nil {
		t.Fatal("sha mismatch should error")
	}
}
