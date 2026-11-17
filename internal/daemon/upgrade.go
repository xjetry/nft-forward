package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"debug/elf"
	"encoding/hex"
	"errors"
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

	cur, err := readELFHeader(runningExe)
	if err != nil {
		return wsproto.UpgradeAck{Error: "read running binary: " + err.Error()}
	}
	if err := checkUpgradeTarget(binary, cur); err != nil {
		log.Printf("upgrade: rejected: %v", err)
		return wsproto.UpgradeAck{Error: err.Error()}
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

// runningExe 是内核为本进程记录的可执行镜像，即启动时 exec 的那个文件。它跟着
// 镜像走而不是跟着路径走：磁盘上同一路径的文件被换掉后（进程尚未重启），按路径
// 读到的是新文件，经它读到的仍是正在运行的这一份。
const runningExe = "/proc/self/exe"

// elfHeaderLen 覆盖 e_ident（16 字节）、e_type（2 字节）与 e_machine（2 字节）；
// 这一段的布局在 ELF32 与 ELF64 里相同。
const elfHeaderLen = 20

// elfTarget 是 ELF 头里决定“这个可执行文件能在哪种机器上跑”的三个字段。
type elfTarget struct {
	class   elf.Class
	data    elf.Data
	machine elf.Machine
}

func (t elfTarget) String() string {
	return fmt.Sprintf("%s/%s/%s", t.machine, t.class, t.data)
}

// parseELFTarget 只读定长头部，不解析节表与程序头表：判定目标机器用不到它们，
// 而 UPX 压缩后的 nft-agent 没有节表，不该让校验结果取决于完整解析器对这类
// 文件的容忍度。
func parseELFTarget(b []byte) (elfTarget, error) {
	if len(b) < elfHeaderLen || string(b[:len(elf.ELFMAG)]) != elf.ELFMAG {
		return elfTarget{}, errors.New("not an ELF executable")
	}
	t := elfTarget{class: elf.Class(b[elf.EI_CLASS]), data: elf.Data(b[elf.EI_DATA])}
	switch t.data {
	case elf.ELFDATA2LSB:
		t.machine = elf.Machine(uint16(b[18]) | uint16(b[19])<<8)
	case elf.ELFDATA2MSB:
		t.machine = elf.Machine(uint16(b[18])<<8 | uint16(b[19]))
	default:
		return elfTarget{}, fmt.Errorf("unknown ELF data encoding %d", b[elf.EI_DATA])
	}
	return t, nil
}

func readELFHeader(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := make([]byte, elfHeaderLen)
	if _, err := io.ReadFull(f, h); err != nil {
		return nil, err
	}
	return h, nil
}

// checkUpgradeTarget 拒绝 ELF 目标（机器类型 / 位宽 / 字节序）与本进程正在运行的
// 可执行镜像不一致的升级包。curHeader 是该镜像的头部（readELFHeader(runningExe)）。
//
// 不变式：装上去的二进制必须能在本机执行。handleUpgrade 在 atomicReplace 之后
// 调 restartSelf，由 systemd 用新文件重启本单元；新文件若不是本机架构，内核会以
// exec format error 拒绝执行它（除非主机为那种架构注册了 binfmt 模拟器），daemon
// 起不来，也就没有进程能再接收一次推送来纠正——只能人工登录重装。
//
// 上游的 sha256 校验（upgradeBinary）保证不了这一点：它证明的是“字节与面板声称
// 的一致”，不是“面板挑对了架构”。不按架构选产物的面板版本只持有 amd64 的
// nft-agent，对 sha 不同的节点一律推它，arm64 节点也不例外。
//
// 基准取正在运行的镜像：它此刻就在本机上跑着，它的目标必然是本机能执行的。
// 不取 exePath 处的磁盘文件，因为那个文件可能已被换成别的东西而进程还没重启；
// 也不取 runtime.GOARCH，那样要另外维护一张 GOARCH → e_machine 对照表。
func checkUpgradeTarget(newBin, curHeader []byte) error {
	cur, err := parseELFTarget(curHeader)
	if err != nil {
		return fmt.Errorf("running binary: %w", err)
	}
	next, err := parseELFTarget(newBin)
	if err != nil {
		return fmt.Errorf("upgrade binary: %w", err)
	}
	if next != cur {
		return fmt.Errorf("upgrade binary targets %s but this host runs %s; refusing to install", next, cur)
	}
	return nil
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
