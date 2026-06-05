package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"nft-forward/internal/wsproto"
)

func (d *Dialer) handleUpgrade(ctx context.Context, u wsproto.Upgrade) wsproto.UpgradeAck {
	log.Printf("upgrade: received version=%s sha256=%s size=%d from=%s",
		u.Version, u.SHA256, u.Size, u.DownloadAt)

	binary, err := downloadBinary(ctx, u)
	if err != nil {
		log.Printf("upgrade: download failed: %v", err)
		return wsproto.UpgradeAck{Error: err.Error()}
	}

	exePath, err := os.Executable()
	if err != nil {
		return wsproto.UpgradeAck{Error: "os.Executable: " + err.Error()}
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return wsproto.UpgradeAck{Error: "resolve symlink: " + err.Error()}
	}

	if err := atomicReplace(exePath, binary); err != nil {
		return wsproto.UpgradeAck{Error: "replace binary: " + err.Error()}
	}
	log.Printf("upgrade: binary replaced at %s, restarting service", exePath)

	go func() {
		time.Sleep(500 * time.Millisecond)
		restartSelf()
	}()

	return wsproto.UpgradeAck{OK: true}
}

func downloadBinary(ctx context.Context, u wsproto.Upgrade) ([]byte, error) {
	dlCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	req, err := http.NewRequestWithContext(dlCtx, "GET", u.DownloadAt, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u.DownloadAt, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", u.DownloadAt, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, u.Size+1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	h := sha256.Sum256(data)
	got := hex.EncodeToString(h[:])
	if got != u.SHA256 {
		return nil, fmt.Errorf("sha256 mismatch: got %s, want %s", got, u.SHA256)
	}
	return data, nil
}

func atomicReplace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nft-forward-upgrade-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func restartSelf() {
	if out, err := exec.Command("systemctl", "restart", "nft-forward").CombinedOutput(); err != nil {
		log.Printf("upgrade: systemctl restart failed: %v: %s", err, out)
	}
}
