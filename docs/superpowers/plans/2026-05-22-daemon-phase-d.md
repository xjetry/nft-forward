# Daemon-ize server / agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **CRITICAL (CLAUDE.md hard rule):** Commit messages, code comments, and KDoc MUST NOT carry process metadata — no "Phase X", "Task N", "Step Y", "Round Z", scheme codenames, "per previous review", etc. Explain *why* (design intent, invariant) instead. When dispatching subagents, pass this rule into their prompt; if their output violates it, clean it up before merging.

**Goal:** Move server-side push / poll and the remote agent role onto the daemon, deleting the standalone `nft-agent` and `nft-server` binaries. After this phase the daemon is the only process that calls `nft.Apply` / `tc.Apply` / `nft.ResolveHosts`, and remote nodes are reached over HTTP through the same client abstraction used for the local unix socket.

**Architecture:**
- `internal/daemonclient` grows a transport layer: `unix:///path` keeps the existing socket dial, `http(s)://host:port` issues HTTP with bearer auth. Same `Health`/`GetRuleset`/`PostRuleset` API for both.
- The daemon takes full ownership of DNS resolution and `tc.Apply`. State stays as owner-segmented raw rules (with `DestHost` preserved); the daemon resolves on every apply and on a periodic refresh tick so DDNS drift no longer requires a live TUI.
- `internal/agent` and `cmd/nft-agent` disappear. The HTTP listener that used to be the agent becomes an optional `--listen` mode on the daemon.
- `cmd/nft-server` disappears. Its bootstrap (panel DB, embedded apply, password reset) folds into `nft-forward server`, but with no embedded agent — the local host is just another node with `address = unix:///var/run/nft-forward.sock`.
- TUI reverts the in-process DNS resolve added when it became a daemon client: it now POSTs raw rules and lets the daemon resolve. This restores DDNS refresh for TUI-owned rules even when the TUI exits.

**Tech Stack:** Go 1.22+ stdlib, existing `nft` / `tc` / `resolver` / `db` packages. No new external dependencies.

---

## File Structure

**Created:**
- `internal/daemon/dns.go` — DNS refresh loop and `resolveOwners` helper.
- `internal/daemon/counters.go` — GET /v1/counters handler.

**Modified:**
- `internal/daemonclient/client.go` — accept any URL (unix/http/https) + bearer token option, add `GetCounters`.
- `internal/daemonclient/types.go` — add `Counter` mirror type.
- `internal/daemonclient/client_test.go` — cover http transport + bearer auth + counters.
- `internal/daemon/applier.go` — `Applier` now applies both nftables and tc; production impl wraps both.
- `internal/daemon/handlers.go` — POST /v1/ruleset/{owner} resolves before applying; reject unresolvable host-only rules; add counters route; add http listener wrapping.
- `internal/daemon/daemon.go` — `Config` gains `Iface`, `HTTPListen`, `TokenPath`; `Run` may start an additional HTTP listener alongside the socket; refresh goroutine wired in.
- `internal/daemon/state.go` — comment/doc only: clarify segments hold raw rules.
- `internal/daemon/handlers_test.go` — adapt fakes to new `Applier` signature; add DNS + counters + http-listener tests.
- `internal/tui/tui.go` — drop `nft.ResolveHosts` from `commit`; POST raw rules.
- `internal/tui/tui_test.go` — adjust assertions to expect raw rules.
- `internal/server/pusher.go` — delete `LocalAddrPrefix` + `Embedded` field; all sends go through `daemonclient`.
- `internal/server/poller.go` — same; `GetCounters` through `daemonclient`.
- `internal/server/server.go` — `NewPusher` signature drops the embedded agent argument.
- `internal/db/migrations.go` (or wherever migrations live — confirm at task time) — add migration that rewrites `nodes.address` from `local://...` to `unix:///var/run/nft-forward.sock`.
- `cmd/nft-forward/main.go` — add `server` subcommand dispatch; add `--listen` / `--token-file` to `daemon` subcommand; delete dead helpers (`preflight`, `promptPersist`, `promptYes`, `runApply`, `runInstallService`, `runUninstall`); fold `cmd/nft-server/main.go` bootstrap (admin reset, password seed, panel start) into a new `runServer` and `runResetAdmin`.
- `docs/daemon-manual-verification.md` — extend with server / remote-daemon scenarios; remove "TUI-only" framing.

**Deleted:**
- `cmd/nft-agent/` (whole directory).
- `cmd/nft-server/` (whole directory).
- `internal/agent/` (whole package — its responsibilities are now in `internal/daemon`).

---

## Sequencing rationale

Order is bottom-up: build the new transport + daemon-side capabilities first, then flip consumers (TUI, then server), then collapse the cmd / cleanup. This keeps `go test ./...` green on each commit and means every commit is independently revertable.

Each task ends with a commit. Commit messages explain *what changed and why* — never which phase/task they belong to.

---

### Task 1: daemonclient supports http(s) transport + bearer auth + counters

**Files:**
- Modify: `internal/daemonclient/client.go`
- Modify: `internal/daemonclient/types.go`
- Modify: `internal/daemonclient/client_test.go`

**Context:** Right now `daemonclient.New(socketPath)` always dials a unix socket and never sends `Authorization`. The server's pusher/poller will reuse this client for remote nodes (HTTP) and the local node (unix), so the constructor needs to accept either form of address and an optional bearer token. We also need `GetCounters` since the poller used to call `GET /v1/counters` on the agent directly.

- [ ] **Step 1: Write the failing tests**

Append to `internal/daemonclient/client_test.go`:

