package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"nft-forward/internal/nft"
)

// DefaultSocketPath is the unix-socket address the host daemon listens on.
// Kept as a constant so callers that always talk to the local daemon don't
// need to repeat the path.
const DefaultSocketPath = "/var/run/nft-forward.sock"

// Client 说 daemon 本机控制口的 HTTP 协议。走哪种传输由地址决定：unix socket 路径
// （可带 "unix://" 前缀）是 daemon 实际监听的地方；"http://" / "https://" 地址按
// 普通 HTTP 拨，daemon 自己不监听 TCP，这种形式是给测试接 httptest 服务用的。两种
// 传输上的请求与应答完全相同。
//
// 控制口不带凭据：谁能连由 socket 文件的属主与权限决定（daemon.ListenSocket 把它建成
// 0660、属组为配置的组）。
type Client struct {
	base       string
	httpClient *http.Client
}

// New parses address and returns a Client wired for that transport.
// Accepted address forms:
//   - "unix:///var/run/nft-forward.sock"
//   - "http://host:port" or "https://host:port"
//
// Plain socket paths ("/var/run/foo.sock") are also accepted for callers that
// haven't yet been updated to the URL form; they're treated as unix://.
func New(address string) (*Client, error) {
	c := &Client{}

	switch {
	case strings.HasPrefix(address, "unix://"):
		sockPath := strings.TrimPrefix(address, "unix://")
		c.base = "http://daemon"
		c.httpClient = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
		}
	case strings.HasPrefix(address, "http://"), strings.HasPrefix(address, "https://"):
		u, err := url.Parse(address)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: parse address %q: %w", address, err)
		}
		c.base = strings.TrimRight(u.String(), "/")
		c.httpClient = &http.Client{Timeout: 10 * time.Second}
	default:
		// Backwards-compatible: bare filesystem path means unix transport.
		c.base = "http://daemon"
		c.httpClient = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", address)
				},
			},
		}
	}
	return c, nil
}

func (c *Client) do(method, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return buf, resp.StatusCode, nil
}

