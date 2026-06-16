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
	"strings"
	"time"

	"nft-forward/internal/nft"
)

// DefaultSocketPath is the unix-socket address the host daemon listens on.
// Kept as a constant so callers that always talk to the local daemon don't
// need to repeat the path.
const DefaultSocketPath = "/var/run/nft-forward.sock"

// Client speaks the daemon's HTTP API. Transport is selected by the address
// scheme: "unix://" dials the local socket; "http://" / "https://" dial the
// remote endpoint with an optional bearer token. Both transports share the
// same JSON request/response shape so callers don't branch on which one is
// in use.
type Client struct {
	base       string
	bearer     string
	httpClient *http.Client
}

type Option func(*Client)

// WithBearerToken sets the Authorization header for HTTP transports.
// Unix-socket transports ignore it: SO_PEERCRED already establishes
// authority and re-adding a static secret would be misleading.
func WithBearerToken(token string) Option {
	return func(c *Client) {
		c.bearer = token
	}
}

// New parses address and returns a Client wired for that transport.
// Accepted address forms:
//   - "unix:///var/run/nft-forward.sock"
//   - "http://host:port" or "https://host:port"
//
// Plain socket paths ("/var/run/foo.sock") are also accepted for callers that
// haven't yet been updated to the URL form; they're treated as unix://.
func New(address string, opts ...Option) (*Client, error) {
	c := &Client{}
	for _, o := range opts {
		o(c)
	}

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
		c.bearer = ""
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
		c.bearer = ""
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
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
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

// StatusResp holds the daemon's connection status.
type StatusResp struct {
	Connected bool   `json:"connected"`
	NodeName  string `json:"node_name,omitempty"`
	NodeID    int64  `json:"node_id,omitempty"`
}

// Status returns the daemon's connection status and node identity.
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
	return resp, nil
}

// ListRules returns the active rule list from the daemon. When connected
// to a server, these are the server-managed rules; otherwise local rules.
func (c *Client) ListRules() ([]nft.Rule, error) {
	buf, code, err := c.do(http.MethodGet, "/v1/rules", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, fmt.Errorf("daemon rules: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var resp struct {
		Rules []nft.Rule `json:"rules"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, fmt.Errorf("daemon rules: decode: %w", err)
	}
	if resp.Rules == nil {
		resp.Rules = []nft.Rule{}
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

// CreateRule creates a rule. When the daemon is connected to a server the
// rule is created server-side; otherwise it is managed locally.
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
	Name       string `json:"name"`
}

// UpdateRule updates an existing rule identified by id. Server rules use
// numeric IDs; local rules use hex IDs.
func (c *Client) UpdateRule(id string, req UpdateRuleReq) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPut, "/v1/rules/"+url.PathEscape(id), body)
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

// DeleteRule deletes a rule identified by id.
func (c *Client) DeleteRule(id string) error {
	buf, code, err := c.do(http.MethodDelete, "/v1/rules/"+url.PathEscape(id), nil)
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

// ApplyRuleset pushes a complete ruleset for the "panel" owner segment to the
// daemon. Used by the server's self-node dispatch path to replicate what the
// WS apply_ruleset frame does for remote nodes. The daemon replaces its panel
// segment atomically and applies to the kernel.
func (c *Client) ApplyRuleset(rules []nft.Rule) error {
	if rules == nil {
		rules = []nft.Rule{}
	}
	body, err := json.Marshal(struct {
		Rules []nft.Rule `json:"rules"`
	}{Rules: rules})
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/apply", body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("daemon apply: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	return nil
}
