package daemon

import "nft-forward/internal/wsproto"

// RunMode 是 daemon 的运行形态：这个 daemon 归谁管。
//
// 形态有三种，零值是单机：
//
//   - 单机（Standalone）：不归任何面板管。规则全是本地规则，由本机控制口增删改；
//     没有拨号器。
//   - 由远端面板托管（RemotePanel）：拨面板的 WebSocket 地址，握手带节点 token，
//     面板按 token 认节点。
//   - 面板机本机（LocalPanel）：面板与 daemon 在同一台机器上，拨面板的本机入口
//     socket，握手不带 token，面板按入口把它认定为 self 节点。
//
// 后两种合称“托管”（Managed）：规则由面板建、由面板下发。
//
// 形态由进程启动参数决定一次：cmd/nft-agent 把命令行参数换成一个 RunMode 放进
// Config.Mode，New 把它存进 Daemon.mode，此后没有任何代码改写它。“一个 daemon 至多
// 归一个面板管”不靠校验保证——三个构造函数各产出一种形态，同时指向两个面板的值
// 构造不出来。
//
// “与面板的会话此刻是否在线”不是形态，而是托管形态下随时会变的状态，问
// Daemon.panelSession。
type RunMode struct {
	kind runKind
	// url、token 只在远端面板托管时有值；socket 只在面板机本机时有值。
	url    string
	token  string
	socket string
}

type runKind uint8

const (
	runStandalone runKind = iota
	runRemotePanel
	runLocalPanel
)

// 形态在本机控制口（GET /v1/status）上的名字。daemonclient.Mode 的取值与它们逐字
// 相同，TestStatusModeNamesAgreeBetweenDaemonAndClient 对照。
const (
	modeNameStandalone  = "standalone"
	modeNameRemotePanel = "remote-panel"
	modeNameLocalPanel  = "local-panel"
)

// Standalone 是单机形态，等于 RunMode 的零值。
func Standalone() RunMode { return RunMode{} }

// RemotePanel 是由远端面板托管的形态：url 是面板的 agent WebSocket 地址，token 是
// 握手用的节点 token。
func RemotePanel(url, token string) RunMode {
	return RunMode{kind: runRemotePanel, url: url, token: token}
}

// LocalPanel 是面板机本机的形态：socket 是面板的本机入口，空串取默认路径
// wsproto.LocalAgentSocketPath。
func LocalPanel(socket string) RunMode {
	if socket == "" {
		socket = wsproto.LocalAgentSocketPath
	}
	return RunMode{kind: runLocalPanel, socket: socket}
}

// Managed 报告这个 daemon 是否归某个面板管（远端面板或本机面板）。
func (m RunMode) Managed() bool { return m.kind != runStandalone }

// Name 是形态在本机控制口上的名字。
func (m RunMode) Name() string {
	switch m.kind {
	case runRemotePanel:
		return modeNameRemotePanel
	case runLocalPanel:
		return modeNameLocalPanel
	default:
		return modeNameStandalone
	}
}
