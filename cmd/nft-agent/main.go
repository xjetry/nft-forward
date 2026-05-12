package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"nft-forward/internal/agent"
	"nft-forward/internal/nft"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/tc"
)

func main() {
	var (
		listen       string
		tokenFile    string
		tokenVal     string
		statePath    string
		iface        string
		skipNftCheck bool
	)
	flag.StringVar(&listen, "listen", ":7878", "HTTP 监听地址（如 :7878、192.168.1.10:7878）")
	flag.StringVar(&iface, "iface", "", "数据面网卡（用于 tc HTB 限速；为空则尝试从默认路由自动检测）")
	flag.StringVar(&tokenFile, "token-file", "/etc/nft-forward/agent.token", "bearer token 文件路径")
	flag.StringVar(&tokenVal, "token", "", "bearer token（优先级高于 token-file，仅供调试）")
	flag.StringVar(&statePath, "state", "/var/lib/nft-forward/agent-state.json", "本地缓存的最近 ruleset 路径，用于开机恢复")
	flag.BoolVar(&skipNftCheck, "skip-nft-check", false, "跳过 nft 与 ip_forward 启动检查（仅供调试）")
	flag.Parse()

	if os.Geteuid() != 0 {
		log.Fatal("nft-agent 必须以 root 身份运行")
	}

	if !skipNftCheck {
		if err := sysdeps.Ensure("nftables", "iproute2"); err != nil {
			log.Fatal(err)
		}
		if !nft.Available() {
			log.Fatal("nft 命令不可用，请先安装 nftables")
		}
		if err := nft.Probe(); err != nil {
			log.Fatalf("nft 检测失败: %v", err)
		}
		if !nft.IPForwardEnabled() {
			log.Println("启用 net.ipv4.ip_forward")
			if err := nft.EnableIPForward(); err != nil {
				log.Fatalf("启用 ip_forward 失败: %v", err)
			}
		}
	}

	token := strings.TrimSpace(tokenVal)
	if token == "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			log.Fatalf("读取 token 文件 %s 失败: %v", tokenFile, err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		log.Fatal("token 为空")
	}

	if iface == "" {
		iface = tc.DefaultIface()
		if iface == "" {
			iface = "eth0"
		}
	}
	if !tc.Available() {
		log.Println("警告: 未找到 tc 命令，bandwidth 限速将无法生效")
	} else {
		log.Printf("数据面网卡: %s", iface)
	}

	a := agent.New(agent.Config{
		Listen:    listen,
		Token:     token,
		StatePath: statePath,
		Iface:     iface,
	})

	if err := a.Bootstrap(); err != nil {
		log.Fatalf("启动 bootstrap 失败: %v", err)
	}
	fmt.Fprintln(os.Stderr, "nft-agent 启动完成，等待 panel 推送")
	if err := a.Serve(); err != nil {
		log.Fatal(err)
	}
}
