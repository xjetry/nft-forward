// nft-agent is the node-side binary: the nftables-controlling daemon (panel-
// managed via --connect, or standalone) plus the interactive TUI. It carries no
// panel/sqlite/web dependencies so it stays small and reproducible.
package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"nft-forward/internal/daemon"
	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/tui"
)

// validateConnectURL rejects a plaintext ws:// control channel unless the
// operator explicitly opts in. The panel↔agent link carries root-level upgrade
// frames, so a MITM on ws:// can push an arbitrary binary and gain RCE — wss://
// is mandatory in production.
func validateConnectURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("--connect 地址无法解析: %v", err)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if allowInsecure {
			fmt.Fprintln(os.Stderr, "警告: 使用明文 ws:// 控制信道，存在中间人注入升级帧的风险，仅限本地测试")
			return nil
		}
		return fmt.Errorf("--connect 必须使用 wss://（明文 ws:// 可被中间人注入升级帧实现 RCE）；如确为本地测试，请加 --insecure-connect")
	default:
		return fmt.Errorf("--connect 协议必须是 wss://（当前 %q）", u.Scheme)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Exit(runDaemon(os.Args[2:]))
	}
	os.Exit(runTUI())
}

func runDaemon(args []string) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "nft-agent daemon 必须以 root 身份运行")
		return 1
	}

	var (
		socketPath     string
		statePath      string
		groupName      string
		iface          string
		connectURL     string
		panelTokenFile string
		portRange      string
		relayHost      string
		relayHostV6    string
		allowInsecure  bool
		serverLocal    bool
	)
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
	fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
	fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group（不存在时回落到默认 group）")
	fs.StringVar(&iface, "iface", "", "tc data-plane iface (auto-detect if empty)")
	fs.StringVar(&connectURL, "connect", "", "panel WebSocket URL (e.g. wss://panel/v1/agents); empty = tui/standalone mode")
	fs.StringVar(&panelTokenFile, "panel-token-file", "/etc/nft-forward/panel.token", "bearer token file (required when --connect is set)")
	fs.StringVar(&portRange, "port-range", "", "端口范围（如 10001-20000），上报给面板")
	fs.StringVar(&relayHost, "relay-host", "", "显式声明数据面 IPv4 地址/域名，覆盖面板的自动识别（用于双出口等场景）")
	fs.StringVar(&relayHostV6, "relay-host-v6", "", "显式声明数据面 IPv6 地址，覆盖面板的自动识别")
	fs.BoolVar(&allowInsecure, "insecure-connect", false, "允许明文 ws:// 控制信道（仅本地测试；生产必须用 wss://）")
	fs.BoolVar(&serverLocal, "server-local", false, "标记本机为面板宿主自身节点：面板经本地 socket 推送规则，重启时保留 panel 段不降级为 tui")
	if err := fs.Parse(args); err != nil {
		// Tolerate unknown flags so a binary upgrade that predates a
		// newly-added install.sh flag doesn't crash the daemon.
		known := filterKnownArgs(fs, args)
		if err2 := fs.Parse(known); err2 != nil {
			fmt.Fprintln(os.Stderr, err2)
			return 2
		}
		fmt.Fprintf(os.Stderr, "警告: 忽略了部分未识别的命令行参数\n")
	}

	if err := sysdeps.Ensure("nftables"); err != nil {
		fmt.Fprintln(os.Stderr, "依赖检查失败:", err)
		return 1
	}
	if !nft.Available() {
		fmt.Fprintln(os.Stderr, "nft 命令不可用，请先安装 nftables")
		return 1
	}
	if err := nft.Probe(); err != nil {
		fmt.Fprintln(os.Stderr, "nft 检测失败:", err)
		return 1
	}
	if !nft.IPForwardEnabled() {
		if err := nft.EnableIPForward(); err != nil {
			fmt.Fprintln(os.Stderr, "启用 ip_forward 失败:", err)
			return 1
		}
	}

	if connectURL != "" {
		if err := validateConnectURL(connectURL, allowInsecure); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	cfg := daemon.Config{
		SocketPath:          socketPath,
		StatePath:           statePath,
		GroupName:           groupName,
		Iface:               iface,
		ConnectURL:          connectURL,
		PortRange:           portRange,
		DeclaredRelayHost:   relayHost,
		DeclaredRelayHostV6: relayHostV6,
		ServerLocal:         serverLocal,
	}
	if connectURL != "" {
		tok, err := os.ReadFile(panelTokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取 panel token:", err)
			return 1
		}
		cfg.ConnectToken = strings.TrimSpace(string(tok))
		if cfg.ConnectToken == "" {
			fmt.Fprintln(os.Stderr, "panel token 文件为空")
			return 1
		}
	}
	d, err := daemon.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon 构造失败:", err)
		return 1
	}
	if err := d.RunWithSignals(); err != nil {
		fmt.Fprintln(os.Stderr, "daemon 运行失败:", err)
		return 1
	}
	return 0
}

func runTUI() int {
	client, err := daemonclient.New(daemonclient.DefaultSocketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建 daemon 客户端失败:", err)
		return 1
	}
	if err := client.Health(); err != nil {
		fmt.Fprintln(os.Stderr, "无法连接 nft-forward daemon:", err)
		fmt.Fprintln(os.Stderr, "请先启动 daemon：sudo systemctl start nft-forward-daemon.service")
		fmt.Fprintln(os.Stderr, "或者临时：sudo nft-agent daemon")
		return 1
	}

	if err := tui.Run(client); err != nil {
		fmt.Fprintln(os.Stderr, "TUI 错误:", err)
		return 1
	}
	return 0
}

// filterKnownArgs strips flags the FlagSet doesn't recognise so an older
// binary can start even when the systemd unit carries flags from a newer
// install.sh.
func filterKnownArgs(fs *flag.FlagSet, args []string) []string {
	known := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) { known[f.Name] = true })
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if known[name] {
			out = append(out, a)
			continue
		}
		// Unknown flag: skip its value arg if present (--foo bar form).
		if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
		}
	}
	return out
}
