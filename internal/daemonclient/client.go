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

// GetRuleset fetches the full segmented ruleset currently held by daemon.
func (c *Client) GetRuleset() (OwnerRuleset, error) {
	buf, code, err := c.do(http.MethodGet, "/v1/ruleset", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, fmt.Errorf("daemon ruleset: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var payload fullPayload
	if err := json.Unmarshal(buf, &payload); err != nil {
		return nil, fmt.Errorf("daemon ruleset: decode: %w", err)
	}
	if payload.Owners == nil {
		payload.Owners = OwnerRuleset{}
	}
	return payload.Owners, nil
}

// PostRuleset replaces the daemon's ruleset segment for owner with rules.
// Passing an empty slice clears the segment. Returns an error with the
// daemon's response body on non-2xx so the caller can show conflict /
// validation messages verbatim.
func (c *Client) PostRuleset(owner string, rules []nft.Rule) error {
	if strings.TrimSpace(owner) == "" {
		return errors.New("daemonclient: owner is required")
	}
	if rules == nil {
		rules = []nft.Rule{}
	}
	body, err := json.Marshal(segmentPayload{Rules: rules})
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/ruleset/"+url.PathEscape(owner), body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("daemon push %s: HTTP %d: %s", owner, code, strings.TrimSpace(string(buf)))
	}
	return nil
}

// ChainEdit asks the daemon to apply an edit (listen_port/mode/comment) to
// this node's hop in a chain. The daemon relays it to the server and blocks
// for the verdict; a non-2xx body is the server's rejection reason (e.g.
// "端口被占用") surfaced verbatim so the TUI can show it.
func (c *Client) ChainEdit(chainID int64, listenPort int, mode, comment string) error {
	body, err := json.Marshal(struct {
		ChainID    int64  `json:"chain_id"`
		ListenPort int    `json:"listen_port"`
		Mode       string `json:"mode"`
		Comment    string `json:"comment"`
	}{chainID, listenPort, mode, comment})
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/chain/edit", body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		// The body is the server's user-facing rejection reason (e.g.
		// "端口被占用"); surface it verbatim with no technical prefix so the
		// TUI can show it as the primary message. Fall back to the status
		// code only when the body is empty and carries no reason.
		msg := strings.TrimSpace(string(buf))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return errors.New(msg)
	}
	return nil
}

// ChainDelete asks the daemon to delete the entire chain this node
// participates in. The daemon relays it to the server and blocks for the
// verdict.
func (c *Client) ChainDelete(chainID int64) error {
	body, err := json.Marshal(struct {
		ChainID int64 `json:"chain_id"`
	}{chainID})
	if err != nil {
		return err
	}
	buf, code, err := c.do(http.MethodPost, "/v1/chain/delete", body)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		// The body is the server's user-facing rejection reason; surface it
		// verbatim with no technical prefix so the TUI shows it as the
		// primary message. Fall back to the status code only on an empty body.
		msg := strings.TrimSpace(string(buf))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", code)
		}
		return errors.New(msg)
	}
	return nil
}
