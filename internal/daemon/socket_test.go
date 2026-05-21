package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// shortSockDir returns a temp directory whose abs path is short enough
// to host a unix socket on macOS (sun_path 104 byte limit). t.TempDir
// on macOS resolves under /var/folders/... which is long; we use /tmp
// to stay well below the limit on both macOS and Linux.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nftf-sock-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestListenSocket_CreatesSocketWith0660(t *testing.T) {
	sockPath := filepath.Join(shortSockDir(t), "test.sock")
	l, err := ListenSocket(sockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("perm = %v, want 0660", info.Mode().Perm())
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("file is not a socket: %v", info.Mode())
	}
}

func TestListenSocket_ReplacesStaleFile(t *testing.T) {
	sockPath := filepath.Join(shortSockDir(t), "test.sock")
	if err := os.WriteFile(sockPath, []byte("stale leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ListenSocket(sockPath, "")
	if err != nil {
		t.Fatalf("ListenSocket failed despite stale file: %v", err)
	}
	defer l.Close()
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatal("stale file was not replaced with a socket")
	}
}

func TestListenSocket_NonexistentGroupIsNotFatal(t *testing.T) {
	sockPath := filepath.Join(shortSockDir(t), "test.sock")
	l, err := ListenSocket(sockPath, "definitely-no-such-group-xyz")
	if err != nil {
		t.Fatalf("missing group should not error: %v", err)
	}
	defer l.Close()
	conn, dialErr := net.Dial("unix", sockPath)
	if dialErr != nil {
		t.Fatalf("dial failed: %v", dialErr)
	}
	conn.Close()
}
