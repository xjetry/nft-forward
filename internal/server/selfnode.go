package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"

	"nft-forward/internal/db"
)

// EnsureSelfNode upserts the panel's built-in self-node row: the node that
// stands for the daemon running on the panel's own host. It appears in every
// nodes list and is handled like any other node; the one difference is how its
// daemon reaches the panel (LocalAgentHandler instead of the public endpoint).
func EnsureSelfNode(d *sql.DB) (*db.Node, error) {
	return db.UpsertSelfNode(d)
}

// LocalAgentHandler 是只给本机 daemon 用的 HTTP 入口，只有 /v1/agents 一条路由，
// 走 Hub.ServeLocalWS：从这里握手的连接一律认定为 self 节点，不验 token。
//
// 所以这个 handler 只能挂在本机才连得到的监听上——ListenLocalAgent 建的 unix
// socket。把它挂到对外的 TCP 监听上，等于任何人都能以 self 节点的身份接入。
func (s *Server) LocalAgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents", s.Hub.ServeLocalWS)
	return mux
}

// ListenLocalAgent 在 path 上建本机入口用的 unix socket，权限 0600：本机入口不验
// token，谁能连上这个 socket 谁就是 self 节点，文件权限就是它的认证。
//
// path 上已有 socket 时先试着连一下。连得上说明另一个面板进程正在这上面服务，
// 这时不抢占并返回错误：抢过来之后本机 daemon 下次重连会连到本进程，被本进程的
// 规则集整体替换。连不上（上一个进程异常退出留下的文件）才删掉重建。
func ListenLocalAgent(path string) (net.Listener, error) {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s 已有另一个面板进程在监听", path)
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("检查 %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("清理残留的 %s: %w", path, err)
	}
	// 先收紧 umask 再建：socket 文件从出现的那一刻起就是 0600，不存在“已建好但
	// 还没 chmod”的窗口。umask 是进程级的，这里只在启动阶段调用一次。
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	return l, nil
}
