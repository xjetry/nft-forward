package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"nft-forward/internal/daemon"
	"nft-forward/internal/nft"
	"nft-forward/internal/store"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/systemd"
	"nft-forward/internal/tui"
)

func main() {
	// Subcommand dispatch must precede flag.Parse() so the global flag set
	// does not try to consume subcommand-specific args.
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		os.Exit(runDaemon())
	}

	var (
		applyOnly  bool
		uninstall  bool
		installSvc bool
	)
	flag.BoolVar(&applyOnly, "apply", false, "加载 rules.json 并应用到内核后退出（开机由 systemd 调用）")
	flag.BoolVar(&installSvc, "install-service", false, "安装 systemd 单元以实现开机持久化后退出")
	flag.BoolVar(&uninstall, "uninstall-service", false, "卸载 systemd 持久化单元后退出")
	flag.Parse()

	switch {
	case applyOnly:
		os.Exit(runApply())
	case installSvc:
		os.Exit(runInstallService())
	case uninstall:
		os.Exit(runUninstall())
	default:
		os.Exit(runTUI())
	}
}

func runDaemon() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "nft-forward daemon 必须以 root 身份运行")
		return 1
	}

	var (
		socketPath string
		statePath  string
		groupName  string
	)
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
	fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
	fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group（不存在时回落到默认 group）")
	if err := fs.Parse(os.Args[1:]); err != nil {
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

	d := daemon.New(daemon.Config{
		SocketPath: socketPath,
		StatePath:  statePath,
		GroupName:  groupName,
	})
	if err := d.RunWithSignals(); err != nil {
		fmt.Fprintln(os.Stderr, "daemon 运行失败:", err)
		return 1
	}
	return 0
}

func runApply() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "必须以 root 身份运行")
		return 1
	}
	if !nft.Available() {
		fmt.Fprintln(os.Stderr, "未找到 nft 命令")
		return 1
	}
	rules, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载规则失败:", err)
		return 1
	}
	if err := nft.Apply(rules); err != nil {
		fmt.Fprintln(os.Stderr, "应用规则失败:", err)
		return 1
	}
	fmt.Printf("nft-forward: 已应用 %d 条规则\n", len(rules))
	return 0
}

func runInstallService() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "必须以 root 身份运行")
		return 1
	}
	if err := systemd.Install(); err != nil {
		fmt.Fprintln(os.Stderr, "安装失败:", err)
		return 1
	}
	fmt.Println("已安装 systemd 单元 nft-forward.service；规则将在开机时自动恢复")
	return 0
}

func runUninstall() int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "必须以 root 身份运行")
		return 1
	}
	if err := systemd.Uninstall(); err != nil {
		fmt.Fprintln(os.Stderr, "卸载失败:", err)
		return 1
	}
	fmt.Println("已移除 systemd 单元；rules.json 与当前内核规则保持不变")
	return 0
}

func runTUI() int {
	if err := preflight(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}

	if !systemd.Installed() {
		if promptPersist() {
			if err := systemd.Install(); err != nil {
				fmt.Fprintln(os.Stderr, "安装持久化服务失败:", err)
				return 1
			}
			fmt.Println("已启用开机持久化：systemd 单元 nft-forward.service")
		}
	}

	rules, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载规则失败:", err)
		return 1
	}
	if err := nft.Apply(rules); err != nil {
		fmt.Fprintln(os.Stderr, "应用规则失败:", err)
		return 1
	}

	if err := tui.Run(rules); err != nil {
		fmt.Fprintln(os.Stderr, "TUI 错误:", err)
		return 1
	}
	return 0
}

func preflight() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("必须以 root 身份运行（尝试: sudo %s）", os.Args[0])
	}

	if err := sysdeps.Ensure("nftables"); err != nil {
		return err
	}

	if err := nft.Probe(); err != nil {
		return err
	}

	if !nft.IPForwardEnabled() {
		fmt.Println("net.ipv4.ip_forward 未启用，正在启用...")
		if err := nft.EnableIPForward(); err != nil {
			return fmt.Errorf("启用 ip_forward 失败: %w", err)
		}
	}
	return nil
}

func promptPersist() bool {
	fmt.Println("尚未配置开机持久化。")
	fmt.Println("启用后将把本程序复制到 /usr/local/sbin/nft-forward，")
	fmt.Println("并注册 systemd 单元，使保存的规则在每次开机时自动恢复。")
	return promptYes("现在启用持久化？[Y/n]: ", true)
}

func promptYes(prompt string, defaultYes bool) bool {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return defaultYes
	}
	return ans == "y" || ans == "yes"
}
