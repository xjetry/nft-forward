package systemd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	ServiceName  = "nft-forward.service"
	ServiceUnit  = "/etc/systemd/system/" + ServiceName
	InstalledBin = "/usr/local/sbin/nft-forward"
)

func unitContent() string {
	// nftables.service loads /etc/nftables.conf which typically starts with
	// `flush ruleset`. We must run after it, otherwise our table is wiped
	// right after we restore it.
	return fmt.Sprintf(`[Unit]
Description=nft-forward 开机恢复已保存的端口转发规则
After=network-pre.target nftables.service
Wants=network-pre.target

[Service]
Type=oneshot
ExecStart=%s --apply
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`, InstalledBin)
}

func Installed() bool {
	_, err := os.Stat(ServiceUnit)
	return err == nil
}

func Install() error {
	src, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}

	if src != InstalledBin {
		if err := copyBinary(src, InstalledBin); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
	}

	if err := os.WriteFile(ServiceUnit, []byte(unitContent()), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", ServiceName); err != nil {
		return err
	}
	return nil
}

func Uninstall() error {
	_ = run("systemctl", "disable", ServiceName)
	_ = run("systemctl", "stop", ServiceName)
	if err := os.Remove(ServiceUnit); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return nil
}

func copyBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %v: %s", name, args, err, stderr.String())
	}
	return nil
}
