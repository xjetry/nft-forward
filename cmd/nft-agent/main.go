// nft-agent is the node-side binary: the nftables-controlling daemon (panel-
// managed via --connect, or standalone) plus the interactive TUI. It carries no
// panel/sqlite/web dependencies so it stays small and reproducible.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"nft-forward/internal/daemon"
	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/tui"
)

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
	)
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
	fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
	fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group（不存在时回落到默认 group）")
	fs.StringVar(&iface, "iface", "", "tc data-plane iface (auto-detect if empty)")
	fs.StringVar(&connectURL, "connect", "", "panel WebSocket URL (e.g. wss://panel/v1/agents); empty = tui/standalone mode")
	fs.StringVar(&panelTokenFile, "panel-token-file", "/etc/nft-forward/panel.token", "bearer token file (required when --connect is set)")
	if err := fs.Parse(args); err != nil {
		return 2
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

	cfg := daemon.Config{
		SocketPath: socketPath,
		StatePath:  statePath,
		GroupName:  groupName,
		Iface:      iface,
		ConnectURL: connectURL,
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
