package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nft-forward/internal/daemon"
	"nft-forward/internal/daemonclient"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/server"
	"nft-forward/internal/store"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/systemd"
	"nft-forward/internal/tui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			os.Exit(runDaemon(os.Args[2:]))
		case "server":
			os.Exit(runServer(os.Args[2:]))
		case "apply":
			os.Exit(runApplyCompat(os.Args[2:]))
		}
	}
	os.Exit(runTUI())
}

func runDaemon(args []string) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "nft-forward daemon 必须以 root 身份运行")
		return 1
	}

	var (
		socketPath string
		statePath  string
		groupName  string
		iface      string
		httpListen string
		tokenFile  string
	)
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket 路径")
	fs.StringVar(&statePath, "state", daemon.DefaultStatePath, "持久化 state 文件路径")
	fs.StringVar(&groupName, "group", daemon.DefaultGroupName, "socket 文件 group（不存在时回落到默认 group）")
	fs.StringVar(&iface, "iface", "", "tc data-plane iface (auto-detect if empty)")
	fs.StringVar(&httpListen, "listen", "", "additionally serve HTTP on this address for remote pushes")
	fs.StringVar(&tokenFile, "token-file", "/etc/nft-forward/daemon.token", "bearer token file (required when --listen is set)")
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
		HTTPListen: httpListen,
	}
	if httpListen != "" {
		cfg.TokenPath = tokenFile
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

func runServer(args []string) int {
	var (
		addr, dbPath, bootstrapPw  string
		resetAdminPw, resetAdminUser string
	)
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	fs.StringVar(&addr,         "addr",                    ":8080",                       "panel HTTP address")
	fs.StringVar(&dbPath,       "db",                      "/var/lib/nft-forward/panel.db", "SQLite database path")
	fs.StringVar(&bootstrapPw,  "bootstrap-admin-password","",                            "set admin password on first boot")
	fs.StringVar(&resetAdminPw, "reset-admin-password",    "",                            "reset admin password and exit")
	fs.StringVar(&resetAdminUser,"reset-admin-username",   "admin",                       "admin username for reset")
	fs.Parse(args)

	if resetAdminPw != "" {
		return runResetAdmin(dbPath, resetAdminUser, resetAdminPw)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := bootstrap(d, bootstrapPw); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	pusher := server.NewPusher(d)
	go pusher.Run()
	poller := server.NewPoller(d, pusher, 5*time.Second)
	go poller.Run()

	srv, err := server.New(d, pusher)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("nft-forward server listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	poller.Stop()
	pusher.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	return 0
}

func runApplyCompat(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	fs.Parse(args)

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "必须以 root 身份运行")
		return 1
	}

	client, err := daemonclient.New(daemonclient.DefaultSocketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon client creation failed:", err)
		return 1
	}

	// store.Load() uses NFT_FORWARD_CONFIG env var if set, otherwise /etc/nft-forward/rules.json
	rules, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载规则失败:", err)
		return 1
	}

	if err := client.PostRuleset("tui", rules); err != nil {
		fmt.Fprintln(os.Stderr, "post rules failed:", err)
		return 1
	}

	fmt.Printf("nft-forward: 已发送 %d 条规则到 daemon\n", len(rules))
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
	client, err := daemonclient.New(daemonclient.DefaultSocketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建 daemon 客户端失败:", err)
		return 1
	}
	if err := client.Health(); err != nil {
		fmt.Fprintln(os.Stderr, "无法连接 nft-forward daemon:", err)
		fmt.Fprintln(os.Stderr, "请先启动 daemon：sudo systemctl start nft-forward-daemon.service")
		fmt.Fprintln(os.Stderr, "或者临时：sudo nft-forward daemon")
		return 1
	}

	if err := tui.Run(client); err != nil {
		fmt.Fprintln(os.Stderr, "TUI 错误:", err)
		return 1
	}
	return 0
}

// runResetAdmin opens the DB without starting the panel and rewrites the
// password of a single admin account. All live sessions for that user are
// invalidated so any leaked cookie immediately stops working.
func runResetAdmin(dbPath, username, newPw string) int {
	d, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库:", err)
		return 1
	}
	defer d.Close()

	u, err := db.GetUserByUsername(d, username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "找不到用户 %q: %v\n", username, err)
		return 1
	}
	if u.Role != "admin" {
		fmt.Fprintf(os.Stderr, "用户 %q 角色为 %s，不是 admin；本命令只重置 admin 账号\n", username, u.Role)
		return 1
	}
	hash, err := server.HashPassword(newPw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "哈希失败:", err)
		return 1
	}
	if _, err := d.Exec(`UPDATE users SET pw_hash=?, disabled=0 WHERE id=?`, hash, u.ID); err != nil {
		fmt.Fprintln(os.Stderr, "写入失败:", err)
		return 1
	}
	_, _ = d.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID)
	db.WriteAudit(d, u.ID, "admin.reset_password_cli", username, "")
	fmt.Printf("已重置 %s 的密码（同时清空其所有活跃会话、解除禁用状态）\n", username)
	return 0
}

func bootstrap(d *sql.DB, pw string) error {
	n, err := db.CountUsers(d)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if pw == "" {
		pw = db.RandToken(8)
	}
	hash, err := server.HashPassword(pw)
	if err != nil {
		return err
	}
	if _, err := db.CreateUser(d, "admin", hash, "admin"); err != nil {
		return err
	}
	fmt.Println("================================================")
	fmt.Println(" 首次启动 - 已创建管理员账号")
	fmt.Println(" 用户名: admin")
	fmt.Println(" 密  码:", pw)
	fmt.Println(" 请妥善保存。可通过 --bootstrap-admin-password 自定义。")
	fmt.Println("================================================")
	return nil
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
