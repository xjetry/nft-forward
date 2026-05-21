package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"nft-forward/internal/nft"
)

// DefaultSocketPath matches what the daemon listens on by default. Tests
// pass a custom path; production callers use this constant.
const DefaultSocketPath = "/var/run/nft-forward.sock"

// Client speaks HTTP over a unix socket to the local nft-forward daemon.
// It is safe to share across goroutines; the underlying http.Client and
// transport already handle concurrent requests.
type Client struct {
	socketPath string
	http       *http.Client
}

// New constructs a Client bound to socketPath.
func New(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

// Health hits GET /v1/health and returns nil on 200 with {"ok":true}.
// Any transport error, non-200 status, or ok=false produces an error
// describing the failure so the caller can surface a precise message.
func (c *Client) Health() error {
	resp, err := c.http.Get("http://unix/v1/health")
	if err != nil {
		return fmt.Errorf("dial daemon socket %s: %w", c.socketPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon health returned status %d: %s", resp.StatusCode, body)
	}
	var got map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return fmt.Errorf("decode health body: %w", err)
	}
	if !got["ok"] {
		return fmt.Errorf("daemon health returned ok=false")
	}
	return nil
}

// GetRuleset fetches the full segmented ruleset currently held by daemon.
func (c *Client) GetRuleset() (OwnerRuleset, error) {
	resp, err := c.http.Get("http://unix/v1/ruleset")
	if err != nil {
		return nil, fmt.Errorf("GET /v1/ruleset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET /v1/ruleset status %d: %s", resp.StatusCode, body)
	}
	var got fullPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return nil, fmt.Errorf("decode ruleset: %w", err)
	}
	if got.Owners == nil {
		got.Owners = OwnerRuleset{}
	}
	return got.Owners, nil
}

// PostRuleset replaces the daemon's ruleset segment for owner with rules.
// Passing an empty slice clears the segment. Returns an error with the
// daemon's response body on non-2xx so the caller can show conflict /
// validation messages verbatim.
func (c *Client) PostRuleset(owner string, rules []nft.Rule) error {
	if owner == "" {
		return fmt.Errorf("owner must not be empty")
	}
	if rules == nil {
		rules = []nft.Rule{}
	}
	b, err := json.Marshal(segmentPayload{Rules: rules})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	url := "http://unix/v1/ruleset/" + owner
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}
