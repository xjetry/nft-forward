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
	log.Printf("upgrade: received version=%s sha256=%s size=%d data=%d from=%s",
		u.Version, u.SHA256, u.Size, len(u.Data), u.DownloadAt)

	// Label-only sync: the panel sends no binary when the running agent's sha
	// already matches the target. Confirm against our own binary (the panel's
	// view may be stale) and, if it holds, just record the new version label —
	// no replace, no restart. On mismatch reject so the panel re-pushes the
	// bytes.
	if len(u.Data) == 0 && u.DownloadAt == "" {
		if u.SHA256 != "" && u.SHA256 == agentSHA() {
			writeAgentIdentity(u.Version, u.SHA256)
			log.Printf("upgrade: already on sha %s, recorded version=%s — restarting to pick up label", u.SHA256, u.Version)
			go restartSelf()
			return wsproto.UpgradeAck{OK: true}
		}
		return wsproto.UpgradeAck{Error: "binary sha mismatch; full push required"}
	}

	binary, err := upgradeBinary(u)
	if err != nil {
		log.Printf("upgrade: obtain binary failed: %v", err)
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
	// Persist the new identity before restart so the relaunched (reproducible)
	// binary reports the right version label, which it cannot self-derive.
	writeAgentIdentity(u.Version, u.SHA256)
	log.Printf("upgrade: binary replaced at %s (%d bytes), scheduling restart", exePath, len(binary))

	go restartSelf()

	return wsproto.UpgradeAck{OK: true}
}

// upgradeBinary returns the new binary for u: the inline Data (sha-verified)
// when present, otherwise an HTTP download from u.DownloadAt. Inline transport
// lets nodes that cannot reach the panel over HTTP still upgrade over the WS
// link; the download path stays for daemons reached by an older panel.
func upgradeBinary(u wsproto.Upgrade) ([]byte, error) {
	if len(u.Data) > 0 {
		sum := sha256.Sum256(u.Data)
		if got := hex.EncodeToString(sum[:]); got != u.SHA256 {
			return nil, fmt.Errorf("sha256 mismatch: got %s, want %s", got, u.SHA256)
		}
		return u.Data, nil
	}
	return downloadBinary(u)
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
