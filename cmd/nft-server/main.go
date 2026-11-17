// nft-server is the panel binary: the web UI (embedded), JSON API, agent hub,
// and SQLite store. The node-side daemon/TUI lives in the separate nft-agent
// binary, which the panel pushes to managed nodes over the WS link.
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

	"nft-forward/internal/db"
	"nft-forward/internal/server"
	"nft-forward/internal/wsproto"
)

func main() {
	args := os.Args[1:]
	// Tolerate the legacy "server" subcommand from the single-binary era so old
	// systemd units (ExecStart=… server --addr) keep working through an upgrade.
	if len(args) > 0 && args[0] == "server" {
		args = args[1:]
	}
	os.Exit(runServer(args))
}

func runServer(args []string) int {
	var (
		addr, dbPath, bootstrapPw    string
		resetAdminPw, resetAdminUser string
		agentSocket                  string
		backupInterval               time.Duration
		backupKeep                   int
	)
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	fs.StringVar(&addr, "addr", ":8080", "panel HTTP address")
	fs.StringVar(&agentSocket, "agent-socket", wsproto.LocalAgentSocketPath, "unix socket the co-located node daemon (nft-agent daemon --server-local) connects through; empty disables it")
	fs.StringVar(&dbPath, "db", "/var/lib/nft-forward/panel.db", "SQLite database path")
	fs.StringVar(&bootstrapPw, "bootstrap-admin-password", "", "set admin password on first boot")
	fs.StringVar(&resetAdminPw, "reset-admin-password", "", "reset admin password and exit")
	fs.StringVar(&resetAdminUser, "reset-admin-username", "admin", "admin username for reset")
	fs.DurationVar(&backupInterval, "backup-interval", 24*time.Hour, "local DB backup interval (0 disables)")
	fs.IntVar(&backupKeep, "backup-keep", 14, "number of local DB backups to retain")
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
	stopBackups := db.StartBackups(d, dbPath, backupInterval, backupKeep)
	defer stopBackups()

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
	localSrv := serveLocalAgent(srv, agentSocket)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	srv.Hub.Close() // send StatusGoingAway to agents before tearing down
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if localSrv != nil {
		_ = localSrv.Shutdown(ctx) // 关闭监听时一并删掉 socket 文件
	}
	return 0
}

// serveLocalAgent 在 socketPath 上起本机入口，供同机的 daemon 接入成为 self 节点。
// socketPath 为空表示不提供；起不来（路径不可写，或另一个面板进程正在这上面服务）
// 不是致命错误——面板照常对外服务，只是 self 节点接不进来，日志里说明原因。
func serveLocalAgent(srv *server.Server, socketPath string) *http.Server {
	if socketPath == "" {
		return nil
	}
	l, err := server.ListenLocalAgent(socketPath)
	if err != nil {
		log.Printf("本机节点入口不可用，面板宿主机上的 daemon 将无法接入: %v", err)
		return nil
	}
	local := &http.Server{Handler: srv.LocalAgentHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("local node entrance listening on %s", socketPath)
		if err := local.Serve(l); err != nil && err != http.ErrServerClosed {
			log.Printf("本机节点入口停止服务: %v", err)
		}
	}()
	return local
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