// Health hits GET /v1/health and returns nil on 2xx with {"ok":true}.
// Any transport error, non-2xx status, or ok=false produces an error
// describing the failure so the caller can surface a precise message.
func (c *Client) Health() error {
	buf, code, err := c.do(http.MethodGet, "/v1/health", nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("daemon health: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var r struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(buf, &r); err != nil {
		return fmt.Errorf("daemon health: decode: %w", err)
	}
	if !r.OK {
		return errors.New("daemon health: ok=false")
	}
	return nil
}

// Mode 是 daemon 的运行形态——它归谁管。取值是 daemon 在 GET /v1/status 里报的
// 名字（internal/daemon 的 RunMode.Name）。
type Mode string

const (
	// ModeStandalone：单机，不归任何面板管，规则全是本地规则。
	ModeStandalone Mode = "standalone"
	// ModeRemotePanel：由远端面板托管。
	ModeRemotePanel Mode = "remote-panel"
	// ModeLocalPanel：面板机本机，由同机的面板托管。
	ModeLocalPanel Mode = "local-panel"
)

// Managed 报告 daemon 是否归某个面板管。
func (m Mode) Managed() bool { return m == ModeRemotePanel || m == ModeLocalPanel }

func (m Mode) known() bool { return m == ModeStandalone || m.Managed() }

// StatusResp 是 daemon 的状态：形态，以及托管形态下与面板的会话状态。
//
// Mode 在 daemon 进程存续期间不变。Connected 只在托管形态下可能为真。NodeName /
// NodeID 是最近一次握手时面板给的节点身份，断线后仍然保留；从未握手成功过时为空。
type StatusResp struct {
	Mode      Mode   `json:"mode"`
	Connected bool   `json:"connected"`
	NodeName  string `json:"node_name,omitempty"`
	NodeID    int64  `json:"node_id,omitempty"`
}

// Status returns the daemon's run mode, session state and node identity.
//
// 认不出的形态按错误返回，不猜成某一种：调用方要靠形态决定显示“本地模式”还是
// “面板托管”，猜错了就是把托管但离线的 daemon 说成单机。daemon 比本程序旧时（它的
// 应答里没有 mode）走的也是这条路。
func (c *Client) Status() (StatusResp, error) {
	buf, code, err := c.do(http.MethodGet, "/v1/status", nil)
	if err != nil {
		return StatusResp{}, err
	}
	if code/100 != 2 {
		return StatusResp{}, fmt.Errorf("daemon status: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var resp StatusResp
	if err := json.Unmarshal(buf, &resp); err != nil {
		return StatusResp{}, fmt.Errorf("daemon status: decode: %w", err)
	}
	if !resp.Mode.known() {
		return StatusResp{}, fmt.Errorf("daemon status: 无法识别的运行形态 %q（daemon 与本程序版本不一致时会这样，重启 daemon 后再试）", string(resp.Mode))
	}
	return resp, nil
}

// Row 是 daemon 规则列表里的一行：规则本身，加上 daemon 给它的地址。
//
// 改或删这一行时把 Ref 原样交回（UpdateRule / DeleteRule）。这一行是本地规则还是
// 面板规则由 Ref 说了算——它是 daemon 按规则的归属填的；不要从 Rule 的字段
// （比如 RuleID 是否为零）去推断。
type Row struct {
	nft.Rule
	Ref RuleRef `json:"ref"`
}

// ListRules returns every rule the daemon is forwarding, each with its address:
// panel-managed rules first, then local ones — regardless of the daemon's run
// mode and of whether it is currently connected to its panel.
//
// 没带地址的行按错误返回：没有地址就无从知道这一行归谁，也就不能改它、删它。
// daemon 比本程序旧时（它的列表里没有 ref）走的是这条路。
func (c *Client) ListRules() ([]Row, error) {
	buf, code, err := c.do(http.MethodGet, "/v1/rules", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, fmt.Errorf("daemon rules: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var resp struct {
		Rules []Row `json:"rules"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, fmt.Errorf("daemon rules: decode: %w", err)
	}
	if resp.Rules == nil {
		resp.Rules = []Row{}
	}
	for _, row := range resp.Rules {
		if row.Ref == (RuleRef{}) {
			return nil, errors.New("daemon rules: 列表里的规则没有带地址（daemon 与本程序版本不一致时会这样，重启 daemon 后再试）")
		}
	}
	return resp.Rules, nil
}

// CreateRuleReq is the body for POST /v1/rules.
type CreateRuleReq struct {
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
	Name       string `json:"name"`
}

// CreateRuleResp is the response from POST /v1/rules.
type CreateRuleResp struct {
	Entry      string `json:"entry"`
	ListenPort int    `json:"listen_port"`
}

// CreateRule creates a rule. 往哪建由 daemon 的形态决定：托管形态下由面板建（面板
// 不可达时 daemon 拒绝，不退回去建本地规则），单机形态下建一条本地规则。
func (c *Client) CreateRule(req CreateRuleReq) (CreateRuleResp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CreateRuleResp{}, err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/rules", body)
	if err != nil {
		return CreateRuleResp{}, err
	}
	if code/100 != 2 {
		msg := strings.TrimSpace(string(buf))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return CreateRuleResp{}, errors.New(msg)
	}
	var resp CreateRuleResp
	if err := json.Unmarshal(buf, &resp); err != nil {
		return CreateRuleResp{}, fmt.Errorf("daemon create rule: decode: %w", err)
	}
	return resp, nil
}

// UpdateRuleReq is the body for PUT /v1/rules/{id}.
type UpdateRuleReq struct {
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
}

// RuleRef 是一条规则在 daemon 上的地址：命名空间加上该命名空间内的 id。
//
// 规则有两个互不相干的 id 命名空间：本地规则用 daemon 生成的字符串 id（nft.Rule.ID，
// 8 位十六进制），面板规则用面板数据库里的整数 id（nft.Rule.RuleID）。十六进制串可能
// 恰好全是数字，两种 id 单看字符串分不开，所以地址必须带上命名空间；daemon 的
// /v1/rules/{命名空间}/{id} 按它分发，不从 id 的长相推断。
//
// 地址的来源是 daemon：ListRules 的每一行带着 daemon 给的 Ref，改删时原样交回。
// LocalRule / PanelRule 不经列表直接造一个地址；本程序里没有这样的调用方（TUI 只用
// 列表给的地址），它们是给别的包的测试造地址用的——RuleRef 的字段不导出，包外没有
// 别的办法构造它。零值不指向任何规则，daemon 以 404 拒绝。
type RuleRef struct {
	ns string
	id string
}

const (
	nsLocal = "local"
	nsPanel = "panel"
)

// LocalRule 指向本地名下 ID 等于 id 的本地规则。
func LocalRule(id string) RuleRef { return RuleRef{ns: nsLocal, id: id} }

// PanelRule 指向面板规则 ruleID。
func PanelRule(ruleID int64) RuleRef {
	return RuleRef{ns: nsPanel, id: strconv.FormatInt(ruleID, 10)}
}

// Local 报告这个地址是否指向本地规则。
func (r RuleRef) Local() bool { return r.ns == nsLocal }

// UnmarshalJSON 读 daemon 在列表里给出的地址 {"ns":…,"id":…}。命名空间认不出或
// id 为空都是错误：这样的地址交回去只会被 daemon 拒绝，不如在读的时候就说清楚。
func (r *RuleRef) UnmarshalJSON(b []byte) error {
	var wire struct {
		NS string `json:"ns"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	if (wire.NS != nsLocal && wire.NS != nsPanel) || wire.ID == "" {
		return fmt.Errorf("规则地址非法：命名空间 %q，id %q", wire.NS, wire.ID)
	}
	*r = RuleRef{ns: wire.NS, id: wire.ID}
	return nil
}

func (r RuleRef) path() string {
	return "/v1/rules/" + r.ns + "/" + url.PathEscape(r.id)
}

// String 便于在测试失败信息与日志里辨认，例如 "panel/7"。
func (r RuleRef) String() string { return r.ns + "/" + r.id }

// UpdateRule updates the rule ref points at.
func (c *Client) UpdateRule(ref RuleRef, req UpdateRuleReq) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPut, ref.path(), body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		msg := strings.TrimSpace(string(buf))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return errors.New(msg)
	}
	return nil
}

// DeleteRule deletes the rule ref points at.
func (c *Client) DeleteRule(ref RuleRef) error {
	buf, code, err := c.do(http.MethodDelete, ref.path(), nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		msg := strings.TrimSpace(string(buf))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return errors.New(msg)
	}
	return nil
}
