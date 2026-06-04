package main

import (
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
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/tui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			os.Exit(runDaemon(os.Args[2:]))
		case "server":
			os.Exit(runServer(os.Args[2:]))
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
	fs.StringVar(&connectURL, "connect", "", "panel WebSocket URL (e.g. wss://panel/v1/agents); empty = tui/server mode")
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

func runServer(args []string) int {
	var (
		addr, dbPath, bootstrapPw    string
		resetAdminPw, resetAdminUser string
	)
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	fs.StringVar(&addr, "addr", ":8080", "panel HTTP address")
	fs.StringVar(&dbPath, "db", "/var/lib/nft-forward/panel.db", "SQLite database path")
	fs.StringVar(&bootstrapPw, "bootstrap-admin-password", "", "set admin password on first boot")
	fs.StringVar(&resetAdminPw, "reset-admin-password", "", "reset admin password and exit")
	fs.StringVar(&resetAdminUser, "reset-admin-username", "admin", "admin username for reset")
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

	srv, err := server.New(d)
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
	srv.Hub.Close() // send StatusGoingAway to agents before tearing down
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
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

func runResetAdmin(dbPath, username, newPw string) int {
	d, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库:", err)
		return 1
	}
	defer d.Close()

	msg, err := server.ResetAdminPassword(d, username, newPw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(msg)
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