```go
func TestClient_HTTPTransport_HealthAndPost(t *testing.T) {
	var gotAuth string
	var gotOwner string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/ruleset/panel":
			gotOwner = "panel"
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := daemonclient.New(srv.URL, daemonclient.WithBearerToken("s3cret"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := c.PostRuleset("panel", []nft.Rule{{Proto: "tcp", SrcPort: 22, DestIP: "10.0.0.1", DestPort: 22}}); err != nil {
		t.Fatalf("PostRuleset: %v", err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", gotAuth)
	}
	if gotOwner != "panel" {
		t.Errorf("owner = %q", gotOwner)
	}
	if !bytes.Contains(gotBody, []byte(`"src_port":22`)) {
		t.Errorf("body missing rule: %s", gotBody)
	}
}

func TestClient_HTTPTransport_GetCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/counters" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"counters":[{"proto":"tcp","listen_port":80,"bytes":123,"packets":4}]}`))
	}))
	defer srv.Close()

	c, err := daemonclient.New(srv.URL, daemonclient.WithBearerToken("tok"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.GetCounters()
	if err != nil {
		t.Fatalf("GetCounters: %v", err)
	}
	if len(got) != 1 || got[0].ListenPort != 80 || got[0].Bytes != 123 {
		t.Errorf("unexpected counters: %+v", got)
	}
}

func TestClient_UnixTransport_StillWorks(t *testing.T) {
	dir := shortSockDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	go func() { _ = http.Serve(ln, mux) }()

	c, err := daemonclient.New("unix://" + sockPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
}
```

Add the missing imports at the top of the file if not already present: `bytes`, `io`, `net`, `net/http`, `net/http/httptest`, `path/filepath`, `nft-forward/internal/daemonclient`, `nft-forward/internal/nft`.

If the existing `shortSockDir` helper lives in a different file in the same package, reuse it; if it's test-only and not exported, copy its definition (see how `internal/daemon` tests share the helper).

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/daemonclient/...`
Expected: compile failure or test failure because `New` does not accept a URL with `http(s)://` scheme, `WithBearerToken` does not exist, and `GetCounters` is not defined.

- [ ] **Step 3: Implement the new transport in client.go**

Replace the existing constructor and add the new types/methods. Full file contents:

```go
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
	base      string
	bearer    string
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

// GetCounters returns per-rule byte/packet counters from the daemon. The
// poller uses this to attribute traffic to tenants.
func (c *Client) GetCounters() ([]Counter, error) {
	buf, code, err := c.do(http.MethodGet, "/v1/counters", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, fmt.Errorf("daemon counters: HTTP %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var payload struct {
		Counters []Counter `json:"counters"`
	}
	if err := json.Unmarshal(buf, &payload); err != nil {
		return nil, fmt.Errorf("daemon counters: decode: %w", err)
	}
	return payload.Counters, nil
}
```

In `types.go`, add:

```go
// Counter mirrors nft.Counter for the daemonclient API. We don't import
// internal/nft here for the same reason OwnerRuleset is mirrored: keep the
// client wire shape free of indirect coupling to daemon internals.
type Counter struct {
	Proto      string `json:"proto"`
	ListenPort int    `json:"listen_port"`
	Bytes      uint64 `json:"bytes"`
	Packets    uint64 `json:"packets"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemonclient/...`
Expected: PASS, including the new http/unix/counters tests.

Also run `go vet ./internal/daemonclient/...` to catch any unused imports.

- [ ] **Step 5: Commit**

```bash
git add internal/daemonclient
git commit -m "daemonclient: dial unix or http based on address, add bearer auth"
```

---

### Task 2: daemon exposes GET /v1/counters

**Files:**
- Create: `internal/daemon/counters.go`
- Modify: `internal/daemon/handlers.go`
- Modify: `internal/daemon/handlers_test.go`

**Context:** The server poller needs counter data for traffic accounting. Today the agent exposes `GET /v1/counters`; the daemon does not. We add a thin handler that calls `nft.Counters()` and serializes the result. Use a function-typed field so tests can substitute a fake without invoking `nft` (which would require root).

- [ ] **Step 1: Write the failing tests**

Append to `internal/daemon/handlers_test.go`:

```go
func TestHandleCounters(t *testing.T) {
	d := newTestDaemon(t)
	d.countersFn = func() ([]nft.Counter, error) {
		return []nft.Counter{{Proto: "tcp", ListenPort: 80, Bytes: 100, Packets: 2}}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/counters", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got struct {
		Counters []nft.Counter `json:"counters"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Counters) != 1 || got.Counters[0].ListenPort != 80 {
		t.Errorf("unexpected counters: %+v", got.Counters)
	}
}

func TestHandleCounters_Error(t *testing.T) {
	d := newTestDaemon(t)
	d.countersFn = func() ([]nft.Counter, error) {
		return nil, fmt.Errorf("nft not available")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/counters", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
```

If `newTestDaemon` does not already exist in the test file, you'll need to find the equivalent factory (e.g. an inline `daemon.New(...)` pattern) and adapt. The point is to construct a daemon where you can override the counter source.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/daemon/...`
Expected: compile failure — `countersFn` field doesn't exist, route not registered.

- [ ] **Step 3: Add the handler**

Create `internal/daemon/counters.go`:

```go
package daemon

import (
	"encoding/json"
	"net/http"

	"nft-forward/internal/nft"
)

// handleCounters returns per-rule counters scraped from nftables. The poller
// uses these for tenant traffic accounting; exposing them on the daemon (not
// on every client) keeps the kernel as the single source of truth.
func (d *Daemon) handleCounters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counters, err := d.countersFn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"counters": counters})
}

// defaultCounters delegates to the live nftables source. We expose this as a
// field so unit tests can swap in a fake without requiring root.
func defaultCounters() ([]nft.Counter, error) {
	return nft.Counters()
}
```

In `internal/daemon/handlers.go`:

1. Add a `countersFn func() ([]nft.Counter, error)` field to the `Daemon` struct.
2. In whatever constructor / `Default*` factory you have (likely `daemon.New`), initialise it: `countersFn: defaultCounters` if `Config.CountersFn` is nil; otherwise use the override.
3. Add `Config.CountersFn func() ([]nft.Counter, error)` so tests / runtimes can inject.
4. In `Handler()`, register `mux.HandleFunc("/v1/counters", d.handleCounters)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon
git commit -m "daemon: expose /v1/counters so external clients can scrape traffic"
```

---

### Task 3: daemon takes over tc.Apply

**Files:**
- Modify: `internal/daemon/applier.go`
- Modify: `internal/daemon/daemon.go` (Config + New)
- Modify: `internal/daemon/handlers.go` (the apply call site)
- Modify: `internal/daemon/handlers_test.go`

**Context:** The agent today does `nft.Apply(...)` then `tc.Apply(rules, iface)`. As the daemon absorbs the agent role, `tc.Apply` must move with it — only the daemon process has the privileges and the global view needed to set up HTB classes correctly. The `Applier` interface gets a second responsibility; we keep it a single method so callers don't have to remember to call two things in the right order.

- [ ] **Step 1: Write the failing test**

In `internal/daemon/handlers_test.go`, find any existing fake applier (it's a struct implementing the existing single-method interface). Extend the fake so it records both nft and tc calls, then add:

```go
func TestApplyInvokesTcAfterNft(t *testing.T) {
	fake := &fakeApplier{}
	d := newTestDaemon(t)
	d.applier = fake
	d.iface = "eth42"

	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, BandwidthMbps: 50}}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code/100 != 2 {
		t.Fatalf("status = %d", w.Code)
	}
	if len(fake.nftCalls) != 1 || len(fake.tcCalls) != 1 {
		t.Fatalf("expected one nft+tc call, got nft=%d tc=%d", len(fake.nftCalls), len(fake.tcCalls))
	}
	if fake.tcCalls[0].iface != "eth42" {
		t.Errorf("tc iface = %q, want eth42", fake.tcCalls[0].iface)
	}
}
```

Update the existing `fakeApplier` to satisfy the new shape:

```go
type fakeTcCall struct {
	rules []nft.Rule
	iface string
}

type fakeApplier struct {
	nftCalls [][]nft.Rule
	tcCalls  []fakeTcCall
	err      error
}

func (f *fakeApplier) Apply(rules []nft.Rule, iface string) error {
	if f.err != nil {
		return f.err
	}
	f.nftCalls = append(f.nftCalls, append([]nft.Rule(nil), rules...))
	f.tcCalls = append(f.tcCalls, fakeTcCall{
		rules: append([]nft.Rule(nil), rules...),
		iface: iface,
	})
	return nil
}
```

If there are other tests that constructed `fakeApplier` with a single-argument call, they will fail to compile — update them in the same edit so the test file builds.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/daemon/...`
Expected: compile failure — `Applier.Apply` still has one argument, the daemon has no `iface` field.

- [ ] **Step 3: Update the Applier interface**

In `internal/daemon/applier.go`:

```go
package daemon

import (
	"nft-forward/internal/nft"
	"nft-forward/internal/tc"
)

// Applier writes a fully-resolved ruleset into the kernel data plane.
// Production daemons drive both nftables (packet forwarding) and tc
// (per-tunnel bandwidth shaping). Tests substitute fakes that record calls
// without requiring root.
type Applier interface {
	Apply(rules []nft.Rule, iface string) error
}

type nftApplier struct{}

func (nftApplier) Apply(rules []nft.Rule, iface string) error {
	if err := nft.Apply(rules); err != nil {
		return err
	}
	// tc runs after nft so a stale class hierarchy never points at a
	// dest IP nft hasn't published yet. If tc fails the kernel keeps the
	// freshly-applied nft ruleset (traffic still forwards, only shaping
	// is missing); that's preferable to rolling nft back and dropping
	// packets.
	return tc.Apply(rules, iface)
}

// DefaultApplier returns the production applier.
func DefaultApplier() Applier { return nftApplier{} }
```

In `internal/daemon/daemon.go`, add `Iface` to `Config`:

```go
type Config struct {
	SocketPath  string
	StatePath   string
	GroupName   string
	Applier     Applier
	LegacyPaths LegacyMigrationPaths
	Iface       string
	CountersFn  func() ([]nft.Counter, error)
}
```

Add an `iface string` field on `Daemon`. In `New`, default it: if `cfg.Iface == ""`, call `tc.DefaultIface()` and fall back to `"eth0"`. Same heuristic the agent used.

In `internal/daemon/handlers.go`, change every `d.applier.Apply(merged)` call to `d.applier.Apply(merged, d.iface)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon
git commit -m "daemon: drive tc shaping alongside nftables so client side stays stateless"
```

---

### Task 4: daemon resolves DNS on the apply path

**Files:**
- Create: `internal/daemon/dns.go`
- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/handlers.go`
- Modify: `internal/daemon/handlers_test.go`

**Context:** With clients pushing raw rules (containing `DestHost`), the daemon must resolve hostnames before handing rules to nftables. We thread the resolver through the apply path and reject pushes whose host-only rules have no usable IP — same policy the agent had — so a typo'd hostname doesn't silently strand a forward.

- [ ] **Step 1: Write the failing test**

Add to `internal/daemon/handlers_test.go`:

```go
func TestApplyResolvesDestHost(t *testing.T) {
	d := newTestDaemon(t)
	// fake resolver: example.com -> 192.0.2.5
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		out := make([]nft.Rule, len(in))
		for i, r := range in {
			out[i] = r
			if r.DestHost == "example.com" {
				out[i].DestIP = "192.0.2.5"
			}
		}
		return out, true, nil
	}

	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestHost: "example.com", DestPort: 80}}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code/100 != 2 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(fake.nftCalls) != 1 {
		t.Fatalf("expected 1 apply call")
	}
	got := fake.nftCalls[0][0]
	if got.DestIP != "192.0.2.5" {
		t.Errorf("DestIP = %q, want 192.0.2.5", got.DestIP)
	}
	// State persists raw rules so a refresh can re-resolve.
	state, err := LoadState(d.statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state["tui"][0].DestHost != "example.com" || state["tui"][0].DestIP != "" {
		t.Errorf("state should keep raw rule, got %+v", state["tui"][0])
	}
}

func TestApplyRejectsUnresolvableHost(t *testing.T) {
	d := newTestDaemon(t)
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		// resolver returns rules unchanged: DestHost still set, DestIP empty.
		return in, false, nil
	}
	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestHost: "nowhere.invalid", DestPort: 80}}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
```

Note: `fake` in the first test refers to the `fakeApplier` from Task 3. If the test factory `newTestDaemon` doesn't expose it, capture it from the same construction site.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/daemon/...`
Expected: compile failure — `resolveFn` field doesn't exist.

- [ ] **Step 3: Implement the resolve hook**

Create `internal/daemon/dns.go`:

```go
package daemon

import (
	"context"
	"fmt"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

// resolveFunc is the apply-time DNS resolver. Production points it at
// nft.ResolveHosts backed by a long-lived resolver.Resolver so positive
// answers are cached; tests inject deterministic fakes.
type resolveFunc func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error)

func defaultResolver(r *resolver.Resolver) resolveFunc {
	return func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error) {
		return nft.ResolveHosts(ctx, rules, r)
	}
}

// requireResolvedHosts returns an error naming the first rule whose DestHost
// did not resolve to an IP. Callers reject the apply rather than silently
// pushing an unreachable rule into nftables.
func requireResolvedHosts(rules []nft.Rule) error {
	for _, r := range rules {
		if r.DestHost != "" && r.DestIP == "" {
			return fmt.Errorf("rule %s/%d: 无法解析目标域名 %s", r.Proto, r.SrcPort, r.DestHost)
		}
	}
	return nil
}
```

In `internal/daemon/daemon.go`:

- Add to `Daemon`: `resolver *resolver.Resolver`, `resolveFn resolveFunc`.
- In `New`: if `cfg.Applier` is nil use default; create `r := resolver.New()`; `d.resolver = r; d.resolveFn = defaultResolver(r)`.

In `internal/daemon/handlers.go` `handleRulesetOwner`, replace the apply call site:

```go
// Snapshot d.owners, replace the segment, merge, resolve, then apply.
// State persists the raw (pre-resolve) rules so the refresh loop can
// re-resolve when an upstream DNS answer changes.
candidate := cloneOwners(d.owners)
candidate[owner] = rules

merged, err := MergedRuleset(candidate)
if err != nil {
    http.Error(w, err.Error(), http.StatusConflict)
    return
}

ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
defer cancel()
resolved, _, err := d.resolveFn(ctx, merged)
if err != nil {
    http.Error(w, err.Error(), http.StatusInternalServerError)
    return
}
if err := requireResolvedHosts(resolved); err != nil {
    http.Error(w, err.Error(), http.StatusBadRequest)
    return
}
if err := d.applier.Apply(resolved, d.iface); err != nil {
    http.Error(w, err.Error(), http.StatusInternalServerError)
    return
}
if err := SaveState(d.statePath, candidate); err != nil {
    http.Error(w, err.Error(), http.StatusInternalServerError)
    return
}
d.owners = candidate
```

(Adjust to match the exact variable names already in `handleRulesetOwner`. The invariant is: apply uses resolved rules, state stores the candidate (raw) map.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon
git commit -m "daemon: resolve hostnames before apply, persist raw rules for re-resolve"
```

---

### Task 5: daemon refreshes DNS on a tick so DDNS drift heals automatically

**Files:**
- Modify: `internal/daemon/dns.go`
- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/handlers_test.go` (or a new `dns_test.go`)

**Context:** Hostnames in stored rules can change IP at any time. The agent ran a 60-second refresh loop; we move that loop into the daemon. It snapshots the owner map, resolves, and re-applies only when at least one IP actually changed. The interval is configurable via `NFT_FORWARD_DNS_INTERVAL` for parity with the agent.

- [ ] **Step 1: Write the failing test**

Add to `internal/daemon/handlers_test.go` (or split into `dns_test.go` if you prefer):

```go
func TestRefreshReAppliesWhenIPChanges(t *testing.T) {
	d := newTestDaemon(t)
	fake := d.applier.(*fakeApplier)

	// Seed an owner segment with a host-only rule.
	d.owners = OwnerRuleset{
		"tui": {{Proto: "tcp", SrcPort: 80, DestHost: "x.example.com", DestPort: 80}},
	}

	answer := "192.0.2.10"
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		out := make([]nft.Rule, len(in))
		changed := false
		for i, r := range in {
			out[i] = r
			if r.DestHost == "x.example.com" {
				if r.DestIP != answer {
					changed = true
				}
				out[i].DestIP = answer
			}
		}
		return out, changed, nil
	}

	if err := d.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(fake.nftCalls) != 1 {
		t.Fatalf("first refresh should apply, got %d", len(fake.nftCalls))
	}

	// Second refresh with the same answer should be a no-op.
	if err := d.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	if len(fake.nftCalls) != 1 {
		t.Fatalf("idempotent refresh applied %d times", len(fake.nftCalls))
	}

	// IP changes -> apply again.
	answer = "192.0.2.11"
	if err := d.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh 3: %v", err)
	}
	if len(fake.nftCalls) != 2 {
		t.Fatalf("expected re-apply after IP change, got %d", len(fake.nftCalls))
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/daemon/...`
Expected: compile failure — `refreshOnce` doesn't exist.

- [ ] **Step 3: Implement the refresh logic**

Append to `internal/daemon/dns.go`:

```go
// refreshOnce performs a single DNS refresh pass: snapshot owners under the
// lock, merge, resolve, and re-apply only when at least one IP changed. The
// last-applied set is held in d.lastResolved so subsequent passes can detect
// "nothing moved" without an extra system call.
func (d *Daemon) refreshOnce(ctx context.Context) error {
	d.mu.Lock()
	snapshot := cloneOwners(d.owners)
	d.mu.Unlock()

	merged, err := MergedRuleset(snapshot)
	if err != nil {
		return err
	}
	resolved, _, err := d.resolveFn(ctx, merged)
	if err != nil {
		return err
	}
	if err := requireResolvedHosts(resolved); err != nil {
		// Don't push a partially-resolved set; just log and try again on the
		// next tick. The caller (the loop) is responsible for log shaping.
		return err
	}
	if !rulesDiffer(d.lastResolved, resolved) {
		return nil
	}
	if err := d.applier.Apply(resolved, d.iface); err != nil {
		return err
	}
	d.mu.Lock()
	d.lastResolved = append([]nft.Rule(nil), resolved...)
	d.mu.Unlock()
	return nil
}

func rulesDiffer(a, b []nft.Rule) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// dnsInterval honours NFT_FORWARD_DNS_INTERVAL for parity with the previous
// agent loop. A zero or invalid value disables the loop.
func dnsInterval() time.Duration {
	if s := os.Getenv("NFT_FORWARD_DNS_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Second
}
```

Add imports `os`, `time` at the top.

In `internal/daemon/daemon.go`:

1. Add `lastResolved []nft.Rule` field on `Daemon`.
2. In the apply path inside `handleRulesetOwner` (Task 4), set `d.lastResolved = append([]nft.Rule(nil), resolved...)` right after a successful apply.
3. In `Run(ctx context.Context)`, after starting the HTTP listener, launch:

```go
go d.refreshLoop(ctx)
```

where `refreshLoop` is:

```go
func (d *Daemon) refreshLoop(ctx context.Context) {
	interval := dnsInterval()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.refreshOnce(ctx); err != nil {
				log.Printf("dns refresh: %v", err)
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon
git commit -m "daemon: periodically re-resolve hostnames so DDNS drift heals without a live client"
```

---

### Task 6: TUI sends raw rules and lets the daemon resolve

**Files:**
- Modify: `internal/tui/tui.go`
- Modify: `internal/tui/tui_test.go`

**Context:** Right now `tui.commit` calls `nft.ResolveHosts` and pushes the resolved rules. That choice is now wrong: when the TUI exits, nothing refreshes those resolved IPs, so DDNS drift strands TUI-owned forwards. The daemon already resolves on apply and on the refresh tick, so the TUI should push raw rules and trust the daemon.

- [ ] **Step 1: Write/adjust the failing test**

Adjust `internal/tui/tui_test.go`. The existing test that exercises `commit` should now expect the fake daemon client to receive a rule with `DestHost` populated and `DestIP` empty. Replace any assertion that the rule was resolved.

```go
func TestCommitPostsRawRules(t *testing.T) {
	fake := &fakeDaemonClient{}
	m := initialModel(fake, []nft.Rule{})
	m.editing = nft.Rule{Proto: "tcp", SrcPort: 80, DestHost: "home.example.com", DestPort: 80}
	if err := m.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if fake.postedOwner != "tui" {
		t.Errorf("owner = %q", fake.postedOwner)
	}
	if len(fake.postedRules) != 1 || fake.postedRules[0].DestHost != "home.example.com" || fake.postedRules[0].DestIP != "" {
		t.Errorf("expected raw rule with DestHost set, got %+v", fake.postedRules)
	}
}
```

(The exact existing test name may differ. The point is: change every "resolved DestIP" assertion to "raw DestHost preserved".)

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/tui/...`
Expected: failure — current `commit` calls `ResolveHosts` and strips `DestHost`.

- [ ] **Step 3: Strip the resolve step**

In `internal/tui/tui.go`, locate `commit` (or whatever method posts to the daemon). Replace:

```go
resolved, _, err := nft.ResolveHosts(ctx, rules, m.resolver)
if err != nil {
    return err
}
return m.client.PostRuleset("tui", resolved)
```

with:

```go
return m.client.PostRuleset("tui", rules)
```

Remove the `resolver` field on `model` if it becomes unused; drop the `nft.ResolveHosts` import if no other call site remains. The model no longer needs a `context.Context` argument unless other code uses it.

If `loadInitialRules` had any resolve-related logic, leave it alone — it reads from the daemon, which returns rules in whatever form the daemon chooses.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tui/...`
Expected: PASS.

Also `go build ./...` to make sure no other caller depended on the removed resolver field.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "tui: push raw rules so the daemon owns DNS resolution and refresh"
```

---

### Task 7: server pusher routes every node through daemonclient

**Files:**
- Modify: `internal/server/pusher.go`
- Modify: `internal/server/server.go` (constructor signature)

**Context:** The pusher currently dispatches on `LocalAddrPrefix` ("local://"): local nodes call the embedded agent in-process, remote nodes do HTTP. Both branches are now expressible as `daemonclient.New(node.Address, WithBearerToken(node.Secret))` — a unix scheme for the local node, an http scheme for remote ones. Once embedded agent is gone, the pusher has a single code path.

- [ ] **Step 1: No new test (yet) — restructure**

This is structural; the existing tests (if any) for pusher use the embedded agent path. We'll add a transport-routing test in Task 8 (poller's tests share the same fake daemon).

- [ ] **Step 2: Rewrite send / constructor**

In `internal/server/pusher.go`, delete the `LocalAddrPrefix` constant and the `Embedded *agent.Agent` field. The new shape:

```go
package server

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

type Pusher struct {
	DB      *sql.DB
	pending chan int64
	stop    chan struct{}
}

func NewPusher(d *sql.DB) *Pusher {
	return &Pusher{
		DB:      d,
		pending: make(chan int64, 256),
		stop:    make(chan struct{}),
	}
}

// (Schedule / Run / Stop / reconcile / pushOne unchanged except: drop calls
// that referenced p.Embedded; pushOne now ends with p.send(n, rules) which
// only has the daemonclient path.)
```

`send`:

```go
func (p *Pusher) send(n *db.Node, rules []nft.Rule) error {
	c, err := daemonclient.New(n.Address, daemonclient.WithBearerToken(n.Secret))
	if err != nil {
		return fmt.Errorf("dial %s: %w", n.Address, err)
	}
	return c.PostRuleset("panel", rules)
}

func (p *Pusher) Probe(n *db.Node) error {
	c, err := daemonclient.New(n.Address, daemonclient.WithBearerToken(n.Secret))
	if err != nil {
		return err
	}
	if err := c.Health(); err != nil {
		return err
	}
	_ = db.MarkNodeSeen(p.DB, n.ID)
	return nil
}
```

(Keep `pushOne`'s rule-build loop — including `resolver.IsHostname` for picking DestHost vs DestIP — exactly as it is, because that lives at the panel layer where DB rows turn into rules. The daemon will resolve on receive.)

In `internal/server/server.go`, change `NewPusher`'s caller chain: the `embedded *agent.Agent` parameter is gone. The `Server` struct's `Pusher` field stays the same type, but `server.New(...)` no longer takes the embedded agent (it didn't in the constructor signature shown — it's the caller in `cmd/nft-server/main.go` that built `NewPusher(d, embedded)`). Just remove the unused `embedded` param from `NewPusher`.

If there's a test file that passed a fake agent in, replace it with nothing.

- [ ] **Step 3: Verify compile + tests**

Run: `go build ./... && go test ./internal/server/...`
Expected: PASS. (If `internal/server` has no tests, the build pass is the gate.)

- [ ] **Step 4: Commit**

```bash
git add internal/server
git commit -m "server: push rules through daemonclient for both local and remote nodes"
```

---

### Task 8: server poller pulls counters via daemonclient + nodes migration

**Files:**
- Modify: `internal/server/poller.go`
- Modify: `internal/db/migrations.go` (or whichever file owns schema bumps)

**Context:** Mirror image of Task 7 for the polling side. Plus, the panel's local node has `address = "local://"` from the embedded-agent era; we rewrite it to `unix:///var/run/nft-forward.sock` on next boot so the pusher / poller produce the same dial as for any other unix-socket node. We do the rewrite as a one-shot UPDATE inside the existing migrations machinery, not a special-case in the panel start path.

- [ ] **Step 1: Update poller to use daemonclient.GetCounters**

In `internal/server/poller.go`, delete the `LocalAddrPrefix` branch and any usage of `po.Pusher.Embedded`. The new `pollOne`:

```go
func (po *Poller) pollOne(n *db.Node) {
	c, err := daemonclient.New(n.Address, daemonclient.WithBearerToken(n.Secret))
	if err != nil {
		log.Printf("poller node=%d: %v", n.ID, err)
		return
	}
	counters, err := c.GetCounters()
	if err != nil {
		log.Printf("poller node=%d: %v", n.ID, err)
		return
	}
	_ = db.MarkNodeSeen(po.DB, n.ID)
	for _, c := range counters {
		f, err := db.GetForwardByNodeProtoPort(po.DB, n.ID, c.Proto, c.ListenPort)
		if err != nil {
			continue
		}
		delta, err := db.UpdateForwardBytes(po.DB, f.ID, int64(c.Bytes))
		if err != nil {
			log.Printf("poller: update forward %d: %v", f.ID, err)
			continue
		}
		if delta > 0 && f.TenantID.Valid {
			if err := db.AddTenantTraffic(po.DB, f.TenantID.Int64, delta); err != nil {
				log.Printf("poller: add tenant traffic: %v", err)
			}
		}
	}
}
```

Note: the inner `c` shadows the outer `c` (client). Rename the loop variable to `ct` to avoid that, e.g.:

```go
for _, ct := range counters {
    f, err := db.GetForwardByNodeProtoPort(po.DB, n.ID, ct.Proto, ct.ListenPort)
    ...
}
```

Drop the `Pusher` field on `Poller` if it was only used for `Pusher.Embedded`. (Keep it if it's still used in `enforceQuotas` — `enforceQuotas` references `po.Pusher.Schedule`, so we keep the field but stop touching `Embedded`.)

- [ ] **Step 2: Add the address migration**

First, locate the migration mechanism. Run:

```bash
grep -RIn "CREATE TABLE" internal/db/
grep -RIn "ALTER TABLE\|migration\|schema_version" internal/db/
```

Read whichever file owns migrations (likely `internal/db/migrations.go` or similar). Add a one-time UPDATE in the appropriate "bump version N -> N+1" function, or as an idempotent statement run at every boot if the project uses idempotent migrations:

```sql
UPDATE nodes SET address = 'unix:///var/run/nft-forward.sock'
WHERE address = 'local://';
```

The exact Go wrapper depends on what's already there; follow the existing pattern. Wherever you add it, include a short WHY comment:

```go
// Earlier panel versions registered the local host with the sentinel
// scheme "local://" and dispatched on it in-process. The local node is now
// reached through the daemon's unix socket like any other node; this UPDATE
// rewrites the address on first boot of the new build.
```

- [ ] **Step 3: Verify**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/server internal/db
git commit -m "server: poll counters via daemonclient; rewrite local node address to unix socket"
```

---

### Task 9: cmd consolidation, daemon listen mode, delete legacy entrypoints

**Files:**
- Modify: `cmd/nft-forward/main.go`
- Modify: `internal/daemon/daemon.go` (add HTTP listener wiring)
- Modify: `internal/daemon/handlers.go` (bearer auth middleware)
- Modify: `internal/daemon/handlers_test.go` (cover the bearer-protected HTTP listener)
- Delete: `cmd/nft-server/` (entire directory)
- Delete: `cmd/nft-agent/` (entire directory)
- Delete: `internal/agent/` (entire directory)

**Context:** This is the biggest task by scope but mechanical. We collapse three cmd directories into one and remove `internal/agent` whose responsibilities were absorbed in Tasks 2–5. The daemon gains a `--listen` flag that brings up an HTTP server alongside the unix socket; the HTTP listener wraps the same `Handler()` with bearer-token authn. We also delete the dead helpers in `cmd/nft-forward/main.go` (`preflight`, `promptPersist`, `promptYes`, `runApply`, `runInstallService`, `runUninstall`) confirmed unused after the daemon migration.

Do this in three sub-steps with a commit each to keep diffs reviewable.

#### 9a. Daemon listen mode + bearer auth

- [ ] **Step 1: Write the failing test**

In `internal/daemon/handlers_test.go`:

```go
func TestHTTPListenerRequiresBearerToken(t *testing.T) {
	d := newTestDaemon(t)
	d.httpToken = "shhh"
	handler := d.httpHandler() // wraps Handler() with bearer middleware

	// Missing token.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d", w.Code)
	}

	// Wrong token.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer nope")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d", w.Code)
	}

	// Right token.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer shhh")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("right token: status = %d", w.Code)
	}
}
```

- [ ] **Step 2: Implement bearer middleware**

In `internal/daemon/daemon.go`:

- Add `httpListen string` and `httpToken string` fields on `Daemon`.
- Add `HTTPListen` and `TokenPath` to `Config`.
- In `New`, if `cfg.TokenPath != ""`, read it: `tok, err := os.ReadFile(cfg.TokenPath); ... d.httpToken = strings.TrimSpace(string(tok))`. Return an error if the file can't be read or the token is empty — booting with HTTP enabled but no token is a configuration bug we should surface loudly.

Add a method:

```go
func (d *Daemon) httpHandler() http.Handler {
	inner := d.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if len(got) <= len(prefix) || got[:len(prefix)] != prefix {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		expect := []byte(d.httpToken)
		actual := []byte(got[len(prefix):])
		if len(expect) != len(actual) || subtle.ConstantTimeCompare(expect, actual) != 1 {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
}
```

Imports: `crypto/subtle`.

In `Run(ctx)`, after `http.Serve(ln, d.Handler())` (the unix socket server), if `d.httpListen != ""` start a second listener:

```go
if d.httpListen != "" {
    go func() {
        srv := &http.Server{
            Addr:              d.httpListen,
            Handler:           d.httpHandler(),
            ReadHeaderTimeout: 5 * time.Second,
        }
        log.Printf("nft-forward daemon listening on %s (http)", d.httpListen)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Printf("http listener: %v", err)
        }
    }()
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 4: Commit (sub-step 9a)**

```bash
git add internal/daemon
git commit -m "daemon: optionally accept remote pushes on a bearer-protected HTTP listener"
```

#### 9b. cmd/nft-forward absorbs server, daemon gains --listen flag

- [ ] **Step 1: Read existing dispatch**

`cmd/nft-forward/main.go` already has `runDaemon` for the `daemon` subcommand and the default-TUI path. Add `runServer` modelled on `cmd/nft-server/main.go`, dropping the embedded-agent bootstrap entirely. The local-node row in the DB is now `unix:///var/run/nft-forward.sock` (Task 8 migration); the server doesn't need to call `nft.Apply`/`tc.Apply` anywhere.

The new dispatcher:

```go
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			os.Exit(runDaemon(os.Args[2:]))
		case "server":
			os.Exit(runServer(os.Args[2:]))
		case "apply":
			os.Exit(runApplyCompat(os.Args[2:])) // see below
		}
	}
	runTUI()
}
```

`runDaemon` flags (extending what's there):

```go
fs := flag.NewFlagSet("daemon", flag.ExitOnError)
fs.StringVar(&socketPath, "socket", daemon.DefaultSocketPath, "unix socket path")
fs.StringVar(&statePath,  "state",  daemon.DefaultStatePath,  "state file path")
fs.StringVar(&groupName,  "group",  daemon.DefaultGroupName,  "socket group owner")
fs.StringVar(&iface,      "iface",  "",                       "tc data-plane iface (auto-detect if empty)")
fs.StringVar(&httpListen, "listen", "",                       "additionally serve HTTP on this address for remote pushes")
fs.StringVar(&tokenFile,  "token-file", "/etc/nft-forward/daemon.token", "bearer token file (required when --listen is set)")
fs.Parse(args)

cfg := daemon.Config{
    SocketPath: socketPath, StatePath: statePath, GroupName: groupName,
    Iface: iface, HTTPListen: httpListen,
}
if httpListen != "" {
    cfg.TokenPath = tokenFile
}
return daemon.New(cfg).RunWithSignals()
```

(Adjust to the exact signatures already in your codebase. Keep root check + sysdeps/nft preflight where it is.)

`runServer` (new):

```go
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
```

Copy `bootstrap` and `runResetAdmin` verbatim from `cmd/nft-server/main.go`. They become package-level functions in `cmd/nft-forward/main.go`.

`runApplyCompat`: this preserves the `nft-forward apply` entrypoint used by older systemd units. It reads rules from a path (default `/etc/nft-forward/rules.json`), opens a `daemonclient` to `DefaultSocketPath`, and POSTs them as owner `tui`. If you don't have time to write this in this sub-step, leave the original `runApply` in place but rewire it to use daemonclient (don't touch the legacy preflight helpers yet — they go in 9c).

- [ ] **Step 2: Verify compile + tests**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 3: Commit (sub-step 9b)**

```bash
git add cmd/nft-forward
git commit -m "cmd: fold server subcommand into single binary; daemon learns --listen for remote pushes"
```

#### 9c. Delete legacy directories + dead helpers

- [ ] **Step 1: Delete files / directories**

```bash
rm -rf cmd/nft-server cmd/nft-agent internal/agent
```

In `cmd/nft-forward/main.go`, delete:

- `preflight()`
- `promptPersist()`
- `promptYes()`
- `runInstallService()`
- `runUninstall()`
- The old `runApply` if you replaced it in 9b; otherwise rewrite it to use daemonclient now.

Search for any remaining references:

```bash
grep -RIn "nft-forward/internal/agent" .
grep -RIn "LocalAddrPrefix\|local://" --include='*.go' .
grep -RIn "preflight\|promptPersist\|promptYes" --include='*.go' .
```

The first command must return nothing. The second must return only the migration SQL in `internal/db` (the UPDATE that rewrites the value). The third must return nothing.

If `internal/server/server.go` (the constructor) still mentions an embedded agent param, fix the call site now.

- [ ] **Step 2: Verify build + tests**

Run: `go build ./... && go test ./...`
Expected: PASS, zero compilation errors.

- [ ] **Step 3: Commit (sub-step 9c)**

```bash
git add -A
git commit -m "drop standalone nft-agent and nft-server binaries now that the daemon owns the data plane"
```

---

### Task 10: refresh manual-verification doc

**Files:**
- Modify: `docs/daemon-manual-verification.md`

**Context:** The walkthrough today covers only the TUI / curl path. Add the panel + remote-daemon scenarios so an operator validating Phase D's behaviour has a script to follow on a real Linux host.

- [ ] **Step 1: Append the new scenarios**

Open `docs/daemon-manual-verification.md` and add a new section *"Server (panel) + daemon"* with:

1. Start daemon: `sudo nft-forward daemon`.
2. Start server: `sudo nft-forward server --addr :8080`.
3. Open `http://host:8080`, log in (admin password printed on first boot).
4. Confirm that `localhost` node is auto-registered with address `unix:///var/run/nft-forward.sock`.
5. Create a forward via the panel UI.
6. `curl --unix-socket /var/run/nft-forward.sock http://daemon/v1/ruleset` must show the rule under owner `panel`.
7. In a second terminal start `nft-forward` (TUI). Add one more rule.
8. `curl ... /v1/ruleset` must show both owners with their respective rules.

Add *"Remote daemon (HTTP) role"*:

1. On the panel host: `nft-forward server --addr :8080`.
2. On a second host: `sudo nft-forward daemon --listen :7878 --token-file /etc/nft-forward/daemon.token` (with a token of your choice written to that file).
3. On the panel: register a node with address `http://second-host:7878` and the same token as secret.
4. Add a forward on that node via the panel.
5. `curl --header "Authorization: Bearer ..." http://second-host:7878/v1/ruleset` shows the pushed rule under owner `panel`.

Remove the "Known limits" line that said "TUI 接入 daemon completed; only server/agent remain on old paths" — they're done now. Replace with a "Known limits" pointing to the next phase (install.sh refactor).

- [ ] **Step 2: Commit**

```bash
git add docs/daemon-manual-verification.md
git commit -m "docs: extend manual verification with panel and remote-daemon scenarios"
```

---

## Self-review pass

After all tasks land, the implementer should run the controller-level checklist:

1. `grep -RIn "nft-forward/internal/agent" .` returns nothing.
2. `grep -RIn "LocalAddrPrefix" --include='*.go' .` returns nothing.
3. `grep -RIn "Phase\|Task #\|Step [0-9]\|Round [0-9]" $(git log --name-only --pretty=format: deea095..HEAD | sort -u | grep '\.go$')` returns nothing — code comments must be free of process metadata. Same for KDoc.
4. `git log deea095..HEAD --format=%B | grep -iE 'phase|task #|step [0-9]|round [0-9]'` returns nothing — commit messages must be free of process metadata.
5. `go test ./... -count=1` is green.
6. `go vet ./...` is clean.
7. Manual smoke: `sudo nft-forward daemon &` then `sudo nft-forward server --addr :8080`; verify the panel comes up, the local node uses `unix://` address, and adding a forward applies through nftables on the host.

If any check fails: fix and amend the offending commit's *successor* (never amend) per CLAUDE.md.

---

## Out of scope (defer to next phase)

- `install.sh` refactor (single-binary, daemon-first, role-stacked).
- Retire the legacy `nft-forward.service` unit.
- Rewrite `README.md` for the new run modes.
- Consider whether `internal/store` still has callers after this phase; if not, remove it.
- `POST /v1/resolve` endpoint from the design spec. The endpoint was envisaged as a pre-resolve helper for clients, but with the daemon resolving on every apply and on the refresh tick no current client needs it. Add it when a concrete consumer appears.
