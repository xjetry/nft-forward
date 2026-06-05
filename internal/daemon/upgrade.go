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
	"strings"
	"time"

	"nft-forward/internal/wsproto"
)

func (d *Dialer) handleUpgrade(ctx context.Context, u wsproto.Upgrade) wsproto.UpgradeAck {
	log.Printf("upgrade: received version=%s sha256=%s size=%d from=%s",
		u.Version, u.SHA256, u.Size, u.DownloadAt)

	binary, err := downloadBinary(u)
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
	log.Printf("upgrade: binary replaced at %s (%d bytes), scheduling restart", exePath, len(binary))

	go restartSelf()

	return wsproto.UpgradeAck{OK: true}
}

func downloadBinary(u wsproto.Upgrade) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.DownloadAt, nil)
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
	log.Printf("upgrade: downloaded %d bytes", len(data))

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

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func restartSelf() {
	time.Sleep(time.Second)
	unit := detectUnit()
	log.Printf("upgrade: restarting unit %s", unit)
	if out, err := exec.Command(
		"systemd-run", "--no-block", "--",
		"systemctl", "restart", unit,
	).CombinedOutput(); err != nil {
		log.Printf("upgrade: systemd-run restart failed: %v: %s — trying direct restart", err, out)
		exec.Command("systemctl", "restart", unit).Start()
	}
}

func detectUnit() string {
	pid := os.Getpid()
	out, err := exec.Command("systemctl", "--pid", fmt.Sprintf("%d", pid), "--no-pager", "-l", "--plain", "--output=short").CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nft-forward") && strings.HasSuffix(line, ".service") {
				return strings.Fields(line)[0]
			}
		}
	}
	for _, name := range []string{"nft-forward-daemon", "nft-forward"} {
		if out, err := exec.Command("systemctl", "is-active", name+".service").CombinedOutput(); err == nil && strings.TrimSpace(string(out)) == "active" {
			return name
		}
	}
	return "nft-forward"
}
