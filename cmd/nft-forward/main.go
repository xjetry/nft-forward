package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"nft-forward/internal/nft"
	"nft-forward/internal/store"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/systemd"
	"nft-forward/internal/tui"
)

func main() {
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

