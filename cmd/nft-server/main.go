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
	"syscall"
	"time"

	"nft-forward/internal/agent"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/server"
	"nft-forward/internal/sysdeps"
	"nft-forward/internal/tc"
)

func main() {
	var (
		addr            string
		dbPath          string
		bootstrapPw     string
		agentIface      string
		resetAdminPw    string
		resetAdminUser  string
	)
	flag.StringVar(&addr, "addr", ":8080", "面板 HTTP 监听地址")
	flag.StringVar(&dbPath, "db", "/var/lib/nft-forward/panel.db", "SQLite 数据库路径")
	flag.StringVar(&bootstrapPw, "bootstrap-admin-password", "", "首次启动给 admin 设置密码（否则随机生成并打印到 stdout）")
	flag.StringVar(&agentIface, "agent-iface", "", "内嵌 agent 的数据面网卡（留空则自动从默认路由检测）")
	flag.StringVar(&resetAdminPw, "reset-admin-password", "", "把指定 admin 账号的密码重置为该值后退出；密码忘了时用")
	flag.StringVar(&resetAdminUser, "reset-admin-username", "admin", "搭配 --reset-admin-password 指定账号名，默认 admin")
	flag.Parse()

	if resetAdminPw != "" {
		os.Exit(runResetAdmin(dbPath, resetAdminUser, resetAdminPw))
	}

	d, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库: %v", err)
	}
	defer d.Close()

	if err := bootstrap(d, bootstrapPw); err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	// The panel always carries an embedded agent for the host it runs on.
	// The agent runs in-process (no network port); pusher/poller call it
	// directly via Go method calls.
	embedded, err := startEmbeddedAgent(d, agentIface)
	if err != nil {
		log.Fatalf("内嵌 agent: %v", err)
	}

	pusher := server.NewPusher(d, embedded)
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
		log.Printf("nft-server listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	poller.Stop()
	pusher.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// startEmbeddedAgent registers the local host as a "localhost" node and
// returns an *agent.Agent the panel can call in-process. No network socket
// is opened — apply/counters are invoked directly via method calls.
func startEmbeddedAgent(d *sql.DB, iface string) (*agent.Agent, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("nft-server 必须以 root 运行（内嵌 agent 需要 nftables/tc 权限）")
	}
	if err := sysdeps.Ensure("nftables", "iproute2"); err != nil {
		return nil, err
	}
	if !nft.Available() {
		return nil, fmt.Errorf("未找到 nft 命令")
	}
	if err := nft.Probe(); err != nil {
		return nil, err
	}
	if !nft.IPForwardEnabled() {
		log.Println("内嵌 agent: 启用 net.ipv4.ip_forward")
		if err := nft.EnableIPForward(); err != nil {
			return nil, err
		}
	}
	if iface == "" {
		iface = tc.DefaultIface()
		if iface == "" {
			iface = "eth0"
		}
	}

	// Address is a sentinel scheme — pusher/poller recognise it and call the
	// embedded agent directly instead of dialing HTTP.
	const localAddr = "local://"

	var nodeID int64
	row := d.QueryRow(`SELECT id FROM nodes WHERE name='localhost'`)
	if err := row.Scan(&nodeID); err != nil {
		// Local node carries no secret because nobody dials it over the wire.
		// We still seed a placeholder so the schema's NOT NULL holds.
		_, err := db.CreateNode(d, "localhost", localAddr, "in-process")
		if err != nil {
			return nil, fmt.Errorf("seed localhost node: %w", err)
		}
		log.Println("内嵌 agent: 已自动注册 localhost 节点")
	} else {
		_, _ = d.Exec(`UPDATE nodes SET address=? WHERE id=?`, localAddr, nodeID)
	}

	a := agent.New(agent.Config{
		Token:     "in-process",
		StatePath: "/var/lib/nft-forward/embedded-agent-state.json",
		Iface:     iface,
	})
	if err := a.Bootstrap(); err != nil {
		return nil, fmt.Errorf("内嵌 agent bootstrap: %w", err)
	}
	log.Printf("内嵌 agent 就绪 (iface=%s)", iface)
	return a, nil
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
